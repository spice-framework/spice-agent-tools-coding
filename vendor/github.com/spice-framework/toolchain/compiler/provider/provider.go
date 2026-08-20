// Package provider builds and validates the compile-time provider catalog.
package provider

import (
	"fmt"
	"go/ast"
	goparser "go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/spice-framework/spice/annotation"
	"github.com/spice-framework/spice/annotation/sdk"
	"github.com/spice-framework/toolchain/compiler/load"
	annotationparser "github.com/spice-framework/toolchain/compiler/parser"
	"github.com/spice-framework/toolchain/compiler/resolve"
)

const (
	acceptedSignature  = "func(dependencies...) T, func(dependencies...) (T, error), func(dependencies...) (T, lifecycle.Cleanup), or func(dependencies...) (T, lifecycle.Cleanup, error)"
	beanPackagePath    = "github.com/spice-framework/spice/bean"
	cleanupPackagePath = "github.com/spice-framework/spice/lifecycle"
	cleanupTypeName    = "Cleanup"
)

var errorType = types.Universe.Lookup("error").Type()

// DependencyKind identifies how generated Go supplies one constructor
// parameter.
type DependencyKind string

const (
	DependencySingle   DependencyKind = "single"
	DependencySlice    DependencyKind = "slice"
	DependencyMap      DependencyKind = "map"
	DependencyOptional DependencyKind = "optional"
	DependencyLazy     DependencyKind = "lazy"
	DependencyProvider DependencyKind = "provider"
)

// Dependency describes one required provider input in declaration order.
// Type is live semantic data owned by the Program used to build the catalog;
// TypeID is the deterministic import-path-qualified representation.
type Dependency struct {
	Index            int
	Name             string
	Type             types.Type
	TypeID           string
	Position         token.Position
	PhysicalPosition token.Position
	Qualifiers       []string
	Kind             DependencyKind
	Element          types.Type
	ElementTypeID    string
}

// MatchType returns the exact bean type selected for this constructor input.
func (dependency Dependency) MatchType() types.Type {
	if dependency.Element != nil {
		return dependency.Element
	}
	return dependency.Type
}

// InterfaceBinding describes one explicit concrete-to-interface exposure.
// Type is resolved from the annotation source file in the Program's existing
// type universe; Expression preserves the developer-authored Go spelling.
type InterfaceBinding struct {
	Expression       string
	Type             types.Type
	TypeID           string
	Position         token.Position
	PhysicalPosition token.Position
}

// Source identifies how a provider value is constructed.
type Source string

const (
	// SourceBean identifies a direct package-level or configuration-method
	// @Bean factory call.
	SourceBean Source = "bean"
	// SourceStereotype identifies a constructible @Service, @Controller, or
	// repository stereotype type.
	SourceStereotype Source = "stereotype"
	// SourceConfiguration identifies a generated typed configuration binder.
	SourceConfiguration Source = "configuration"
	// SourceEvent identifies a generated typed event topic marker.
	SourceEvent Source = "event"
	// SourceStarter identifies an explicitly selected starter entrypoint.
	SourceStarter Source = "starter"
	// SourceAutoConfiguration identifies a statically decoded library default
	// selected by an explicit Go import.
	SourceAutoConfiguration Source = "auto-configuration"
)

// Construction identifies the generated construction form.
type Construction string

const (
	// ConstructionFactory calls one validated ordinary Go constructor.
	ConstructionFactory Construction = "factory"
	// ConstructionAllocate emits new(T) for one exported concrete struct.
	ConstructionAllocate Construction = "allocate"
)

// Provider describes one exact-type construction node. Bean providers call a
// validated package-level function or configuration method; configuration
// property providers are synthesized from validated @ConfigurationProperties
// metadata.
type Provider struct {
	Source           Source
	Role             string
	Construction     Construction
	Symbol           load.Symbol
	Constructor      load.Symbol
	SymbolID         string
	Name             string
	ExplicitName     bool
	Aliases          []string
	Qualifiers       []string
	Primary          bool
	Fallback         bool
	Order            int64
	Scope            sdk.BeanScope
	PackagePath      string
	Position         token.Position
	PhysicalPosition token.Position
	Output           types.Type
	OutputTypeID     string
	Dependencies     []Dependency
	Interfaces       []InterfaceBinding
	ReturnsCleanup   bool
	ReturnsError     bool
	SourceID         string
	SourceVersion    string
}

// Entrypoint identifies one explicitly selected starter provider function.
type Entrypoint struct {
	PackagePath   string
	Symbol        string
	SourceID      string
	SourceVersion string
	Source        Source
	Name          string
	Aliases       []string
	Qualifiers    []string
	Primary       bool
	Fallback      bool
	Order         int64
}

// Diagnostic is one deterministic source-positioned catalog failure.
type Diagnostic struct {
	Position         token.Position
	PhysicalPosition token.Position
	ProviderID       string
	Kind             string
	Message          string
	Fixes            []SuggestedFix
}

// SuggestedFix is one provider-owned source repair. The shared diagnostic
// adapter adds workspace identity and overlay versions without making the
// provider package depend on an editor transport.
type SuggestedFix struct {
	Title             string
	AppliesAt         token.Position
	AppliesAtPhysical token.Position
	Edits             []SuggestedEdit
}

