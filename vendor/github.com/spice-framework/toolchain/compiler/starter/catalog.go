// @import { NamedInterface } from "github.com/spice-framework/spice/annotation/modulith"

// Package starter adapts explicitly supplied public starter manifests into
// compiler annotation and application-bootstrap metadata.
//
// @NamedInterface("starter")
package starter

import (
	"fmt"
	"runtime"
	"sort"

	"github.com/spice-framework/spice/annotation"
	"github.com/spice-framework/spice/annotation/sdk"
	startersdk "github.com/spice-framework/spice/annotation/sdk/starter"
	compilerbootstrap "github.com/spice-framework/toolchain/compiler/bootstrap"
	"github.com/spice-framework/toolchain/compiler/provider"
	"github.com/spice-framework/toolchain/compiler/resolve"
)

// Catalog is an immutable, deterministic set of compatible starter manifests
// and their compiler contributions.
type Catalog struct {
	manifests            []startersdk.Manifest
	definitions          []annotation.Definition
	bootstrapDefinitions []compilerbootstrap.Definition
	entryPoints          map[string][]compilerbootstrap.EntryPoint
	entryPointPackages   []string
}

type dependencyContract struct {
	sourceID string
	version  string
	license  string
}

// New validates manifests against the current Spice API and Go runtime.
func New(manifests ...startersdk.Manifest) (Catalog, error) {
	return NewWithCompatibility(startersdk.APIVersion, runtime.Version(), manifests...)
}

// NewWithCompatibility validates, normalizes, and indexes an explicitly
// supplied manifest set. It exists so build tools and tests can make the
// compatibility environment deterministic.
func NewWithCompatibility(
	spiceAPI string,
	goVersion string,
	manifests ...startersdk.Manifest,
) (Catalog, error) {
	if len(manifests) == 0 {
		return Catalog{}, fmt.Errorf("starter catalog requires at least one manifest")
	}

	normalized := append([]startersdk.Manifest(nil), manifests...)
	sort.SliceStable(normalized, func(i, j int) bool {
		return normalized[i].Spec().ID < normalized[j].Spec().ID
	})

	result := Catalog{
		manifests:   normalized,
		entryPoints: make(map[string][]compilerbootstrap.EntryPoint),
	}
	manifestIDs := make(map[string]struct{}, len(normalized))
	annotationSources := make(map[string]string)
	capabilitySources := make(map[compilerbootstrap.Capability]string)
	entryPointSources := make(map[string]string)
	entryPointPackages := make(map[string]struct{})
	dependencyContracts := make(map[string]dependencyContract)
	for _, definition := range compilerbootstrap.Builtins() {
		capabilitySources[definition.Capability] = "Spice built-in"
	}
	for _, manifest := range normalized {
		if err := manifest.Compatible(spiceAPI, goVersion); err != nil {
			return Catalog{}, fmt.Errorf("compose starter catalog: %w", err)
		}
		spec := manifest.Spec()
		if _, duplicate := manifestIDs[spec.ID]; duplicate {
			return Catalog{}, fmt.Errorf(
				"compose starter catalog: duplicate starter manifest %q",
				spec.ID,
			)
		}
		manifestIDs[spec.ID] = struct{}{}
		if err := registerEntryPoints(
			spec,
			entryPointSources,
			entryPointPackages,
		); err != nil {
			return Catalog{}, err
		}
		if err := registerDependencyContracts(spec, dependencyContracts); err != nil {
			return Catalog{}, err
		}

		for _, definition := range manifest.Definitions() {
			if source, duplicate := annotationSources[definition.Name]; duplicate {
				return Catalog{}, fmt.Errorf(
					"compose starter catalog: annotation @%s is contributed by both %q and %q",
					definition.Name,
					source,
					spec.ID,
				)
			}
			annotationSources[definition.Name] = spec.ID
			result.definitions = append(result.definitions, definition)
		}

		for _, feature := range spec.ApplicationFeatures {
			entryPoints := bootstrapEntryPoints(feature.EntryPoints)
			capability := compilerbootstrap.Capability(feature.Capability)
			if source, duplicate := capabilitySources[capability]; duplicate {
				return Catalog{}, fmt.Errorf(
					"compose starter catalog: capability %q is contributed by both %q and %q",
					capability,
					source,
					spec.ID,
				)
			}
			capabilitySources[capability] = spec.ID
			result.bootstrapDefinitions = append(
				result.bootstrapDefinitions,
				bootstrapDefinition(spec, feature, entryPoints),
			)
			result.entryPoints[feature.Annotation] = append(
				[]compilerbootstrap.EntryPoint(nil),
				entryPoints...,
			)
		}
	}

	sort.SliceStable(result.definitions, func(i, j int) bool {
		return result.definitions[i].Name < result.definitions[j].Name
	})
	sort.SliceStable(result.bootstrapDefinitions, func(i, j int) bool {
		left, right := result.bootstrapDefinitions[i], result.bootstrapDefinitions[j]
		if left.Capability != right.Capability {
			return left.Capability < right.Capability
		}
		return left.Annotation < right.Annotation
	})
	for packagePath := range entryPointPackages {
		result.entryPointPackages = append(result.entryPointPackages, packagePath)
	}
	sort.Strings(result.entryPointPackages)
	return result, nil
}

