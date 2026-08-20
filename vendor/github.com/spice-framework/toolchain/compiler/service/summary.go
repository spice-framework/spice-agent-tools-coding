package service

import (
	"bytes"
	"go/token"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/spice-framework/spice/annotation"
	"github.com/spice-framework/spice/annotation/sdk"
	"github.com/spice-framework/toolchain/compiler/application"
	compilerbootstrap "github.com/spice-framework/toolchain/compiler/bootstrap"
	"github.com/spice-framework/toolchain/compiler/descriptor"
	"github.com/spice-framework/toolchain/compiler/diagnostic"
	"github.com/spice-framework/toolchain/compiler/load"
	"github.com/spice-framework/toolchain/compiler/modulith"
	"github.com/spice-framework/toolchain/compiler/provider"
	"github.com/spice-framework/toolchain/compiler/resolve"
)

func summarizeAnnotations(
	workspaceRoot string,
	resolution resolve.Result,
) []Annotation {
	result := make([]Annotation, len(resolution.Occurrences))
	for index, occurrence := range resolution.Occurrences {
		physical := occurrence.PhysicalPosition
		if physical.Filename == "" {
			physical = token.Position{
				Filename: occurrence.PhysicalFile,
				Offset:   occurrence.PhysicalOffset,
			}
		}
		result[index] = Annotation{
			Name:              occurrence.Annotation.Name,
			Spelling:          occurrence.Spelling,
			Raw:               occurrence.Annotation.Raw,
			Target:            occurrence.Target,
			Declaration:       occurrence.Name,
			SymbolID:          occurrence.SymbolID,
			PackagePath:       occurrence.PackagePath,
			DefinitionPackage: occurrence.Definition.Package,
			DefinitionSymbol:  occurrence.Definition.Symbol,
			Location: diagnostic.SourceMappedLocation(
				workspaceRoot,
				occurrence.DisplayPosition.Filename,
				physical.Filename,
				occurrence.DisplayPosition.Line,
				occurrence.DisplayPosition.Column,
				occurrence.DisplayPosition.Offset,
				physical.Line,
				physical.Column,
				physical.Offset,
			),
		}
	}
	return result
}

func summarizeProviderGraph(model application.Model) ProviderGraph {
	providers := model.Providers()
	result := ProviderGraph{
		Providers: summarizeProviders(providers),
	}
	edges := model.Edges()
	result.Edges = make([]ProviderEdge, len(edges))
	for index, edge := range edges {
		result.Edges[index] = ProviderEdge{
			ConsumerID:      edge.ConsumerID,
			DependencyID:    edge.DependencyID,
			RequiredTypeID:  edge.RequiredTypeID,
			ParameterIndex:  edge.ParameterIndex,
			ParameterName:   edge.ParameterName,
			DependencyKind:  edge.DependencyKind,
			CollectionIndex: edge.CollectionIndex,
		}
	}
	return result
}

func summarizeProviders(providers []provider.Provider) []Provider {
	result := make([]Provider, len(providers))
	for index, item := range providers {
		dependencies := make(
			[]ProviderDependency,
			len(item.Dependencies),
		)
		for dependencyIndex, dependency := range item.Dependencies {
			dependencies[dependencyIndex] = ProviderDependency{
				Index:         dependency.Index,
				Name:          dependency.Name,
				TypeID:        dependency.TypeID,
				Kind:          dependency.Kind,
				ElementTypeID: dependency.ElementTypeID,
				Qualifiers: append(
					[]string(nil),
					dependency.Qualifiers...,
				),
			}
		}
		result[index] = Provider{
			ID:             item.SymbolID,
			Name:           item.Name,
			ExplicitName:   item.ExplicitName,
			PackagePath:    item.PackagePath,
			OutputTypeID:   item.OutputTypeID,
			Source:         item.Source,
			Aliases:        append([]string(nil), item.Aliases...),
			Qualifiers:     append([]string(nil), item.Qualifiers...),
			Primary:        item.Primary,
			Fallback:       item.Fallback,
			Order:          item.Order,
			Scope:          item.Scope,
			Dependencies:   dependencies,
			ReturnsCleanup: item.ReturnsCleanup,
			ReturnsError:   item.ReturnsError,
		}
	}
	return result
}

