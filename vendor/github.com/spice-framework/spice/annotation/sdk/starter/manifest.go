// Package starter defines deterministic compatibility and annotation metadata
// for opt-in Spice integrations.
package starter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/spice-framework/spice/annotation"
)

const (
	// Schema is the current starter manifest wire schema.
	Schema = "spice.starter/v1"
	// APIVersion is the compiler/runtime contract implemented by this Spice line.
	APIVersion = "v1alpha1"
)

// ActivationMode identifies how application code deliberately enables a
// starter. Dependency presence is never an activation mode.
type ActivationMode string

const (
	// ActivationExplicitConstructor requires application or generated code to
	// call a declared constructor.
	ActivationExplicitConstructor ActivationMode = "explicit-constructor"
	// ActivationExplicitAnnotation requires a declared qualified annotation and
	// compiler selection of its declared entrypoints.
	ActivationExplicitAnnotation ActivationMode = "explicit-annotation"
)

// EntryPoint identifies one exported Go constructor or factory.
type EntryPoint struct {
	Package string `json:"package"`
	Symbol  string `json:"symbol"`
}

// Dependency records one reviewed direct third-party module.
type Dependency struct {
	Module  string `json:"module"`
	Version string `json:"version"`
	License string `json:"license"`
}

// ArgumentSpec is the portable annotation-argument definition used by starter
// manifests.
type ArgumentSpec struct {
	Name             string            `json:"name"`
	Kinds            []annotation.Kind `json:"kinds"`
	ListElementKinds []annotation.Kind `json:"list_element_kinds,omitempty"`
	Required         bool              `json:"required,omitempty"`
	Positional       bool              `json:"positional,omitempty"`
}

// AnnotationSpec is one qualified annotation contributed by a starter.
type AnnotationSpec struct {
	Name       string              `json:"name"`
	Targets    []annotation.Target `json:"targets"`
	Repeatable bool                `json:"repeatable,omitempty"`
	Arguments  []ArgumentSpec      `json:"arguments,omitempty"`
}

// OptionSpec defines deterministic semantic validation for one application
// feature option.
type OptionSpec struct {
	Name           string            `json:"name"`
	Kind           annotation.Kind   `json:"kind"`
	ListItemKinds  []annotation.Kind `json:"list_item_kinds,omitempty"`
	AllowedStrings []string          `json:"allowed_strings,omitempty"`
	Required       bool              `json:"required,omitempty"`
	UniqueItems    bool              `json:"unique_items,omitempty"`
	MinimumItems   int               `json:"minimum_items,omitempty"`
	SortItems      bool              `json:"sort_items,omitempty"`
}

// FeatureSpec maps a contributed application annotation to an immutable
// capability and its runtime requirements.
type FeatureSpec struct {
	Annotation   string       `json:"annotation"`
	Capability   string       `json:"capability"`
	EntryPoints  []EntryPoint `json:"entry_points"`
	Options      []OptionSpec `json:"options,omitempty"`
	Requirements []string     `json:"requirements,omitempty"`
}

// Activation declares the only supported explicit activation paths.
type Activation struct {
	Mode        ActivationMode `json:"mode"`
	EntryPoints []EntryPoint   `json:"entry_points"`
}

// Spec is the portable input used to construct or decode a Manifest.
type Spec struct {
	Schema              string           `json:"schema"`
	ID                  string           `json:"id"`
	Version             string           `json:"version"`
	Module              string           `json:"module"`
	SpiceAPI            string           `json:"spice_api"`
	MinimumGo           string           `json:"minimum_go"`
	License             string           `json:"license"`
	Review              string           `json:"review"`
	Activation          Activation       `json:"activation"`
	Capabilities        []string         `json:"capabilities"`
	Dependencies        []Dependency     `json:"dependencies,omitempty"`
	Annotations         []AnnotationSpec `json:"annotations,omitempty"`
	ApplicationFeatures []FeatureSpec    `json:"application_features,omitempty"`
}

// Manifest is an immutable, validated starter compatibility record.
type Manifest struct {
	spec        Spec
	definitions []annotation.Definition
}

