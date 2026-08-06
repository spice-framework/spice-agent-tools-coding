// Package bootstrap compiles qualified application-platform annotations into
// deterministic, immutable metadata for the Spice application IR.
package bootstrap

import (
	"fmt"
	"go/token"
	"slices"
	"sort"
	"strings"

	"github.com/spice-framework/spice/annotation"
	"github.com/spice-framework/spice/annotation/sdk"
	"github.com/spice-framework/toolchain/compiler/resolve"
)

const (
	// ManagementAnnotation enables an explicit set of management endpoints.
	ManagementAnnotation = "management.Enable"
	// LoggingAnnotation enables Spice's default structured application logging.
	LoggingAnnotation = "observability.Logging"
)

// Capability identifies one explicitly enabled application-platform feature.
type Capability string

const (
	// CapabilityManagement is the generated management HTTP surface.
	CapabilityManagement Capability = "management"
	// CapabilityLogging is structured lifecycle and HTTP logging.
	CapabilityLogging Capability = "observability.logging"
	// CapabilityHTTPObservation composes explicitly selected starter outputs
	// into generated HTTP route observation.
	CapabilityHTTPObservation Capability = "observability.http-server"
)

// RuntimeCapability identifies application-graph behavior required by a
// compiled bootstrap feature.
type RuntimeCapability string

const (
	// RuntimeHTTPServeMux requires a selected *net/http.ServeMux provider.
	RuntimeHTTPServeMux RuntimeCapability = "http.serve-mux"
)

// Endpoint identifies one supported management endpoint.
type Endpoint string

const (
	EndpointHealth      Endpoint = "health"
	EndpointLiveness    Endpoint = "liveness"
	EndpointReadiness   Endpoint = "readiness"
	EndpointInfo        Endpoint = "info"
	EndpointMetrics     Endpoint = "metrics"
	EndpointConfigProps Endpoint = "configprops"
	EndpointModules     Endpoint = "modules"
)

// OptionDefinition describes the semantic rules for one feature argument.
// It complements the syntax-level annotation definition without callbacks,
// reflection, global registration, or target-specific compiler switches.
type OptionDefinition struct {
	Name           string
	Kind           annotation.Kind
	ListItemKinds  []annotation.Kind
	AllowedStrings []string
	Required       bool
	UniqueItems    bool
	MinimumItems   int
	SortItems      bool
}

// Definition maps one qualified annotation to generated capability metadata.
// Callers pass definitions explicitly to Compile, which is the extension seam
// used by first- and third-party starter manifest catalogs.
type Definition struct {
	Annotation    string
	Capability    Capability
	Options       []OptionDefinition
	Requirements  []RuntimeCapability
	SourceID      string
	SourceVersion string
	EntryPoints   []EntryPoint
}

// EntryPoint identifies one explicit starter constructor available to a
// generated application feature.
type EntryPoint struct {
	Package string
	Symbol  string
}

// Option is one normalized, typed feature setting.
type Option struct {
	Name  string
	value annotation.Value
}

// Value returns a defensive deep copy of the normalized value.
func (o Option) Value() annotation.Value {
	return cloneValue(o.value)
}

// Feature is one explicitly enabled capability on an @Application marker.
type Feature struct {
	Annotation       string
	Capability       Capability
	SourceID         string
	SourceVersion    string
	Position         token.Position
	PhysicalPosition token.Position
	options          []Option
	requirements     []RuntimeCapability
	entryPoints      []EntryPoint
}

// Options returns normalized settings sorted by name.
func (f Feature) Options() []Option {
	return cloneOptions(f.options)
}

// Requirements returns graph capabilities required by the feature.
func (f Feature) Requirements() []RuntimeCapability {
	return append([]RuntimeCapability(nil), f.requirements...)
}

// EntryPoints returns explicit starter constructors in deterministic order.
func (f Feature) EntryPoints() []EntryPoint {
	return append([]EntryPoint(nil), f.entryPoints...)
}

// StringList returns a defensive copy of one normalized string-list option.
func (f Feature) StringList(name string) ([]string, bool) {
	for _, option := range f.options {
		if option.Name != name || option.value.Kind != annotation.KindList {
			continue
		}
		values := make([]string, len(option.value.List))
		for index, item := range option.value.List {
			if item.Kind != annotation.KindString {
				return nil, false
			}
			values[index] = item.String
		}
		return values, true
	}
	return nil, false
}

