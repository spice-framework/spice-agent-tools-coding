package lsp

import (
	"bytes"
	"context"
	"fmt"
	goparser "go/parser"
	"go/token"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/spice-framework/spice/annotation"
	"github.com/spice-framework/spice/annotation/sdk"
	"github.com/spice-framework/toolchain/compiler/diagnostic"
	annotationparser "github.com/spice-framework/toolchain/compiler/parser"
	compilerservice "github.com/spice-framework/toolchain/compiler/service"
)

type textDocumentPositionParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Position     protocolPosition       `json:"position"`
}

type codeActionParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Range        protocolRange          `json:"range"`
	Context      struct {
		Diagnostics []protocolDiagnostic `json:"diagnostics"`
		Only        []string             `json:"only,omitempty"`
	} `json:"context"`
}

type completionItem struct {
	Label            string         `json:"label"`
	Kind             int            `json:"kind,omitempty"`
	Detail           string         `json:"detail,omitempty"`
	Documentation    *markupContent `json:"documentation,omitempty"`
	SortText         string         `json:"sortText,omitempty"`
	FilterText       string         `json:"filterText,omitempty"`
	InsertTextFormat int            `json:"insertTextFormat,omitempty"`
	Preselect        bool           `json:"preselect,omitempty"`
	TextEdit         protocolEdit   `json:"textEdit"`
	AdditionalEdits  []protocolEdit `json:"additionalTextEdits,omitempty"`
}

type markupContent struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

type protocolEdit struct {
	Range   protocolRange `json:"range"`
	NewText string        `json:"newText"`
}

type completionList struct {
	IsIncomplete bool             `json:"isIncomplete"`
	Items        []completionItem `json:"items"`
}

type hoverResult struct {
	Contents markupContent  `json:"contents"`
	Range    *protocolRange `json:"range,omitempty"`
}

type signatureHelpResult struct {
	Signatures      []signatureInformation `json:"signatures"`
	ActiveSignature int                    `json:"activeSignature,omitempty"`
	ActiveParameter int                    `json:"activeParameter,omitempty"`
}

type signatureInformation struct {
	Label         string                 `json:"label"`
	Documentation *markupContent         `json:"documentation,omitempty"`
	Parameters    []parameterInformation `json:"parameters,omitempty"`
}

type parameterInformation struct {
	Label         string         `json:"label"`
	Documentation *markupContent `json:"documentation,omitempty"`
}

type protocolCodeAction struct {
	Title       string                 `json:"title"`
	Kind        string                 `json:"kind"`
	IsPreferred bool                   `json:"isPreferred"`
	Edit        *protocolWorkspaceEdit `json:"edit,omitempty"`
	Command     *protocolCommand       `json:"command,omitempty"`
}

type protocolCommand struct {
	Title     string `json:"title"`
	Command   string `json:"command"`
	Arguments []any  `json:"arguments,omitempty"`
}

type protocolWorkspaceEdit struct {
	DocumentChanges []protocolDocumentEdit `json:"documentChanges"`
}

type protocolDocumentEdit struct {
	TextDocument protocolOptionalVersionedDocument `json:"textDocument"`
	Edits        []protocolEdit                    `json:"edits"`
}

type protocolOptionalVersionedDocument struct {
	URI     string `json:"uri"`
	Version int    `json:"version"`
}

type metadataView struct {
	definitions    []compilerservice.AnnotationDefinition
	annotations    []compilerservice.Annotation
	providers      []compilerservice.Provider
	modules        []compilerservice.Module
	configurations []compilerservice.Configuration
	enums          []compilerservice.Enum
	goInterfaces   compilerservice.GoInterfaceCatalog
	actions        []diagnostic.SuggestedFix
	sourcePath     string
}

// Annotation catalog discovery may start an offline Go package load on a cold
// workspace. Keep the request bounded, but leave enough headroom for the race
// detector, Windows process startup, and large module graphs so completion
// does not silently degrade to an empty catalog under ordinary load.
const annotationCatalogTimeout = 15 * time.Second

func (server *Server) completion(message rpcMessage) error {
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
		return server.writer.response(
			message.ID,
			completionList{Items: []completionItem{}},
		)
	}
	offset, valid := byteOffset(source.content, params.Position)
	if !valid {
		return server.writer.failure(
			message.ID,
			invalidParamsCode,
			"completion position is outside the current document",
		)
	}
	completionContext := inspectCompletionContext(source.content, offset)
	if catalogCompletionContext(completionContext.kind) {
		metadata.definitions = server.catalogCompletionDefinitions(
			source.root,
			metadata.definitions,
		)
	}
	items := completionItems(source.content, offset, metadata)
	return server.writer.response(
		message.ID,
		completionList{Items: items},
	)
}

func catalogCompletionContext(kind completionContextKind) bool {
	switch kind {
	case completionAnnotation, completionImportSymbol, completionImportPath:
		return true
	case completionNone, completionArgument, completionConfiguration:
		return false
	}
	return false
}

func (server *Server) catalogCompletionDefinitions(
	root string,
	current []compilerservice.AnnotationDefinition,
) []compilerservice.AnnotationDefinition {
	server.mu.Lock()
	workspace := server.workspaces[root]
	if workspace == nil {
		workspace = server.workspaces[pathKey(root)]
	}
	var compiler *compilerservice.Service
	if workspace != nil {
		compiler = workspace.service
	}
	done := server.done
	server.mu.Unlock()
	if compiler == nil {
		return current
	}
	select {
	case <-done:
		return current
	default:
	}
	ctx, cancel := context.WithTimeout(
		context.Background(),
		annotationCatalogTimeout,
	)
	defer cancel()
	catalog, err := compiler.AnnotationCatalog(ctx, root)
	if err != nil {
		return current
	}
	return mergeAnnotationDefinitions(current, catalog)
}

func mergeAnnotationDefinitions(
	current []compilerservice.AnnotationDefinition,
	catalog []compilerservice.AnnotationDefinition,
) []compilerservice.AnnotationDefinition {
	result := slices.Clone(current)
	positions := make(map[string]int, len(result))
	for index, definition := range result {
		key := annotationDefinitionKey(definition)
		if key != "" {
			positions[key] = index
		}
	}
	for _, candidate := range catalog {
		key := annotationDefinitionKey(candidate)
		if index, found := positions[key]; found {
			if result[index].Implementation.Tool ==
				candidate.Implementation.Tool {
				result[index].Implementation.Authorized = candidate.Implementation.Authorized
				result[index].Implementation.AuthorizationKnown = candidate.Implementation.AuthorizationKnown
			}
			continue
		}
		positions[key] = len(result)
		result = append(result, candidate)
	}
	return result
}

func annotationDefinitionKey(
	definition compilerservice.AnnotationDefinition,
) string {
	if definition.DescriptorPackage == "" ||
		definition.DescriptorSymbol == "" {
		return ""
	}
	return definition.DescriptorPackage + "\x00" +
		definition.DescriptorSymbol
}

