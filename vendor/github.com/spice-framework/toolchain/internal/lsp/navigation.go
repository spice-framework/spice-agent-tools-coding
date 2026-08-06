package lsp

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spice-framework/spice/annotation/sdk"
	"github.com/spice-framework/toolchain/compiler/diagnostic"
	compilerservice "github.com/spice-framework/toolchain/compiler/service"
)

const (
	annotationReferencePath = "docs/annotations.md"
	annotationReferenceURL  = "https://github.com/spice-framework/spice/blob/main/docs/annotations.md#built-in-definitions-and-targets"
	maxReferenceBytes       = 2 << 20
)

type protocolDocumentLink struct {
	Range   protocolRange `json:"range"`
	Target  string        `json:"target"`
	Tooltip string        `json:"tooltip,omitempty"`
}

type protocolLocationLink struct {
	OriginSelectionRange protocolRange `json:"originSelectionRange"`
	TargetURI            string        `json:"targetUri"`
	TargetRange          protocolRange `json:"targetRange"`
	TargetSelectionRange protocolRange `json:"targetSelectionRange"`
}

type annotationOccurrence struct {
	name  string
	start int
	end   int
}

type annotationReference struct {
	uri       string
	rangeItem protocolRange
}

type goInterfaceReference struct {
	contract compilerservice.GoInterface
	start    int
	end      int
}

func (server *Server) definition(message rpcMessage) error {
	if !message.request() {
		return nil
	}
	var params textDocumentPositionParams
	if err := decodeParams(message.Params, &params); err != nil {
		return server.writer.failure(
			message.ID,
			invalidParamsCode,
			err.Error(),
		)
	}
	source, metadata, found := server.featureSnapshot(
		params.TextDocument.URI,
	)
	if !found {
		return server.writer.response(message.ID, []protocolLocationLink{})
	}
	offset, valid := byteOffset(source.content, params.Position)
	if !valid {
		return server.writer.response(message.ID, []protocolLocationLink{})
	}
	if reference, interfaceFound := goInterfaceReferenceAt(
		source,
		metadata,
		offset,
	); interfaceFound && reference.contract.HasLocation {
		origin := protocolRangeAtOffsets(
			source.content,
			reference.start,
			reference.end,
		)
		return server.writer.response(message.ID, []protocolLocationLink{{
			OriginSelectionRange: origin,
			TargetURI:            reference.contract.Location.URI,
			TargetRange: protocolRangeFromLocation(
				reference.contract.Location,
			),
			TargetSelectionRange: protocolRangeFromLocation(
				reference.contract.Location,
			),
		}})
	}
	occurrence, found := annotationOccurrenceAt(source.content, offset)
	if !found {
		return server.writer.response(message.ID, []protocolLocationLink{})
	}
	definition, found := definitionForOccurrence(
		source,
		metadata,
		occurrence,
	)
	if !found {
		return server.writer.response(message.ID, []protocolLocationLink{})
	}
	if definition.HasDescriptorLocation {
		return server.writer.response(message.ID, []protocolLocationLink{
			locationLink(source.content, occurrence, definition.DescriptorLocation),
		})
	}
	reference, found := localAnnotationReference(source.root, definition.Name)
	if !found {
		return server.writer.response(message.ID, []protocolLocationLink{})
	}
	return server.writer.response(message.ID, []protocolLocationLink{{
		OriginSelectionRange: protocolRangeAtOffsets(
			source.content,
			occurrence.start,
			occurrence.end,
		),
		TargetURI:            reference.uri,
		TargetRange:          reference.rangeItem,
		TargetSelectionRange: reference.rangeItem,
	}})
}

func goInterfaceReferenceAt(
	source document,
	metadata metadataView,
	offset int,
) (goInterfaceReference, bool) {
	context := inspectCompletionContext(source.content, offset)
	if context.kind != completionArgument {
		return goInterfaceReference{}, false
	}
	definitions := fileScopedDefinitions(
		source.content,
		metadata.definitions,
	)
	definition, found := annotationDefinition(
		definitions,
		context.annotation,
	)
	if !found {
		return goInterfaceReference{}, false
	}
	argument, found := findCompletionArgument(
		definition.Arguments,
		context.argument,
	)
	if !found ||
		argument.ValueDomain != sdk.ValueDomainGoInterface {
		return goInterfaceReference{}, false
	}
	start, end, expression, found := qualifiedGoTypeAt(
		source.content,
		offset,
	)
	if !found {
		return goInterfaceReference{}, false
	}
	qualifier, name, qualified := strings.Cut(expression, ".")
	if !qualified {
		name = qualifier
		qualifier = ""
	}
	sourcePackage := interfaceSourcePackage(
		metadata.goInterfaces,
		source.path,
	)
	imports := inspectGoImports(source.content)
	namespaces := inspectSpiceNamespaces(source.content)
	for _, pkg := range metadata.goInterfaces.Packages {
		if !interfacePackageMatches(
			pkg,
			qualifier,
			sourcePackage,
			imports,
			namespaces,
		) {
			continue
		}
		for _, contract := range pkg.Interfaces {
			if contract.Name == name {
				return goInterfaceReference{
					contract: contract,
					start:    start,
					end:      end,
				}, true
			}
		}
	}
	return goInterfaceReference{}, false
}