// SuggestedEdit is one zero-width source insertion or replacement expressed
// in the loaded program's display and physical coordinate systems.
type SuggestedEdit struct {
	Position         token.Position
	PhysicalPosition token.Position
	NewText          string
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

// Catalog is the immutable-by-convention result of validating resolved @Bean
// annotations from one loaded Program.
type Catalog struct {
	providers   []Provider
	diagnostics []Diagnostic
}

// Providers returns a defensive copy of deterministic provider records.
func (c Catalog) Providers() []Provider {
	result := make([]Provider, len(c.providers))
	copy(result, c.providers)
	for i := range result {
		result[i].Dependencies = append([]Dependency(nil), result[i].Dependencies...)
		for dependencyIndex := range result[i].Dependencies {
			result[i].Dependencies[dependencyIndex].Qualifiers = append(
				[]string(nil),
				result[i].Dependencies[dependencyIndex].Qualifiers...,
			)
		}
		result[i].Aliases = append([]string(nil), result[i].Aliases...)
		result[i].Qualifiers = append(
			[]string(nil),
			result[i].Qualifiers...,
		)
		result[i].Interfaces = append(
			[]InterfaceBinding(nil),
			result[i].Interfaces...,
		)
	}
	return result
}

// Diagnostics returns a defensive copy of deterministic diagnostics.
func (c Catalog) Diagnostics() []Diagnostic {
	result := make([]Diagnostic, len(c.diagnostics))
	for index, item := range c.diagnostics {
		result[index] = item
		result[index].Fixes = cloneSuggestedFixes(item.Fixes)
	}
	return result
}

func cloneSuggestedFixes(items []SuggestedFix) []SuggestedFix {
	result := make([]SuggestedFix, len(items))
	for index, item := range items {
		result[index] = item
		result[index].Edits = append([]SuggestedEdit(nil), item.Edits...)
	}
	return result
}

// Add returns a catalog extended with validated compiler-synthesized provider
// records. It preserves existing diagnostics and rechecks exact output
// uniqueness across the combined catalog.
func Add(catalog Catalog, additions ...Provider) Catalog {
	result := Catalog{
		providers:   catalog.Providers(),
		diagnostics: catalog.Diagnostics(),
	}
	for _, addition := range additions {
		addition.Dependencies = append([]Dependency(nil), addition.Dependencies...)
		for dependencyIndex := range addition.Dependencies {
			addition.Dependencies[dependencyIndex].Qualifiers = append(
				[]string(nil),
				addition.Dependencies[dependencyIndex].Qualifiers...,
			)
		}
		addition.Aliases = append([]string(nil), addition.Aliases...)
		addition.Qualifiers = append(
			[]string(nil),
			addition.Qualifiers...,
		)
		addition.Interfaces = append(
			[]InterfaceBinding(nil),
			addition.Interfaces...,
		)
		result.providers = append(result.providers, addition)
	}
	sort.SliceStable(result.providers, func(i, j int) bool {
		return result.providers[i].SymbolID < result.providers[j].SymbolID
	})
	normalizeProviderMetadata(result.providers)
	if len(result.diagnostics) == 0 {
		result.diagnostics = append(
			beanIdentityDiagnostics(result.providers),
			nonSelectableDuplicateDiagnostics(result.providers)...,
		)
	}
	sortDiagnostics(result.diagnostics)
	return result
}

// Merge combines already validated catalogs and rechecks exact output
// uniqueness across the result.
func Merge(catalogs ...Catalog) Catalog {
	result := Catalog{}
	for _, catalog := range catalogs {
		result.providers = append(result.providers, catalog.Providers()...)
		result.diagnostics = append(result.diagnostics, catalog.Diagnostics()...)
	}
	sort.SliceStable(result.providers, func(i, j int) bool {
		return result.providers[i].SymbolID < result.providers[j].SymbolID
	})
	normalizeProviderMetadata(result.providers)
	if len(result.diagnostics) == 0 {
		result.diagnostics = append(
			beanIdentityDiagnostics(result.providers),
			nonSelectableDuplicateDiagnostics(result.providers)...,
		)
	}
	sortDiagnostics(result.diagnostics)
	return result
}

// BuildEntrypoints validates explicit starter functions from the same typed
// Program used for application analysis. It never executes the functions.
func BuildEntrypoints(program *load.Program, entrypoints []Entrypoint) Catalog {
	catalog := Catalog{}
	if program == nil {
		catalog.diagnostics = []Diagnostic{{
			Kind:    "internal",
			Message: "starter entrypoint catalog requires a loaded program",
		}}
		return catalog
	}

	symbols := make(map[string]load.Symbol)
	for _, symbol := range program.Symbols() {
		if symbol.Kind == load.SymbolFunction && symbol.Receiver == "" {
			symbols[symbol.PackagePath+"\x00"+symbol.Name] = symbol
		}
	}
	fileSets := make(map[string]*token.FileSet)
	for _, pkg := range program.Packages() {
		if pkg.Raw != nil && pkg.Raw.Fset != nil {
			fileSets[pkg.Path] = pkg.Raw.Fset
		}
	}
	cleanupType := canonicalCleanupType(program)
	seen := make(map[string]struct{}, len(entrypoints))
	for _, entrypoint := range entrypoints {
		if problem := entrypointProblem(entrypoint); problem != "" {
			catalog.diagnostics = append(catalog.diagnostics, Diagnostic{
				Kind:    "invalid-entrypoint",
				Message: problem,
			})
			continue
		}
		key := entrypoint.PackagePath + "\x00" + entrypoint.Symbol
		if _, duplicate := seen[key]; duplicate {
			catalog.diagnostics = append(catalog.diagnostics, Diagnostic{
				Kind: "duplicate-entrypoint",
				Message: fmt.Sprintf(
					"starter entrypoint %s.%s is selected more than once",
					entrypoint.PackagePath,
					entrypoint.Symbol,
				),
			})
			continue
		}
		seen[key] = struct{}{}
		symbol, found := symbols[key]
		if !found {
			catalog.diagnostics = append(catalog.diagnostics, Diagnostic{
				Kind: "missing-entrypoint",
				Message: fmt.Sprintf(
					"starter %q entrypoint %s.%s is not a loaded package-level function",
					entrypoint.SourceID,
					entrypoint.PackagePath,
					entrypoint.Symbol,
				),
			})
			continue
		}
		occurrence := resolve.Occurrence{
			Target:          annotation.TargetFunction,
			Name:            symbol.Name,
			SymbolID:        symbol.ID,
			PackagePath:     symbol.PackagePath,
			PhysicalFile:    symbol.PhysicalPosition.Filename,
			PhysicalOffset:  symbol.PhysicalPosition.Offset,
			DisplayPosition: symbol.Position,
		}
		source, role := entrypointSource(entrypoint)
		item, diagnostic := analyzeProvider(
			occurrence,
			symbol,
			fileSets[symbol.PackagePath],
			cleanupType,
			source,
			entrypoint.SourceID,
			entrypoint.SourceVersion,
			role,
		)
		if diagnostic != nil {
			catalog.diagnostics = append(catalog.diagnostics, *diagnostic)
			continue
		}
		applyEntrypointMetadata(&item, entrypoint)
		catalog.providers = append(catalog.providers, item)
	}
	sort.SliceStable(catalog.providers, func(i, j int) bool {
		return catalog.providers[i].SymbolID < catalog.providers[j].SymbolID
	})
	normalizeProviderMetadata(catalog.providers)
	catalog.diagnostics = append(
		catalog.diagnostics,
		beanIdentityDiagnostics(catalog.providers)...,
	)
	catalog.diagnostics = append(
		catalog.diagnostics,
		nonSelectableDuplicateDiagnostics(catalog.providers)...,
	)
	sortDiagnostics(catalog.diagnostics)
	return catalog
}

func entrypointSource(entrypoint Entrypoint) (Source, string) {
	if entrypoint.Source == SourceAutoConfiguration {
		return SourceAutoConfiguration, "auto-configuration factory"
	}
	return SourceStarter, "starter entrypoint"
}

func applyEntrypointMetadata(item *Provider, entrypoint Entrypoint) {
	if entrypoint.Name != "" {
		item.Name = entrypoint.Name
		item.ExplicitName = true
	}
	item.Aliases = append(item.Aliases, entrypoint.Aliases...)
	item.Qualifiers = append(item.Qualifiers, entrypoint.Qualifiers...)
	item.Primary = entrypoint.Primary
	item.Fallback = entrypoint.Fallback
	item.Order = entrypoint.Order
}

func entrypointProblem(entrypoint Entrypoint) string {
	switch {
	case entrypoint.Source != "" &&
		entrypoint.Source != SourceStarter &&
		entrypoint.Source != SourceAutoConfiguration:
		return fmt.Sprintf("entrypoint source %q is unsupported", entrypoint.Source)
	case strings.TrimSpace(entrypoint.PackagePath) == "" ||
		strings.TrimSpace(entrypoint.PackagePath) != entrypoint.PackagePath:
		return "starter entrypoint requires a trimmed package path"
	case !token.IsExported(entrypoint.Symbol):
		return fmt.Sprintf(
			"starter entrypoint %s.%s requires an exported Go symbol",
			entrypoint.PackagePath,
			entrypoint.Symbol,
		)
	case strings.TrimSpace(entrypoint.SourceID) == "" ||
		strings.TrimSpace(entrypoint.SourceID) != entrypoint.SourceID:
		return fmt.Sprintf(
			"starter entrypoint %s.%s requires a trimmed source ID",
			entrypoint.PackagePath,
			entrypoint.Symbol,
		)
	case strings.TrimSpace(entrypoint.SourceVersion) == "" ||
		strings.TrimSpace(entrypoint.SourceVersion) != entrypoint.SourceVersion:
		return fmt.Sprintf(
			"starter entrypoint %s.%s requires a trimmed source version",
			entrypoint.PackagePath,
			entrypoint.Symbol,
		)
	default:
		return ""
	}
}

// Build validates explicit factories, constructible stereotypes, and explicit
// interface bindings using the exact live symbols and types already owned by
// program. It never reloads, reflects on, or executes application code.
func Build(program *load.Program, resolution resolve.Result) Catalog {
	catalog := Catalog{}
	if program == nil {
		catalog.diagnostics = []Diagnostic{{Kind: "internal", Message: "provider catalog requires a loaded program"}}
		return catalog
	}

	build := newBuildContext(program)

	for _, occurrence := range resolution.Occurrences {
		contribution, contributed := occurrence.DescriptorContribution(
			sdk.ContributionProvider,
		)
		if !occurrence.HasContribution(sdk.ContributionProvider) {
			continue
		}
		symbol, ok := build.symbols[occurrence.SymbolID]
		if !ok {
			catalog.diagnostics = append(catalog.diagnostics, occurrenceDiagnostic(
				occurrence,
				"missing-symbol",
				fmt.Sprintf("@Bean target %q has no stable typed symbol in the loaded program", occurrence.Name),
			))
			continue
		}
		provider, diagnostic := analyzeProvider(
			occurrence,
			symbol,
			build.fileSets[symbol.PackagePath],
			build.cleanupType,
			SourceBean,
			"",
			"",
			"@Bean provider",
		)
		if diagnostic != nil {
			catalog.diagnostics = append(catalog.diagnostics, *diagnostic)
			continue
		}
		if contributed {
			provider.Name = contribution.Provider.Name
			provider.ExplicitName =
				contribution.Provider.Name != ""
			provider.Aliases = append(
				[]string(nil),
				contribution.Provider.Aliases...,
			)
		} else {
			name, aliases := unboundProviderIdentity(
				occurrence.Annotation.Arguments,
			)
			if name != "" {
				provider.Name = name
				provider.ExplicitName = true
			}
			provider.Aliases = aliases
		}
		if provider.Name == "" {
			provider.Name = lowerInitial(symbol.Name)
		}
		provider.Scope = sdk.BeanScopeSingleton
		catalog.providers = append(catalog.providers, provider)
	}

	stereotypesByPackage := stereotypeCounts(resolution)
	for _, occurrence := range resolution.Occurrences {
		contribution, present := occurrence.Contribution(
			sdk.ContributionStereotype,
		)
		if !present {
			continue
		}
		if !contribution.Stereotype.Construct {
			continue
		}
		item, diagnostic := build.stereotypeProvider(
			occurrence,
			*contribution.Stereotype,
			stereotypesByPackage[occurrence.PackagePath],
		)
		if diagnostic != nil {
			catalog.diagnostics = append(
				catalog.diagnostics,
				*diagnostic,
			)
			continue
		}
		catalog.providers = append(catalog.providers, item)
	}

	attachInterfaceBindings(&catalog, build, resolution)
	attachBeanMetadata(&catalog, resolution)
	var methodDiagnostics []Diagnostic
	catalog.providers, methodDiagnostics = validConfigurationMethods(
		catalog.providers,
	)
	catalog.diagnostics = append(catalog.diagnostics, methodDiagnostics...)
	normalizeProviderMetadata(catalog.providers)
	sort.SliceStable(catalog.providers, func(i, j int) bool {
		return catalog.providers[i].SymbolID < catalog.providers[j].SymbolID
	})
	catalog.diagnostics = append(
		catalog.diagnostics,
		beanIdentityDiagnostics(catalog.providers)...,
	)
	catalog.diagnostics = append(
		catalog.diagnostics,
		nonSelectableDuplicateDiagnostics(catalog.providers)...,
	)
	sortDiagnostics(catalog.diagnostics)
	return catalog
}

func validConfigurationMethods(
	providers []Provider,
) ([]Provider, []Diagnostic) {
	valid := make([]Provider, 0, len(providers))
	var diagnostics []Diagnostic
	for index := range providers {
		item := &providers[index]
		if item.Source != SourceBean ||
			item.Constructor.Kind != load.SymbolMethod {
			valid = append(valid, *item)
			continue
		}
		matches := 0
		if len(item.Dependencies) != 0 {
			receiver := item.Dependencies[0].Type
			for candidateIndex := range providers {
				candidate := &providers[candidateIndex]
				if candidate.Source == SourceStereotype &&
					candidate.Role == "configuration" &&
					types.Identical(candidate.Output, receiver) {
					matches++
				}
			}
		}
		if matches == 1 {
			valid = append(valid, *item)
			continue
		}
		diagnostics = append(diagnostics, beanIdentityDiagnostic(
			item,
			"configuration-method-owner",
			fmt.Sprintf(
				"@Bean method %s receiver requires exactly one constructible @Configuration provider, found %d",
				item.Symbol.DisplayLabel,
				matches,
			),
		))
	}
	return valid, diagnostics
}

func unboundProviderIdentity(
	arguments []annotation.Argument,
) (string, []string) {
	name := ""
	var aliases []string
	for _, argument := range arguments {
		switch {
		case argument.Name == "name" &&
			argument.Value.Kind == annotation.KindString:
			name = argument.Value.String
		case argument.Name == "aliases" &&
			argument.Value.Kind == annotation.KindList:
			for _, item := range argument.Value.List {
				if item.Kind == annotation.KindString {
					aliases = append(aliases, item.String)
				}
			}
		}
	}
	return name, aliases
}

type buildContext struct {
	program     *load.Program
	symbols     map[string]load.Symbol
	packages    map[string]load.Package
	fileSets    map[string]*token.FileSet
	cleanupType types.Type
}

func newBuildContext(program *load.Program) buildContext {
	result := buildContext{
		program:  program,
		symbols:  make(map[string]load.Symbol),
		packages: make(map[string]load.Package),
		fileSets: make(map[string]*token.FileSet),
	}
	for _, symbol := range program.Symbols() {
		result.symbols[symbol.ID] = symbol
	}
	for _, pkg := range program.Packages() {
		result.packages[pkg.Path] = pkg
		if pkg.Raw != nil && pkg.Raw.Fset != nil {
			result.fileSets[pkg.Path] = pkg.Raw.Fset
		}
	}
	result.cleanupType = canonicalCleanupType(program)
	return result
}

func stereotypeCounts(resolution resolve.Result) map[string]int {
	result := make(map[string]int)
	for _, occurrence := range resolution.Occurrences {
		contribution, present := occurrence.Contribution(
			sdk.ContributionStereotype,
		)
		if present && contribution.Stereotype.Construct {
			result[occurrence.PackagePath]++
		}
	}
	return result
}

func (build buildContext) stereotypeProvider(
	occurrence resolve.Occurrence,
	contribution sdk.StereotypeContribution,
	packageStereotypes int,
) (Provider, *Diagnostic) {
	symbol, found := build.symbols[occurrence.SymbolID]
	if !found {
		diagnostic := occurrenceDiagnostic(
			occurrence,
			"missing-symbol",
			fmt.Sprintf(
				"@%s target %q has no stable typed symbol in the loaded program",
				occurrence.Annotation.Name,
				occurrence.Name,
			),
		)
		return Provider{}, &diagnostic
	}
	named, problem := stereotypeType(symbol, contribution.Role)
	if problem != nil {
		diagnostic := symbolDiagnostic(
			occurrence,
			symbol,
			problem.kind,
			problem.message,
		)
		return Provider{}, &diagnostic
	}

	constructor, found, lookupProblem := build.selectConstructor(
		occurrence,
		symbol,
		contribution.Constructor,
		packageStereotypes,
	)
	if lookupProblem != nil {
		diagnostic := symbolDiagnostic(
			occurrence,
			symbol,
			lookupProblem.kind,
			lookupProblem.message,
		)
		return Provider{}, &diagnostic
	}
	if !found {
		return allocatedStereotypeProvider(
			occurrence,
			symbol,
			named,
			contribution.Role,
		), nil
	}

	constructorOccurrence := occurrence
	constructorOccurrence.Target = annotation.TargetFunction
	constructorOccurrence.Annotation.Arguments = nil
	item, diagnostic := analyzeProvider(
		constructorOccurrence,
		constructor,
		build.fileSets[constructor.PackagePath],
		build.cleanupType,
		SourceStereotype,
		"",
		"",
		"@"+occurrence.Annotation.Name+" constructor",
	)
	if diagnostic != nil {
		return Provider{}, diagnostic
	}
	if !stereotypeResult(item.Output, named) {
		diagnostic := symbolDiagnostic(
			occurrence,
			symbol,
			"constructor-result",
			fmt.Sprintf(
				"@%s %s constructor %s returns %s; it must return %s or *%s as its exact provided value",
				occurrence.Annotation.Name,
				symbol.DisplayLabel,
				constructor.DisplayLabel,
				item.OutputTypeID,
				symbol.Name,
				symbol.Name,
			),
		)
		return Provider{}, &diagnostic
	}
	item.Symbol = symbol
	item.SymbolID = symbol.ID
	item.Role = contribution.Role
	item.Name = lowerInitial(symbol.Name)
	if contribution.Name != "" {
		item.Name = contribution.Name
		item.ExplicitName = true
	}
	item.Aliases = append([]string(nil), contribution.Aliases...)
	item.Scope = sdk.BeanScopeSingleton
	item.PackagePath = symbol.PackagePath
	item.Position = occurrence.DisplayPosition
	item.PhysicalPosition = occurrence.PhysicalPosition
	item.Constructor = constructor
	return item, nil
}

func stereotypeType(
	symbol load.Symbol,
	role string,
) (*types.Named, *providerProblem) {
	typeName, ok := symbol.Object.(*types.TypeName)
	if symbol.Kind != load.SymbolType || !ok || typeName.IsAlias() {
		return nil, &providerProblem{
			kind: "invalid-stereotype",
			message: fmt.Sprintf(
				"@%s %s must target a defined named type",
				role,
				symbol.DisplayLabel,
			),
		}
	}
	named, ok := types.Unalias(typeName.Type()).(*types.Named)
	if !ok {
		return nil, &providerProblem{
			kind:    "invalid-stereotype",
			message: fmt.Sprintf("@%s %s must target a defined named type", role, symbol.DisplayLabel),
		}
	}
	if !token.IsExported(symbol.Name) {
		return nil, &providerProblem{
			kind:    "unexported-stereotype",
			message: fmt.Sprintf("@%s %s must be exported", role, symbol.DisplayLabel),
		}
	}
	if named.TypeParams() != nil && named.TypeParams().Len() != 0 {
		return nil, &providerProblem{
			kind:    "generic-stereotype",
			message: fmt.Sprintf("@%s %s must not declare type parameters", role, symbol.DisplayLabel),
		}
	}
	if _, ok := named.Underlying().(*types.Struct); !ok {
		return nil, &providerProblem{
			kind:    "invalid-stereotype",
			message: fmt.Sprintf("@%s %s must have a struct underlying type", role, symbol.DisplayLabel),
		}
	}
	return named, nil
}

func stereotypeResult(output types.Type, named *types.Named) bool {
	return types.Identical(output, named) ||
		types.Identical(output, types.NewPointer(named))
}

func allocatedStereotypeProvider(
	occurrence resolve.Occurrence,
	symbol load.Symbol,
	named *types.Named,
	role string,
) Provider {
	output := types.NewPointer(named)
	return Provider{
		Source:           SourceStereotype,
		Role:             role,
		Construction:     ConstructionAllocate,
		Symbol:           symbol,
		SymbolID:         symbol.ID,
		Name:             lowerInitial(symbol.Name),
		PackagePath:      symbol.PackagePath,
		Position:         occurrence.DisplayPosition,
		PhysicalPosition: occurrence.PhysicalPosition,
		Output:           output,
		OutputTypeID:     TypeID(output),
		Scope:            sdk.BeanScopeSingleton,
	}
}

func (build buildContext) selectConstructor(
	occurrence resolve.Occurrence,
	symbol load.Symbol,
	explicit string,
	packageStereotypes int,
) (load.Symbol, bool, *providerProblem) {
	if explicit != "" {
		constructor, err := build.resolveFunction(
			occurrence,
			explicit,
		)
		if err != nil {
			return load.Symbol{}, false, &providerProblem{
				kind: "constructor",
				message: fmt.Sprintf(
					"@%s %s constructor %q is invalid: %v",
					occurrence.Annotation.Name,
					symbol.DisplayLabel,
					explicit,
					err,
				),
			}
		}
		return constructor, true, nil
	}
	if constructor, found := build.packageFunction(
		symbol.PackagePath,
		"New"+symbol.Name,
	); found {
		return constructor, true, nil
	}
	if packageStereotypes == 1 {
		if constructor, found := build.packageFunction(
			symbol.PackagePath,
			"New",
		); found {
			return constructor, true, nil
		}
	}
	return load.Symbol{}, false, nil
}

func (build buildContext) packageFunction(
	packagePath string,
	name string,
) (load.Symbol, bool) {
	for _, symbol := range build.symbols {
		if symbol.PackagePath == packagePath &&
			symbol.Name == name &&
			symbol.Kind == load.SymbolFunction &&
			symbol.Receiver == "" {
			return symbol, true
		}
	}
	return load.Symbol{}, false
}

func lowerInitial(name string) string {
	if name == "" {
		return ""
	}
	first, size := utf8.DecodeRuneInString(name)
	return string(unicode.ToLower(first)) + name[size:]
}

func (build buildContext) resolveFunction(
	occurrence resolve.Occurrence,
	expression string,
) (load.Symbol, error) {
	parsed, err := goparser.ParseExpr(expression)
	if err != nil {
		return load.Symbol{}, fmt.Errorf("parse Go symbol: %w", err)
	}
	object, err := build.resolveObject(occurrence, parsed)
	if err != nil {
		return load.Symbol{}, err
	}
	function, ok := object.(*types.Func)
	if !ok {
		return load.Symbol{}, fmt.Errorf("%q does not name a function", expression)
	}
	signature, ok := function.Type().(*types.Signature)
	if !ok || signature.Recv() != nil {
		return load.Symbol{}, fmt.Errorf(
			"%q must name a package-level function",
			expression,
		)
	}
	for _, symbol := range build.symbols {
		if symbol.Object == function {
			return symbol, nil
		}
	}
	pkg := function.Pkg()
	if pkg == nil {
		return load.Symbol{}, fmt.Errorf(
			"%q has no source package",
			expression,
		)
	}
	position := token.Position{}
	if loaded, found := build.packages[pkg.Path()]; found &&
		loaded.Raw != nil &&
		loaded.Raw.Fset != nil {
		position = loaded.Raw.Fset.PositionFor(function.Pos(), true)
	}
	return load.Symbol{
		ID:               pkg.Path() + "." + function.Name(),
		DisplayLabel:     pkg.Name() + "." + function.Name(),
		Kind:             load.SymbolFunction,
		Name:             function.Name(),
		PackagePath:      pkg.Path(),
		Position:         position,
		PhysicalPosition: position,
		Object:           function,
		Signature:        signature,
	}, nil
}

func (build buildContext) resolveObject(
	occurrence resolve.Occurrence,
	expression ast.Expr,
) (types.Object, error) {
	pkg, file, err := build.sourceFile(occurrence)
	if err != nil {
		return nil, err
	}
	switch value := expression.(type) {
	case *ast.Ident:
		if object := pkg.Types.Scope().Lookup(value.Name); object != nil {
			return object, nil
		}
		if object := types.Universe.Lookup(value.Name); object != nil {
			return object, nil
		}
		return nil, fmt.Errorf(
			"go symbol %q is not declared in package %s",
			value.Name,
			pkg.Path,
		)
	case *ast.SelectorExpr:
		qualifier, ok := value.X.(*ast.Ident)
		if !ok {
			return nil, fmt.Errorf(
				"go symbol must be an identifier or package-qualified selector",
			)
		}
		imported, importErr := build.importedPackage(
			pkg,
			file,
			qualifier.Name,
		)
		if importErr != nil {
			return nil, importErr
		}
		object := imported.Scope().Lookup(value.Sel.Name)
		if object == nil {
			return nil, fmt.Errorf(
				"package %s has no exported symbol %q",
				imported.Path(),
				value.Sel.Name,
			)
		}
		if !object.Exported() {
			return nil, fmt.Errorf(
				"symbol %s.%s is not accessible",
				qualifier.Name,
				value.Sel.Name,
			)
		}
		return object, nil
	default:
		return nil, fmt.Errorf(
			"go symbol must be an identifier or package-qualified selector",
		)
	}
}

func (build buildContext) sourceFile(
	occurrence resolve.Occurrence,
) (load.Package, *ast.File, error) {
	pkg, found := build.packages[occurrence.PackagePath]
	if !found || pkg.Types == nil {
		return load.Package{}, nil, fmt.Errorf(
			"package %q is not available in the typed program",
			occurrence.PackagePath,
		)
	}
	physical := filepath.Clean(occurrence.PhysicalFile)
	for _, source := range pkg.Files {
		if filepath.Clean(source.PhysicalPath) == physical &&
			source.Syntax != nil {
			return pkg, source.Syntax, nil
		}
	}
	return load.Package{}, nil, fmt.Errorf(
		"source file %q is not available in package %s",
		occurrence.PhysicalFile,
		occurrence.PackagePath,
	)
}

func (build buildContext) importedPackage(
	pkg load.Package,
	file *ast.File,
	qualifier string,
) (*types.Package, error) {
	for _, spec := range file.Imports {
		pathValue, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		imported := pkg.Raw.Imports[pathValue]
		if imported == nil || imported.Types == nil {
			continue
		}
		name := imported.Types.Name()
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name == qualifier {
			return imported.Types, nil
		}
	}
	packagePath, found := spiceNamespaceImport(
		pkg,
		file,
		qualifier,
	)
	if found {
		imported, loaded := build.packages[packagePath]
		if !loaded || imported.Types == nil {
			return nil, fmt.Errorf(
				"spice namespace %q resolves to package %q, which is not available in the typed program",
				qualifier,
				packagePath,
			)
		}
		return imported.Types, nil
	}
	return nil, fmt.Errorf(
		"package qualifier %q is not imported by Go or a file-scoped Spice namespace import",
		qualifier,
	)
}

func spiceNamespaceImport(
	pkg load.Package,
	file *ast.File,
	qualifier string,
) (string, bool) {
	if pkg.Raw == nil || pkg.Raw.Fset == nil {
		return "", false
	}
	for _, group := range file.Comments {
		for _, comment := range group.List {
			position := pkg.Raw.Fset.PositionFor(comment.Pos(), true)
			directive, recognized, err := annotationparser.ParseImportComment(
				comment.Text,
				position,
			)
			if err != nil || !recognized ||
				directive.Kind != annotation.ImportNamespace ||
				directive.Namespace != qualifier {
				continue
			}
			return directive.Package, true
		}
	}
	return "", false
}

func attachInterfaceBindings(
	catalog *Catalog,
	build buildContext,
	resolution resolve.Result,
) {
	index := make(map[string]int, len(catalog.providers))
	constructorIndex := make(map[string]int, len(catalog.providers))
	for providerIndex := range catalog.providers {
		index[catalog.providers[providerIndex].SymbolID] = providerIndex
		constructorID := catalog.providers[providerIndex].Constructor.ID
		if constructorID != "" {
			constructorIndex[constructorID] = providerIndex
		}
	}
	for _, occurrence := range resolution.Occurrences {
		contribution, present := occurrence.Contribution(
			sdk.ContributionInterface,
		)
		if !present {
			continue
		}
		providerIndex, found := index[occurrence.SymbolID]
		if !found && occurrence.Target == annotation.TargetParameter {
			providerIndex, found = constructorIndex[occurrence.SymbolID]
		}
		if !found {
			catalog.diagnostics = append(
				catalog.diagnostics,
				occurrenceDiagnostic(
					occurrence,
					"interface-binding-provider",
					fmt.Sprintf(
						"@%s requires the same declaration to be a constructible stereotype or concrete-returning @Bean",
						occurrence.Annotation.Name,
					),
				),
			)
			continue
		}
		item := &catalog.providers[providerIndex]
		if _, isInterface := types.Unalias(
			item.Output,
		).Underlying().(*types.Interface); isInterface {
			catalog.diagnostics = append(
				catalog.diagnostics,
				occurrenceDiagnostic(
					occurrence,
					"redundant-interface-binding",
					fmt.Sprintf(
						"@%s is redundant on %s because its factory already returns exact interface type %s",
						occurrence.Annotation.Name,
						item.SymbolID,
						item.OutputTypeID,
					),
				),
			)
			continue
		}
		for _, expression := range contribution.Interface.Interfaces {
			binding, problem := build.resolveInterfaceBinding(
				occurrence,
				expression,
				item.Output,
			)
			if problem != nil {
				catalog.diagnostics = append(
					catalog.diagnostics,
					occurrenceDiagnostic(
						occurrence,
						problem.kind,
						problem.message,
					),
				)
				continue
			}
			item.Interfaces = append(item.Interfaces, binding)
		}
		sort.SliceStable(item.Interfaces, func(i, j int) bool {
			return item.Interfaces[i].TypeID <
				item.Interfaces[j].TypeID
		})
	}
}

func attachBeanMetadata(
	catalog *Catalog,
	resolution resolve.Result,
) {
	index := make(map[string]int, len(catalog.providers))
	for providerIndex := range catalog.providers {
		index[catalog.providers[providerIndex].SymbolID] = providerIndex
	}
	orderSeen := make(map[string]resolve.Occurrence)
	scopeSeen := make(map[string]resolve.Occurrence)
	for _, occurrence := range resolution.Occurrences {
		contribution, present := occurrence.Contribution(
			sdk.ContributionBeanMetadata,
		)
		if !present {
			continue
		}
		providerIndex, found := index[occurrence.SymbolID]
		if !found {
			catalog.diagnostics = append(
				catalog.diagnostics,
				occurrenceDiagnostic(
					occurrence,
					"bean-metadata-provider",
					fmt.Sprintf(
						"@%s requires a constructible stereotype or @Bean declaration",
						occurrence.Annotation.Name,
					),
				),
			)
			continue
		}
		item := &catalog.providers[providerIndex]
		if occurrence.Target == annotation.TargetParameter {
			attachParameterMetadata(
				catalog,
				item,
				occurrence,
				*contribution.BeanMetadata,
			)
			continue
		}
		attachProviderMetadata(
			catalog,
			item,
			occurrence,
			*contribution.BeanMetadata,
			orderSeen,
			scopeSeen,
		)
	}
	for index := range catalog.providers {
		item := &catalog.providers[index]
		if item.Primary && item.Fallback {
			catalog.diagnostics = append(
				catalog.diagnostics,
				Diagnostic{
					Position:         item.Position,
					PhysicalPosition: item.PhysicalPosition,
					ProviderID:       item.SymbolID,
					Kind:             "conflicting-selection",
					Message: fmt.Sprintf(
						"bean %q cannot be both @Primary and @Fallback",
						item.Name,
					),
				},
			)
		}
	}
}

func attachParameterMetadata(
	catalog *Catalog,
	item *Provider,
	occurrence resolve.Occurrence,
	metadata sdk.BeanMetadataContribution,
) {
	if metadata.Primary || metadata.Fallback ||
		metadata.Order != nil || metadata.Scope != "" {
		catalog.diagnostics = append(
			catalog.diagnostics,
			occurrenceDiagnostic(
				occurrence,
				"parameter-metadata",
				fmt.Sprintf(
					"@%s contributes bean metadata that is not valid on a constructor parameter",
					occurrence.Annotation.Name,
				),
			),
		)
		return
	}
	if occurrence.ParameterIndex < 0 ||
		occurrence.ParameterIndex >= len(item.Dependencies) {
		catalog.diagnostics = append(
			catalog.diagnostics,
			occurrenceDiagnostic(
				occurrence,
				"parameter-index",
				fmt.Sprintf(
					"@%s parameter index %d is outside constructor %s",
					occurrence.Annotation.Name,
					occurrence.ParameterIndex,
					item.Constructor.DisplayLabel,
				),
			),
		)
		return
	}
	dependency := &item.Dependencies[occurrence.ParameterIndex]
	for _, qualifier := range metadata.Qualifiers {
		if slices.Contains(dependency.Qualifiers, qualifier) {
			catalog.diagnostics = append(
				catalog.diagnostics,
				occurrenceDiagnostic(
					occurrence,
					"duplicate-qualifier",
					fmt.Sprintf(
						"constructor parameter %d %q repeats qualifier %q",
						dependency.Index,
						dependency.Name,
						qualifier,
					),
				),
			)
			continue
		}
		dependency.Qualifiers = append(
			dependency.Qualifiers,
			qualifier,
		)
	}
	sort.Strings(dependency.Qualifiers)
	if occurrence.ParameterPosition.IsValid() {
		dependency.Position = occurrence.ParameterPosition
	}
	if occurrence.ParameterPhysicalPosition.IsValid() {
		dependency.PhysicalPosition = occurrence.ParameterPhysicalPosition
	}
}

func attachProviderMetadata(
	catalog *Catalog,
	item *Provider,
	occurrence resolve.Occurrence,
	metadata sdk.BeanMetadataContribution,
	orderSeen map[string]resolve.Occurrence,
	scopeSeen map[string]resolve.Occurrence,
) {
	for _, qualifier := range metadata.Qualifiers {
		if slices.Contains(item.Qualifiers, qualifier) {
			catalog.diagnostics = append(
				catalog.diagnostics,
				occurrenceDiagnostic(
					occurrence,
					"duplicate-qualifier",
					fmt.Sprintf(
						"bean %q repeats qualifier %q",
						item.Name,
						qualifier,
					),
				),
			)
			continue
		}
		item.Qualifiers = append(item.Qualifiers, qualifier)
	}
	if metadata.Primary {
		item.Primary = true
	}
	if metadata.Fallback {
		item.Fallback = true
	}
	if metadata.Order != nil {
		if previous, duplicate := orderSeen[item.SymbolID]; duplicate {
			catalog.diagnostics = append(
				catalog.diagnostics,
				occurrenceDiagnostic(
					occurrence,
					"duplicate-order",
					fmt.Sprintf(
						"bean %q repeats @Order; first declaration is at %s",
						item.Name,
						renderedPosition(previous.DisplayPosition),
					),
				),
			)
		} else {
			orderSeen[item.SymbolID] = occurrence
			item.Order = *metadata.Order
		}
	}
	if metadata.Scope != "" {
		if previous, duplicate := scopeSeen[item.SymbolID]; duplicate {
			catalog.diagnostics = append(
				catalog.diagnostics,
				occurrenceDiagnostic(
					occurrence,
					"duplicate-scope",
					fmt.Sprintf(
						"bean %q declares multiple scopes; first declaration is at %s",
						item.Name,
						renderedPosition(previous.DisplayPosition),
					),
				),
			)
		} else {
			scopeSeen[item.SymbolID] = occurrence
			item.Scope = metadata.Scope
		}
	}
	sort.Strings(item.Qualifiers)
}

func (build buildContext) resolveInterfaceBinding(
	occurrence resolve.Occurrence,
	expression string,
	output types.Type,
) (InterfaceBinding, *providerProblem) {
	parsed, err := goparser.ParseExpr(expression)
	if err != nil {
		return InterfaceBinding{}, &providerProblem{
			kind: "interface-expression",
			message: fmt.Sprintf(
				"@%s interface expression %q is not valid Go: %v",
				occurrence.Annotation.Name,
				expression,
				err,
			),
		}
	}
	value, err := build.resolveType(occurrence, parsed)
	if err != nil {
		return InterfaceBinding{}, &providerProblem{
			kind: "interface-expression",
			message: fmt.Sprintf(
				"@%s interface expression %q cannot be resolved: %v",
				occurrence.Annotation.Name,
				expression,
				err,
			),
		}
	}
	named, ok := types.Unalias(value).(*types.Named)
	if !ok {
		return InterfaceBinding{}, &providerProblem{
			kind: "interface-expression",
			message: fmt.Sprintf(
				"@%s expression %q must name a Go interface type; anonymous and pointer-to-interface expressions are not supported",
				occurrence.Annotation.Name,
				expression,
			),
		}
	}
	contract, ok := named.Underlying().(*types.Interface)
	if !ok {
		return InterfaceBinding{}, &providerProblem{
			kind: "interface-expression",
			message: fmt.Sprintf(
				"@%s expression %q resolves to %s, not an interface",
				occurrence.Annotation.Name,
				expression,
				TypeID(value),
			),
		}
	}
	contract.Complete()
	if !contract.IsMethodSet() {
		return InterfaceBinding{}, &providerProblem{
			kind: "constraint-interface",
			message: fmt.Sprintf(
				"@%s interface %q is constraint-only and cannot be a runtime dependency",
				occurrence.Annotation.Name,
				expression,
			),
		}
	}
	if !types.Implements(output, contract) {
		return InterfaceBinding{}, &providerProblem{
			kind: "interface-method-set",
			message: fmt.Sprintf(
				"@%s bean output %s does not implement %s; pointer and value method sets are checked exactly",
				occurrence.Annotation.Name,
				TypeID(output),
				TypeID(value),
			),
		}
	}
	return InterfaceBinding{
		Expression:       expression,
		Type:             value,
		TypeID:           TypeID(value),
		Position:         occurrence.DisplayPosition,
		PhysicalPosition: occurrence.PhysicalPosition,
	}, nil
}

func (build buildContext) resolveType(
	occurrence resolve.Occurrence,
	expression ast.Expr,
) (types.Type, error) {
	switch value := expression.(type) {
	case *ast.ParenExpr:
		return build.resolveType(occurrence, value.X)
	case *ast.StarExpr:
		element, err := build.resolveType(occurrence, value.X)
		if err != nil {
			return nil, err
		}
		return types.NewPointer(element), nil
	case *ast.Ident, *ast.SelectorExpr:
		object, err := build.resolveObject(occurrence, value)
		if err != nil {
			return nil, err
		}
		typeName, ok := object.(*types.TypeName)
		if !ok {
			return nil, fmt.Errorf("%q does not name a type", object.Name())
		}
		return typeName.Type(), nil
	case *ast.IndexExpr:
		return build.instantiateType(
			occurrence,
			value.X,
			[]ast.Expr{value.Index},
		)
	case *ast.IndexListExpr:
		return build.instantiateType(
			occurrence,
			value.X,
			value.Indices,
		)
	default:
		return nil, fmt.Errorf(
			"only named interfaces and instantiated named generic interfaces are supported",
		)
	}
}

func (build buildContext) instantiateType(
	occurrence resolve.Occurrence,
	base ast.Expr,
	arguments []ast.Expr,
) (types.Type, error) {
	baseType, err := build.resolveType(occurrence, base)
	if err != nil {
		return nil, err
	}
	named, ok := types.Unalias(baseType).(*types.Named)
	if !ok {
		return nil, fmt.Errorf("generic interface base is not a named type")
	}
	typeArguments := make([]types.Type, len(arguments))
	for index, argument := range arguments {
		typeArguments[index], err = build.resolveType(
			occurrence,
			argument,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"type argument %d: %w",
				index,
				err,
			)
		}
	}
	instantiated, err := types.Instantiate(
		nil,
		named,
		typeArguments,
		true,
	)
	if err != nil {
		return nil, fmt.Errorf("instantiate generic interface: %w", err)
	}
	return instantiated, nil
}