// String returns one normalized string option.
func (f Feature) String(name string) (string, bool) {
	for _, option := range f.options {
		if option.Name == name &&
			option.value.Kind == annotation.KindString {
			return option.value.String, true
		}
	}
	return "", false
}

// Management is the typed view of management bootstrap metadata.
type Management struct {
	endpoints []Endpoint
	access    string
}

// Endpoints returns the normalized endpoint allowlist.
func (m Management) Endpoints() []Endpoint {
	return append([]Endpoint(nil), m.endpoints...)
}

// Access returns the normalized management network-origin policy.
func (m Management) Access() string {
	if m.access == "" {
		return "loopback"
	}
	return m.access
}

// Metadata is the immutable set of compiled features for one application.
type Metadata struct {
	features []Feature
}

// Features returns features in deterministic capability and annotation order.
func (m Metadata) Features() []Feature {
	return cloneFeatures(m.features)
}

// Enabled reports whether one capability was explicitly enabled.
func (m Metadata) Enabled(capability Capability) bool {
	_, found := m.feature(capability)
	return found
}

// Management returns typed management metadata when explicitly enabled.
func (m Metadata) Management() (Management, bool) {
	feature, found := m.feature(CapabilityManagement)
	if !found {
		return Management{}, false
	}
	values, valid := feature.StringList("expose")
	if !valid {
		return Management{}, false
	}
	endpoints := make([]Endpoint, len(values))
	for index, value := range values {
		endpoints[index] = Endpoint(value)
	}
	access, _ := feature.String("access")
	return Management{endpoints: endpoints, access: access}, true
}

// MissingRequirements returns enabled requirements absent from available.
func (m Metadata) MissingRequirements(available []RuntimeCapability) []Feature {
	missing := make([]Feature, 0)
	for _, feature := range m.features {
		for _, requirement := range feature.requirements {
			if slices.Contains(available, requirement) {
				continue
			}
			missing = append(missing, cloneFeature(feature))
			break
		}
	}
	return missing
}

func (m Metadata) feature(capability Capability) (Feature, bool) {
	for _, feature := range m.features {
		if feature.Capability == capability {
			return cloneFeature(feature), true
		}
	}
	return Feature{}, false
}

// Application identifies one validated marker eligible for companion
// bootstrap annotations.
type Application struct {
	SymbolID string
	Name     string
}

// Diagnostic is one source-positioned bootstrap annotation failure.
type Diagnostic struct {
	Position         token.Position
	PhysicalPosition token.Position
	SymbolID         string
	Annotation       string
	Kind             string
	Message          string
}

// Error renders a compiler-style diagnostic.
func (d Diagnostic) Error() string {
	position := d.Position
	if position.Filename == "" {
		position.Filename = "<unknown>"
	}
	if position.Line <= 0 {
		position.Line = 1
	}
	if position.Column <= 0 {
		position.Column = 1
	}
	return fmt.Sprintf("%s:%d:%d: %s", position.Filename, position.Line, position.Column, d.Message)
}

// Result is the immutable output of compiling bootstrap annotations.
type Result struct {
	metadata    map[string]Metadata
	diagnostics []Diagnostic
}

// Metadata returns a defensive copy for one application symbol.
func (r Result) Metadata(symbolID string) Metadata {
	return cloneMetadata(r.metadata[symbolID])
}

// Diagnostics returns deterministic source-positioned failures.
func (r Result) Diagnostics() []Diagnostic {
	return append([]Diagnostic(nil), r.diagnostics...)
}

// Builtins returns fresh declarative bootstrap definitions shipped with Spice.
func Builtins() []Definition {
	return []Definition{
		{
			Annotation: ManagementAnnotation,
			Capability: CapabilityManagement,
			Options: []OptionDefinition{
				{
					Name:           "expose",
					Kind:           annotation.KindList,
					ListItemKinds:  []annotation.Kind{annotation.KindString},
					AllowedStrings: endpointNames(),
					Required:       true,
					UniqueItems:    true,
					MinimumItems:   1,
					SortItems:      true,
				},
				{
					Name:           "access",
					Kind:           annotation.KindString,
					AllowedStrings: []string{"public", "loopback"},
				},
			},
			Requirements: []RuntimeCapability{RuntimeHTTPServeMux},
		},
		{
			Annotation: LoggingAnnotation,
			Capability: CapabilityLogging,
		},
	}
}