func interfacePackageMatches(
	pkg compilerservice.GoInterfacePackage,
	qualifier string,
	sourcePackage string,
	imports goImportState,
	namespaces spiceNamespaceState,
) bool {
	if qualifier == "" {
		return pkg.Path == sourcePackage
	}
	if namespace, found := namespaces.byPath[pkg.Path]; found {
		return namespace == qualifier
	}
	alias, found := imports.byPath[pkg.Path]
	if !found {
		return false
	}
	if alias != "" {
		return alias == qualifier
	}
	return pkg.Name == qualifier
}

func qualifiedGoTypeAt(
	content []byte,
	offset int,
) (int, int, string, bool) {
	start, end, found := goTypeReferenceBounds(content, offset)
	if !found {
		return 0, 0, "", false
	}
	value := string(content[start:end])
	if !validQualifiedGoType(value) {
		return 0, 0, "", false
	}
	return start, end, value, true
}

func goTypeReferenceBounds(
	content []byte,
	offset int,
) (int, int, bool) {
	if offset < 0 || offset > len(content) {
		return 0, 0, false
	}
	cursor := offset
	if cursor == len(content) ||
		cursor > 0 && !goTypeReferenceByte(content[cursor]) {
		cursor--
	}
	if cursor < 0 || cursor >= len(content) ||
		!goTypeReferenceByte(content[cursor]) {
		return 0, 0, false
	}
	start := cursor
	for start > 0 && goTypeReferenceByte(content[start-1]) {
		start--
	}
	end := cursor + 1
	for end < len(content) && goTypeReferenceByte(content[end]) {
		end++
	}
	return start, end, true
}

func validQualifiedGoType(value string) bool {
	if strings.Count(value, ".") > 1 ||
		strings.HasPrefix(value, ".") ||
		strings.HasSuffix(value, ".") {
		return false
	}
	for segment := range strings.SplitSeq(value, ".") {
		if !validGoIdentifier(segment) {
			return false
		}
	}
	return true
}

func goTypeReferenceByte(value byte) bool {
	return value == '.' ||
		value == '_' ||
		value >= 'a' && value <= 'z' ||
		value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9'
}

func validGoIdentifier(value string) bool {
	if value == "" ||
		value[0] >= '0' && value[0] <= '9' {
		return false
	}
	for index := range len(value) {
		if value[index] == '.' || !goTypeReferenceByte(value[index]) {
			return false
		}
	}
	return true
}

func (server *Server) implementation(message rpcMessage) error {
	if !message.request() {
		return nil
	}
	var params textDocumentPositionParams
	if err := decodeParams(message.Params, &params); err != nil {
		return server.writer.failure(message.ID, invalidParamsCode, err.Error())
	}
	source, metadata, found := server.featureSnapshot(params.TextDocument.URI)
	if !found {
		return server.writer.response(message.ID, []protocolLocationLink{})
	}
	offset, valid := byteOffset(source.content, params.Position)
	if !valid {
		return server.writer.response(message.ID, []protocolLocationLink{})
	}
	occurrence, found := annotationOccurrenceAt(source.content, offset)
	if !found {
		return server.writer.response(message.ID, []protocolLocationLink{})
	}
	definition, found := definitionForOccurrence(source, metadata, occurrence)
	if !found || !definition.Implementation.HasLocation {
		return server.writer.response(message.ID, []protocolLocationLink{})
	}
	return server.writer.response(message.ID, []protocolLocationLink{
		locationLink(
			source.content,
			occurrence,
			definition.Implementation.Location,
		),
	})
}

func (server *Server) documentLinks(message rpcMessage) error {
	if !message.request() {
		return nil
	}
	var params semanticTokensParams
	if err := decodeParams(message.Params, &params); err != nil {
		return server.writer.failure(
			message.ID,
			invalidParamsCode,
			err.Error(),
		)
	}
	source, metadata, found := server.featureSnapshot(
		params.TextDocument.URI,
	)
	if !found {
		return server.writer.response(message.ID, []protocolDocumentLink{})
	}
	links := make([]protocolDocumentLink, 0)
	for _, occurrence := range annotationOccurrences(source.content) {
		definition, found := definitionForOccurrence(
			source,
			metadata,
			occurrence,
		)
		if !found {
			continue
		}
		var target string
		if definition.HasDescriptorLocation {
			target = locationTarget(definition.DescriptorLocation)
		} else if reference, local := localAnnotationReference(
			source.root,
			definition.Name,
		); local {
			target = localReferenceTarget(reference)
		} else {
			target = annotationReferenceURL
		}
		tooltip := fmt.Sprintf(
			"Open the @%s annotation definition",
			occurrence.name,
		)
		if definition.DescriptorPackage != "" {
			tooltip = fmt.Sprintf(
				"Open %s.%s",
				definition.DescriptorPackage,
				definition.DescriptorSymbol,
			)
		}
		links = append(links, protocolDocumentLink{
			Range: protocolRangeAtOffsets(
				source.content,
				occurrence.start,
				occurrence.end,
			),
			Target:  target,
			Tooltip: tooltip,
		})
	}
	return server.writer.response(message.ID, links)
}