func completionItems(
	content []byte,
	offset int,
	metadata metadataView,
) []completionItem {
	context := inspectCompletionContext(content, offset)
	switch context.kind {
	case completionNone:
		return []completionItem{}
	case completionAnnotation:
		return annotationCompletionItems(context, metadata.definitions, content)
	case completionArgument:
		metadata.definitions = fileScopedDefinitions(
			content,
			metadata.definitions,
		)
		if items, handled := goInterfaceCompletionItems(
			context,
			metadata,
			content,
		); handled {
			return items
		}
		return argumentCompletionItems(context, metadata, content)
	case completionConfiguration:
		return configurationCompletionItems(context, metadata, content)
	case completionImportSymbol:
		return annotationImportSymbolItems(
			context,
			metadata.definitions,
			content,
		)
	case completionImportPath:
		return annotationImportPathItems(
			context,
			metadata.definitions,
			content,
		)
	default:
		return []completionItem{}
	}
}

type completionContextKind uint8

const (
	completionNone completionContextKind = iota
	completionAnnotation
	completionArgument
	completionConfiguration
	completionImportSymbol
	completionImportPath
)

type completionContext struct {
	kind           completionContextKind
	annotation     string
	argument       string
	start          int
	end            int
	rawAnnotation  bool
	insideString   bool
	existingPrefix string
}

func inspectCompletionContext(content []byte, offset int) completionContext {
	lineStart, found := contentLineAtOffset(content, offset)
	if !found {
		return completionContext{kind: completionNone, start: offset, end: offset}
	}
	prefix := content[lineStart:offset]
	if importContext, found := inspectAnnotationImportContext(
		content,
		lineStart,
		offset,
	); found {
		return importContext
	}
	at := strings.LastIndexByte(string(prefix), '@')
	if at >= 0 {
		absoluteAt := lineStart + at
		raw, valid := annotationPrefix(prefix[:at])
		if !valid {
			return completionContext{
				kind:  completionNone,
				start: offset,
				end:   offset,
			}
		}
		after := string(content[absoluteAt+1 : offset])
		open := strings.IndexByte(after, '(')
		if open < 0 {
			return completionContext{
				kind:           completionAnnotation,
				start:          absoluteAt,
				end:            offset,
				rawAnnotation:  raw,
				existingPrefix: after,
			}
		}
		annotationName := strings.TrimSpace(after[:open])
		if validAnnotationName(annotationName) {
			argument, start, insideString := currentArgumentContext(
				content,
				absoluteAt+1+open+1,
				offset,
			)
			return completionContext{
				kind:         completionArgument,
				annotation:   annotationName,
				argument:     argument,
				start:        start,
				end:          offset,
				insideString: insideString,
			}
		}
	}
	start := wordStart(content, offset, configurationCharacter)
	return completionContext{
		kind:           completionConfiguration,
		start:          start,
		end:            offset,
		existingPrefix: string(content[start:offset]),
		insideString:   insideStringLiteral(content[lineStart:offset]),
	}
}

func inspectAnnotationImportContext(
	content []byte,
	lineStart int,
	offset int,
) (completionContext, bool) {
	prefix := string(content[lineStart:offset])
	directive := strings.Index(prefix, "@import")
	if directive < 0 {
		return completionContext{}, false
	}
	after := prefix[directive+len("@import"):]
	if from := strings.LastIndex(after, `from "`); from >= 0 {
		valueStart := lineStart + directive + len("@import") +
			from + len(`from "`)
		if !strings.Contains(string(content[valueStart:offset]), `"`) {
			return completionContext{
				kind:           completionImportPath,
				start:          valueStart,
				end:            offset,
				existingPrefix: string(content[valueStart:offset]),
				insideString:   true,
			}, true
		}
	}
	open := strings.LastIndexByte(after, '{')
	closingBrace := strings.LastIndexByte(after, '}')
	if open >= 0 && closingBrace < open {
		start := wordStart(content, offset, annotationCharacter)
		return completionContext{
			kind:           completionImportSymbol,
			start:          start,
			end:            offset,
			existingPrefix: string(content[start:offset]),
		}, true
	}
	return completionContext{}, true
}

func annotationPrefix(prefix []byte) (raw bool, valid bool) {
	trimmed := strings.TrimSpace(string(prefix))
	switch trimmed {
	case "":
		return true, true
	case "//":
		return false, true
	default:
		return false, false
	}
}

func validAnnotationName(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character != '.' &&
			character != '_' &&
			!unicode.IsLetter(character) &&
			!unicode.IsDigit(character) {
			return false
		}
	}
	return true
}

func currentArgumentContext(
	content []byte,
	start int,
	offset int,
) (string, int, bool) {
	segment := content[start:offset]
	insideString := insideStringLiteral(segment)
	word := wordStart(content, offset, argumentValueCharacter)
	beforeCursor := string(segment)
	if equals := strings.LastIndexByte(beforeCursor, '='); equals >= 0 {
		beforeEquals := beforeCursor[:equals]
		separator := strings.LastIndexByte(beforeEquals, ',')
		field := strings.TrimSpace(beforeEquals[separator+1:])
		return field, word, insideString
	}
	fieldStart := wordStart(content, offset, annotationCharacter)
	return "", fieldStart, insideString
}

func annotationCompletionItems(
	context completionContext,
	definitions []compilerservice.AnnotationDefinition,
	content []byte,
) []completionItem {
	itemRange := protocolRangeAtOffsets(content, context.start, context.end)
	candidates := annotationCompletionCandidates(content, definitions)
	items := make([]completionItem, 0, len(candidates))
	for _, candidate := range candidates {
		definition := candidate.definition
		if context.existingPrefix != "" &&
			!strings.HasPrefix(
				strings.ToLower(definition.Name),
				strings.ToLower(context.existingPrefix),
			) {
			continue
		}
		insert := "@" + annotationSnippet(definition)
		if context.rawAnnotation {
			insert = "// " + insert
		}
		documentation := markupContent{
			Kind:  "markdown",
			Value: annotationDocumentation(definition),
		}
		item := completionItem{
			Label:            "@" + definition.Name,
			Kind:             14,
			Detail:           annotationCompletionDetail(definition),
			Documentation:    &documentation,
			SortText:         definition.Name,
			FilterText:       "@" + definition.Name,
			InsertTextFormat: 2,
			TextEdit: protocolEdit{
				Range:   itemRange,
				NewText: insert,
			},
		}
		if candidate.importDirective != "" {
			item.AdditionalEdits = []protocolEdit{
				annotationImportEdit(content, candidate.importDirective),
			}
		}
		items = append(items, item)
	}
	return items
}

type annotationCompletionCandidate struct {
	definition      compilerservice.AnnotationDefinition
	importDirective string
}