type providerProblem struct {
	kind    string
	message string
}

type resultMetadata struct {
	output         types.Type
	returnsCleanup bool
	returnsError   bool
}

func analyzeProvider(
	occurrence resolve.Occurrence,
	symbol load.Symbol,
	fileSet *token.FileSet,
	cleanupType types.Type,
	source Source,
	sourceID string,
	sourceVersion string,
	role string,
) (Provider, *Diagnostic) {
	label := symbol.DisplayLabel
	if label == "" {
		label = symbol.ID
	}
	targetLabel := role + " " + label
	if source == SourceBean {
		targetLabel = "@Bean " + label
	}
	label = role + " " + label
	signature, problem := providerSignature(
		occurrence,
		symbol,
		source,
		label,
		targetLabel,
	)
	if problem != nil {
		diagnostic := symbolDiagnostic(occurrence, symbol, problem.kind, problem.message)
		return Provider{}, &diagnostic
	}
	results, problem := analyzeResults(signature.Results(), cleanupType, label)
	if problem != nil {
		diagnostic := symbolDiagnostic(occurrence, symbol, problem.kind, problem.message)
		return Provider{}, &diagnostic
	}

	dependencies := providerDependencies(signature, fileSet)
	if signature.Recv() != nil {
		dependencies = providerMethodDependencies(signature, fileSet)
	}
	return Provider{
		Source:           source,
		Construction:     ConstructionFactory,
		Symbol:           symbol,
		Constructor:      symbol,
		SymbolID:         symbol.ID,
		Name:             symbol.Name,
		PackagePath:      symbol.PackagePath,
		Position:         occurrence.DisplayPosition,
		PhysicalPosition: token.Position{Filename: occurrence.PhysicalFile, Offset: occurrence.PhysicalOffset},
		Output:           results.output,
		OutputTypeID:     TypeID(results.output),
		Dependencies:     dependencies,
		ReturnsCleanup:   results.returnsCleanup,
		ReturnsError:     results.returnsError,
		SourceID:         sourceID,
		SourceVersion:    sourceVersion,
		Scope:            sdk.BeanScopeSingleton,
	}, nil
}