func registerEntryPoints(
	spec startersdk.Spec,
	sources map[string]string,
	packages map[string]struct{},
) error {
	for _, entryPoint := range spec.Activation.EntryPoints {
		key := entryPoint.Package + "\x00" + entryPoint.Symbol
		if source, duplicate := sources[key]; duplicate {
			return fmt.Errorf(
				"compose starter catalog: entrypoint %s.%s is contributed by both %q and %q",
				entryPoint.Package,
				entryPoint.Symbol,
				source,
				spec.ID,
			)
		}
		sources[key] = spec.ID
		packages[entryPoint.Package] = struct{}{}
	}
	return nil
}

func registerDependencyContracts(
	spec startersdk.Spec,
	contracts map[string]dependencyContract,
) error {
	for _, dependency := range spec.Dependencies {
		contract, duplicate := contracts[dependency.Module]
		if duplicate &&
			(contract.version != dependency.Version ||
				contract.license != dependency.License) {
			return fmt.Errorf(
				"compose starter catalog: dependency %s has conflicting reviews from %q (%s, %s) and %q (%s, %s)",
				dependency.Module,
				contract.sourceID,
				contract.version,
				contract.license,
				spec.ID,
				dependency.Version,
				dependency.License,
			)
		}
		if !duplicate {
			contracts[dependency.Module] = dependencyContract{
				sourceID: spec.ID,
				version:  dependency.Version,
				license:  dependency.License,
			}
		}
	}
	return nil
}

// Registry returns base plus every contributed annotation definition. A
// collision with a built-in or another base definition fails closed.
func (catalog Catalog) Registry(base annotation.Registry) (annotation.Registry, error) {
	definitions := base.Definitions()
	definitions = append(definitions, catalog.definitions...)
	registry, err := annotation.NewRegistry(definitions...)
	if err != nil {
		return annotation.Registry{}, fmt.Errorf("compose starter annotation registry: %w", err)
	}
	return registry, nil
}

// Manifests returns compatible manifests in stable identity order.
func (catalog Catalog) Manifests() []startersdk.Manifest {
	return append([]startersdk.Manifest(nil), catalog.manifests...)
}

// BootstrapDefinitions returns fresh feature definitions suitable for
// application.BuildOptions.
func (catalog Catalog) BootstrapDefinitions() []compilerbootstrap.Definition {
	return cloneBootstrapDefinitions(catalog.bootstrapDefinitions)
}

// EntryPoints returns the explicit starter constructors associated with a
// contributed application annotation.
func (catalog Catalog) EntryPoints(annotationName string) []compilerbootstrap.EntryPoint {
	return append(
		[]compilerbootstrap.EntryPoint(nil),
		catalog.entryPoints[annotationName]...,
	)
}

// EntryPointPackages returns every exact constructor package required to
// resolve this explicit catalog in one typed compiler program.
func (catalog Catalog) EntryPointPackages() []string {
	return append([]string(nil), catalog.entryPointPackages...)
}

// ProviderEntrypoints returns constructors activated by the resolved
// application source. Explicit-constructor manifests activate all declared
// constructors; explicit-annotation manifests activate only feature-mapped
// constructors whose annotation occurs on an @Application marker. Repeated
// occurrences never duplicate a provider.
func (catalog Catalog) ProviderEntrypoints(
	occurrences []resolve.Occurrence,
) []provider.Entrypoint {
	annotations := applicationAnnotations(occurrences)
	var result []provider.Entrypoint
	for _, manifest := range catalog.manifests {
		spec := manifest.Spec()
		selected := selectedEntryPoints(spec, annotations)
		for _, entryPoint := range selected {
			result = append(result, provider.Entrypoint{
				PackagePath:   entryPoint.Package,
				Symbol:        entryPoint.Symbol,
				SourceID:      spec.ID,
				SourceVersion: spec.Version,
			})
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].PackagePath != result[j].PackagePath {
			return result[i].PackagePath < result[j].PackagePath
		}
		if result[i].Symbol != result[j].Symbol {
			return result[i].Symbol < result[j].Symbol
		}
		return result[i].SourceID < result[j].SourceID
	})
	return result
}