func definitionForOccurrence(
	source document,
	metadata metadataView,
	occurrence annotationOccurrence,
) (compilerservice.AnnotationDefinition, bool) {
	position := protocolPositionAtOffset(source.content, occurrence.start)
	for _, summary := range metadata.annotations {
		if summary.Spelling != occurrence.name ||
			pathKey(summary.Location.Path) != pathKey(source.path) {
			continue
		}
		start := summary.Location.Range.Start
		if start.Offset != occurrence.start &&
			(start.Line != position.Line+1 ||
				start.Column != position.Character+1) {
			continue
		}
		for _, definition := range metadata.definitions {
			if summary.DefinitionPackage != "" &&
				definition.DescriptorPackage == summary.DefinitionPackage &&
				definition.DescriptorSymbol == summary.DefinitionSymbol {
				return definition, true
			}
			if summary.DefinitionPackage == "" &&
				definition.Name == summary.Name {
				return definition, true
			}
		}
	}
	definitions := fileScopedDefinitions(source.content, metadata.definitions)
	return annotationDefinition(definitions, occurrence.name)
}

func locationLink(
	source []byte,
	occurrence annotationOccurrence,
	target diagnostic.Location,
) protocolLocationLink {
	return protocolLocationLink{
		OriginSelectionRange: protocolRangeAtOffsets(
			source,
			occurrence.start,
			occurrence.end,
		),
		TargetURI:            target.URI,
		TargetRange:          protocolRangeFromLocation(target),
		TargetSelectionRange: protocolRangeFromLocation(target),
	}
}

func protocolRangeFromLocation(location diagnostic.Location) protocolRange {
	return protocolRange{
		Start: protocolPosition{
			Line:      max(location.Range.Start.Line-1, 0),
			Character: max(location.Range.Start.Column-1, 0),
		},
		End: protocolPosition{
			Line:      max(location.Range.End.Line-1, 0),
			Character: max(location.Range.End.Column-1, 0),
		},
	}
}

func locationTarget(location diagnostic.Location) string {
	position := location.Range.Start
	return fmt.Sprintf(
		"%s#%d,%d",
		strings.TrimSuffix(location.URI, "#"),
		position.Line,
		position.Column,
	)
}

func annotationOccurrences(content []byte) []annotationOccurrence {
	var result []annotationOccurrence
	for start := 0; start <= len(content); {
		relativeEnd := bytes.IndexByte(content[start:], '\n')
		last := relativeEnd < 0
		end := len(content)
		if !last {
			end = start + relativeEnd
		}
		line := bytes.TrimSuffix(content[start:end], []byte{'\r'})
		nameStart, nameEnd, found := annotationNameBounds(line)
		if found {
			result = append(result, annotationOccurrence{
				name:  string(line[nameStart+1 : nameEnd]),
				start: start + nameStart,
				end:   start + nameEnd,
			})
		}
		if last {
			break
		}
		start = end + 1
	}
	return result
}

func annotationOccurrenceAt(
	content []byte,
	offset int,
) (annotationOccurrence, bool) {
	for _, occurrence := range annotationOccurrences(content) {
		if offset >= occurrence.start && offset <= occurrence.end {
			return occurrence, true
		}
	}
	return annotationOccurrence{}, false
}

func knownAnnotation(
	definitions []compilerservice.AnnotationDefinition,
	name string,
) bool {
	for _, definition := range definitions {
		if definition.Name == name {
			return true
		}
	}
	return false
}

func localAnnotationReference(
	root string,
	name string,
) (annotationReference, bool) {
	if root == "" || name == "" {
		return annotationReference{}, false
	}
	path := filepath.Join(root, filepath.FromSlash(annotationReferencePath))
	info, err := os.Stat(path)
	if err != nil ||
		!info.Mode().IsRegular() ||
		info.Size() > maxReferenceBytes {
		return annotationReference{}, false
	}
	content, err := os.ReadFile(filepath.Clean(path))
	if err != nil || len(content) > maxReferenceBytes {
		return annotationReference{}, false
	}
	marker := []byte("| `@" + name + "` |")
	offset := bytes.Index(content, marker)
	if offset < 0 {
		return annotationReference{}, false
	}
	start := offset + len("| `")
	end := start + len("@"+name)
	uri, err := fileURI(path)
	if err != nil {
		return annotationReference{}, false
	}
	return annotationReference{
		uri:       uri,
		rangeItem: protocolRangeAtOffsets(content, start, end),
	}, true
}

func localReferenceTarget(reference annotationReference) string {
	position := reference.rangeItem.Start
	return fmt.Sprintf(
		"%s#%d,%d",
		strings.TrimSuffix(reference.uri, "#"),
		position.Line+1,
		position.Character+1,
	)
}