func annotationCompletionCandidates(
	content []byte,
	definitions []compilerservice.AnnotationDefinition,
) []annotationCompletionCandidate {
	directives := importDirectives(content)
	var scoped []compilerservice.AnnotationDefinition
	if len(directives) != 0 {
		scoped = fileScopedDefinitions(content, definitions)
	}
	result := make(
		[]annotationCompletionCandidate,
		0,
		len(definitions)+len(scoped),
	)
	imported := make(map[string]struct{}, len(scoped))
	localNames := make(map[string]struct{}, len(scoped))
	for _, definition := range scoped {
		key := definition.DescriptorPackage + "\x00" +
			definition.DescriptorSymbol
		imported[key] = struct{}{}
		localNames[definition.Name] = struct{}{}
		result = append(result, annotationCompletionCandidate{
			definition: definition,
		})
	}
	for _, definition := range definitions {
		if definition.DescriptorPackage == "" ||
			definition.DescriptorSymbol == "" {
			if len(directives) == 0 {
				result = append(result, annotationCompletionCandidate{
					definition: definition,
				})
			}
			continue
		}
		key := definition.DescriptorPackage + "\x00" +
			definition.DescriptorSymbol
		if _, found := imported[key]; found {
			continue
		}
		local := definition.DescriptorSymbol
		directive := fmt.Sprintf(
			`// @import { %s } from "%s"`,
			definition.DescriptorSymbol,
			definition.DescriptorPackage,
		)
		if _, collision := localNames[local]; collision {
			namespace := path.Base(definition.DescriptorPackage)
			local = namespace + "." + definition.DescriptorSymbol
			directive = fmt.Sprintf(
				`// @import * as %s from "%s"`,
				namespace,
				definition.DescriptorPackage,
			)
		}
		definition.Name = local
		localNames[local] = struct{}{}
		result = append(result, annotationCompletionCandidate{
			definition:      definition,
			importDirective: directive,
		})
	}
	return result
}

func annotationImportEdit(
	content []byte,
	directive string,
) protocolEdit {
	offset, existing := annotationImportInsertionOffset(content)
	prefix := ""
	if !existing {
		prefix = "\n"
	}
	return protocolEdit{
		Range:   protocolRangeAtOffsets(content, offset, offset),
		NewText: prefix + directive + "\n",
	}
}

func annotationImportInsertionOffset(content []byte) (int, bool) {
	lastDirectiveEnd := -1
	packageEnd := 0
	lastGoImportEnd := 0
	inImportBlock := false
	for start := 0; start <= len(content); {
		relativeEnd := bytes.IndexByte(content[start:], '\n')
		last := relativeEnd < 0
		end := len(content)
		next := end
		if !last {
			end = start + relativeEnd
			next = end + 1
		}
		line := strings.TrimSpace(
			string(bytes.TrimSuffix(content[start:end], []byte{'\r'})),
		)
		switch {
		case strings.HasPrefix(line, "// @import "):
			lastDirectiveEnd = next
		case strings.HasPrefix(line, "package "):
			packageEnd = next
		case line == "import (":
			inImportBlock = true
			lastGoImportEnd = next
		case inImportBlock:
			lastGoImportEnd = next
			if line == ")" {
				inImportBlock = false
			}
		case strings.HasPrefix(line, "import "):
			lastGoImportEnd = next
		}
		if last {
			break
		}
		start = next
	}
	if lastDirectiveEnd >= 0 {
		return lastDirectiveEnd, true
	}
	if lastGoImportEnd > packageEnd {
		return lastGoImportEnd, false
	}
	return packageEnd, false
}

func annotationImportSymbolItems(
	context completionContext,
	definitions []compilerservice.AnnotationDefinition,
	content []byte,
) []completionItem {
	itemRange := protocolRangeAtOffsets(content, context.start, context.end)
	var items []completionItem
	seen := make(map[string]struct{})
	for _, definition := range definitions {
		if definition.DescriptorSymbol == "" ||
			definition.DescriptorPackage == "" ||
			context.existingPrefix != "" &&
				!strings.HasPrefix(
					strings.ToLower(definition.DescriptorSymbol),
					strings.ToLower(context.existingPrefix),
				) {
			continue
		}
		key := definition.DescriptorPackage + "\x00" +
			definition.DescriptorSymbol
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		documentation := markupContent{
			Kind:  "markdown",
			Value: annotationDocumentation(definition),
		}
		items = append(items, completionItem{
			Label:         definition.DescriptorSymbol,
			Kind:          3,
			Detail:        definition.DescriptorPackage,
			Documentation: &documentation,
			SortText: definition.DescriptorSymbol + "\x00" +
				definition.DescriptorPackage,
			TextEdit: protocolEdit{
				Range:   itemRange,
				NewText: definition.DescriptorSymbol,
			},
		})
	}
	sort.SliceStable(items, func(left, right int) bool {
		return items[left].SortText < items[right].SortText
	})
	return items
}

func annotationImportPathItems(
	context completionContext,
	definitions []compilerservice.AnnotationDefinition,
	content []byte,
) []completionItem {
	itemRange := protocolRangeAtOffsets(content, context.start, context.end)
	packages := make(map[string]compilerservice.AnnotationDefinition)
	for _, definition := range definitions {
		packagePath := definition.DescriptorPackage
		if packagePath == "" ||
			context.existingPrefix != "" &&
				!strings.HasPrefix(packagePath, context.existingPrefix) {
			continue
		}
		if _, found := packages[packagePath]; !found {
			packages[packagePath] = definition
		}
	}
	paths := make([]string, 0, len(packages))
	for packagePath := range packages {
		paths = append(paths, packagePath)
	}
	slices.Sort(paths)
	items := make([]completionItem, len(paths))
	for index, packagePath := range paths {
		definition := packages[packagePath]
		items[index] = completionItem{
			Label:    packagePath,
			Kind:     9,
			Detail:   annotationProvenanceDetail(definition),
			SortText: packagePath,
			TextEdit: protocolEdit{
				Range:   itemRange,
				NewText: packagePath,
			},
		}
	}
	return items
}

func annotationProvenanceDetail(
	definition compilerservice.AnnotationDefinition,
) string {
	var details []string
	version := definition.Provenance.Version
	if version == "" {
		version = "local"
	}
	if definition.Provenance.Module != "" {
		details = append(
			details,
			definition.Provenance.Module+"@"+version,
		)
	}
	if definition.Implementation.Tool != "" {
		details = append(
			details,
			"go tool "+definition.Implementation.Tool,
		)
	}
	if definition.Implementation.AuthorizationKnown {
		status := "tool not declared"
		if definition.Implementation.Authorized {
			status = "tool declared"
		}
		details = append(details, status)
	}
	return strings.Join(details, " · ")
}

func annotationCompletionDetail(
	definition compilerservice.AnnotationDefinition,
) string {
	var details []string
	if targets := annotationTargetDetail(definition); targets != "" {
		details = append(details, targets)
	}
	if definition.DescriptorPackage != "" {
		details = append(details, definition.DescriptorPackage)
	}
	if provenance := annotationProvenanceDetail(definition); provenance != "" {
		details = append(details, provenance)
	}
	return strings.Join(details, " · ")
}

func annotationSnippet(
	definition compilerservice.AnnotationDefinition,
) string {
	var required []compilerservice.AnnotationArgument
	for _, argument := range definition.Arguments {
		if argument.Required {
			required = append(required, argument)
		}
	}
	if len(required) == 0 {
		return definition.Name
	}
	values := make([]string, len(required))
	for index, argument := range required {
		value := argumentSnippet(argument, index+1)
		if argument.Positional {
			values[index] = value
		} else {
			values[index] = argument.Name + "=" + value
		}
	}
	return definition.Name + "(" + strings.Join(values, ", ") + ")"
}