var (
	semanticVersionPattern = regexp.MustCompile(
		`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?(\+[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?$`,
	)
	apiVersionPattern = regexp.MustCompile(`^v[1-9][0-9]*(?:(?:alpha|beta)[1-9][0-9]*)?$`)
	modulePathPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~/-]*$`)
	symbolPattern     = regexp.MustCompile(`^[A-Z][A-Za-z0-9_]*$`)
	annotationPattern = regexp.MustCompile(
		`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)+$`,
	)
	identityPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[.-][a-z0-9]+)*$`)
	licensePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+-]*$`)
)

// New validates and deterministically normalizes a starter specification.
func New(spec Spec) (Manifest, error) {
	_, err := validateSpec(spec)
	if err != nil {
		return Manifest{}, err
	}
	normalized := cloneSpec(spec)
	normalizeSpec(&normalized)
	definitions, err := annotationDefinitions(normalized.Annotations)
	if err != nil {
		return Manifest{}, fmt.Errorf("normalize starter manifest annotations: %w", err)
	}
	return Manifest{
		spec:        normalized,
		definitions: cloneDefinitions(definitions),
	}, nil
}

// Must constructs a manifest or panics for package-owned invalid metadata.
func Must(spec Spec) Manifest {
	manifest, err := New(spec)
	if err != nil {
		panic(err)
	}
	return manifest
}

// Parse strictly decodes and validates one starter manifest.
func Parse(content []byte) (Manifest, error) {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var spec Spec
	if err := decoder.Decode(&spec); err != nil {
		return Manifest{}, fmt.Errorf("decode starter manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Manifest{}, errors.New("decode starter manifest: trailing JSON value")
		}
		return Manifest{}, fmt.Errorf("decode starter manifest trailing data: %w", err)
	}
	manifest, err := New(spec)
	if err != nil {
		return Manifest{}, fmt.Errorf("validate starter manifest: %w", err)
	}
	return manifest, nil
}

// Spec returns a defensive copy of the normalized portable specification.
func (manifest Manifest) Spec() Spec {
	return cloneSpec(manifest.spec)
}

// Definitions returns defensive annotation definitions suitable for an
// explicitly composed compiler registry.
func (manifest Manifest) Definitions() []annotation.Definition {
	return cloneDefinitions(manifest.definitions)
}

// JSON returns canonical indented JSON with a final newline.
func (manifest Manifest) JSON() ([]byte, error) {
	validated, err := New(manifest.spec)
	if err != nil {
		return nil, fmt.Errorf("encode starter manifest: %w", err)
	}
	content, err := json.MarshalIndent(validated.spec, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode starter manifest: %w", err)
	}
	return append(content, '\n'), nil
}

// MarshalJSON implements json.Marshaler using the normalized wire spec.
func (manifest Manifest) MarshalJSON() ([]byte, error) {
	validated, err := New(manifest.spec)
	if err != nil {
		return nil, fmt.Errorf("encode starter manifest: %w", err)
	}
	return json.Marshal(validated.spec)
}

// Compatible verifies the exact Spice API and minimum Go version contract.
func (manifest Manifest) Compatible(spiceAPI, goVersion string) error {
	validated, err := New(manifest.spec)
	if err != nil {
		return fmt.Errorf("check starter compatibility: %w", err)
	}
	spec := validated.spec
	if spiceAPI != spec.SpiceAPI {
		return fmt.Errorf(
			"starter %q requires Spice API %q, got %q",
			spec.ID,
			spec.SpiceAPI,
			spiceAPI,
		)
	}
	current, err := parseGoVersion(goVersion)
	if err != nil {
		return fmt.Errorf("check starter %q compatibility: %w", spec.ID, err)
	}
	minimum, err := parseGoVersion(spec.MinimumGo)
	if err != nil {
		return fmt.Errorf("check starter %q compatibility: invalid manifest minimum Go version: %w", spec.ID, err)
	}
	if compareGoVersion(current, minimum) < 0 {
		return fmt.Errorf(
			"starter %q requires Go %s or newer, got %s",
			spec.ID,
			spec.MinimumGo,
			goVersion,
		)
	}
	return nil
}

func validateSpec(spec Spec) ([]annotation.Definition, error) {
	if err := validateSpecIdentity(spec); err != nil {
		return nil, err
	}
	if _, err := parseGoVersion(spec.MinimumGo); err != nil {
		return nil, fmt.Errorf("starter %q minimum Go version: %w", spec.ID, err)
	}
	if err := validateCapabilities(spec.Capabilities); err != nil {
		return nil, fmt.Errorf("starter %q: %w", spec.ID, err)
	}
	if err := validateActivation(spec); err != nil {
		return nil, fmt.Errorf("starter %q: %w", spec.ID, err)
	}
	if err := validateDependencies(spec.Dependencies); err != nil {
		return nil, fmt.Errorf("starter %q: %w", spec.ID, err)
	}
	definitions, err := annotationDefinitions(spec.Annotations)
	if err != nil {
		return nil, fmt.Errorf("starter %q annotations: %w", spec.ID, err)
	}
	if err := validateFeatures(spec, definitions); err != nil {
		return nil, fmt.Errorf("starter %q application features: %w", spec.ID, err)
	}
	return definitions, nil
}

func validateSpecIdentity(spec Spec) error {
	switch {
	case spec.Schema != Schema:
		return fmt.Errorf("starter manifest schema must be %q, got %q", Schema, spec.Schema)
	case !validModulePath(spec.ID):
		return fmt.Errorf("starter manifest ID %q is not a valid import-path identity", spec.ID)
	case !validModulePath(spec.Module):
		return fmt.Errorf("starter %q module %q is not a valid module path", spec.ID, spec.Module)
	case spec.ID != spec.Module && !strings.HasPrefix(spec.ID, spec.Module+"/"):
		return fmt.Errorf("starter %q must belong to module %q", spec.ID, spec.Module)
	case !semanticVersionPattern.MatchString(spec.Version):
		return fmt.Errorf("starter %q version %q is not semantic version metadata", spec.ID, spec.Version)
	case !apiVersionPattern.MatchString(spec.SpiceAPI):
		return fmt.Errorf("starter %q Spice API %q is invalid", spec.ID, spec.SpiceAPI)
	case !licensePattern.MatchString(spec.License):
		return fmt.Errorf("starter %q license %q is not an SPDX-style identity", spec.ID, spec.License)
	case strings.TrimSpace(spec.Review) == "" || strings.TrimSpace(spec.Review) != spec.Review:
		return fmt.Errorf("starter %q requires a trimmed dependency review reference", spec.ID)
	}
	return nil
}

func validateCapabilities(capabilities []string) error {
	if len(capabilities) == 0 {
		return errors.New("at least one capability is required")
	}
	return validateUniqueIdentities("capability", capabilities)
}

func validateActivation(spec Spec) error {
	if len(spec.Activation.EntryPoints) == 0 {
		return errors.New("activation requires at least one entrypoint")
	}
	seen := make(map[string]struct{}, len(spec.Activation.EntryPoints))
	for _, entrypoint := range spec.Activation.EntryPoints {
		if !validModulePath(entrypoint.Package) {
			return fmt.Errorf("activation entrypoint package %q is invalid", entrypoint.Package)
		}
		if entrypoint.Package != spec.Module &&
			!strings.HasPrefix(entrypoint.Package, spec.Module+"/") {
			return fmt.Errorf(
				"activation entrypoint package %q must belong to starter module %q",
				entrypoint.Package,
				spec.Module,
			)
		}
		if !symbolPattern.MatchString(entrypoint.Symbol) {
			return fmt.Errorf("activation entrypoint symbol %q must be exported", entrypoint.Symbol)
		}
		key := entrypoint.Package + "\x00" + entrypoint.Symbol
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf(
				"activation entrypoint %s.%s is duplicated",
				entrypoint.Package,
				entrypoint.Symbol,
			)
		}
		seen[key] = struct{}{}
	}
	switch spec.Activation.Mode {
	case ActivationExplicitConstructor:
		if len(spec.Annotations) != 0 || len(spec.ApplicationFeatures) != 0 {
			return errors.New("explicit-constructor activation cannot declare annotations or application features")
		}
	case ActivationExplicitAnnotation:
		if len(spec.Annotations) == 0 || len(spec.ApplicationFeatures) == 0 {
			return errors.New("explicit-annotation activation requires annotations and application features")
		}
	default:
		return fmt.Errorf("activation mode %q is unsupported", spec.Activation.Mode)
	}
	return nil
}

func validateDependencies(dependencies []Dependency) error {
	seen := make(map[string]struct{}, len(dependencies))
	for _, dependency := range dependencies {
		if !validModulePath(dependency.Module) {
			return fmt.Errorf("dependency module %q is invalid", dependency.Module)
		}
		if !semanticVersionPattern.MatchString(strings.TrimPrefix(dependency.Version, "v")) {
			return fmt.Errorf(
				"dependency %q version %q is not semantic version metadata",
				dependency.Module,
				dependency.Version,
			)
		}
		if !licensePattern.MatchString(dependency.License) {
			return fmt.Errorf(
				"dependency %q license %q is not an SPDX-style identity",
				dependency.Module,
				dependency.License,
			)
		}
		if _, duplicate := seen[dependency.Module]; duplicate {
			return fmt.Errorf("dependency module %q is duplicated", dependency.Module)
		}
		seen[dependency.Module] = struct{}{}
	}
	return nil
}

func annotationDefinitions(specs []AnnotationSpec) ([]annotation.Definition, error) {
	definitions := make([]annotation.Definition, 0, len(specs))
	for _, spec := range specs {
		if !annotationPattern.MatchString(spec.Name) {
			return nil, fmt.Errorf("annotation name %q must be qualified", spec.Name)
		}
		if err := validateAnnotationSpec(spec); err != nil {
			return nil, fmt.Errorf("annotation %q: %w", spec.Name, err)
		}
		targets, err := annotation.NewTargetSet(spec.Targets...)
		if err != nil {
			return nil, fmt.Errorf("annotation %q targets: %w", spec.Name, err)
		}
		arguments := make([]annotation.ArgumentDefinition, len(spec.Arguments))
		for index, argument := range spec.Arguments {
			arguments[index] = annotation.ArgumentDefinition{
				Name:             argument.Name,
				Kinds:            append([]annotation.Kind(nil), argument.Kinds...),
				ListElementKinds: append([]annotation.Kind(nil), argument.ListElementKinds...),
				Required:         argument.Required,
				Positional:       argument.Positional,
			}
		}
		definitions = append(definitions, annotation.Definition{
			Name:       spec.Name,
			Targets:    targets,
			Repeatable: spec.Repeatable,
			Arguments:  arguments,
		})
	}
	registry, err := annotation.NewRegistry(definitions...)
	if err != nil {
		return nil, err
	}
	return registry.Definitions(), nil
}

func validateAnnotationSpec(spec AnnotationSpec) error {
	seenTargets := make(map[annotation.Target]struct{}, len(spec.Targets))
	for _, target := range spec.Targets {
		if _, duplicate := seenTargets[target]; duplicate {
			return fmt.Errorf("target %q is duplicated", target)
		}
		seenTargets[target] = struct{}{}
	}
	for _, argument := range spec.Arguments {
		if err := validateKinds("kind", argument.Kinds); err != nil {
			return fmt.Errorf("argument %q: %w", argument.Name, err)
		}
		if err := validateKinds("list element kind", argument.ListElementKinds); err != nil {
			return fmt.Errorf("argument %q: %w", argument.Name, err)
		}
	}
	return nil
}

func validateKinds(label string, values []annotation.Kind) error {
	seen := make(map[annotation.Kind]struct{}, len(values))
	for _, value := range values {
		switch value {
		case annotation.KindString,
			annotation.KindInteger,
			annotation.KindBoolean,
			annotation.KindIdentifier,
			annotation.KindList:
		default:
			return fmt.Errorf("%s %q is unsupported", label, value)
		}
		if _, duplicate := seen[value]; duplicate {
			return fmt.Errorf("%s %q is duplicated", label, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func validateFeatures(spec Spec, definitions []annotation.Definition) error {
	definitionIndex := make(map[string]annotation.Definition, len(definitions))
	for _, definition := range definitions {
		definitionIndex[definition.Name] = definition
	}
	capabilities := make(map[string]struct{}, len(spec.Capabilities))
	for _, capability := range spec.Capabilities {
		capabilities[capability] = struct{}{}
	}
	seenAnnotations := make(map[string]struct{}, len(spec.ApplicationFeatures))
	seenCapabilities := make(map[string]struct{}, len(spec.ApplicationFeatures))
	declaredEntryPoints := make(map[string]struct{}, len(spec.Activation.EntryPoints))
	for _, entryPoint := range spec.Activation.EntryPoints {
		declaredEntryPoints[entryPointKey(entryPoint)] = struct{}{}
	}
	referencedEntryPoints := make(map[string]struct{}, len(spec.Activation.EntryPoints))
	for _, feature := range spec.ApplicationFeatures {
		definition, found := definitionIndex[feature.Annotation]
		if !found {
			return fmt.Errorf("feature annotation %q is not declared", feature.Annotation)
		}
		if !definition.Targets.Contains(annotation.TargetFunction) {
			return fmt.Errorf("feature annotation %q must allow function targets", feature.Annotation)
		}
		if _, declared := capabilities[feature.Capability]; !declared {
			return fmt.Errorf("feature capability %q is not declared by the starter", feature.Capability)
		}
		if _, duplicate := seenAnnotations[feature.Annotation]; duplicate {
			return fmt.Errorf("feature annotation %q is duplicated", feature.Annotation)
		}
		if _, duplicate := seenCapabilities[feature.Capability]; duplicate {
			return fmt.Errorf("feature capability %q is duplicated", feature.Capability)
		}
		seenAnnotations[feature.Annotation] = struct{}{}
		seenCapabilities[feature.Capability] = struct{}{}
		if err := validateFeatureEntryPoints(
			feature,
			declaredEntryPoints,
			referencedEntryPoints,
		); err != nil {
			return err
		}
		if err := validateFeatureOptions(feature, definition); err != nil {
			return err
		}
		if err := validateUniqueIdentities("runtime requirement", feature.Requirements); err != nil {
			return fmt.Errorf("feature @%s: %w", feature.Annotation, err)
		}
	}
	return validateActivationCoverage(spec, referencedEntryPoints)
}

func validateActivationCoverage(spec Spec, referenced map[string]struct{}) error {
	if spec.Activation.Mode != ActivationExplicitAnnotation {
		return nil
	}
	for _, entryPoint := range spec.Activation.EntryPoints {
		if _, found := referenced[entryPointKey(entryPoint)]; found {
			continue
		}
		return fmt.Errorf(
			"activation entrypoint %s.%s is not selected by any application feature",
			entryPoint.Package,
			entryPoint.Symbol,
		)
	}
	return nil
}

func validateFeatureEntryPoints(
	feature FeatureSpec,
	declared map[string]struct{},
	referenced map[string]struct{},
) error {
	if len(feature.EntryPoints) == 0 {
		return fmt.Errorf(
			"feature @%s requires at least one activation entrypoint",
			feature.Annotation,
		)
	}
	seen := make(map[string]struct{}, len(feature.EntryPoints))
	for _, entryPoint := range feature.EntryPoints {
		key := entryPointKey(entryPoint)
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf(
				"feature @%s entrypoint %s.%s is duplicated",
				feature.Annotation,
				entryPoint.Package,
				entryPoint.Symbol,
			)
		}
		if _, found := declared[key]; !found {
			return fmt.Errorf(
				"feature @%s entrypoint %s.%s is not declared by activation",
				feature.Annotation,
				entryPoint.Package,
				entryPoint.Symbol,
			)
		}
		seen[key] = struct{}{}
		referenced[key] = struct{}{}
	}
	return nil
}

func entryPointKey(entryPoint EntryPoint) string {
	return entryPoint.Package + "\x00" + entryPoint.Symbol
}

func validateFeatureOptions(feature FeatureSpec, definition annotation.Definition) error {
	arguments := make(map[string]annotation.ArgumentDefinition, len(definition.Arguments))
	for _, argument := range definition.Arguments {
		arguments[argument.Name] = argument
	}
	seen := make(map[string]struct{}, len(feature.Options))
	for _, option := range feature.Options {
		argument, found := arguments[option.Name]
		if !found {
			return fmt.Errorf("feature @%s option %q is not an annotation argument", feature.Annotation, option.Name)
		}
		if _, duplicate := seen[option.Name]; duplicate {
			return fmt.Errorf("feature @%s option %q is duplicated", feature.Annotation, option.Name)
		}
		seen[option.Name] = struct{}{}
		if !containsKind(argument.Kinds, option.Kind) {
			return fmt.Errorf(
				"feature @%s option %q kind %q is not accepted by its annotation argument",
				feature.Annotation,
				option.Name,
				option.Kind,
			)
		}
		if option.MinimumItems < 0 {
			return fmt.Errorf("feature @%s option %q has a negative minimum", feature.Annotation, option.Name)
		}
		if err := validateKinds("list item kind", option.ListItemKinds); err != nil {
			return fmt.Errorf("feature @%s option %q: %w", feature.Annotation, option.Name, err)
		}
		for _, kind := range option.ListItemKinds {
			if !containsKind(argument.ListElementKinds, kind) {
				return fmt.Errorf(
					"feature @%s option %q list kind %q is not accepted by its annotation argument",
					feature.Annotation,
					option.Name,
					kind,
				)
			}
		}
		if err := validateUniqueStrings("allowed string", option.AllowedStrings); err != nil {
			return fmt.Errorf("feature @%s option %q: %w", feature.Annotation, option.Name, err)
		}
	}
	return nil
}

func validModulePath(value string) bool {
	if value == "" ||
		strings.TrimSpace(value) != value ||
		strings.Contains(value, "\\") ||
		strings.Contains(value, "//") ||
		strings.HasSuffix(value, "/") ||
		!modulePathPattern.MatchString(value) {
		return false
	}
	for segment := range strings.SplitSeq(value, "/") {
		if segment == "." || segment == ".." || segment == "" {
			return false
		}
	}
	return true
}

func validateUniqueIdentities(label string, values []string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !identityPattern.MatchString(value) {
			return fmt.Errorf("%s %q is invalid", label, value)
		}
		if _, duplicate := seen[value]; duplicate {
			return fmt.Errorf("%s %q is duplicated", label, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func validateUniqueStrings(label string, values []string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != value {
			return fmt.Errorf("%s %q contains surrounding whitespace", label, value)
		}
		if _, duplicate := seen[value]; duplicate {
			return fmt.Errorf("%s %q is duplicated", label, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func containsKind(values []annotation.Kind, target annotation.Kind) bool {
	return slices.Contains(values, target)
}

func parseGoVersion(value string) ([3]int, error) {
	normalized := strings.TrimPrefix(value, "go")
	parts := strings.Split(normalized, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return [3]int{}, fmt.Errorf("go version %q must contain major and minor numbers", value)
	}
	var result [3]int
	for index, part := range parts {
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 {
			return [3]int{}, fmt.Errorf("go version %q contains invalid number %q", value, part)
		}
		result[index] = number
	}
	if result[0] < 1 {
		return [3]int{}, fmt.Errorf("go version %q has an invalid major version", value)
	}
	return result, nil
}

func compareGoVersion(left, right [3]int) int {
	for index := range left {
		switch {
		case left[index] < right[index]:
			return -1
		case left[index] > right[index]:
			return 1
		}
	}
	return 0
}

func normalizeSpec(spec *Spec) {
	sort.Strings(spec.Capabilities)
	sort.SliceStable(spec.Activation.EntryPoints, func(i, j int) bool {
		left, right := spec.Activation.EntryPoints[i], spec.Activation.EntryPoints[j]
		if left.Package != right.Package {
			return left.Package < right.Package
		}
		return left.Symbol < right.Symbol
	})
	sort.SliceStable(spec.Dependencies, func(i, j int) bool {
		return spec.Dependencies[i].Module < spec.Dependencies[j].Module
	})
	for index := range spec.Annotations {
		normalizeAnnotation(&spec.Annotations[index])
	}
	sort.SliceStable(spec.Annotations, func(i, j int) bool {
		return spec.Annotations[i].Name < spec.Annotations[j].Name
	})
	for index := range spec.ApplicationFeatures {
		normalizeFeature(&spec.ApplicationFeatures[index])
	}
	sort.SliceStable(spec.ApplicationFeatures, func(i, j int) bool {
		left, right := spec.ApplicationFeatures[i], spec.ApplicationFeatures[j]
		if left.Capability != right.Capability {
			return left.Capability < right.Capability
		}
		return left.Annotation < right.Annotation
	})
}

func normalizeAnnotation(spec *AnnotationSpec) {
	sort.SliceStable(spec.Targets, func(i, j int) bool {
		return spec.Targets[i] < spec.Targets[j]
	})
	for index := range spec.Arguments {
		sort.SliceStable(spec.Arguments[index].Kinds, func(i, j int) bool {
			return spec.Arguments[index].Kinds[i] < spec.Arguments[index].Kinds[j]
		})
		sort.SliceStable(spec.Arguments[index].ListElementKinds, func(i, j int) bool {
			return spec.Arguments[index].ListElementKinds[i] <
				spec.Arguments[index].ListElementKinds[j]
		})
	}
	sort.SliceStable(spec.Arguments, func(i, j int) bool {
		return spec.Arguments[i].Name < spec.Arguments[j].Name
	})
}

func normalizeFeature(spec *FeatureSpec) {
	sort.Strings(spec.Requirements)
	sort.SliceStable(spec.EntryPoints, func(i, j int) bool {
		left, right := spec.EntryPoints[i], spec.EntryPoints[j]
		if left.Package != right.Package {
			return left.Package < right.Package
		}
		return left.Symbol < right.Symbol
	})
	for index := range spec.Options {
		sort.SliceStable(spec.Options[index].ListItemKinds, func(i, j int) bool {
			return spec.Options[index].ListItemKinds[i] < spec.Options[index].ListItemKinds[j]
		})
		sort.Strings(spec.Options[index].AllowedStrings)
	}
	sort.SliceStable(spec.Options, func(i, j int) bool {
		return spec.Options[i].Name < spec.Options[j].Name
	})
}

func cloneSpec(spec Spec) Spec {
	result := spec
	result.Activation.EntryPoints = append([]EntryPoint(nil), spec.Activation.EntryPoints...)
	result.Capabilities = append([]string(nil), spec.Capabilities...)
	result.Dependencies = append([]Dependency(nil), spec.Dependencies...)
	result.Annotations = make([]AnnotationSpec, len(spec.Annotations))
	for index, definition := range spec.Annotations {
		result.Annotations[index] = definition
		result.Annotations[index].Targets = append([]annotation.Target(nil), definition.Targets...)
		result.Annotations[index].Arguments = make([]ArgumentSpec, len(definition.Arguments))
		for argumentIndex, argument := range definition.Arguments {
			result.Annotations[index].Arguments[argumentIndex] = argument
			result.Annotations[index].Arguments[argumentIndex].Kinds = append(
				[]annotation.Kind(nil),
				argument.Kinds...,
			)
			result.Annotations[index].Arguments[argumentIndex].ListElementKinds = append(
				[]annotation.Kind(nil),
				argument.ListElementKinds...,
			)
		}
	}
	result.ApplicationFeatures = make([]FeatureSpec, len(spec.ApplicationFeatures))
	for index, feature := range spec.ApplicationFeatures {
		result.ApplicationFeatures[index] = feature
		result.ApplicationFeatures[index].EntryPoints = append(
			[]EntryPoint(nil),
			feature.EntryPoints...,
		)
		result.ApplicationFeatures[index].Requirements = append(
			[]string(nil),
			feature.Requirements...,
		)
		result.ApplicationFeatures[index].Options = make([]OptionSpec, len(feature.Options))
		for optionIndex, option := range feature.Options {
			result.ApplicationFeatures[index].Options[optionIndex] = option
			result.ApplicationFeatures[index].Options[optionIndex].ListItemKinds = append(
				[]annotation.Kind(nil),
				option.ListItemKinds...,
			)
			result.ApplicationFeatures[index].Options[optionIndex].AllowedStrings = append(
				[]string(nil),
				option.AllowedStrings...,
			)
		}
	}
	return result
}

func cloneDefinitions(definitions []annotation.Definition) []annotation.Definition {
	result := make([]annotation.Definition, len(definitions))
	for index, definition := range definitions {
		result[index] = definition
		result[index].Arguments = make([]annotation.ArgumentDefinition, len(definition.Arguments))
		for argumentIndex, argument := range definition.Arguments {
			result[index].Arguments[argumentIndex] = argument
			result[index].Arguments[argumentIndex].Kinds = append(
				[]annotation.Kind(nil),
				argument.Kinds...,
			)
			result[index].Arguments[argumentIndex].ListElementKinds = append(
				[]annotation.Kind(nil),
				argument.ListElementKinds...,
			)
		}
	}
	return result
}