func summarizeModuleGraph(model modulith.Model) ModuleGraph {
	modules := model.Modules()
	result := ModuleGraph{Modules: make([]Module, len(modules))}
	for index, item := range modules {
		packages := item.Packages()
		packagePaths := make([]string, len(packages))
		for packageIndex, ownedPackage := range packages {
			packagePaths[packageIndex] = ownedPackage.Path
		}
		interfaces := item.NamedInterfaces()
		named := make([]NamedInterface, len(interfaces))
		for interfaceIndex, namedInterface := range interfaces {
			named[interfaceIndex] = NamedInterface{
				Name:        namedInterface.Name,
				PackagePath: namedInterface.PackagePath,
			}
		}
		allowedItems := item.AllowedDependencies()
		allowed := make([]ModuleDependency, len(allowedItems))
		for dependencyIndex, dependency := range allowedItems {
			allowed[dependencyIndex] = ModuleDependency{
				ModuleID: dependency.ModuleID,
				API:      dependency.Interface,
			}
		}
		result.Modules[index] = Module{
			ID:                  item.ID,
			RootPackage:         item.RootPackage,
			Packages:            packagePaths,
			NamedInterfaces:     named,
			AllowedDependencies: allowed,
		}
	}
	edges := model.Edges()
	result.Edges = make([]ModuleEdge, len(edges))
	for index, edge := range edges {
		result.Edges[index] = ModuleEdge{
			FromModule:  edge.FromModule,
			ToModule:    edge.ToModule,
			FromPackage: edge.FromPackage,
			ToPackage:   edge.ToPackage,
			API:         edge.API,
			Exported:    edge.Exported,
			Allowed:     edge.Allowed,
		}
	}
	unassigned := model.UnassignedPackages()
	result.UnassignedPackages = make([]string, len(unassigned))
	for index, item := range unassigned {
		result.UnassignedPackages[index] = item.Path
	}
	return result
}

func summarizeConfigurations(model application.Model) []Configuration {
	items := model.Configurations()
	result := make([]Configuration, len(items))
	for index, item := range items {
		fields := item.Fields()
		summaries := make([]ConfigurationField, len(fields))
		for fieldIndex, field := range fields {
			defaultValue := field.Default
			if field.Secret {
				defaultValue = ""
			}
			summaries[fieldIndex] = ConfigurationField{
				Name:        field.Name,
				Key:         field.Key,
				TypeID:      field.TypeID,
				Environment: field.Environment,
				Default:     defaultValue,
				HasDefault:  field.HasDefault,
				Required:    field.Required,
				Secret:      field.Secret,
			}
		}
		result[index] = Configuration{
			SymbolID:    item.SymbolID,
			Name:        item.Name,
			PackagePath: item.PackagePath,
			TypeID:      item.TypeID,
			Prefix:      item.Prefix,
			Module:      item.Module,
			Fields:      summaries,
		}
	}
	return result
}

func summarizeEnums(
	workspaceRoot string,
	model application.Model,
) []Enum {
	items := model.Enums()
	result := make([]Enum, len(items))
	for index, item := range items {
		members := item.Members()
		summaries := make([]EnumMember, len(members))
		for memberIndex, member := range members {
			summaries[memberIndex] = EnumMember{
				Name:  member.Name,
				Value: member.Value,
			}
		}
		result[index] = Enum{
			SymbolID:    item.SymbolID,
			Name:        item.Name,
			PackagePath: item.PackagePath,
			TypeID:      item.TypeID,
			Underlying:  item.Underlying,
			Location: diagnostic.SourceMappedLocation(
				workspaceRoot,
				item.Position.Filename,
				item.PhysicalPosition.Filename,
				item.Position.Line,
				item.Position.Column,
				item.Position.Offset,
				item.PhysicalPosition.Line,
				item.PhysicalPosition.Column,
				item.PhysicalPosition.Offset,
			),
			Members: summaries,
		}
	}
	return result
}