func providerSignature(
	occurrence resolve.Occurrence,
	symbol load.Symbol,
	source Source,
	label string,
	targetLabel string,
) (*types.Signature, *providerProblem) {
	packageFunction := occurrence.Target == annotation.TargetFunction &&
		symbol.Kind == load.SymbolFunction && symbol.Receiver == ""
	configurationMethod := source == SourceBean &&
		occurrence.Target == annotation.TargetMethod &&
		symbol.Kind == load.SymbolMethod && symbol.Receiver != ""
	if !packageFunction && !configurationMethod {
		return nil, &providerProblem{
			kind:    "invalid-target",
			message: fmt.Sprintf("%s must target a package-level function or a method on a constructible @Configuration type", targetLabel),
		}
	}
	signature := symbol.Signature
	if signature == nil {
		return nil, &providerProblem{
			kind:    "missing-signature",
			message: fmt.Sprintf("%s has no typed function signature", label),
		}
	}
	if signature.TypeParams() != nil && signature.TypeParams().Len() > 0 {
		return nil, signatureProblem("generic", label, "generic provider functions are not supported")
	}
	if signature.Variadic() {
		return nil, signatureProblem("variadic", label, "variadic provider functions are not supported")
	}
	return signature, nil
}

func analyzeResults(results *types.Tuple, cleanupType types.Type, label string) (resultMetadata, *providerProblem) {
	switch results.Len() {
	case 0:
		return resultMetadata{}, signatureProblem(
			"zero-results",
			label,
			"provider functions must return one provided value",
		)
	case 1:
		return analyzeOneResult(results, label)
	case 2:
		return analyzeTwoResults(results, cleanupType, label)
	case 3:
		return analyzeThreeResults(results, cleanupType, label)
	default:
		return analyzeExcessResults(results, cleanupType, label)
	}
}