func applicationAnnotations(
	occurrences []resolve.Occurrence,
) map[string]struct{} {
	applicationSymbols := make(map[string]struct{})
	for _, occurrence := range occurrences {
		if occurrence.HasContribution(sdk.ContributionApplication) &&
			occurrence.SymbolID != "" {
			applicationSymbols[occurrence.SymbolID] = struct{}{}
		}
	}
	annotations := make(map[string]struct{}, len(occurrences))
	for _, occurrence := range occurrences {
		if _, application := applicationSymbols[occurrence.SymbolID]; application {
			annotations[occurrence.Annotation.Name] = struct{}{}
		}
	}
	return annotations
}

func selectedEntryPoints(
	spec startersdk.Spec,
	annotations map[string]struct{},
) []startersdk.EntryPoint {
	if spec.Activation.Mode == startersdk.ActivationExplicitConstructor {
		return append([]startersdk.EntryPoint(nil), spec.Activation.EntryPoints...)
	}
	selected := make(map[string]startersdk.EntryPoint)
	for _, feature := range spec.ApplicationFeatures {
		if _, enabled := annotations[feature.Annotation]; !enabled {
			continue
		}
		for _, entryPoint := range feature.EntryPoints {
			selected[entryPoint.Package+"\x00"+entryPoint.Symbol] = entryPoint
		}
	}
	result := make([]startersdk.EntryPoint, 0, len(selected))
	for _, entryPoint := range selected {
		result = append(result, entryPoint)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Package != result[j].Package {
			return result[i].Package < result[j].Package
		}
		return result[i].Symbol < result[j].Symbol
	})
	return result
}

func bootstrapDefinition(
	spec startersdk.Spec,
	feature startersdk.FeatureSpec,
	entryPoints []compilerbootstrap.EntryPoint,
) compilerbootstrap.Definition {
	options := make([]compilerbootstrap.OptionDefinition, len(feature.Options))
	for index, option := range feature.Options {
		options[index] = compilerbootstrap.OptionDefinition{
			Name:           option.Name,
			Kind:           option.Kind,
			ListItemKinds:  append([]annotation.Kind(nil), option.ListItemKinds...),
			AllowedStrings: append([]string(nil), option.AllowedStrings...),
			Required:       option.Required,
			UniqueItems:    option.UniqueItems,
			MinimumItems:   option.MinimumItems,
			SortItems:      option.SortItems,
		}
	}
	requirements := make([]compilerbootstrap.RuntimeCapability, len(feature.Requirements))
	for index, requirement := range feature.Requirements {
		requirements[index] = compilerbootstrap.RuntimeCapability(requirement)
	}
	return compilerbootstrap.Definition{
		Annotation:    feature.Annotation,
		Capability:    compilerbootstrap.Capability(feature.Capability),
		Options:       options,
		Requirements:  requirements,
		SourceID:      spec.ID,
		SourceVersion: spec.Version,
		EntryPoints:   append([]compilerbootstrap.EntryPoint(nil), entryPoints...),
	}
}

func bootstrapEntryPoints(values []startersdk.EntryPoint) []compilerbootstrap.EntryPoint {
	result := make([]compilerbootstrap.EntryPoint, len(values))
	for index, value := range values {
		result[index] = compilerbootstrap.EntryPoint{
			Package: value.Package,
			Symbol:  value.Symbol,
		}
	}
	return result
}

func cloneBootstrapDefinitions(
	definitions []compilerbootstrap.Definition,
) []compilerbootstrap.Definition {
	result := make([]compilerbootstrap.Definition, len(definitions))
	for index, definition := range definitions {
		result[index] = definition
		result[index].Options = make([]compilerbootstrap.OptionDefinition, len(definition.Options))
		for optionIndex, option := range definition.Options {
			result[index].Options[optionIndex] = option
			result[index].Options[optionIndex].ListItemKinds = append(
				[]annotation.Kind(nil),
				option.ListItemKinds...,
			)
			result[index].Options[optionIndex].AllowedStrings = append(
				[]string(nil),
				option.AllowedStrings...,
			)
		}
		result[index].Requirements = append(
			[]compilerbootstrap.RuntimeCapability(nil),
			definition.Requirements...,
		)
		result[index].EntryPoints = append(
			[]compilerbootstrap.EntryPoint(nil),
			definition.EntryPoints...,
		)
	}
	return result
}