func summarizeDefinitions(
	workspaceRoot string,
	registry annotation.Registry,
	extensions []compilerbootstrap.Definition,
	descriptors []descriptor.Descriptor,
	program *load.Program,
	implementationPositions map[sdk.Symbol]token.Position,
) []AnnotationDefinition {
	bootstrapDefinitions := append(
		compilerbootstrap.Builtins(),
		extensions...,
	)
	allowedStrings := make(map[string]map[string][]string)
	for _, definition := range bootstrapDefinitions {
		options := make(map[string][]string, len(definition.Options))
		for _, option := range definition.Options {
			options[option.Name] = slices.Clone(option.AllowedStrings)
		}
		allowedStrings[definition.Annotation] = options
	}
	items := registry.Definitions()
	result := make([]AnnotationDefinition, len(items))
	descriptorByName := make(map[string]descriptor.Descriptor, len(descriptors))
	for _, item := range descriptors {
		descriptorByName[item.Definition.Name] = item
	}
	implementationLocations := implementationSymbolLocations(
		workspaceRoot,
		program,
		descriptors,
		implementationPositions,
	)
	for index, item := range items {
		arguments := make([]AnnotationArgument, len(item.Arguments))
		for argumentIndex, argument := range item.Arguments {
			arguments[argumentIndex] = AnnotationArgument{
				Name:             argument.Name,
				Kinds:            slices.Clone(argument.Kinds),
				ListElementKinds: slices.Clone(argument.ListElementKinds),
				ValueDomain:      argument.ValueDomain,
				AllowedStrings: slices.Clone(
					allowedStrings[item.Name][argument.Name],
				),
				Required:   argument.Required,
				Positional: argument.Positional,
				Variadic:   argument.Variadic,
			}
		}
		result[index] = AnnotationDefinition{
			Name:       item.Name,
			Targets:    item.Targets.Values(),
			Repeatable: item.Repeatable,
			Arguments:  arguments,
		}
		decoded, found := descriptorByName[item.Name]
		if found {
			enrichDefinition(
				workspaceRoot,
				&result[index],
				decoded,
				implementationLocations,
			)
		}
	}
	return result
}

func enrichDefinition(
	workspaceRoot string,
	result *AnnotationDefinition,
	decoded descriptor.Descriptor,
	implementationLocations map[string]diagnostic.Location,
) {
	definition := decoded.Definition
	result.Summary = definition.Summary
	result.Documentation = decoded.Documentation
	result.DescriptorPackage = decoded.Package
	result.DescriptorSymbol = decoded.Symbol
	result.DescriptorLocation = symbolLocation(
		workspaceRoot,
		decoded.Position,
		decoded.Symbol,
	)
	result.HasDescriptorLocation = result.DescriptorLocation.Path != ""
	result.Examples = make([]AnnotationExample, len(definition.Examples))
	for index, example := range definition.Examples {
		result.Examples[index] = AnnotationExample{
			Title: example.Title,
			Code:  example.Code,
		}
	}
	result.Compatibility = AnnotationCompatibility{
		Since:        definition.Compatibility.Since,
		MinimumSpice: definition.Compatibility.MinimumSpice,
	}
	implementation := definition.Implementation
	implementationKey := decoded.Handler.Package + "\x00" +
		decoded.Handler.Name
	location, found := implementationLocations[implementationKey]
	result.Implementation = AnnotationImplementation{
		Tool:        implementation.Tool,
		Handler:     decoded.Handler.Package + "." + decoded.Handler.Name,
		Protocol:    string(implementation.Protocol),
		Package:     decoded.Handler.Package,
		Symbol:      decoded.Handler.Name,
		Location:    location,
		HasLocation: found,
	}
	provenance := decoded.Provenance
	result.Provenance = AnnotationProvenance{
		Module:             provenance.Path,
		Version:            provenance.Version,
		ReplacementModule:  provenance.ReplacementPath,
		ReplacementVersion: provenance.ReplacementVersion,
		ReplacementDir:     provenance.ReplacementDir,
		LocalReplacement:   provenance.LocalReplacement,
	}
	arguments := make(map[string]int, len(result.Arguments))
	for index, argument := range result.Arguments {
		arguments[argument.Name] = index
	}
	for _, argument := range definition.Arguments {
		index, found := arguments[argument.Name]
		if !found {
			continue
		}
		result.Arguments[index].AllowedStrings = slices.Clone(
			argument.AllowedValues,
		)
		result.Arguments[index].Description = argument.Description
		result.Arguments[index].Default = argument.Default
	}
}