func analyzeOneResult(results *types.Tuple, label string) (resultMetadata, *providerProblem) {
	output := results.At(0).Type()
	if isError(output) {
		return resultMetadata{}, signatureProblem("error-only", label, "error cannot be the only result")
	}
	return resultMetadata{output: output}, nil
}

func analyzeTwoResults(
	results *types.Tuple,
	cleanupType types.Type,
	label string,
) (resultMetadata, *providerProblem) {
	first := results.At(0).Type()
	second := results.At(1).Type()
	if isError(first) {
		return resultMetadata{}, signatureProblem(
			"error-position",
			label,
			"error must be the final and only additional result",
		)
	}
	switch {
	case isError(second):
		return resultMetadata{output: first, returnsError: true}, nil
	case isCleanup(second, cleanupType):
		return resultMetadata{output: first, returnsCleanup: true}, nil
	default:
		return resultMetadata{}, signatureProblem(
			"invalid-second-result",
			label,
			"provider functions may return only one provided value; the second result must be lifecycle.Cleanup or error",
		)
	}
}

func analyzeThreeResults(
	results *types.Tuple,
	cleanupType types.Type,
	label string,
) (resultMetadata, *providerProblem) {
	first := results.At(0).Type()
	second := results.At(1).Type()
	third := results.At(2).Type()
	errorResults, cleanupResults := metadataCounts([]types.Type{second, third}, cleanupType)
	if cleanupResults > 1 {
		return resultMetadata{}, signatureProblem(
			"too-many-results",
			label,
			"provider functions may return at most one lifecycle.Cleanup metadata result",
		)
	}
	if errorResults > 1 {
		return resultMetadata{}, signatureProblem(
			"too-many-results",
			label,
			"provider functions may return at most one error result",
		)
	}
	if isError(first) {
		return resultMetadata{}, signatureProblem(
			"error-position",
			label,
			"error must be the final and only additional result",
		)
	}
	if !isCleanup(second, cleanupType) {
		reason := "the second result must be lifecycle.Cleanup"
		if isError(second) {
			reason = "error must follow lifecycle.Cleanup as the final result"
		}
		return resultMetadata{}, signatureProblem("cleanup-position", label, reason)
	}
	if !isError(third) {
		reason := "the third result must be error"
		if isCleanup(third, cleanupType) {
			reason = "only one lifecycle.Cleanup metadata result is allowed and it must be second"
		}
		return resultMetadata{}, signatureProblem("error-position", label, reason)
	}
	return resultMetadata{output: first, returnsCleanup: true, returnsError: true}, nil
}

