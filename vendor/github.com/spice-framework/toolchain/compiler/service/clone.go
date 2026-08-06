package service

import (
	"slices"

	"github.com/spice-framework/toolchain/compiler/diagnostic"
)

func cloneProviderGraph(graph ProviderGraph) ProviderGraph {
	result := ProviderGraph{
		Providers: make([]Provider, len(graph.Providers)),
		Edges:     slices.Clone(graph.Edges),
	}
	for index, item := range graph.Providers {
		result.Providers[index] = item
		result.Providers[index].Dependencies = slices.Clone(item.Dependencies)
		result.Providers[index].Aliases = slices.Clone(item.Aliases)
		result.Providers[index].Qualifiers = slices.Clone(item.Qualifiers)
		for dependencyIndex := range result.Providers[index].Dependencies {
			result.Providers[index].
				Dependencies[dependencyIndex].
				Qualifiers = slices.Clone(
				item.Dependencies[dependencyIndex].Qualifiers,
			)
		}
	}
	return result
}

func cloneModuleGraph(graph ModuleGraph) ModuleGraph {
	result := ModuleGraph{
		Modules:            make([]Module, len(graph.Modules)),
		Edges:              slices.Clone(graph.Edges),
		UnassignedPackages: slices.Clone(graph.UnassignedPackages),
	}
	for index, item := range graph.Modules {
		result.Modules[index] = item
		result.Modules[index].Packages = slices.Clone(item.Packages)
		result.Modules[index].NamedInterfaces = slices.Clone(
			item.NamedInterfaces,
		)
		result.Modules[index].AllowedDependencies = slices.Clone(
			item.AllowedDependencies,
		)
	}
	return result
}

func cloneConfigurations(items []Configuration) []Configuration {
	result := make([]Configuration, len(items))
	for index, item := range items {
		result[index] = item
		result[index].Fields = slices.Clone(item.Fields)
	}
	return result
}

func cloneGoInterfaceCatalog(
	catalog GoInterfaceCatalog,
) GoInterfaceCatalog {
	result := GoInterfaceCatalog{
		Packages: make(
			[]GoInterfacePackage,
			len(catalog.Packages),
		),
	}
	for packageIndex, item := range catalog.Packages {
		result.Packages[packageIndex] = item
		result.Packages[packageIndex].Files = slices.Clone(item.Files)
		result.Packages[packageIndex].Interfaces = make(
			[]GoInterface,
			len(item.Interfaces),
		)
		for interfaceIndex, contract := range item.Interfaces {
			result.Packages[packageIndex].Interfaces[interfaceIndex] = contract
			result.Packages[packageIndex].
				Interfaces[interfaceIndex].
				TypeParameters = slices.Clone(contract.TypeParameters)
			result.Packages[packageIndex].
				Interfaces[interfaceIndex].
				Methods = slices.Clone(contract.Methods)
		}
	}
	return result
}

func cloneDefinitions(items []AnnotationDefinition) []AnnotationDefinition {
	result := make([]AnnotationDefinition, len(items))
	for index, item := range items {
		result[index] = item
		result[index].Targets = slices.Clone(item.Targets)
		result[index].Arguments = make(
			[]AnnotationArgument,
			len(item.Arguments),
		)
		result[index].Examples = slices.Clone(item.Examples)
		for argumentIndex, argument := range item.Arguments {
			result[index].Arguments[argumentIndex] = argument
			result[index].Arguments[argumentIndex].Kinds = slices.Clone(
				argument.Kinds,
			)
			result[index].Arguments[argumentIndex].ListElementKinds = slices.Clone(argument.ListElementKinds)
			result[index].Arguments[argumentIndex].AllowedStrings = slices.Clone(argument.AllowedStrings)
		}
	}
	return result
}

func cloneActions(items []diagnostic.SuggestedFix) []diagnostic.SuggestedFix {
	result := make([]diagnostic.SuggestedFix, len(items))
	for index, item := range items {
		result[index] = item
		if item.AppliesTo != nil {
			location := *item.AppliesTo
			if location.Display != nil {
				display := *location.Display
				location.Display = &display
			}
			result[index].AppliesTo = &location
		}
		result[index].Edits = make(
			[]diagnostic.TextEdit,
			len(item.Edits),
		)
		for editIndex, edit := range item.Edits {
			result[index].Edits[editIndex] = edit
			if edit.DocumentVersion != nil {
				version := *edit.DocumentVersion
				result[index].Edits[editIndex].DocumentVersion = &version
			}
			if edit.Location.Display != nil {
				display := *edit.Location.Display
				result[index].Edits[editIndex].Location.Display = &display
			}
		}
	}
	return result
}

func cloneResult(result Result) Result {
	result.diagnostics = diagnostic.NewSet(result.diagnostics.Items()...)
	result.annotations = slices.Clone(result.annotations)
	result.providerGraph = cloneProviderGraph(result.providerGraph)
	result.autoConfigs = slices.Clone(result.autoConfigs)
	result.moduleGraph = cloneModuleGraph(result.moduleGraph)
	result.configurations = cloneConfigurations(result.configurations)
	result.goInterfaces = cloneGoInterfaceCatalog(result.goInterfaces)
	result.definitions = cloneDefinitions(result.definitions)
	result.actions = cloneActions(result.actions)
	return result
}