func implementationSymbolLocations(
	workspaceRoot string,
	program *load.Program,
	descriptors []descriptor.Descriptor,
	fallback map[sdk.Symbol]token.Position,
) map[string]diagnostic.Location {
	requested := make(map[string]struct{}, len(descriptors))
	for _, item := range descriptors {
		source := item.Handler
		requested[source.Package+"\x00"+source.Name] = struct{}{}
	}
	result := make(map[string]diagnostic.Location, len(requested))
	if program != nil {
		for _, symbol := range program.Symbols() {
			key := symbol.PackagePath + "\x00" + symbol.Name
			if _, found := requested[key]; !found || symbol.Receiver != "" {
				continue
			}
			location := symbolLocation(
				workspaceRoot,
				symbol.PhysicalPosition,
				symbol.Name,
			)
			if location.Path != "" {
				result[key] = location
			}
		}
	}
	for symbol, position := range fallback {
		key := symbol.Package + "\x00" + symbol.Name
		if _, found := requested[key]; !found {
			continue
		}
		if _, found := result[key]; found {
			continue
		}
		location := symbolLocation(workspaceRoot, position, symbol.Name)
		if location.Path != "" {
			result[key] = location
		}
	}
	return result
}

func symbolLocation(
	workspaceRoot string,
	position token.Position,
	symbol string,
) diagnostic.Location {
	if position.Filename == "" {
		return diagnostic.Location{}
	}
	location := diagnostic.SourceLocation(
		workspaceRoot,
		position.Filename,
		position.Filename,
		position.Line,
		position.Column,
		position.Offset,
	)
	length := len(symbol)
	if length > 1 {
		location.Range.End.Column += length - 1
		location.Range.End.Offset += length - 1
	}
	return location
}

func overlaySafeFixes(
	set diagnostic.Set,
	overlay map[string]Document,
) diagnostic.Set {
	items := set.Items()
	for index := range items {
		if !strings.HasPrefix(items[index].Code, "spice.load.") ||
			len(items[index].Fixes) != 0 {
			continue
		}
		filePath := filepathFromSlash(items[index].Location.Path)
		document, found := overlay[filePath]
		if !found {
			continue
		}
		edit, found := annotationCommentPrefixEdit(
			items[index].Location,
			document,
		)
		if !found {
			continue
		}
		items[index] = items[index].WithFixes(diagnostic.SuggestedFix{
			Title: annotationCommentFixTitle,
			Edits: []diagnostic.TextEdit{edit},
		})
	}
	return diagnostic.NewSet(items...)
}

func legacyImportFixes(
	set diagnostic.Set,
	overlay map[string]Document,
) diagnostic.Set {
	const legacy = "@spice.import"
	items := set.Items()
	code := diagnostic.Code("resolution", "annotation-import-legacy")
	for index := range items {
		if items[index].Code != code || len(items[index].Fixes) != 0 {
			continue
		}
		filePath := filepathFromSlash(items[index].Location.Path)
		document, found := overlay[filePath]
		if !found {
			continue
		}
		start := items[index].Location.Range.Start.Offset
		end := start + len(legacy)
		if start < 0 ||
			end > len(document.Content) ||
			string(document.Content[start:end]) != legacy {
			continue
		}
		location := items[index].Location
		location.Range.End = location.Range.Start
		location.Range.End.Column += len(legacy)
		location.Range.End.Offset += len(legacy)
		if location.Display != nil {
			display := *location.Display
			display.Range.End = display.Range.Start
			display.Range.End.Column += len(legacy)
			display.Range.End.Offset += len(legacy)
			location.Display = &display
		}
		items[index] = items[index].WithFixes(diagnostic.SuggestedFix{
			Title: legacyImportFixTitle,
			Edits: []diagnostic.TextEdit{{
				Location: location,
				NewText:  "@import",
			}},
		})
	}
	return diagnostic.NewSet(items...)
}