func analyzeExcessResults(
	results *types.Tuple,
	cleanupType types.Type,
	label string,
) (resultMetadata, *providerProblem) {
	metadata := make([]types.Type, 0, results.Len())
	for variable := range results.Variables() {
		metadata = append(metadata, variable.Type())
	}
	errorResults, cleanupResults := metadataCounts(metadata, cleanupType)
	reason := "provider functions may return only one provided value, one optional lifecycle.Cleanup, and one optional final error"
	if cleanupResults > 1 {
		reason = "provider functions may return at most one lifecycle.Cleanup result"
	} else if errorResults > 1 {
		reason = "provider functions may return at most one error result"
	}
	return resultMetadata{}, signatureProblem("too-many-results", label, reason)
}

func metadataCounts(metadata []types.Type, cleanupType types.Type) (errorResults, cleanupResults int) {
	for _, result := range metadata {
		if isError(result) {
			errorResults++
		}
		if isCleanup(result, cleanupType) {
			cleanupResults++
		}
	}
	return errorResults, cleanupResults
}

func providerDependencies(signature *types.Signature, fileSet *token.FileSet) []Dependency {
	dependencies := make([]Dependency, signature.Params().Len())
	for index := 0; index < signature.Params().Len(); index++ {
		dependencies[index] = dependencyFromVariable(
			signature.Params().At(index),
			index,
			fileSet,
		)
	}
	return dependencies
}