// Compile resolves explicitly supplied definitions against validated Spice
// application-marker symbols without rereading comments or source files.
func Compile(
	resolution resolve.Result,
	applications []Application,
	definitions []Definition,
) Result {
	result := Result{metadata: make(map[string]Metadata, len(applications))}
	applicationIndex := indexApplications(applications)
	definitionIndex, definitionDiagnostics := indexDefinitions(definitions)
	capabilityIndex := indexCapabilities(definitionIndex)
	result.diagnostics = append(result.diagnostics, definitionDiagnostics...)
	if len(definitionDiagnostics) != 0 {
		sortDiagnostics(result.diagnostics)
		return result
	}

	seen := make(map[string]resolve.Occurrence)
	for _, occurrence := range resolution.Occurrences {
		definition, recognized, contributed := bootstrapDefinition(
			occurrence,
			definitionIndex,
			capabilityIndex,
		)
		if !recognized {
			if contributed {
				result.diagnostics = append(
					result.diagnostics,
					occurrenceDiagnostic(
						occurrence,
						"unsupported-capability",
						"annotation tool contributed a bootstrap capability without a selected compiler definition",
					),
				)
			}
			continue
		}
		if occurrence.Target != annotation.TargetFunction {
			result.diagnostics = append(
				result.diagnostics,
				occurrenceDiagnostic(
					occurrence,
					"invalid-target",
					fmt.Sprintf(
						"bootstrap annotation @%s must target an @Application function",
						occurrence.Annotation.Name,
					),
				),
			)
			continue
		}
		if _, applicationFound := applicationIndex[occurrence.SymbolID]; !applicationFound {
			result.diagnostics = append(
				result.diagnostics,
				occurrenceDiagnostic(
					occurrence,
					"missing-application",
					fmt.Sprintf(
						"bootstrap annotation @%s requires @Application on the same function",
						occurrence.Annotation.Name,
					),
				),
			)
			continue
		}
		key := occurrence.SymbolID + "\x00" +
			string(definition.Capability)
		if previous, duplicate := seen[key]; duplicate {
			result.diagnostics = append(
				result.diagnostics,
				occurrenceDiagnostic(
					occurrence,
					"duplicate-annotation",
					fmt.Sprintf(
						"bootstrap annotation @%s is duplicated or conflicting on application %q; first declaration is at %s",
						occurrence.Annotation.Name,
						occurrence.Name,
						renderPosition(previous.DisplayPosition),
					),
				),
			)
			continue
		}
		seen[key] = occurrence
		feature, diagnostics := compileFeature(occurrence, definition)
		result.diagnostics = append(result.diagnostics, diagnostics...)
		if len(diagnostics) == 0 {
			metadata := result.metadata[occurrence.SymbolID]
			metadata.features = append(metadata.features, feature)
			result.metadata[occurrence.SymbolID] = metadata
		}
	}

	for symbolID, metadata := range result.metadata {
		sortFeatures(metadata.features)
		result.metadata[symbolID] = metadata
	}
	sortDiagnostics(result.diagnostics)
	return result
}

func indexCapabilities(
	definitions map[string]Definition,
) map[Capability]Definition {
	result := make(map[Capability]Definition, len(definitions))
	for _, definition := range definitions {
		result[definition.Capability] = definition
	}
	return result
}

func bootstrapDefinition(
	occurrence resolve.Occurrence,
	annotations map[string]Definition,
	capabilities map[Capability]Definition,
) (Definition, bool, bool) {
	if contribution, found := occurrence.Contribution(
		sdk.ContributionBootstrap,
	); found {
		definition, recognized := capabilities[Capability(
			contribution.Bootstrap.Capability,
		)]
		return definition, recognized, true
	}
	definition, recognized := annotations[occurrence.Annotation.Name]
	return definition, recognized, false
}

func indexApplications(applications []Application) map[string]Application {
	result := make(map[string]Application, len(applications))
	for _, application := range applications {
		result[application.SymbolID] = application
	}
	return result
}