func argumentSnippet(
	argument compilerservice.AnnotationArgument,
	tabstop int,
) string {
	if len(argument.AllowedStrings) != 0 {
		return fmt.Sprintf(
			`["${%d:%s}"]`,
			tabstop,
			argument.AllowedStrings[0],
		)
	}
	if slices.Contains(argument.Kinds, annotation.KindList) {
		return fmt.Sprintf(`["${%d:value}"]`, tabstop)
	}
	if slices.Contains(argument.Kinds, annotation.KindBoolean) {
		return fmt.Sprintf("${%d:true}", tabstop)
	}
	if slices.Contains(argument.Kinds, annotation.KindInteger) {
		return fmt.Sprintf("${%d:0}", tabstop)
	}
	return fmt.Sprintf(`"${%d:value}"`, tabstop)
}

type goImportState struct {
	byPath   map[string]string
	unusable map[string]struct{}
	occupied map[string]struct{}
}

type spiceNamespaceState struct {
	byPath   map[string]string
	occupied map[string]struct{}
}

func goInterfaceCompletionItems(
	context completionContext,
	metadata metadataView,
	content []byte,
) ([]completionItem, bool) {
	definition, found := annotationDefinition(
		metadata.definitions,
		context.annotation,
	)
	if !found {
		return nil, false
	}
	argument, found := findCompletionArgument(
		definition.Arguments,
		context.argument,
	)
	if !found || argument.ValueDomain != sdk.ValueDomainGoInterface {
		return nil, false
	}
	if context.insideString {
		return []completionItem{}, true
	}

	imports := inspectGoImports(content)
	namespaces := inspectSpiceNamespaces(content)
	sourcePackage := interfaceSourcePackage(
		metadata.goInterfaces,
		metadata.sourcePath,
	)
	prefix := strings.TrimSpace(
		string(content[context.start:context.end]),
	)
	itemRange := protocolRangeAtOffsets(content, context.start, context.end)
	var items []completionItem
	for _, pkg := range metadata.goInterfaces.Packages {
		for _, contract := range pkg.Interfaces {
			item, available := goInterfaceCompletionItem(
				content,
				imports,
				namespaces,
				pkg,
				contract,
				sourcePackage,
				prefix,
				itemRange,
			)
			if !available {
				continue
			}
			items = append(items, item)
		}
	}
	if len(items) != 0 {
		// Competing editor word completions cannot supply the required
		// namespace import. Ask clients to select the first deterministic
		// compiler-backed candidate so accepting completion applies the
		// annotation and its provenance edit as one operation.
		items[0].Preselect = true
	}
	return items, true
}

func goInterfaceCompletionItem(
	content []byte,
	imports goImportState,
	namespaces spiceNamespaceState,
	pkg compilerservice.GoInterfacePackage,
	contract compilerservice.GoInterface,
	sourcePackage string,
	prefix string,
	itemRange protocolRange,
) (completionItem, bool) {
	samePackage := pkg.Path != "" && pkg.Path == sourcePackage
	if !samePackage && !contract.Exported {
		return completionItem{}, false
	}
	qualifier, addImport, available := interfaceQualifier(
		imports,
		namespaces,
		pkg,
		samePackage,
	)
	if !available {
		return completionItem{}, false
	}
	lookup := contract.Name
	if qualifier != "" {
		lookup = qualifier + "." + contract.Name
	}
	if !interfacePrefixMatches(prefix, lookup, contract.Name) {
		return completionItem{}, false
	}
	documentation := markupContent{
		Kind:  "markdown",
		Value: goInterfaceDocumentation(contract),
	}
	item := completionItem{
		Label:            lookup,
		Kind:             8,
		Detail:           contract.PackagePath,
		Documentation:    &documentation,
		SortText:         "0000-" + lookup + "\x00" + contract.PackagePath,
		FilterText:       lookup + " " + contract.Name,
		InsertTextFormat: 2,
		TextEdit: protocolEdit{
			Range: itemRange,
			NewText: goInterfaceInsertText(
				lookup,
				contract.TypeParameters,
			),
		},
	}
	if addImport {
		item.AdditionalEdits = []protocolEdit{
			annotationImportEdit(
				content,
				fmt.Sprintf(
					`// @import * as %s from "%s"`,
					qualifier,
					contract.PackagePath,
				),
			),
		}
	}
	return item, true
}

func inspectSpiceNamespaces(content []byte) spiceNamespaceState {
	state := spiceNamespaceState{
		byPath:   make(map[string]string),
		occupied: make(map[string]struct{}),
	}
	for _, directive := range importDirectives(content) {
		if directive.Kind != annotation.ImportNamespace {
			continue
		}
		state.byPath[directive.Package] = directive.Namespace
		state.occupied[directive.Namespace] = struct{}{}
	}
	return state
}

func findCompletionArgument(
	arguments []compilerservice.AnnotationArgument,
	name string,
) (compilerservice.AnnotationArgument, bool) {
	for _, argument := range arguments {
		if name != "" && argument.Name == name ||
			name == "" && argument.Positional {
			return argument, true
		}
	}
	return compilerservice.AnnotationArgument{}, false
}

func interfaceSourcePackage(
	catalog compilerservice.GoInterfaceCatalog,
	sourcePath string,
) string {
	if sourcePath == "" {
		return ""
	}
	sourceKey := pathKey(sourcePath)
	for _, pkg := range catalog.Packages {
		for _, file := range pkg.Files {
			if pathKey(file) == sourceKey {
				return pkg.Path
			}
		}
	}
	return ""
}

func inspectGoImports(content []byte) goImportState {
	state := goImportState{
		byPath:   make(map[string]string),
		unusable: make(map[string]struct{}),
		occupied: make(map[string]struct{}),
	}
	fileSet := token.NewFileSet()
	file, err := goparser.ParseFile(
		fileSet,
		"",
		content,
		goparser.ImportsOnly|goparser.SkipObjectResolution,
	)
	if err != nil {
		return state
	}
	for _, specification := range file.Imports {
		packagePath, err := strconv.Unquote(specification.Path.Value)
		if err != nil {
			continue
		}
		alias := ""
		if specification.Name != nil {
			alias = specification.Name.Name
			if alias == "_" || alias == "." {
				state.unusable[packagePath] = struct{}{}
				continue
			}
		}
		state.byPath[packagePath] = alias
		if alias != "" {
			state.occupied[alias] = struct{}{}
			continue
		}
		if base := path.Base(packagePath); base != "." && base != "/" {
			state.occupied[base] = struct{}{}
		}
	}
	return state
}

func interfaceQualifier(
	imports goImportState,
	namespaces spiceNamespaceState,
	pkg compilerservice.GoInterfacePackage,
	samePackage bool,
) (qualifier string, addImport bool, available bool) {
	if samePackage {
		return "", false, true
	}
	if _, unusable := imports.unusable[pkg.Path]; unusable {
		return "", false, false
	}
	if alias, imported := imports.byPath[pkg.Path]; imported {
		if alias != "" {
			return alias, false, true
		}
		return pkg.Name, false, pkg.Name != ""
	}
	if namespace, imported := namespaces.byPath[pkg.Path]; imported {
		return namespace, false, namespace != ""
	}
	if pkg.Name == "" {
		return "", false, false
	}
	return availableGoQualifier(
		pkg.Name,
		mergedNamespaceOccupancy(imports, namespaces),
	), true, true
}