func providerMethodDependencies(
	signature *types.Signature,
	fileSet *token.FileSet,
) []Dependency {
	dependencies := make(
		[]Dependency,
		0,
		1+signature.Params().Len(),
	)
	dependencies = append(
		dependencies,
		dependencyFromVariable(signature.Recv(), 0, fileSet),
	)
	for index := range signature.Params().Len() {
		dependencies = append(
			dependencies,
			dependencyFromVariable(
				signature.Params().At(index),
				index+1,
				fileSet,
			),
		)
	}
	return dependencies
}

func dependencyFromVariable(
	parameter *types.Var,
	index int,
	fileSet *token.FileSet,
) Dependency {
	dependency := Dependency{
		Index:  index,
		Name:   parameter.Name(),
		Type:   parameter.Type(),
		TypeID: TypeID(parameter.Type()),
		Kind:   DependencySingle,
	}
	classifyDependency(&dependency)
	if fileSet != nil && parameter.Pos().IsValid() {
		dependency.Position = fileSet.PositionFor(parameter.Pos(), true)
		dependency.PhysicalPosition = fileSet.PositionFor(parameter.Pos(), false)
	}
	return dependency
}

func classifyDependency(dependency *Dependency) {
	if dependency == nil || dependency.Type == nil {
		return
	}
	switch value := types.Unalias(dependency.Type).(type) {
	case *types.Slice:
		dependency.Kind = DependencySlice
		dependency.Element = value.Elem()
	case *types.Map:
		if types.Identical(value.Key(), types.Typ[types.String]) {
			dependency.Kind = DependencyMap
			dependency.Element = value.Elem()
		}
	case *types.Named:
		classifyBeanHandle(dependency, value)
	}
	if dependency.Element != nil {
		dependency.ElementTypeID = TypeID(dependency.Element)
	}
}

func classifyBeanHandle(
	dependency *Dependency,
	value *types.Named,
) {
	if value.Obj() == nil || value.Obj().Pkg() == nil ||
		value.Obj().Pkg().Path() != beanPackagePath ||
		value.TypeArgs() == nil || value.TypeArgs().Len() != 1 {
		return
	}
	switch value.Obj().Name() {
	case "Optional":
		dependency.Kind = DependencyOptional
	case "Lazy":
		dependency.Kind = DependencyLazy
	case "Provider":
		dependency.Kind = DependencyProvider
	default:
		return
	}
	dependency.Element = value.TypeArgs().At(0)
}

func signatureProblem(kind, label, reason string) *providerProblem {
	return &providerProblem{kind: kind, message: invalidSignatureMessage(label, reason)}
}

func invalidSignatureMessage(label, reason string) string {
	return fmt.Sprintf("%s has an unsupported signature: %s; accepted forms are %s", label, reason, acceptedSignature)
}

func isError(value types.Type) bool {
	return value != nil && types.Identical(value, errorType)
}