func indexDefinitions(definitions []Definition) (map[string]Definition, []Diagnostic) {
	result := make(map[string]Definition, len(definitions))
	capabilities := make(map[Capability]string, len(definitions))
	var diagnostics []Diagnostic
	for _, definition := range definitions {
		if problem := definitionProblem(definition); problem != "" {
			diagnostics = append(diagnostics, Diagnostic{
				Annotation: definition.Annotation,
				Kind:       "invalid-definition",
				Message:    problem,
			})
			continue
		}
		if _, duplicate := result[definition.Annotation]; duplicate {
			diagnostics = append(diagnostics, Diagnostic{
				Annotation: definition.Annotation,
				Kind:       "duplicate-definition",
				Message: fmt.Sprintf(
					"bootstrap definition for @%s is declared more than once",
					definition.Annotation,
				),
			})
			continue
		}
		if annotationName, duplicate := capabilities[definition.Capability]; duplicate {
			diagnostics = append(diagnostics, Diagnostic{
				Annotation: definition.Annotation,
				Kind:       "duplicate-capability",
				Message: fmt.Sprintf(
					"bootstrap capability %q is declared by both @%s and @%s",
					definition.Capability,
					annotationName,
					definition.Annotation,
				),
			})
			continue
		}
		capabilities[definition.Capability] = definition.Annotation
		result[definition.Annotation] = cloneDefinition(definition)
	}
	return result, diagnostics
}

func definitionProblem(definition Definition) string {
	if strings.TrimSpace(definition.Annotation) == "" ||
		strings.TrimSpace(definition.Annotation) != definition.Annotation {
		return "bootstrap definition requires a trimmed annotation name"
	}
	if strings.TrimSpace(string(definition.Capability)) == "" {
		return fmt.Sprintf(
			"bootstrap definition for @%s requires a capability",
			definition.Annotation,
		)
	}
	if problem := sourceDefinitionProblem(definition); problem != "" {
		return problem
	}
	return optionDefinitionProblem(definition)
}

func sourceDefinitionProblem(definition Definition) string {
	if (definition.SourceID == "") != (definition.SourceVersion == "") {
		return fmt.Sprintf(
			"bootstrap definition for @%s must declare both source ID and source version",
			definition.Annotation,
		)
	}
	if strings.TrimSpace(definition.SourceID) != definition.SourceID ||
		strings.TrimSpace(definition.SourceVersion) != definition.SourceVersion {
		return fmt.Sprintf(
			"bootstrap definition for @%s requires trimmed source metadata",
			definition.Annotation,
		)
	}
	if len(definition.EntryPoints) != 0 && definition.SourceID == "" {
		return fmt.Sprintf(
			"bootstrap definition for @%s cannot declare entrypoints without source metadata",
			definition.Annotation,
		)
	}
	seenEntryPoints := make(map[string]struct{}, len(definition.EntryPoints))
	for _, entryPoint := range definition.EntryPoints {
		if strings.TrimSpace(entryPoint.Package) == "" ||
			strings.TrimSpace(entryPoint.Package) != entryPoint.Package ||
			strings.TrimSpace(entryPoint.Symbol) == "" ||
			strings.TrimSpace(entryPoint.Symbol) != entryPoint.Symbol {
			return fmt.Sprintf(
				"bootstrap definition for @%s contains an invalid entrypoint",
				definition.Annotation,
			)
		}
		key := entryPoint.Package + "\x00" + entryPoint.Symbol
		if _, duplicate := seenEntryPoints[key]; duplicate {
			return fmt.Sprintf(
				"bootstrap definition for @%s contains duplicate entrypoint %s.%s",
				definition.Annotation,
				entryPoint.Package,
				entryPoint.Symbol,
			)
		}
		seenEntryPoints[key] = struct{}{}
	}
	return ""
}

func optionDefinitionProblem(definition Definition) string {
	seen := make(map[string]struct{}, len(definition.Options))
	for _, option := range definition.Options {
		if strings.TrimSpace(option.Name) == "" ||
			strings.TrimSpace(option.Name) != option.Name {
			return fmt.Sprintf(
				"bootstrap definition for @%s contains an invalid option name",
				definition.Annotation,
			)
		}
		if _, duplicate := seen[option.Name]; duplicate {
			return fmt.Sprintf(
				"bootstrap definition for @%s contains duplicate option %q",
				definition.Annotation,
				option.Name,
			)
		}
		seen[option.Name] = struct{}{}
		if option.MinimumItems < 0 {
			return fmt.Sprintf(
				"bootstrap definition for @%s option %q has a negative minimum item count",
				definition.Annotation,
				option.Name,
			)
		}
	}
	return ""
}