func mergedNamespaceOccupancy(
	imports goImportState,
	namespaces spiceNamespaceState,
) map[string]struct{} {
	occupied := make(
		map[string]struct{},
		len(imports.occupied)+len(namespaces.occupied),
	)
	for name := range imports.occupied {
		occupied[name] = struct{}{}
	}
	for name := range namespaces.occupied {
		occupied[name] = struct{}{}
	}
	return occupied
}

func availableGoQualifier(
	base string,
	occupied map[string]struct{},
) string {
	if _, used := occupied[base]; !used {
		return base
	}
	for suffix := 2; ; suffix++ {
		candidate := fmt.Sprintf("%s%d", base, suffix)
		if _, used := occupied[candidate]; !used {
			return candidate
		}
	}
}

func interfacePrefixMatches(prefix, lookup, name string) bool {
	if prefix == "" {
		return true
	}
	prefix = strings.ToLower(prefix)
	if strings.Contains(prefix, ".") {
		return strings.HasPrefix(strings.ToLower(lookup), prefix)
	}
	return strings.HasPrefix(strings.ToLower(name), prefix) ||
		strings.HasPrefix(strings.ToLower(lookup), prefix)
}

func goInterfaceInsertText(
	lookup string,
	parameters []string,
) string {
	if len(parameters) == 0 {
		return lookup
	}
	var content strings.Builder
	content.WriteString(lookup)
	content.WriteByte('[')
	for index, parameter := range parameters {
		if index != 0 {
			content.WriteString(", ")
		}
		fmt.Fprintf(&content, "${%d:%s}", index+1, parameter)
	}
	content.WriteByte(']')
	return content.String()
}

func goInterfaceDocumentation(
	contract compilerservice.GoInterface,
) string {
	var content strings.Builder
	fmt.Fprintf(
		&content,
		"`%s` from `%s`.\n\n",
		contract.Name,
		contract.PackagePath,
	)
	if len(contract.TypeParameters) != 0 {
		fmt.Fprintf(
			&content,
			"Type parameters: `%s`.\n\n",
			strings.Join(contract.TypeParameters, "`, `"),
		)
	}
	if len(contract.Methods) == 0 {
		content.WriteString("Runtime interface with an empty method set.")
	} else {
		content.WriteString("Complete method set:\n")
		for _, method := range contract.Methods {
			fmt.Fprintf(
				&content,
				"\n- `%s %s`",
				method.Name,
				strings.TrimPrefix(method.Signature, "func"),
			)
		}
	}
	content.WriteString(
		"\n\nResolved by Spice's typed Go program; the IDE does not infer DI eligibility.",
	)
	return content.String()
}

func argumentCompletionItems(
	context completionContext,
	metadata metadataView,
	content []byte,
) []completionItem {
	definition, found := annotationDefinition(
		metadata.definitions,
		context.annotation,
	)
	if !found {
		return []completionItem{}
	}
	itemRange := protocolRangeAtOffsets(content, context.start, context.end)
	for _, argument := range definition.Arguments {
		if argument.Name != context.argument ||
			len(argument.AllowedStrings) == 0 {
			continue
		}
		return valueCompletionItems(
			argument.AllowedStrings,
			context,
			itemRange,
			"allowed "+argument.Name+" value",
		)
	}
	if context.annotation == "Module" &&
		context.argument == "allowedDependencies" {
		var values []string
		for _, module := range metadata.modules {
			values = append(values, module.ID)
			for _, named := range module.NamedInterfaces {
				values = append(values, module.ID+"::"+named.Name)
			}
		}
		slices.Sort(values)
		return valueCompletionItems(
			values,
			context,
			itemRange,
			"application module API",
		)
	}
	items := make([]completionItem, 0, len(definition.Arguments))
	for index, argument := range definition.Arguments {
		documentation := markupContent{
			Kind: "markdown",
			Value: fmt.Sprintf(
				"`%s` accepts %s.",
				argument.Name,
				argumentKinds(argument),
			),
		}
		items = append(items, completionItem{
			Label:            argument.Name,
			Kind:             5,
			Detail:           "Spice annotation argument",
			Documentation:    &documentation,
			SortText:         fmt.Sprintf("%03d-%s", index, argument.Name),
			InsertTextFormat: 2,
			TextEdit: protocolEdit{
				Range: itemRange,
				NewText: argument.Name + "=" +
					argumentSnippet(argument, 1),
			},
		})
	}
	return items
}

func valueCompletionItems(
	values []string,
	context completionContext,
	itemRange protocolRange,
	detail string,
) []completionItem {
	items := make([]completionItem, 0, len(values))
	for _, value := range values {
		insert := value
		if !context.insideString {
			insert = `"` + value + `"`
		}
		items = append(items, completionItem{
			Label:    value,
			Kind:     12,
			Detail:   detail,
			SortText: value,
			TextEdit: protocolEdit{Range: itemRange, NewText: insert},
		})
	}
	return items
}

func configurationCompletionItems(
	context completionContext,
	metadata metadataView,
	content []byte,
) []completionItem {
	if !context.insideString {
		return []completionItem{}
	}
	itemRange := protocolRangeAtOffsets(content, context.start, context.end)
	items := make([]completionItem, 0)
	for _, configuration := range metadata.configurations {
		for _, field := range configuration.Fields {
			if context.existingPrefix != "" &&
				!strings.HasPrefix(field.Key, context.existingPrefix) {
				continue
			}
			documentation := markupContent{
				Kind:  "markdown",
				Value: configurationFieldDocumentation(configuration, field),
			}
			items = append(items, completionItem{
				Label:         field.Key,
				Kind:          10,
				Detail:        field.TypeID,
				Documentation: &documentation,
				SortText:      field.Key,
				TextEdit: protocolEdit{
					Range:   itemRange,
					NewText: field.Key,
				},
			})
		}
	}
	sort.SliceStable(items, func(left, right int) bool {
		return items[left].Label < items[right].Label
	})
	return items
}

func (server *Server) hover(message rpcMessage) error {
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
		return server.writer.response(message.ID, nil)
	}
	offset, valid := byteOffset(source.content, params.Position)
	if !valid {
		return server.writer.response(message.ID, nil)
	}
	if reference, interfaceFound := goInterfaceReferenceAt(
		source,
		metadata,
		offset,
	); interfaceFound {
		return server.writer.response(message.ID, hoverResult{
			Contents: markupContent{
				Kind: "markdown",
				Value: goInterfaceDocumentation(
					reference.contract,
				),
			},
			Range: new(protocolRangeAtOffsets(
				source.content,
				reference.start,
				reference.end,
			)),
		})
	}
	value, start, end := tokenAt(source.content, offset)
	if occurrence, occurrenceFound := annotationOccurrenceAt(
		source.content,
		offset,
	); occurrenceFound {
		definition, found := definitionForOccurrence(
			source,
			metadata,
			occurrence,
		)
		if !found {
			return server.writer.response(message.ID, nil)
		}
		return server.writer.response(message.ID, hoverResult{
			Contents: markupContent{
				Kind:  "markdown",
				Value: annotationDocumentation(definition),
			},
			Range: new(protocolRangeAtOffsets(source.content, start, end)),
		})
	}
	for _, configuration := range metadata.configurations {
		for _, field := range configuration.Fields {
			if value == field.Key {
				return server.writer.response(message.ID, hoverResult{
					Contents: markupContent{
						Kind: "markdown",
						Value: configurationFieldDocumentation(
							configuration,
							field,
						),
					},
					Range: new(protocolRangeAtOffsets(
						source.content,
						start,
						end,
					)),
				})
			}
		}
	}
	for _, module := range metadata.modules {
		if value == module.ID {
			return server.writer.response(message.ID, hoverResult{
				Contents: markupContent{
					Kind: "markdown",
					Value: fmt.Sprintf(
						"`%s` owns %d package(s) and exposes %d named interface(s).",
						module.ID,
						len(module.Packages),
						len(module.NamedInterfaces),
					),
				},
				Range: new(protocolRangeAtOffsets(
					source.content,
					start,
					end,
				)),
			})
		}
	}
	return server.writer.response(message.ID, nil)
}