func annotationCommentPrefixEdit(
	location diagnostic.Location,
	document Document,
) (diagnostic.TextEdit, bool) {
	line := location.Range.Start.Line
	if line <= 0 {
		return diagnostic.TextEdit{}, false
	}
	start, end, found := sourceLine(document.Content, line)
	if !found {
		return diagnostic.TextEdit{}, false
	}
	content := document.Content[start:end]
	indent := 0
	for indent < len(content) &&
		(content[indent] == ' ' || content[indent] == '\t') {
		indent++
	}
	if indent >= len(content) || content[indent] != '@' {
		return diagnostic.TextEdit{}, false
	}
	position := diagnostic.Position{
		Line:   line,
		Column: indent + 1,
		Offset: start + indent,
	}
	editLocation := location
	editLocation.Range = diagnostic.Range{Start: position, End: position}
	if editLocation.Display != nil {
		display := *editLocation.Display
		display.Range = editLocation.Range
		editLocation.Display = &display
	}
	version := document.Version
	return diagnostic.TextEdit{
		Location:        editLocation,
		DocumentVersion: &version,
		NewText:         "// ",
	}, true
}

func sourceLine(
	content []byte,
	oneBasedLine int,
) (int, int, bool) {
	if oneBasedLine <= 0 {
		return 0, 0, false
	}
	start := 0
	for line := 1; line < oneBasedLine; line++ {
		index := bytes.IndexByte(content[start:], '\n')
		if index < 0 {
			return 0, 0, false
		}
		start += index + 1
	}
	end := start
	for end < len(content) && content[end] != '\n' && content[end] != '\r' {
		end++
	}
	return start, end, true
}

func actionsFromDiagnostics(
	set diagnostic.Set,
) []diagnostic.SuggestedFix {
	var result []diagnostic.SuggestedFix
	for _, item := range set.Items() {
		result = append(result, item.Fixes...)
	}
	sort.SliceStable(result, func(left, right int) bool {
		return result[left].Title < result[right].Title
	})
	unique := result[:0]
	for _, item := range result {
		if len(unique) != 0 &&
			equalSuggestedFix(unique[len(unique)-1], item) {
			continue
		}
		unique = append(unique, item)
	}
	return cloneActions(unique)
}

func equalSuggestedFix(
	left diagnostic.SuggestedFix,
	right diagnostic.SuggestedFix,
) bool {
	if left.Title != right.Title ||
		!equalOptionalLocation(left.AppliesTo, right.AppliesTo) ||
		len(left.Edits) != len(right.Edits) {
		return false
	}
	for index, leftEdit := range left.Edits {
		rightEdit := right.Edits[index]
		if leftEdit.NewText != rightEdit.NewText ||
			leftEdit.Location.URI != rightEdit.Location.URI ||
			leftEdit.Location.Path != rightEdit.Location.Path ||
			leftEdit.Location.Range != rightEdit.Location.Range ||
			!equalVersion(
				leftEdit.DocumentVersion,
				rightEdit.DocumentVersion,
			) {
			return false
		}
	}
	return true
}

func equalOptionalLocation(
	left *diagnostic.Location,
	right *diagnostic.Location,
) bool {
	switch {
	case left == nil || right == nil:
		return left == right
	default:
		if left.URI != right.URI ||
			left.Path != right.Path ||
			left.Range != right.Range {
			return false
		}
		switch {
		case left.Display == nil || right.Display == nil:
			return left.Display == right.Display
		default:
			return *left.Display == *right.Display
		}
	}
}

func equalVersion(left, right *int) bool {
	switch {
	case left == nil || right == nil:
		return left == right
	default:
		return *left == *right
	}
}

func versionDiagnostics(
	set diagnostic.Set,
	overlay map[string]Document,
) diagnostic.Set {
	items := set.Items()
	for itemIndex := range items {
		for fixIndex := range items[itemIndex].Fixes {
			for editIndex := range items[itemIndex].Fixes[fixIndex].Edits {
				edit := &items[itemIndex].Fixes[fixIndex].Edits[editIndex]
				if edit.DocumentVersion != nil {
					continue
				}
				nativePath := filepathFromSlash(edit.Location.Path)
				document, found := overlay[nativePath]
				if !found {
					continue
				}
				version := document.Version
				edit.DocumentVersion = &version
			}
		}
	}
	return diagnostic.NewSet(items...)
}

func filepathFromSlash(value string) string {
	return filepath.Clean(filepath.FromSlash(value))
}