func compileFeature(
	occurrence resolve.Occurrence,
	definition Definition,
) (Feature, []Diagnostic) {
	feature := Feature{
		Annotation:       occurrence.Annotation.Name,
		Capability:       definition.Capability,
		SourceID:         definition.SourceID,
		SourceVersion:    definition.SourceVersion,
		Position:         occurrence.DisplayPosition,
		PhysicalPosition: token.Position{Filename: occurrence.PhysicalFile, Offset: occurrence.PhysicalOffset},
		requirements:     append([]RuntimeCapability(nil), definition.Requirements...),
		entryPoints:      append([]EntryPoint(nil), definition.EntryPoints...),
	}
	options, diagnostics := compileOptions(occurrence, definition.Options)
	feature.options = options
	return feature, diagnostics
}

func compileOptions(
	occurrence resolve.Occurrence,
	definitions []OptionDefinition,
) ([]Option, []Diagnostic) {
	arguments := occurrence.Annotation.Arguments
	if contribution, found := occurrence.Contribution(
		sdk.ContributionBootstrap,
	); found {
		arguments = make(
			[]annotation.Argument,
			len(contribution.Bootstrap.Options),
		)
		for index, option := range contribution.Bootstrap.Options {
			arguments[index] = annotation.Argument{
				Name:  option.Name,
				Value: bootstrapOptionValue(option.Value),
			}
		}
	}
	definitionIndex := make(map[string]OptionDefinition, len(definitions))
	for _, definition := range definitions {
		definitionIndex[definition.Name] = definition
	}
	assigned := make(map[string]bool, len(definitions))
	options := make([]Option, 0, len(arguments))
	var diagnostics []Diagnostic
	for _, argument := range arguments {
		definition, found := definitionIndex[argument.Name]
		if !found {
			diagnostics = append(diagnostics, optionDiagnostic(
				occurrence,
				"unknown-option",
				argument.Name,
				fmt.Sprintf(
					"bootstrap annotation @%s does not define option %q",
					occurrence.Annotation.Name,
					argument.Name,
				),
			))
			continue
		}
		if assigned[argument.Name] {
			diagnostics = append(diagnostics, optionDiagnostic(
				occurrence,
				"duplicate-option",
				argument.Name,
				fmt.Sprintf(
					"bootstrap annotation @%s assigns option %q more than once",
					occurrence.Annotation.Name,
					argument.Name,
				),
			))
			continue
		}
		assigned[argument.Name] = true
		option, optionDiagnostics := compileOption(occurrence, argument, definition)
		diagnostics = append(diagnostics, optionDiagnostics...)
		if len(optionDiagnostics) == 0 {
			options = append(options, option)
		}
	}
	for _, definition := range definitions {
		if definition.Required && !assigned[definition.Name] {
			diagnostics = append(diagnostics, optionDiagnostic(
				occurrence,
				"missing-option",
				definition.Name,
				fmt.Sprintf(
					"bootstrap annotation @%s requires option %q",
					occurrence.Annotation.Name,
					definition.Name,
				),
			))
		}
	}
	sort.SliceStable(options, func(i, j int) bool {
		return options[i].Name < options[j].Name
	})
	return options, diagnostics
}

func bootstrapOptionValue(value sdk.ContributionValue) annotation.Value {
	result := annotation.Value{
		Kind:       value.Kind,
		String:     value.String,
		Integer:    value.Integer,
		Boolean:    value.Boolean,
		Identifier: value.Identifier,
		List:       make([]annotation.Value, len(value.List)),
	}
	for index, item := range value.List {
		result.List[index] = bootstrapOptionValue(item)
	}
	return result
}

