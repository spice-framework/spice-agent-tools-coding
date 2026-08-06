package starter

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spice-framework/toolchain/compiler/resolve"
)

// DependencyRequirement identifies one exact reviewed module version required
// by a selected starter.
type DependencyRequirement struct {
	SourceID string
	Module   string
	Version  string
	License  string
}

// ModuleVersion is one selected module from the application's Go module graph.
// Replacement fields are empty when the module is not replaced.
type ModuleVersion struct {
	Path               string
	Version            string
	Main               bool
	ReplacementPath    string
	ReplacementVersion string
}

// DependencyDiagnostic is one deterministic starter dependency-alignment
// failure.
type DependencyDiagnostic struct {
	SourceID string
	Module   string
	Kind     string
	Message  string
}

// Error renders the dependency diagnostic.
func (diagnostic DependencyDiagnostic) Error() string {
	return diagnostic.Message
}

// Dependencies returns every reviewed dependency requirement in deterministic
// module and starter order.
func (catalog Catalog) Dependencies() []DependencyRequirement {
	return catalog.dependencies(nil)
}

// ActiveDependencies returns reviewed requirements only for manifests
// activated by the supplied application annotations.
func (catalog Catalog) ActiveDependencies(
	occurrences []resolve.Occurrence,
) []DependencyRequirement {
	annotations := applicationAnnotations(occurrences)
	active := make(map[string]struct{})
	for _, manifest := range catalog.manifests {
		spec := manifest.Spec()
		if len(selectedEntryPoints(spec, annotations)) != 0 {
			active[spec.ID] = struct{}{}
		}
	}
	return catalog.dependencies(active)
}

func (catalog Catalog) dependencies(
	active map[string]struct{},
) []DependencyRequirement {
	var result []DependencyRequirement
	for _, manifest := range catalog.manifests {
		spec := manifest.Spec()
		if active != nil {
			if _, selected := active[spec.ID]; !selected {
				continue
			}
		}
		for _, dependency := range spec.Dependencies {
			result = append(result, DependencyRequirement{
				SourceID: spec.ID,
				Module:   dependency.Module,
				Version:  dependency.Version,
				License:  dependency.License,
			})
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Module != result[j].Module {
			return result[i].Module < result[j].Module
		}
		return result[i].SourceID < result[j].SourceID
	})
	return result
}

// ValidateModuleVersions requires every reviewed dependency to resolve to its
// exact reviewed version. Unreviewed replacements fail closed.
func (catalog Catalog) ValidateModuleVersions(
	modules []ModuleVersion,
) []DependencyDiagnostic {
	return validateModuleVersions(catalog.Dependencies(), modules)
}

// ValidateActiveModuleVersions validates reviewed dependencies only for
// starters activated by the supplied application annotations.
func (catalog Catalog) ValidateActiveModuleVersions(
	occurrences []resolve.Occurrence,
	modules []ModuleVersion,
) []DependencyDiagnostic {
	return validateModuleVersions(catalog.ActiveDependencies(occurrences), modules)
}

func validateModuleVersions(
	requirements []DependencyRequirement,
	modules []ModuleVersion,
) []DependencyDiagnostic {
	if len(requirements) == 0 {
		return nil
	}
	index, diagnostics := moduleVersionIndex(modules)
	for _, requirement := range requirements {
		module, found := index[requirement.Module]
		switch {
		case !found:
			diagnostics = append(diagnostics, dependencyDiagnostic(
				requirement,
				"missing",
				fmt.Sprintf(
					"starter %q requires reviewed dependency %s at %s, but that module is absent from the application build list",
					requirement.SourceID,
					requirement.Module,
					requirement.Version,
				),
			))
		case module.ReplacementPath != "" &&
			(module.ReplacementPath != requirement.Module ||
				module.ReplacementVersion != requirement.Version):
			replacement := module.ReplacementPath
			if module.ReplacementVersion != "" {
				replacement += " " + module.ReplacementVersion
			}
			diagnostics = append(diagnostics, dependencyDiagnostic(
				requirement,
				"replacement",
				fmt.Sprintf(
					"starter %q requires reviewed dependency %s at %s, but the application replaces it with %s; review and publish matching starter metadata before using a replacement",
					requirement.SourceID,
					requirement.Module,
					requirement.Version,
					replacement,
				),
			))
		case resolvedModuleVersion(module) != requirement.Version:
			diagnostics = append(diagnostics, dependencyDiagnostic(
				requirement,
				"version",
				fmt.Sprintf(
					"starter %q requires reviewed dependency %s at %s, but the application resolves %s",
					requirement.SourceID,
					requirement.Module,
					requirement.Version,
					renderModuleVersion(module),
				),
			))
		}
	}
	sortDependencyDiagnostics(diagnostics)
	return diagnostics
}

func moduleVersionIndex(
	modules []ModuleVersion,
) (map[string]ModuleVersion, []DependencyDiagnostic) {
	result := make(map[string]ModuleVersion, len(modules))
	var diagnostics []DependencyDiagnostic
	for _, module := range modules {
		if module.Path == "" || strings.TrimSpace(module.Path) != module.Path {
			diagnostics = append(diagnostics, DependencyDiagnostic{
				Kind:    "invalid-module-graph",
				Message: fmt.Sprintf("Go module graph contains invalid module path %q", module.Path),
			})
			continue
		}
		if _, duplicate := result[module.Path]; duplicate {
			diagnostics = append(diagnostics, DependencyDiagnostic{
				Module:  module.Path,
				Kind:    "invalid-module-graph",
				Message: fmt.Sprintf("Go module graph contains duplicate module %q", module.Path),
			})
			continue
		}
		result[module.Path] = module
	}
	return result, diagnostics
}

func dependencyDiagnostic(
	requirement DependencyRequirement,
	kind string,
	message string,
) DependencyDiagnostic {
	return DependencyDiagnostic{
		SourceID: requirement.SourceID,
		Module:   requirement.Module,
		Kind:     kind,
		Message:  message,
	}
}

func resolvedModuleVersion(module ModuleVersion) string {
	if module.ReplacementPath != "" {
		return module.ReplacementVersion
	}
	return module.Version
}

func renderModuleVersion(module ModuleVersion) string {
	version := resolvedModuleVersion(module)
	if version == "" {
		if module.Main {
			return "the unversioned main module"
		}
		return "an unversioned module"
	}
	return version
}

func sortDependencyDiagnostics(diagnostics []DependencyDiagnostic) {
	sort.SliceStable(diagnostics, func(i, j int) bool {
		if diagnostics[i].Module != diagnostics[j].Module {
			return diagnostics[i].Module < diagnostics[j].Module
		}
		if diagnostics[i].SourceID != diagnostics[j].SourceID {
			return diagnostics[i].SourceID < diagnostics[j].SourceID
		}
		if diagnostics[i].Kind != diagnostics[j].Kind {
			return diagnostics[i].Kind < diagnostics[j].Kind
		}
		return diagnostics[i].Message < diagnostics[j].Message
	})
}