func (server *Server) signatureHelp(message rpcMessage) error {
	if !message.request() {
		return nil
	}
	var params textDocumentPositionParams
	if err := decodeParams(message.Params, &params); err != nil {
		return server.writer.failure(message.ID, invalidParamsCode, err.Error())
	}
	source, metadata, found := server.featureSnapshot(params.TextDocument.URI)
	if !found {
		return server.writer.response(message.ID, nil)
	}
	offset, valid := byteOffset(source.content, params.Position)
	if !valid {
		return server.writer.response(message.ID, nil)
	}
	result, found := signatureHelpAt(
		source.content,
		offset,
		metadata.definitions,
	)
	if !found {
		return server.writer.response(message.ID, nil)
	}
	return server.writer.response(message.ID, result)
}

func signatureHelpAt(
	content []byte,
	offset int,
	definitions []compilerservice.AnnotationDefinition,
) (signatureHelpResult, bool) {
	context := inspectCompletionContext(content, offset)
	if context.kind != completionArgument {
		return signatureHelpResult{}, false
	}
	definitions = fileScopedDefinitions(content, definitions)
	definition, found := annotationDefinition(
		definitions,
		context.annotation,
	)
	if !found {
		return signatureHelpResult{}, false
	}
	parameters := make(
		[]parameterInformation,
		len(definition.Arguments),
	)
	active := 0
	for index, argument := range definition.Arguments {
		documentation := markupContent{
			Kind:  "markdown",
			Value: argumentDocumentation(argument),
		}
		parameters[index] = parameterInformation{
			Label:         annotationArgumentLabel(argument),
			Documentation: &documentation,
		}
		if context.argument == argument.Name {
			active = index
		}
	}
	documentation := markupContent{
		Kind:  "markdown",
		Value: annotationDocumentation(definition),
	}
	return signatureHelpResult{
		Signatures: []signatureInformation{{
			Label: "@" + definition.Name + "(" +
				strings.Join(parameterLabels(parameters), ", ") + ")",
			Documentation: &documentation,
			Parameters:    parameters,
		}},
		ActiveParameter: active,
	}, true
}

func parameterLabels(values []parameterInformation) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = value.Label
	}
	return result
}

func annotationArgumentLabel(
	argument compilerservice.AnnotationArgument,
) string {
	label := argument.Name + ": " + argumentKinds(argument)
	if !argument.Required {
		label += "?"
	}
	return label
}

func argumentDocumentation(
	argument compilerservice.AnnotationArgument,
) string {
	var content strings.Builder
	content.WriteString(argument.Description)
	if argument.Default != "" {
		fmt.Fprintf(&content, "\n\nDefault: `%s`.", argument.Default)
	}
	if len(argument.AllowedStrings) != 0 {
		fmt.Fprintf(
			&content,
			"\n\nAllowed values: `%s`.",
			strings.Join(argument.AllowedStrings, "`, `"),
		)
	}
	return content.String()
}

func (server *Server) codeAction(message rpcMessage) error {
	if !message.request() {
		return nil
	}
	var params codeActionParams
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
		return server.writer.response(
			message.ID,
			[]protocolCodeAction{},
		)
	}
	actions := server.protocolCodeActions(
		source,
		params.Range,
		metadata.actions,
	)
	metadata.definitions = server.catalogCompletionDefinitions(
		source.root,
		metadata.definitions,
	)
	actions = append(
		actions,
		enumCodeActions(
			source,
			params.Range,
			metadata.enums,
		)...,
	)
	actions = append(
		actions,
		server.annotationToolCodeActions(
			source,
			params.Range,
			metadata,
		)...,
	)
	return server.writer.response(message.ID, actions)
}

func (server *Server) protocolCodeActions(
	requestDocument document,
	requestRange protocolRange,
	fixes []diagnostic.SuggestedFix,
) []protocolCodeAction {
	var result []protocolCodeAction
	for _, fix := range fixes {
		action, relevant, safe := server.protocolCodeAction(
			requestDocument,
			requestRange,
			fix,
		)
		if safe && relevant {
			result = append(result, action)
		}
	}
	return result
}

func (server *Server) protocolCodeAction(
	requestDocument document,
	requestRange protocolRange,
	fix diagnostic.SuggestedFix,
) (protocolCodeAction, bool, bool) {
	byDocument := make(map[string][]protocolEdit)
	versions := make(map[string]int)
	relevant := false
	server.mu.Lock()
	defer server.mu.Unlock()
	if fix.AppliesTo != nil {
		document := server.documentForLocationLocked(*fix.AppliesTo)
		if document == nil {
			return protocolCodeAction{}, false, false
		}
		anchor := protocolRangeFromCompiler(
			fix.AppliesTo.Range,
			document.content,
		)
		relevant = document.uri == requestDocument.uri &&
			rangesOverlap(anchor, requestRange)
	}
	for _, edit := range fix.Edits {
		document := server.documentForEditLocked(edit)
		if document == nil ||
			edit.DocumentVersion == nil ||
			*edit.DocumentVersion != document.version {
			return protocolCodeAction{}, false, false
		}
		converted := protocolEdit{
			Range: protocolRangeFromCompiler(
				edit.Location.Range,
				document.content,
			),
			NewText: edit.NewText,
		}
		if document.uri == requestDocument.uri &&
			rangesOverlap(converted.Range, requestRange) {
			relevant = true
		}
		byDocument[document.uri] = append(
			byDocument[document.uri],
			converted,
		)
		versions[document.uri] = document.version
	}
	uris := make([]string, 0, len(byDocument))
	for uri := range byDocument {
		uris = append(uris, uri)
	}
	sort.Strings(uris)
	changes := make([]protocolDocumentEdit, 0, len(uris))
	for _, uri := range uris {
		changes = append(changes, protocolDocumentEdit{
			TextDocument: protocolOptionalVersionedDocument{
				URI:     uri,
				Version: versions[uri],
			},
			Edits: byDocument[uri],
		})
	}
	edit := protocolWorkspaceEdit{DocumentChanges: changes}
	return protocolCodeAction{
		Title:       fix.Title,
		Kind:        "quickfix",
		IsPreferred: true,
		Edit:        &edit,
	}, relevant, true
}

func (server *Server) documentForEditLocked(
	edit diagnostic.TextEdit,
) *document {
	return server.documentForLocationLocked(edit.Location)
}