func compileOption(
	occurrence resolve.Occurrence,
	argument annotation.Argument,
	definition OptionDefinition,
) (Option, []Diagnostic) {
	if argument.Value.Kind != definition.Kind {
		return Option{}, []Diagnostic{optionDiagnostic(
			occurrence,
			"invalid-option-kind",
			definition.Name,
			fmt.Sprintf(
				"bootstrap annotation @%s option %q requires %s, got %s",
				occurrence.Annotation.Name,
				definition.Name,
				definition.Kind,
				argument.Value.Kind,
			),
		)}
	}
	value := cloneValue(argument.Value)
	if value.Kind != annotation.KindList {
		text := valueText(value)
		if len(definition.AllowedStrings) != 0 &&
			!slices.Contains(definition.AllowedStrings, text) {
			return Option{}, []Diagnostic{optionDiagnostic(
				occurrence,
				"unsupported-value",
				definition.Name,
				fmt.Sprintf(
					"bootstrap annotation @%s option %q has unsupported value %q; allowed values: %s",
					occurrence.Annotation.Name,
					definition.Name,
					text,
					strings.Join(definition.AllowedStrings, ", "),
				),
			)}
		}
		return Option{Name: definition.Name, value: value}, nil
	}
	diagnostics := validateListOption(occurrence, definition, value.List)
	if len(diagnostics) != 0 {
		return Option{}, diagnostics
	}
	if definition.SortItems {
		sort.SliceStable(value.List, func(i, j int) bool {
			return valueText(value.List[i]) < valueText(value.List[j])
		})
	}
	return Option{Name: definition.Name, value: value}, nil
}

func validateListOption(
	occurrence resolve.Occurrence,
	definition OptionDefinition,
	values []annotation.Value,
) []Diagnostic {
	var diagnostics []Diagnostic
	if len(values) < definition.MinimumItems {
		diagnostics = append(diagnostics, optionDiagnostic(
			occurrence,
			"too-few-items",
			definition.Name,
			fmt.Sprintf(
				"bootstrap annotation @%s option %q requires at least %d item(s)",
				occurrence.Annotation.Name,
				definition.Name,
				definition.MinimumItems,
			),
		))
	}
	seen := make(map[string]struct{}, len(values))
	for index, value := range values {
		if len(definition.ListItemKinds) != 0 &&
			!slices.Contains(definition.ListItemKinds, value.Kind) {
			diagnostics = append(diagnostics, optionDiagnostic(
				occurrence,
				"invalid-list-item-kind",
				definition.Name,
				fmt.Sprintf(
					"bootstrap annotation @%s option %q item %d requires %s, got %s",
					occurrence.Annotation.Name,
					definition.Name,
					index,
					kindsText(definition.ListItemKinds),
					value.Kind,
				),
			))
			continue
		}
		text := valueText(value)
		if len(definition.AllowedStrings) != 0 &&
			!slices.Contains(definition.AllowedStrings, text) {
			diagnostics = append(diagnostics, optionDiagnostic(
				occurrence,
				"unsupported-item",
				definition.Name,
				fmt.Sprintf(
					"bootstrap annotation @%s option %q contains unsupported value %q; allowed values: %s",
					occurrence.Annotation.Name,
					definition.Name,
					text,
					strings.Join(definition.AllowedStrings, ", "),
				),
			))
		}
		if !definition.UniqueItems {
			continue
		}
		if _, duplicate := seen[text]; duplicate {
			diagnostics = append(diagnostics, optionDiagnostic(
				occurrence,
				"duplicate-item",
				definition.Name,
				fmt.Sprintf(
					"bootstrap annotation @%s option %q contains duplicate value %q",
					occurrence.Annotation.Name,
					definition.Name,
					text,
				),
			))
		}
		seen[text] = struct{}{}
	}
	return diagnostics
}

func valueText(value annotation.Value) string {
	switch value.Kind {
	case annotation.KindString:
		return value.String
	case annotation.KindIdentifier:
		return value.Identifier
	case annotation.KindInteger:
		return fmt.Sprintf("%d", value.Integer)
	case annotation.KindBoolean:
		return fmt.Sprintf("%t", value.Boolean)
	case annotation.KindList:
		return ""
	}
	return ""
}

func optionDiagnostic(
	occurrence resolve.Occurrence,
	kind string,
	option string,
	message string,
) Diagnostic {
	diagnostic := occurrenceDiagnostic(occurrence, kind, message)
	diagnostic.SymbolID = occurrence.SymbolID + "#" + option
	return diagnostic
}

func occurrenceDiagnostic(
	occurrence resolve.Occurrence,
	kind string,
	message string,
) Diagnostic {
	return Diagnostic{
		Position: occurrence.DisplayPosition,
		PhysicalPosition: token.Position{
			Filename: occurrence.PhysicalFile,
			Offset:   occurrence.PhysicalOffset,
		},
		SymbolID:   occurrence.SymbolID,
		Annotation: occurrence.Annotation.Name,
		Kind:       kind,
		Message:    message,
	}
}