func canonicalCleanupType(program *load.Program) types.Type {
	cleanup := loadedNamedType(program, cleanupPackagePath, cleanupTypeName)
	contextType := loadedNamedType(program, "context", "Context")
	if cleanup == nil || contextType == nil {
		return nil
	}
	signature, ok := cleanup.Underlying().(*types.Signature)
	if !ok || signature.Variadic() ||
		(signature.TypeParams() != nil && signature.TypeParams().Len() > 0) ||
		signature.Params().Len() != 1 || signature.Results().Len() != 1 {
		return nil
	}
	if !types.Identical(signature.Params().At(0).Type(), contextType) ||
		!isError(signature.Results().At(0).Type()) {
		return nil
	}
	return cleanup
}

func loadedNamedType(program *load.Program, packagePath, typeName string) *types.Named {
	if program == nil {
		return nil
	}
	seen := make(map[*types.Package]struct{})
	var found *types.Named
	valid := true
	var visit func(*types.Package)
	visit = func(pkg *types.Package) {
		if pkg == nil {
			return
		}
		if _, ok := seen[pkg]; ok {
			return
		}
		seen[pkg] = struct{}{}
		if pkg.Path() == packagePath {
			object, ok := pkg.Scope().Lookup(typeName).(*types.TypeName)
			if !ok || object.IsAlias() {
				valid = false
			} else {
				named, ok := object.Type().(*types.Named)
				switch {
				case !ok || named.Obj() != object:
					valid = false
				case found == nil:
					found = named
				case !types.Identical(found, named):
					valid = false
				}
			}
		}
		for _, imported := range pkg.Imports() {
			visit(imported)
		}
	}
	for _, pkg := range program.Packages() {
		visit(pkg.Types)
	}
	if !valid {
		return nil
	}
	return found
}

func isCleanup(value, cleanupType types.Type) bool {
	return value != nil && cleanupType != nil && types.Identical(value, cleanupType)
}

// TypeID returns a deterministic readable Go type string using package import
// paths rather than source aliases or filesystem paths.
func TypeID(value types.Type) string {
	if value == nil {
		return "<invalid>"
	}
	return types.TypeString(value, func(pkg *types.Package) string {
		if pkg == nil {
			return ""
		}
		return pkg.Path()
	})
}

func normalizeProviderMetadata(providers []Provider) {
	for index := range providers {
		item := &providers[index]
		if item.Name == "" {
			item.Name = lowerInitial(item.Symbol.Name)
		}
		if item.Name == "" {
			item.Name = item.SymbolID
		}
		if item.Scope == "" {
			item.Scope = sdk.BeanScopeSingleton
		}
		sort.Strings(item.Aliases)
		sort.Strings(item.Qualifiers)
		for dependencyIndex := range item.Dependencies {
			sort.Strings(item.Dependencies[dependencyIndex].Qualifiers)
		}
	}
}

func beanIdentityDiagnostics(providers []Provider) []Diagnostic {
	type owner struct {
		provider int
		explicit bool
	}
	owners := make(map[string]owner)
	var diagnostics []Diagnostic
	for index := range providers {
		item := &providers[index]
		names := append([]string{item.Name}, item.Aliases...)
		seen := make(map[string]struct{}, len(names))
		for nameIndex, name := range names {
			explicit := item.ExplicitName || nameIndex > 0
			if _, duplicate := seen[name]; duplicate {
				diagnostics = append(
					diagnostics,
					beanIdentityDiagnostic(
						item,
						"duplicate-bean-alias",
						fmt.Sprintf(
							"bean %q repeats name or alias %q",
							item.Name,
							name,
						),
					),
				)
				continue
			}
			seen[name] = struct{}{}
			if previous, duplicate := owners[name]; duplicate &&
				(previous.explicit || explicit) {
				other := &providers[previous.provider]
				diagnostics = append(
					diagnostics,
					beanIdentityDiagnostic(
						item,
						"duplicate-bean-name",
						fmt.Sprintf(
							"bean name or alias %q is owned by both %s at %s and %s at %s",
							name,
							providerLabel(other),
							renderedPosition(other.Position),
							providerLabel(item),
							renderedPosition(item.Position),
						),
					),
				)
				continue
			}
			owners[name] = owner{
				provider: index,
				explicit: explicit,
			}
		}
	}
	return diagnostics
}

func nonSelectableDuplicateDiagnostics(
	providers []Provider,
) []Diagnostic {
	var groups [][]int
	for index := range providers {
		placed := false
		for groupIndex := range groups {
			if types.Identical(
				providers[index].Output,
				providers[groups[groupIndex][0]].Output,
			) {
				groups[groupIndex] = append(
					groups[groupIndex],
					index,
				)
				placed = true
				break
			}
		}
		if !placed {
			groups = append(groups, []int{index})
		}
	}
	var diagnostics []Diagnostic
	for _, group := range groups {
		if len(group) < 2 || allSelectableProviders(group, providers) {
			continue
		}
		labels := make([]string, len(group))
		for index, providerIndex := range group {
			item := &providers[providerIndex]
			labels[index] = fmt.Sprintf(
				"%s at %s",
				providerLabel(item),
				renderedPosition(item.Position),
			)
		}
		first := &providers[group[0]]
		diagnostics = append(diagnostics, Diagnostic{
			Position:         first.Position,
			PhysicalPosition: first.PhysicalPosition,
			ProviderID:       first.SymbolID,
			Kind:             "duplicate-output",
			Message: fmt.Sprintf(
				"multiple providers produce exact type %s where generated sources are not selectable: %s",
				first.OutputTypeID,
				strings.Join(labels, ", "),
			),
		})
	}
	return diagnostics
}

func allSelectableProviders(
	group []int,
	providers []Provider,
) bool {
	symbols := make(map[string]struct{}, len(group))
	for _, index := range group {
		symbolID := providers[index].SymbolID
		if symbolID != "" {
			if _, duplicate := symbols[symbolID]; duplicate {
				return false
			}
			symbols[symbolID] = struct{}{}
		}
		if providers[index].Source != SourceBean &&
			providers[index].Source != SourceStereotype &&
			providers[index].Source != SourceStarter &&
			providers[index].Source != SourceAutoConfiguration &&
			providers[index].Source != "" {
			return false
		}
	}
	return true
}

func beanIdentityDiagnostic(
	item *Provider,
	kind string,
	message string,
) Diagnostic {
	return Diagnostic{
		Position:         item.Position,
		PhysicalPosition: item.PhysicalPosition,
		ProviderID:       item.SymbolID,
		Kind:             kind,
		Message:          message,
	}
}

func providerLabel(item *Provider) string {
	if item.Symbol.DisplayLabel != "" {
		return item.Symbol.DisplayLabel
	}
	if item.Name != "" {
		return item.Name
	}
	return item.SymbolID
}

func occurrenceDiagnostic(occurrence resolve.Occurrence, kind, message string) Diagnostic {
	return Diagnostic{
		Position: occurrence.DisplayPosition,
		PhysicalPosition: token.Position{
			Filename: occurrence.PhysicalFile,
			Offset:   occurrence.PhysicalOffset,
		},
		ProviderID: occurrence.SymbolID,
		Kind:       kind,
		Message:    message,
	}
}

func symbolDiagnostic(occurrence resolve.Occurrence, symbol load.Symbol, kind, message string) Diagnostic {
	diagnostic := occurrenceDiagnostic(occurrence, kind, message)
	if symbol.ID != "" {
		diagnostic.ProviderID = symbol.ID
	}
	if diagnostic.Position.Filename == "" {
		diagnostic.Position = symbol.Position
	}
	if diagnostic.PhysicalPosition.Filename == "" {
		diagnostic.PhysicalPosition = symbol.PhysicalPosition
	}
	return diagnostic
}

func renderedPosition(position token.Position) string {
	filename := position.Filename
	if filename == "" {
		filename = "<unknown>"
	}
	line := position.Line
	if line <= 0 {
		line = 1
	}
	column := position.Column
	if column <= 0 {
		column = 1
	}
	return fmt.Sprintf("%s:%d:%d", filename, line, column)
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
		if left.ProviderID != right.ProviderID {
			return left.ProviderID < right.ProviderID
		}
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		return left.Message < right.Message
	})
}