func (server *Server) documentForLocationLocked(
	location diagnostic.Location,
) *document {
	if document := server.documents[location.URI]; document != nil {
		return document
	}
	for _, document := range server.documents {
		if pathKey(document.path) == pathKey(location.Path) {
			return document
		}
	}
	return nil
}

func (server *Server) featureSnapshot(
	uri string,
) (document, metadataView, bool) {
	server.mu.Lock()
	defer server.mu.Unlock()
	source := server.documents[uri]
	if source == nil {
		return document{}, metadataView{}, false
	}
	workspace := server.workspaces[source.root]
	if workspace == nil || !workspace.hasLatest {
		return cloneDocument(*source), metadataView{
			sourcePath: source.path,
		}, true
	}
	latest := workspace.latest
	view := metadataView{
		definitions:    latest.AnnotationDefinitions(),
		annotations:    latest.Annotations(),
		providers:      latest.ProviderGraph().Providers,
		modules:        latest.ModuleGraph().Modules,
		configurations: latest.Configurations(),
		enums:          latest.Enums(),
		goInterfaces:   latest.GoInterfaces(),
		actions:        latest.CodeActions(),
		sourcePath:     source.path,
	}
	if workspace.hasGood {
		if len(view.definitions) == 0 {
			view.definitions = workspace.lastGood.AnnotationDefinitions()
		}
		if len(view.annotations) == 0 {
			view.annotations = workspace.lastGood.Annotations()
		}
		if len(view.providers) == 0 {
			view.providers = workspace.lastGood.ProviderGraph().Providers
		}
		if len(view.modules) == 0 {
			view.modules = workspace.lastGood.ModuleGraph().Modules
		}
		if len(view.configurations) == 0 {
			view.configurations = workspace.lastGood.Configurations()
		}
		if len(view.enums) == 0 {
			view.enums = workspace.lastGood.Enums()
		}
		if len(view.goInterfaces.Packages) == 0 {
			view.goInterfaces = workspace.lastGood.GoInterfaces()
		}
	}
	return cloneDocument(*source), view, true
}

func cloneDocument(source document) document {
	source.content = slices.Clone(source.content)
	return source
}

func annotationDefinition(
	definitions []compilerservice.AnnotationDefinition,
	name string,
) (compilerservice.AnnotationDefinition, bool) {
	for _, definition := range definitions {
		if definition.Name == name {
			return definition, true
		}
	}
	return compilerservice.AnnotationDefinition{}, false
}

func annotationDocumentation(
	definition compilerservice.AnnotationDefinition,
) string {
	var content strings.Builder
	writeAnnotationOverview(&content, definition)
	writeAnnotationArguments(&content, definition.Arguments)
	writeAnnotationExamples(&content, definition.Examples)
	writeAnnotationDescriptor(&content, definition)
	writeAnnotationProvenance(&content, definition.Provenance)
	writeAnnotationImplementation(&content, definition.Implementation)
	writeAnnotationCompatibility(&content, definition.Compatibility)
	return content.String()
}

func writeAnnotationOverview(
	content *strings.Builder,
	definition compilerservice.AnnotationDefinition,
) {
	fmt.Fprintf(content, "`@%s`\n\n", definition.Name)
	if definition.Summary != "" {
		content.WriteString(definition.Summary)
		content.WriteString("\n\n")
	}
	if definition.Documentation != "" {
		content.WriteString(definition.Documentation)
		content.WriteString("\n\n")
	}
	if len(definition.Targets) == 0 {
		content.WriteString(
			"Import this descriptor to load its typed targets and arguments.",
		)
	} else {
		fmt.Fprintf(
			content,
			"Valid targets: %s.",
			annotationTargetDetail(definition),
		)
	}
	if definition.Repeatable {
		content.WriteString(" Repeatable.")
	}
}

func writeAnnotationArguments(
	content *strings.Builder,
	arguments []compilerservice.AnnotationArgument,
) {
	if len(arguments) == 0 {
		return
	}
	content.WriteString("\n\nArguments:\n")
	for _, argument := range arguments {
		fmt.Fprintf(
			content,
			"\n- `%s`: %s",
			argument.Name,
			argumentKinds(argument),
		)
		if argument.Required {
			content.WriteString(" (required)")
		}
		if argument.Description != "" {
			fmt.Fprintf(content, " — %s", argument.Description)
		}
		if argument.Default != "" {
			fmt.Fprintf(content, " Default: `%s`.", argument.Default)
		}
	}
}

func writeAnnotationExamples(
	content *strings.Builder,
	examples []compilerservice.AnnotationExample,
) {
	if len(examples) == 0 {
		return
	}
	content.WriteString("\n\nExamples:\n")
	for _, example := range examples {
		fmt.Fprintf(
			content,
			"\n**%s**\n\n```go\n%s\n```\n",
			example.Title,
			example.Code,
		)
	}
}

func writeAnnotationDescriptor(
	content *strings.Builder,
	definition compilerservice.AnnotationDefinition,
) {
	if definition.DescriptorPackage != "" {
		fmt.Fprintf(
			content,
			"\n\nDescriptor: `%s.%s`.",
			definition.DescriptorPackage,
			definition.DescriptorSymbol,
		)
	}
}

func writeAnnotationProvenance(
	content *strings.Builder,
	provenance compilerservice.AnnotationProvenance,
) {
	if provenance.Module == "" {
		return
	}
	version := provenance.Version
	if version == "" {
		version = "local"
	}
	fmt.Fprintf(
		content,
		"\n\nModule: `%s@%s`.",
		provenance.Module,
		version,
	)
	if provenance.ReplacementModule != "" ||
		provenance.ReplacementDir != "" {
		if provenance.ReplacementDir != "" {
			fmt.Fprintf(
				content,
				" Resolved from local replacement `%s`.",
				provenance.ReplacementDir,
			)
			return
		}
		replacement := provenance.ReplacementModule
		fmt.Fprintf(
			content,
			" Replaced by `%s`.",
			replacement,
		)
	}
}

func writeAnnotationImplementation(
	content *strings.Builder,
	implementation compilerservice.AnnotationImplementation,
) {
	if implementation.Tool != "" {
		fmt.Fprintf(
			content,
			"\n\nImplementation: `go tool %s` handler `%s` using protocol `%s`.",
			implementation.Tool,
			implementation.Handler,
			implementation.Protocol,
		)
		if implementation.Package != "" && implementation.Symbol != "" {
			fmt.Fprintf(
				content,
				" Source: `%s.%s`.",
				implementation.Package,
				implementation.Symbol,
			)
		}
		if implementation.AuthorizationKnown {
			authorization := "not declared by the target go.mod"
			if implementation.Authorized {
				authorization = "authorized by the target go.mod"
			}
			fmt.Fprintf(content, " Tool is %s.", authorization)
		}
	}
}

func writeAnnotationCompatibility(
	content *strings.Builder,
	compatibility compilerservice.AnnotationCompatibility,
) {
	if compatibility.Since != "" {
		fmt.Fprintf(
			content,
			"\n\nCompatibility: since Spice `%s`; minimum Spice `%s`.",
			compatibility.Since,
			compatibility.MinimumSpice,
		)
	}
}