func endpointNames() []string {
	return []string{
		string(EndpointHealth),
		string(EndpointLiveness),
		string(EndpointReadiness),
		string(EndpointInfo),
		string(EndpointMetrics),
		string(EndpointConfigProps),
		string(EndpointModules),
	}
}

func kindsText(kinds []annotation.Kind) string {
	values := make([]string, len(kinds))
	for index, kind := range kinds {
		values[index] = string(kind)
	}
	if len(values) == 1 {
		return values[0]
	}
	return "one of " + strings.Join(values, ", ")
}

func sortFeatures(features []Feature) {
	sort.SliceStable(features, func(i, j int) bool {
		if features[i].Capability != features[j].Capability {
			return features[i].Capability < features[j].Capability
		}
		return features[i].Annotation < features[j].Annotation
	})
}

func sortDiagnostics(diagnostics []Diagnostic) {
	sort.SliceStable(diagnostics, func(i, j int) bool {
		left, right := diagnostics[i], diagnostics[j]
		if left.PhysicalPosition.Filename != right.PhysicalPosition.Filename {
			return left.PhysicalPosition.Filename < right.PhysicalPosition.Filename
		}
		if left.PhysicalPosition.Offset != right.PhysicalPosition.Offset {
			return left.PhysicalPosition.Offset < right.PhysicalPosition.Offset
		}
		if left.Annotation != right.Annotation {
			return left.Annotation < right.Annotation
		}
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		if left.SymbolID != right.SymbolID {
			return left.SymbolID < right.SymbolID
		}
		return left.Message < right.Message
	})
}

func cloneDefinition(definition Definition) Definition {
	result := definition
	result.Options = make([]OptionDefinition, len(definition.Options))
	for index, option := range definition.Options {
		result.Options[index] = option
		result.Options[index].ListItemKinds = append(
			[]annotation.Kind(nil),
			option.ListItemKinds...,
		)
		result.Options[index].AllowedStrings = append(
			[]string(nil),
			option.AllowedStrings...,
		)
	}
	result.Requirements = append([]RuntimeCapability(nil), definition.Requirements...)
	result.EntryPoints = append([]EntryPoint(nil), definition.EntryPoints...)
	for index := range result.Options {
		sort.SliceStable(result.Options[index].ListItemKinds, func(i, j int) bool {
			return result.Options[index].ListItemKinds[i] <
				result.Options[index].ListItemKinds[j]
		})
		sort.Strings(result.Options[index].AllowedStrings)
	}
	sort.SliceStable(result.Options, func(i, j int) bool {
		return result.Options[i].Name < result.Options[j].Name
	})
	sort.SliceStable(result.Requirements, func(i, j int) bool {
		return result.Requirements[i] < result.Requirements[j]
	})
	sort.SliceStable(result.EntryPoints, func(i, j int) bool {
		left, right := result.EntryPoints[i], result.EntryPoints[j]
		if left.Package != right.Package {
			return left.Package < right.Package
		}
		return left.Symbol < right.Symbol
	})
	return result
}

func cloneMetadata(metadata Metadata) Metadata {
	return Metadata{features: cloneFeatures(metadata.features)}
}

func cloneFeatures(features []Feature) []Feature {
	result := make([]Feature, len(features))
	for index, feature := range features {
		result[index] = cloneFeature(feature)
	}
	return result
}

func cloneFeature(feature Feature) Feature {
	feature.options = cloneOptions(feature.options)
	feature.requirements = append([]RuntimeCapability(nil), feature.requirements...)
	feature.entryPoints = append([]EntryPoint(nil), feature.entryPoints...)
	return feature
}

func cloneOptions(options []Option) []Option {
	result := make([]Option, len(options))
	for index, option := range options {
		result[index] = Option{Name: option.Name, value: cloneValue(option.value)}
	}
	return result
}

func cloneValue(value annotation.Value) annotation.Value {
	result := value
	result.List = make([]annotation.Value, len(value.List))
	for index, item := range value.List {
		result.List[index] = cloneValue(item)
	}
	return result
}

func renderPosition(position token.Position) string {
	filename := position.Filename
	if filename == "" {
		filename = "<unknown>"
	}
	line, column := position.Line, position.Column
	if line <= 0 {
		line = 1
	}
	if column <= 0 {
		column = 1
	}
	return fmt.Sprintf("%s:%d:%d", filename, line, column)
}