func fileScopedDefinitions(
	content []byte,
	definitions []compilerservice.AnnotationDefinition,
) []compilerservice.AnnotationDefinition {
	directives := importDirectives(content)
	if len(directives) == 0 {
		return definitions
	}
	bySource := make(map[string]compilerservice.AnnotationDefinition)
	for _, definition := range definitions {
		if definition.DescriptorPackage == "" ||
			definition.DescriptorSymbol == "" {
			continue
		}
		key := definition.DescriptorPackage + "\x00" +
			definition.DescriptorSymbol
		bySource[key] = definition
	}
	result := make(
		[]compilerservice.AnnotationDefinition,
		0,
		len(definitions),
	)
	seen := make(map[string]struct{})
	for _, directive := range directives {
		switch directive.Kind {
		case annotation.ImportNamed:
			for _, binding := range directive.Bindings {
				key := directive.Package + "\x00" + binding.Imported
				definition, found := bySource[key]
				if !found {
					continue
				}
				definition.Name = binding.Local
				if _, duplicate := seen[definition.Name]; duplicate {
					continue
				}
				seen[definition.Name] = struct{}{}
				result = append(result, definition)
			}
		case annotation.ImportNamespace:
			for _, definition := range definitions {
				if definition.DescriptorPackage != directive.Package {
					continue
				}
				definition.Name = directive.Namespace + "." +
					definition.DescriptorSymbol
				if _, duplicate := seen[definition.Name]; duplicate {
					continue
				}
				seen[definition.Name] = struct{}{}
				result = append(result, definition)
			}
		}
	}
	sort.SliceStable(result, func(left, right int) bool {
		return result[left].Name < result[right].Name
	})
	return result
}

func importDirectives(content []byte) []annotation.ImportDirective {
	var result []annotation.ImportDirective
	lineNumber := 1
	for start := 0; start <= len(content); lineNumber++ {
		relativeEnd := bytes.IndexByte(content[start:], '\n')
		last := relativeEnd < 0
		end := len(content)
		if !last {
			end = start + relativeEnd
		}
		line := strings.TrimSpace(
			string(bytes.TrimSuffix(content[start:end], []byte{'\r'})),
		)
		directive, recognized, err := annotationparser.ParseImportComment(
			line,
			token.Position{Line: lineNumber, Column: 1, Offset: start},
		)
		if recognized && err == nil {
			result = append(result, directive)
		}
		if last {
			break
		}
		start = end + 1
	}
	return result
}

func annotationTargetDetail(
	definition compilerservice.AnnotationDefinition,
) string {
	values := make([]string, len(definition.Targets))
	for index, target := range definition.Targets {
		values[index] = string(target)
	}
	return strings.Join(values, ", ")
}

func argumentKinds(argument compilerservice.AnnotationArgument) string {
	values := make([]string, len(argument.Kinds))
	for index, kind := range argument.Kinds {
		values[index] = string(kind)
	}
	return strings.Join(values, " or ")
}

func configurationFieldDocumentation(
	configuration compilerservice.Configuration,
	field compilerservice.ConfigurationField,
) string {
	var content strings.Builder
	fmt.Fprintf(
		&content,
		"`%s` · `%s`",
		field.Key,
		field.TypeID,
	)
	if field.Environment != "" {
		fmt.Fprintf(
			&content,
			"\n\nEnvironment: `%s`.",
			field.Environment,
		)
	}
	if field.Required {
		content.WriteString(" Required.")
	}
	if field.Secret {
		content.WriteString(" Secret value; defaults are redacted.")
	} else if field.HasDefault {
		fmt.Fprintf(&content, " Default: `%s`.", field.Default)
	}
	if configuration.Module != "" {
		fmt.Fprintf(&content, " Module: `%s`.", configuration.Module)
	}
	return content.String()
}

func tokenAt(content []byte, offset int) (string, int, int) {
	start := wordStart(content, offset, hoverCharacter)
	end := offset
	for end < len(content) && hoverCharacter(content[end]) {
		end++
	}
	return string(content[start:end]), start, end
}

func wordStart(
	content []byte,
	offset int,
	allowed func(byte) bool,
) int {
	start := min(max(offset, 0), len(content))
	for start > 0 && allowed(content[start-1]) {
		start--
	}
	return start
}

func annotationCharacter(value byte) bool {
	return value == '.' ||
		value == '_' ||
		value >= 'a' && value <= 'z' ||
		value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9'
}

func argumentValueCharacter(value byte) bool {
	return annotationCharacter(value) ||
		value == '/' ||
		value == ':' ||
		value == '-'
}

func configurationCharacter(value byte) bool {
	return argumentValueCharacter(value)
}

func hoverCharacter(value byte) bool {
	return configurationCharacter(value) || value == '@'
}

func insideStringLiteral(content []byte) bool {
	quoted := false
	escaped := false
	for _, value := range content {
		switch {
		case escaped:
			escaped = false
		case value == '\\':
			escaped = true
		case value == '"':
			quoted = !quoted
		}
	}
	return quoted
}

func byteOffset(
	content []byte,
	position protocolPosition,
) (int, bool) {
	if position.Line < 0 || position.Character < 0 {
		return 0, false
	}
	start, end, found := contentLine(content, position.Line+1)
	if !found {
		return 0, false
	}
	units := 0
	for offset := start; offset < end; {
		if units == position.Character {
			return offset, true
		}
		character, size := utf8.DecodeRune(content[offset:end])
		next := 1
		if character > 0xFFFF {
			next = 2
		}
		if units+next > position.Character {
			return 0, false
		}
		units += next
		offset += size
	}
	if units == position.Character {
		return end, true
	}
	return 0, false
}

func protocolRangeAtOffsets(
	content []byte,
	start int,
	end int,
) protocolRange {
	return protocolRange{
		Start: protocolPositionAtOffset(content, start),
		End:   protocolPositionAtOffset(content, end),
	}
}

func protocolPositionAtOffset(
	content []byte,
	offset int,
) protocolPosition {
	offset = min(max(offset, 0), len(content))
	line := 0
	lineStart := 0
	for index, value := range content[:offset] {
		if value == '\n' {
			line++
			lineStart = index + 1
		}
	}
	return protocolPosition{
		Line:      line,
		Character: utf16Length(content[lineStart:offset]),
	}
}

func contentLineAtOffset(
	content []byte,
	offset int,
) (int, bool) {
	if offset < 0 || offset > len(content) {
		return 0, false
	}
	start := offset
	for start > 0 && content[start-1] != '\n' {
		start--
	}
	return start, true
}

func contentLine(
	content []byte,
	oneBasedLine int,
) (int, int, bool) {
	if oneBasedLine <= 0 {
		return 0, 0, false
	}
	if len(content) == 0 {
		return 0, 0, oneBasedLine == 1
	}
	start := 0
	for line := 1; line < oneBasedLine; line++ {
		index := slices.Index(content[start:], byte('\n'))
		if index < 0 {
			return 0, 0, false
		}
		start += index + 1
	}
	end := start
	for end < len(content) &&
		content[end] != '\n' &&
		content[end] != '\r' {
		end++
	}
	return start, end, true
}

func rangesOverlap(left, right protocolRange) bool {
	return comparePosition(left.End, right.Start) >= 0 &&
		comparePosition(right.End, left.Start) >= 0
}

func comparePosition(left, right protocolPosition) int {
	if left.Line != right.Line {
		return left.Line - right.Line
	}
	return left.Character - right.Character
}
