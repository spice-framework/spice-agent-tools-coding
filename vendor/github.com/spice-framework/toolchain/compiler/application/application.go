// @import { NamedInterface } from "github.com/spice-framework/spice/annotation/modulith"

// Package application assembles the immutable Spice application model from
// one resolved, type-aware compiler program.
//
// @NamedInterface("application")
package application

import (
	"fmt"
	"go/token"
	"go/types"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/spice-framework/spice/annotation"
	"github.com/spice-framework/spice/annotation/sdk"
	compilerasync "github.com/spice-framework/toolchain/compiler/async"
	compilerbootstrap "github.com/spice-framework/toolchain/compiler/bootstrap"
	compilercache "github.com/spice-framework/toolchain/compiler/cache"
	"github.com/spice-framework/toolchain/compiler/configuration"
	"github.com/spice-framework/toolchain/compiler/controller"
	compilerevent "github.com/spice-framework/toolchain/compiler/event"
	"github.com/spice-framework/toolchain/compiler/graph"
	"github.com/spice-framework/toolchain/compiler/lifecycle"
	"github.com/spice-framework/toolchain/compiler/load"
	"github.com/spice-framework/toolchain/compiler/modulith"
	"github.com/spice-framework/toolchain/compiler/provider"
	"github.com/spice-framework/toolchain/compiler/resolve"
	compilerschedule "github.com/spice-framework/toolchain/compiler/schedule"
	compilertransaction "github.com/spice-framework/toolchain/compiler/transaction"
)

const (
	acceptedMainMarkerSignature   = "package main: func main()"
	acceptedLegacyMarkerSignature = "func(applicationRoots...)"
)

// Stage identifies the compiler phase that produced a model diagnostic.
type Stage string

const (
	// StageResolution identifies invalid resolved annotation input.
	StageResolution Stage = "resolution"
	// StageProvider identifies provider catalog validation.
	StageProvider Stage = "provider"
	// StageGraph identifies provider graph validation.
	StageGraph Stage = "graph"
	// StageLifecycle identifies lifecycle hook validation.
	StageLifecycle Stage = "lifecycle"
	// StageSchedule identifies scheduled-method validation.
	StageSchedule Stage = "schedule"
	// StageAsync identifies asynchronous-method validation.
	StageAsync Stage = "async"
	// StageTransaction identifies generated transaction-boundary validation.
	StageTransaction Stage = "transaction"
	// StageEvent identifies typed event topic and listener validation.
	StageEvent Stage = "event"
	// StageModule identifies application-module architecture validation.
	StageModule Stage = "module"
	// StageConfiguration identifies typed configuration validation.
	StageConfiguration Stage = "configuration"
	// StageController identifies typed HTTP controller validation.
	StageController Stage = "controller"
	// StageCache identifies generated HTTP cache-boundary validation.
	StageCache Stage = "cache"
	// StageBootstrap identifies application-platform feature validation.
	StageBootstrap Stage = "bootstrap"
	// StageApplication identifies application marker and root validation.
	StageApplication Stage = "application"
)

// Root identifies one exact provider type requested by an @Application marker.
type Root struct {
	Index            int
	Name             string
	Type             types.Type
	TypeID           string
	ProviderID       string
	Position         token.Position
	PhysicalPosition token.Position
}

// Target describes one validated @Application marker. The marker function is
// compile-time metadata only; Spice never invokes its body during analysis.
type Target struct {
	Symbol           load.Symbol
	SymbolID         string
	Name             string
	PackagePath      string
	Position         token.Position
	PhysicalPosition token.Position
	roots            []Root
	bootstrap        compilerbootstrap.Metadata
	automatic        bool
}

// Roots returns the target's roots in function-parameter order.
func (t Target) Roots() []Root {
	return append([]Root(nil), t.roots...)
}

// Bootstrap returns immutable, typed application-platform feature metadata.
func (t Target) Bootstrap() compilerbootstrap.Metadata {
	return t.bootstrap
}

// AutomaticDiscovery reports whether this is the preferred package-main
// marker whose application graph is discovered from the selected local module
// scope rather than enumerated as marker parameters.
func (t Target) AutomaticDiscovery() bool {
	return t.automatic
}

// Diagnostic is one deterministic source-positioned application-model failure.
type Diagnostic struct {
	Stage            Stage
	Position         token.Position
	PhysicalPosition token.Position
	SymbolID         string
	Kind             string
	Message          string
	Fixes            []provider.SuggestedFix
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

// Model is the immutable-by-convention application IR assembled from one
// loaded Program and one resolved annotation result.
type Model struct {
	providers    []provider.Provider
	edges        []graph.Edge
	components   []lifecycle.Component
	jobs         []compilerschedule.Job
	asyncTasks   []compilerasync.Task
	events       []compilerevent.Topic
	transactions []compilertransaction.Boundary
	caches       []compilercache.Boundary
	configTypes  []configuration.Type
	controllers  []controller.Controller
	targets      []Target
	moduleModel  modulith.Model
	diagnostics  []Diagnostic
}

// BuildOptions supplies explicitly composed compiler extensions. The caller
// remains responsible for validating annotations with a registry containing
// matching definitions before building the application model.
type BuildOptions struct {
	BootstrapDefinitions []compilerbootstrap.Definition
	ProviderCatalogs     []provider.Catalog
	// PrimaryProviderCatalog reuses a catalog already built from this exact
	// Program and resolution. It lets compiler services expose partial,
	// type-safe authoring metadata without rebuilding provider semantics.
	PrimaryProviderCatalog *provider.Catalog
}

// Providers returns providers in deterministic dependency-first construction
// order. Cleanup capabilities remain attached to each provider record.
func (m Model) Providers() []provider.Provider {
	result := make([]provider.Provider, len(m.providers))
	for index := range m.providers {
		result[index] = cloneProvider(m.providers[index])
	}
	return result
}

// Edges returns exact-type provider graph edges in stable consumer and
// parameter order.
func (m Model) Edges() []graph.Edge {
	return append([]graph.Edge(nil), m.edges...)
}

// Components returns lifecycle components in provider construction order.
func (m Model) Components() []lifecycle.Component {
	result := make([]lifecycle.Component, len(m.components))
	for index := range m.components {
		result[index] = cloneComponent(m.components[index])
	}
	return result
}

// Jobs returns immutable provider-owned scheduling metadata in stable method
// identity order.
func (m Model) Jobs() []compilerschedule.Job {
	return append([]compilerschedule.Job(nil), m.jobs...)
}

// AsyncTasks returns immutable provider-owned asynchronous method metadata in
// stable method identity order.
func (m Model) AsyncTasks() []compilerasync.Task {
	return append([]compilerasync.Task(nil), m.asyncTasks...)
}

// Events returns generated typed event topics in stable marker identity order.
func (m Model) Events() []compilerevent.Topic {
	return append([]compilerevent.Topic(nil), m.events...)
}

// Transactions returns immutable generated transaction boundaries in stable
// route identity order.
func (m Model) Transactions() []compilertransaction.Boundary {
	return append([]compilertransaction.Boundary(nil), m.transactions...)
}

// Caches returns immutable generated HTTP cache boundaries in stable route
// identity order.
func (m Model) Caches() []compilercache.Boundary {
	return append([]compilercache.Boundary(nil), m.caches...)
}

// Configurations returns validated typed configuration declarations in stable
// symbol order.
func (m Model) Configurations() []configuration.Type {
	return append([]configuration.Type(nil), m.configTypes...)
}

// Controllers returns validated provider-owned HTTP controllers in stable
// symbol order.
func (m Model) Controllers() []controller.Controller {
	return append([]controller.Controller(nil), m.controllers...)
}

// Targets returns validated application targets in stable symbol order.
func (m Model) Targets() []Target {
	result := make([]Target, len(m.targets))
	copy(result, m.targets)
	for index := range result {
		result[index].roots = append([]Root(nil), m.targets[index].roots...)
	}
	return result
}

// Modules returns discovered application modules in stable import-path order.
func (m Model) Modules() []modulith.Module {
	return m.moduleModel.Modules()
}

// ModuleEdges returns distinct cross-module Go import edges.
func (m Model) ModuleEdges() []modulith.Edge {
	return m.moduleModel.Edges()
}

// ModuleCycles returns deterministic strongly connected module components.
func (m Model) ModuleCycles() []modulith.Cycle {
	return m.moduleModel.Cycles()
}

// UnassignedPackages returns selected packages not owned by any module root.
func (m Model) UnassignedPackages() []modulith.Package {
	return m.moduleModel.UnassignedPackages()
}

// Diagnostics returns deterministic diagnostics. A model with diagnostics must
// not be used for generation.
func (m Model) Diagnostics() []Diagnostic {
	result := make([]Diagnostic, len(m.diagnostics))
	for index, item := range m.diagnostics {
		result[index] = item
		result[index].Fixes = cloneProviderFixes(item.Fixes)
	}
	return result
}

func cloneProviderFixes(
	items []provider.SuggestedFix,
) []provider.SuggestedFix {
	result := make([]provider.SuggestedFix, len(items))
	for index, item := range items {
		result[index] = item
		result[index].Edits = append(
			[]provider.SuggestedEdit(nil),
			item.Edits...,
		)
	}
	return result
}

// Build assembles provider, graph, lifecycle, and application-root metadata
// from the existing single-program pipeline. It never reloads source, reparses
// files, reflects on declarations, or executes application/provider bodies.
func Build(program *load.Program, resolution resolve.Result) Model {
	return BuildWithOptions(program, resolution, BuildOptions{})
}

// BuildWithOptions assembles an application model with explicitly supplied
// bootstrap feature definitions in addition to Spice's built-in definitions.
func BuildWithOptions(
	program *load.Program,
	resolution resolve.Result,
	options BuildOptions,
) Model {
	model := Model{}
	if program == nil {
		model.diagnostics = []Diagnostic{{
			Stage:   StageApplication,
			Kind:    "internal",
			Message: "application model requires a loaded program",
		}}
		return model
	}
	if len(resolution.Diagnostics) != 0 {
		model.diagnostics = resolutionDiagnostics(resolution.Diagnostics)
		return model
	}

	model.moduleModel = modulith.Build(program, resolution)
	if diagnostics := model.moduleModel.Diagnostics(); len(diagnostics) != 0 {
		model.diagnostics = moduleDiagnostics(diagnostics)
		return model
	}

	configurationCatalog := configuration.Build(program, resolution, model.moduleModel)
	if diagnostics := configurationCatalog.Diagnostics(); len(diagnostics) != 0 {
		model.diagnostics = configurationDiagnostics(diagnostics)
		return model
	}
	model.configTypes = configurationCatalog.Types()

	providerCatalog, events, providerModelDiagnostics := buildProviderMetadata(
		program,
		resolution,
		model.configTypes,
		model.moduleModel,
		options.PrimaryProviderCatalog,
		options.ProviderCatalogs,
	)
	model.events = events
	model.diagnostics = providerModelDiagnostics
	if len(model.diagnostics) != 0 {
		return model
	}

	providerGraph := graph.Build(providerCatalog)
	if diagnostics := providerGraph.Diagnostics(); len(diagnostics) != 0 {
		model.diagnostics = graphDiagnostics(diagnostics)
		return model
	}

	model.controllers,
		model.transactions,
		model.caches,
		model.diagnostics = buildHTTPMetadata(
		program,
		resolution,
		providerCatalog,
		model.moduleModel,
	)
	if len(model.diagnostics) != 0 {
		return model
	}

	lifecycleCatalog := lifecycle.Build(program, resolution, providerCatalog)
	if diagnostics := lifecycleCatalog.Diagnostics(); len(diagnostics) != 0 {
		model.diagnostics = lifecycleDiagnostics(diagnostics)
		return model
	}
	scheduleCatalog := compilerschedule.Build(
		program,
		resolution,
		providerCatalog,
	)
	if diagnostics := scheduleCatalog.Diagnostics(); len(diagnostics) != 0 {
		model.diagnostics = scheduleDiagnostics(diagnostics)
		return model
	}
	asyncCatalog := compilerasync.Build(
		program,
		resolution,
		providerCatalog,
	)
	if diagnostics := asyncCatalog.Diagnostics(); len(diagnostics) != 0 {
		model.diagnostics = asyncDiagnostics(diagnostics)
		return model
	}
	if diagnostics := scopedComponentDiagnostics(
		providerCatalog.Providers(),
		model.controllers,
		lifecycleCatalog.Components(),
		scheduleCatalog.Jobs(),
		asyncCatalog.Tasks(),
		model.events,
	); len(diagnostics) != 0 {
		model.diagnostics = diagnostics
		return model
	}

	model.providers = providerGraph.ConstructionOrder()
	model.edges = providerGraph.Edges()
	model.components = orderedComponents(model.providers, lifecycleCatalog.Components())
	model.jobs = scheduleCatalog.Jobs()
	model.asyncTasks = asyncCatalog.Tasks()
	model.targets, model.diagnostics = applicationTargets(program, resolution, providerCatalog.Providers())
	if len(model.diagnostics) != 0 {
		return model
	}
	bootstrapDefinitions := compilerbootstrap.Builtins()
	bootstrapDefinitions = append(
		bootstrapDefinitions,
		options.BootstrapDefinitions...,
	)
	bootstrapResult := compilerbootstrap.Compile(
		resolution,
		bootstrapApplications(model.targets),
		bootstrapDefinitions,
	)
	if diagnostics := bootstrapResult.Diagnostics(); len(diagnostics) != 0 {
		model.diagnostics = bootstrapDiagnostics(diagnostics)
		return model
	}
	for index := range model.targets {
		model.targets[index].bootstrap = bootstrapResult.Metadata(model.targets[index].SymbolID)
	}
	model.diagnostics = bootstrapRequirementDiagnostics(
		model.targets,
		model.providers,
		model.edges,
	)
	return model
}

type scopedProviderUse struct {
	providerID       string
	stage            Stage
	role             string
	position         token.Position
	physicalPosition token.Position
	symbolID         string
}

func scopedComponentDiagnostics(
	providers []provider.Provider,
	controllers []controller.Controller,
	components []lifecycle.Component,
	jobs []compilerschedule.Job,
	tasks []compilerasync.Task,
	events []compilerevent.Topic,
) []Diagnostic {
	byID := make(map[string]provider.Provider, len(providers))
	for _, item := range providers {
		byID[item.SymbolID] = item
	}
	var uses []scopedProviderUse
	for _, item := range controllers {
		uses = append(uses, scopedProviderUse{
			providerID:       item.ProviderID,
			stage:            StageController,
			role:             "HTTP controller",
			position:         item.Position,
			physicalPosition: item.PhysicalPosition,
			symbolID:         item.SymbolID,
		})
	}
	for _, item := range components {
		hook := item.Start
		if hook == nil {
			hook = item.Stop
		}
		if hook == nil {
			continue
		}
		uses = append(uses, scopedProviderUse{
			providerID:       item.Provider.SymbolID,
			stage:            StageLifecycle,
			role:             "lifecycle component",
			position:         hook.Position,
			physicalPosition: hook.PhysicalPosition,
			symbolID:         hook.MethodID,
		})
	}
	for _, item := range jobs {
		uses = append(uses, scopedProviderUse{
			providerID:       item.ProviderID,
			stage:            StageSchedule,
			role:             "scheduled component",
			position:         item.Position,
			physicalPosition: item.PhysicalPosition,
			symbolID:         item.MethodID,
		})
	}
	for _, item := range tasks {
		uses = append(uses, scopedProviderUse{
			providerID:       item.ProviderID,
			stage:            StageAsync,
			role:             "asynchronous component",
			position:         item.Position,
			physicalPosition: item.PhysicalPosition,
			symbolID:         item.MethodID,
		})
	}
	for _, topic := range events {
		for _, listener := range topic.Listeners() {
			uses = append(uses, scopedProviderUse{
				providerID:       listener.ProviderID,
				stage:            StageEvent,
				role:             "event listener",
				position:         listener.Position,
				physicalPosition: listener.PhysicalPosition,
				symbolID:         listener.MethodID,
			})
		}
	}
	var diagnostics []Diagnostic
	for _, use := range uses {
		item, found := byID[use.providerID]
		if !found || item.Scope == sdk.BeanScopeSingleton {
			continue
		}
		diagnostics = append(diagnostics, Diagnostic{
			Stage:            use.stage,
			Position:         use.position,
			PhysicalPosition: use.physicalPosition,
			SymbolID:         use.symbolID,
			Kind:             "scoped-component",
			Message: fmt.Sprintf(
				"%s %q is owned by bean %q with %s scope; framework-owned components require singleton scope because no caller lease exists to own cleanup",
				use.role,
				use.symbolID,
				item.Name,
				item.Scope,
			),
		})
	}
	sortDiagnostics(diagnostics)
	return diagnostics
}

func buildProviderMetadata(
	program *load.Program,
	resolution resolve.Result,
	configurations []configuration.Type,
	modules modulith.Model,
	primary *provider.Catalog,
	extensions []provider.Catalog,
) (provider.Catalog, []compilerevent.Topic, []Diagnostic) {
	var providerCatalog provider.Catalog
	if primary == nil {
		providerCatalog = provider.Build(program, resolution)
	} else {
		providerCatalog = *primary
	}
	if diagnostics := providerCatalog.Diagnostics(); len(diagnostics) != 0 {
		return providerCatalog, nil, providerDiagnostics(diagnostics)
	}
	providerCatalog = provider.Add(
		providerCatalog,
		configurationProviders(configurations)...,
	)
	if diagnostics := providerCatalog.Diagnostics(); len(diagnostics) != 0 {
		return provider.Catalog{}, nil, providerDiagnostics(diagnostics)
	}
	catalogs := make([]provider.Catalog, 1, 1+len(extensions))
	catalogs[0] = providerCatalog
	catalogs = append(catalogs, extensions...)
	providerCatalog = provider.Merge(catalogs...)
	if diagnostics := providerCatalog.Diagnostics(); len(diagnostics) != 0 {
		return provider.Catalog{}, nil, providerDiagnostics(diagnostics)
	}
	eventCatalog := compilerevent.Build(
		program,
		resolution,
		providerCatalog,
		modules,
	)
	if diagnostics := eventCatalog.Diagnostics(); len(diagnostics) != 0 {
		return provider.Catalog{}, nil, eventDiagnostics(diagnostics)
	}
	providerCatalog = provider.Add(
		providerCatalog,
		eventCatalog.Providers()...,
	)
	if diagnostics := providerCatalog.Diagnostics(); len(diagnostics) != 0 {
		return provider.Catalog{}, nil, providerDiagnostics(diagnostics)
	}
	return providerCatalog, eventCatalog.Topics(), nil
}

func buildHTTPMetadata(
	program *load.Program,
	resolution resolve.Result,
	providers provider.Catalog,
	modules modulith.Model,
) (
	[]controller.Controller,
	[]compilertransaction.Boundary,
	[]compilercache.Boundary,
	[]Diagnostic,
) {
	controllerCatalog := controller.Build(
		program,
		resolution,
		providers,
		modules,
	)
	if diagnostics := controllerCatalog.Diagnostics(); len(diagnostics) != 0 {
		return nil, nil, nil, controllerDiagnostics(diagnostics)
	}
	controllers := controllerCatalog.Controllers()
	transactionCatalog := compilertransaction.Build(
		program,
		resolution,
		providers,
		controllers,
	)
	if diagnostics := transactionCatalog.Diagnostics(); len(diagnostics) != 0 {
		return nil, nil, nil, transactionDiagnostics(diagnostics)
	}
	cacheCatalog := compilercache.Build(
		program,
		resolution,
		controllers,
	)
	if diagnostics := cacheCatalog.Diagnostics(); len(diagnostics) != 0 {
		return nil, nil, nil, cacheDiagnostics(diagnostics)
	}
	return controllers,
		transactionCatalog.Boundaries(),
		cacheCatalog.Boundaries(),
		nil
}

func bootstrapApplications(targets []Target) []compilerbootstrap.Application {
	result := make([]compilerbootstrap.Application, len(targets))
	for index, target := range targets {
		result[index] = compilerbootstrap.Application{
			SymbolID: target.SymbolID,
			Name:     target.Name,
		}
	}
	return result
}

func bootstrapRequirementDiagnostics(
	targets []Target,
	providers []provider.Provider,
	edges []graph.Edge,
) []Diagnostic {
	var diagnostics []Diagnostic
	for _, target := range targets {
		available := targetRuntimeCapabilities(target, providers, edges)
		for _, feature := range target.bootstrap.Features() {
			for _, requirement := range feature.Requirements() {
				if slices.Contains(available, requirement) {
					continue
				}
				diagnostics = append(diagnostics, Diagnostic{
					Stage:            StageBootstrap,
					Position:         feature.Position,
					PhysicalPosition: feature.PhysicalPosition,
					SymbolID:         target.SymbolID,
					Kind:             "missing-capability",
					Message: fmt.Sprintf(
						"bootstrap annotation @%s on application %q requires selected application graph capability %q",
						feature.Annotation,
						target.Name,
						requirement,
					),
				})
			}
			diagnostics = append(
				diagnostics,
				bootstrapEntrypointRoleDiagnostics(
					target,
					feature,
					providers,
				)...,
			)
		}
	}
	sortDiagnostics(diagnostics)
	return diagnostics
}

func bootstrapEntrypointRoleDiagnostics(
	target Target,
	feature compilerbootstrap.Feature,
	providers []provider.Provider,
) []Diagnostic {
	if feature.Capability != compilerbootstrap.CapabilityHTTPObservation {
		return nil
	}
	var diagnostics []Diagnostic
	for _, entrypoint := range feature.EntryPoints() {
		matches := starterEntrypointProviders(feature, entrypoint, providers)
		switch {
		case len(matches) != 1:
			diagnostics = append(
				diagnostics,
				bootstrapFeatureDiagnostic(
					target,
					feature,
					"invalid-entrypoint",
					fmt.Sprintf(
						"bootstrap annotation @%s requires exactly one selected provider for HTTP observer entrypoint %s.%s, found %d",
						feature.Annotation,
						entrypoint.Package,
						entrypoint.Symbol,
						len(matches),
					),
				),
			)
		case !validHTTPObserverType(matches[0].Output):
			diagnostics = append(
				diagnostics,
				bootstrapFeatureDiagnostic(
					target,
					feature,
					"invalid-entrypoint-type",
					fmt.Sprintf(
						"bootstrap annotation @%s entrypoint %s.%s returns %s, which does not implement exact web.HTTPObserver",
						feature.Annotation,
						entrypoint.Package,
						entrypoint.Symbol,
						matches[0].OutputTypeID,
					),
				),
			)
		}
	}
	return diagnostics
}

func starterEntrypointProviders(
	feature compilerbootstrap.Feature,
	entrypoint compilerbootstrap.EntryPoint,
	providers []provider.Provider,
) []provider.Provider {
	var result []provider.Provider
	for _, item := range providers {
		if item.Source == provider.SourceStarter &&
			item.SourceID == feature.SourceID &&
			item.SourceVersion == feature.SourceVersion &&
			item.PackagePath == entrypoint.Package &&
			item.Name == entrypoint.Symbol {
			result = append(result, item)
		}
	}
	return result
}

func bootstrapFeatureDiagnostic(
	target Target,
	feature compilerbootstrap.Feature,
	kind string,
	message string,
) Diagnostic {
	return Diagnostic{
		Stage:            StageBootstrap,
		Position:         feature.Position,
		PhysicalPosition: feature.PhysicalPosition,
		SymbolID:         target.SymbolID,
		Kind:             kind,
		Message:          message,
	}
}

func validHTTPObserverType(value types.Type) bool {
	method := types.NewMethodSet(value).Lookup(nil, "BeginHTTP")
	if method == nil {
		return false
	}
	signature, ok := method.Obj().Type().(*types.Signature)
	if !ok || signature.Variadic() ||
		signature.Params().Len() != 2 ||
		signature.Results().Len() != 2 ||
		!exactNamedType(
			signature.Params().At(0).Type(),
			"context",
			"Context",
		) ||
		!exactNamedType(
			signature.Params().At(1).Type(),
			"github.com/spice-framework/spice/web",
			"RouteMetadata",
		) ||
		!exactNamedType(
			signature.Results().At(0).Type(),
			"context",
			"Context",
		) {
		return false
	}
	finish, ok := types.Unalias(
		signature.Results().At(1).Type(),
	).(*types.Signature)
	return ok &&
		!finish.Variadic() &&
		finish.Params().Len() == 1 &&
		finish.Results().Len() == 0 &&
		exactNamedType(
			finish.Params().At(0).Type(),
			"github.com/spice-framework/spice/web",
			"HTTPResult",
		)
}

func exactNamedType(value types.Type, packagePath, name string) bool {
	named, ok := types.Unalias(value).(*types.Named)
	return ok &&
		named.Obj() != nil &&
		named.Obj().Pkg() != nil &&
		named.Obj().Pkg().Path() == packagePath &&
		named.Obj().Name() == name
}

func targetRuntimeCapabilities(
	target Target,
	providers []provider.Provider,
	edges []graph.Edge,
) []compilerbootstrap.RuntimeCapability {
	reachable := reachableProviders(target, edges)
	for _, item := range providers {
		if !target.automatic && !reachable[item.SymbolID] {
			continue
		}
		if pointerNamedType(item.Output, "net/http", "ServeMux") {
			return []compilerbootstrap.RuntimeCapability{compilerbootstrap.RuntimeHTTPServeMux}
		}
	}
	return nil
}

func reachableProviders(target Target, edges []graph.Edge) map[string]bool {
	dependencies := make(map[string][]string)
	for _, edge := range edges {
		dependencies[edge.ConsumerID] = append(
			dependencies[edge.ConsumerID],
			edge.DependencyID,
		)
	}
	reachable := make(map[string]bool)
	queue := make([]string, 0, len(target.roots))
	for _, root := range target.roots {
		queue = append(queue, root.ProviderID)
	}
	for len(queue) != 0 {
		providerID := queue[0]
		queue = queue[1:]
		if reachable[providerID] {
			continue
		}
		reachable[providerID] = true
		queue = append(queue, dependencies[providerID]...)
	}
	return reachable
}

func pointerNamedType(value types.Type, packagePath, name string) bool {
	pointer, ok := types.Unalias(value).(*types.Pointer)
	if !ok {
		return false
	}
	named, ok := types.Unalias(pointer.Elem()).(*types.Named)
	return ok && named.Obj() != nil && named.Obj().Pkg() != nil &&
		named.Obj().Pkg().Path() == packagePath && named.Obj().Name() == name
}

func configurationProviders(configTypes []configuration.Type) []provider.Provider {
	result := make([]provider.Provider, len(configTypes))
	for index, configType := range configTypes {
		result[index] = provider.Provider{
			Source:           provider.SourceConfiguration,
			Symbol:           configType.Symbol,
			SymbolID:         configType.SymbolID,
			Name:             configType.Name,
			PackagePath:      configType.PackagePath,
			Position:         configType.Position,
			PhysicalPosition: configType.PhysicalPosition,
			Output:           configType.Type,
			OutputTypeID:     configType.TypeID,
		}
	}
	return result
}

func applicationTargets(
	program *load.Program,
	resolution resolve.Result,
	providers []provider.Provider,
) ([]Target, []Diagnostic) {
	symbols := make(map[string]load.Symbol)
	for _, symbol := range program.Symbols() {
		symbols[symbol.ID] = symbol
	}
	fileSets := packageFileSets(program)
	seen := make(map[string]resolve.Occurrence)
	var targets []Target
	var diagnostics []Diagnostic
	for _, occurrence := range resolution.Occurrences {
		if !occurrence.HasContribution(sdk.ContributionApplication) {
			continue
		}
		if previous, duplicate := seen[occurrence.SymbolID]; duplicate {
			diagnostics = append(diagnostics, occurrenceDiagnostic(
				occurrence,
				"duplicate-annotation",
				fmt.Sprintf(
					"@Application marker %q is declared more than once; first declaration is at %s",
					occurrence.Name,
					renderPosition(previous.DisplayPosition),
				),
			))
			continue
		}
		seen[occurrence.SymbolID] = occurrence

		symbol, ok := symbols[occurrence.SymbolID]
		if !ok {
			diagnostics = append(diagnostics, occurrenceDiagnostic(
				occurrence,
				"missing-symbol",
				fmt.Sprintf("@Application target %q has no stable typed symbol in the loaded program", occurrence.Name),
			))
			continue
		}
		target, markerDiagnostics := analyzeMarker(
			occurrence,
			symbol,
			fileSets[symbol.PackagePath],
			providers,
		)
		diagnostics = append(diagnostics, markerDiagnostics...)
		if len(markerDiagnostics) == 0 {
			targets = append(targets, target)
		}
	}
	sort.SliceStable(targets, func(i, j int) bool {
		return targets[i].SymbolID < targets[j].SymbolID
	})
	sortDiagnostics(diagnostics)
	return targets, diagnostics
}

func analyzeMarker(
	occurrence resolve.Occurrence,
	symbol load.Symbol,
	fileSet *token.FileSet,
	providers []provider.Provider,
) (Target, []Diagnostic) {
	signature, diagnostic := markerSignature(occurrence, symbol)
	if diagnostic != nil {
		return Target{}, []Diagnostic{*diagnostic}
	}
	roots, diagnostics := applicationRoots(occurrence, symbol, signature, fileSet, providers)
	if len(diagnostics) != 0 {
		return Target{}, diagnostics
	}
	return Target{
		Symbol:           symbol,
		SymbolID:         symbol.ID,
		Name:             applicationTargetName(symbol),
		PackagePath:      symbol.PackagePath,
		Position:         occurrence.DisplayPosition,
		PhysicalPosition: token.Position{Filename: occurrence.PhysicalFile, Offset: occurrence.PhysicalOffset},
		roots:            roots,
		automatic:        preferredMainMarker(symbol),
	}, nil
}

func markerSignature(occurrence resolve.Occurrence, symbol load.Symbol) (*types.Signature, *Diagnostic) {
	label := symbolLabel(symbol)
	if occurrence.Target != annotation.TargetFunction ||
		symbol.Kind != load.SymbolFunction ||
		symbol.Receiver != "" {
		diagnostic := symbolDiagnostic(
			occurrence,
			symbol,
			"invalid-target",
			fmt.Sprintf("@Application %s must target a package-level function", label),
		)
		return nil, &diagnostic
	}
	if len(occurrence.Annotation.Arguments) != 0 {
		diagnostic := symbolDiagnostic(
			occurrence,
			symbol,
			"arguments",
			fmt.Sprintf("@Application marker %s does not accept annotation arguments", label),
		)
		return nil, &diagnostic
	}
	signature := symbol.Signature
	if signature == nil {
		diagnostic := symbolDiagnostic(
			occurrence,
			symbol,
			"missing-signature",
			fmt.Sprintf("@Application marker %s has no typed function signature", label),
		)
		return nil, &diagnostic
	}
	if signature.TypeParams() != nil && signature.TypeParams().Len() != 0 {
		return nil, invalidSignatureDiagnostic(
			occurrence,
			symbol,
			"generic",
			"generic application markers are not supported",
		)
	}
	if signature.Variadic() {
		return nil, invalidSignatureDiagnostic(
			occurrence,
			symbol,
			"variadic",
			"application markers must be non-variadic",
		)
	}
	if signature.Results().Len() != 0 {
		return nil, invalidSignatureDiagnostic(
			occurrence,
			symbol,
			"results",
			fmt.Sprintf("application markers must return no results, got %d", signature.Results().Len()),
		)
	}
	if symbol.Name == "main" {
		if !preferredMainMarker(symbol) {
			return nil, invalidSignatureDiagnostic(
				occurrence,
				symbol,
				"main-package",
				"the preferred main marker must be declared in package main",
			)
		}
		if signature.Params().Len() != 0 {
			return nil, invalidSignatureDiagnostic(
				occurrence,
				symbol,
				"main-parameters",
				fmt.Sprintf(
					"package-main application markers must accept no parameters, got %d",
					signature.Params().Len(),
				),
			)
		}
	}
	return signature, nil
}

func preferredMainMarker(symbol load.Symbol) bool {
	return symbol.Name == "main" &&
		symbol.Object != nil &&
		symbol.Object.Pkg() != nil &&
		symbol.Object.Pkg().Name() == "main"
}

func applicationTargetName(symbol load.Symbol) string {
	if !preferredMainMarker(symbol) {
		return symbol.Name
	}
	name := path.Base(symbol.PackagePath)
	if name == "" || name == "." || name == "/" {
		return "application"
	}
	if name[0] >= 'a' && name[0] <= 'z' {
		name = string(name[0]-('a'-'A')) + name[1:]
	}
	return name
}

func applicationRoots(
	occurrence resolve.Occurrence,
	symbol load.Symbol,
	signature *types.Signature,
	fileSet *token.FileSet,
	providers []provider.Provider,
) ([]Root, []Diagnostic) {
	roots := make([]Root, 0, signature.Params().Len())
	var diagnostics []Diagnostic
	for index := 0; index < signature.Params().Len(); index++ {
		parameter := signature.Params().At(index)
		item := Root{
			Index:  index,
			Name:   parameter.Name(),
			Type:   parameter.Type(),
			TypeID: provider.TypeID(parameter.Type()),
		}
		if fileSet != nil && parameter.Pos().IsValid() {
			item.Position = fileSet.PositionFor(parameter.Pos(), true)
			item.PhysicalPosition = fileSet.PositionFor(parameter.Pos(), false)
		}
		match, problem := exactProvider(
			parameter.Type(),
			parameter.Name(),
			providers,
		)
		if problem != "" {
			diagnostics = append(diagnostics, rootDiagnostic(
				occurrence,
				symbol,
				item,
				fmt.Sprintf(
					"@Application marker %s root %s requires exact provider type %s, but %s",
					symbolLabel(symbol),
					parameterLabel(item),
					item.TypeID,
					problem,
				),
			))
			continue
		}
		item.ProviderID = match.SymbolID
		roots = append(roots, item)
	}
	return roots, diagnostics
}

func exactProvider(
	required types.Type,
	parameterName string,
	providers []provider.Provider,
) (provider.Provider, string) {
	candidates := exactProviderCandidates(required, providers)
	if len(candidates) == 0 {
		return provider.Provider{},
			"no @Bean provider produces that type"
	}
	candidates = selectRootCandidates(candidates, parameterName)
	if len(candidates) != 1 {
		return provider.Provider{}, ambiguousRootProviders(candidates)
	}
	if candidates[0].Scope != sdk.BeanScopeSingleton {
		return provider.Provider{},
			fmt.Sprintf(
				"selected bean %q has %s scope; application roots must be singleton",
				candidates[0].Name,
				candidates[0].Scope,
			)
	}
	return candidates[0], ""
}

func exactProviderCandidates(
	required types.Type,
	providers []provider.Provider,
) []provider.Provider {
	var candidates []provider.Provider
	for _, candidate := range providers {
		if types.Identical(required, candidate.Output) {
			candidates = append(candidates, candidate)
		}
	}
	return candidates
}

func selectRootCandidates(
	candidates []provider.Provider,
	parameterName string,
) []provider.Provider {
	var regular []provider.Provider
	for _, candidate := range candidates {
		if !candidate.Fallback {
			regular = append(regular, candidate)
		}
	}
	if len(regular) != 0 {
		candidates = regular
	}
	if len(candidates) > 1 {
		var primary []provider.Provider
		for _, candidate := range candidates {
			if candidate.Primary {
				primary = append(primary, candidate)
			}
		}
		if len(primary) != 0 {
			candidates = primary
		}
	}
	if len(candidates) > 1 && parameterName != "" {
		var named []provider.Provider
		for _, candidate := range candidates {
			if candidate.Name == parameterName ||
				slices.Contains(candidate.Aliases, parameterName) {
				named = append(named, candidate)
			}
		}
		if len(named) != 0 {
			candidates = named
		}
	}
	return candidates
}

func ambiguousRootProviders(candidates []provider.Provider) string {
	labels := make([]string, len(candidates))
	for index, candidate := range candidates {
		labels[index] = candidate.Name + " (" +
			candidate.SymbolID + ")"
	}
	sort.Strings(labels)
	return "multiple beans remain after fallback, primary, and parameter-name selection: " +
		strings.Join(labels, ", ")
}

func orderedComponents(
	providers []provider.Provider,
	components []lifecycle.Component,
) []lifecycle.Component {
	byProvider := make(map[string]lifecycle.Component, len(components))
	for _, component := range components {
		byProvider[component.Provider.SymbolID] = component
	}
	result := make([]lifecycle.Component, 0, len(components))
	for _, item := range providers {
		if component, ok := byProvider[item.SymbolID]; ok {
			result = append(result, cloneComponent(component))
		}
	}
	return result
}

func packageFileSets(program *load.Program) map[string]*token.FileSet {
	result := make(map[string]*token.FileSet)
	for _, pkg := range program.Packages() {
		if pkg.Raw != nil && pkg.Raw.Fset != nil {
			result[pkg.Path] = pkg.Raw.Fset
		}
	}
	return result
}

func invalidSignatureDiagnostic(
	occurrence resolve.Occurrence,
	symbol load.Symbol,
	kind string,
	reason string,
) *Diagnostic {
	accepted := acceptedLegacyMarkerSignature
	if symbol.Name == "main" {
		accepted = acceptedMainMarkerSignature
	}
	diagnostic := symbolDiagnostic(
		occurrence,
		symbol,
		kind,
		fmt.Sprintf(
			"@Application marker %s has an unsupported signature: %s; accepted form is %s",
			symbolLabel(symbol),
			reason,
			accepted,
		),
	)
	return &diagnostic
}

func rootDiagnostic(
	occurrence resolve.Occurrence,
	symbol load.Symbol,
	root Root,
	message string,
) Diagnostic {
	diagnostic := symbolDiagnostic(occurrence, symbol, "missing-root-provider", message)
	if root.Position.Filename != "" {
		diagnostic.Position = root.Position
	}
	if root.PhysicalPosition.Filename != "" {
		diagnostic.PhysicalPosition = root.PhysicalPosition
	}
	return diagnostic
}

func occurrenceDiagnostic(occurrence resolve.Occurrence, kind, message string) Diagnostic {
	physical := occurrence.PhysicalPosition
	if physical.Filename == "" {
		physical = token.Position{
			Filename: occurrence.PhysicalFile,
			Offset:   occurrence.PhysicalOffset,
		}
	}
	return Diagnostic{
		Stage:            StageApplication,
		Position:         occurrence.DisplayPosition,
		PhysicalPosition: physical,
		SymbolID:         occurrence.SymbolID,
		Kind:             kind,
		Message:          message,
	}
}

func symbolDiagnostic(
	occurrence resolve.Occurrence,
	symbol load.Symbol,
	kind string,
	message string,
) Diagnostic {
	diagnostic := occurrenceDiagnostic(occurrence, kind, message)
	if diagnostic.Position.Filename == "" {
		diagnostic.Position = symbol.Position
	}
	if diagnostic.PhysicalPosition.Filename == "" {
		diagnostic.PhysicalPosition = symbol.PhysicalPosition
	}
	return diagnostic
}

func resolutionDiagnostics(diagnostics []resolve.Diagnostic) []Diagnostic {
	result := make([]Diagnostic, len(diagnostics))
	for index, diagnostic := range diagnostics {
		result[index] = Diagnostic{
			Stage: StageResolution,
			Position: token.Position{
				Filename: diagnostic.Position.Filename,
				Offset:   diagnostic.Position.Offset,
				Line:     diagnostic.Position.Line,
				Column:   diagnostic.Position.Column,
			},
			PhysicalPosition: resolutionPhysicalPosition(diagnostic),
			Kind:             diagnostic.Kind,
			Message:          diagnostic.Message,
		}
	}
	sortDiagnostics(result)
	return result
}

func resolutionPhysicalPosition(
	diagnostic resolve.Diagnostic,
) token.Position {
	if diagnostic.PhysicalPosition.Filename != "" {
		return diagnostic.PhysicalPosition
	}
	return token.Position{
		Filename: diagnostic.PhysicalFile,
		Offset:   diagnostic.PhysicalOffset,
	}
}

func providerDiagnostics(diagnostics []provider.Diagnostic) []Diagnostic {
	result := make([]Diagnostic, len(diagnostics))
	for index, diagnostic := range diagnostics {
		result[index] = Diagnostic{
			Stage:            StageProvider,
			Position:         diagnostic.Position,
			PhysicalPosition: diagnostic.PhysicalPosition,
			SymbolID:         diagnostic.ProviderID,
			Kind:             diagnostic.Kind,
			Message:          diagnostic.Message,
			Fixes:            cloneProviderFixes(diagnostic.Fixes),
		}
	}
	sortDiagnostics(result)
	return result
}

func configurationDiagnostics(diagnostics []configuration.Diagnostic) []Diagnostic {
	result := make([]Diagnostic, len(diagnostics))
	for index, diagnostic := range diagnostics {
		result[index] = Diagnostic{
			Stage:            StageConfiguration,
			Position:         diagnostic.Position,
			PhysicalPosition: diagnostic.PhysicalPosition,
			SymbolID:         diagnostic.SymbolID,
			Kind:             diagnostic.Kind,
			Message:          diagnostic.Message,
		}
	}
	sortDiagnostics(result)
	return result
}

func controllerDiagnostics(diagnostics []controller.Diagnostic) []Diagnostic {
	result := make([]Diagnostic, len(diagnostics))
	for index, diagnostic := range diagnostics {
		result[index] = Diagnostic{
			Stage:            StageController,
			Position:         diagnostic.Position,
			PhysicalPosition: diagnostic.PhysicalPosition,
			SymbolID:         diagnostic.SymbolID,
			Kind:             diagnostic.Kind,
			Message:          diagnostic.Message,
		}
	}
	sortDiagnostics(result)
	return result
}

func graphDiagnostics(diagnostics []graph.Diagnostic) []Diagnostic {
	result := make([]Diagnostic, len(diagnostics))
	for index, diagnostic := range diagnostics {
		result[index] = Diagnostic{
			Stage:            StageGraph,
			Position:         diagnostic.Position,
			PhysicalPosition: diagnostic.PhysicalPosition,
			SymbolID:         diagnostic.ProviderID,
			Kind:             diagnostic.Kind,
			Message:          diagnostic.Message,
		}
	}
	sortDiagnostics(result)
	return result
}

func lifecycleDiagnostics(diagnostics []lifecycle.Diagnostic) []Diagnostic {
	result := make([]Diagnostic, len(diagnostics))
	for index, diagnostic := range diagnostics {
		result[index] = Diagnostic{
			Stage:            StageLifecycle,
			Position:         diagnostic.Position,
			PhysicalPosition: diagnostic.PhysicalPosition,
			SymbolID:         diagnostic.MethodID,
			Kind:             diagnostic.Kind,
			Message:          diagnostic.Message,
		}
	}
	sortDiagnostics(result)
	return result
}

func scheduleDiagnostics(
	diagnostics []compilerschedule.Diagnostic,
) []Diagnostic {
	result := make([]Diagnostic, len(diagnostics))
	for index, diagnostic := range diagnostics {
		result[index] = Diagnostic{
			Stage:            StageSchedule,
			Position:         diagnostic.Position,
			PhysicalPosition: diagnostic.PhysicalPosition,
			SymbolID:         diagnostic.MethodID,
			Kind:             diagnostic.Kind,
			Message:          diagnostic.Message,
		}
	}
	sortDiagnostics(result)
	return result
}

func asyncDiagnostics(
	diagnostics []compilerasync.Diagnostic,
) []Diagnostic {
	result := make([]Diagnostic, len(diagnostics))
	for index, diagnostic := range diagnostics {
		result[index] = Diagnostic{
			Stage:            StageAsync,
			Position:         diagnostic.Position,
			PhysicalPosition: diagnostic.PhysicalPosition,
			SymbolID:         diagnostic.MethodID,
			Kind:             diagnostic.Kind,
			Message:          diagnostic.Message,
		}
	}
	sortDiagnostics(result)
	return result
}

func transactionDiagnostics(
	diagnostics []compilertransaction.Diagnostic,
) []Diagnostic {
	result := make([]Diagnostic, len(diagnostics))
	for index, diagnostic := range diagnostics {
		result[index] = Diagnostic{
			Stage:            StageTransaction,
			Position:         diagnostic.Position,
			PhysicalPosition: diagnostic.PhysicalPosition,
			SymbolID:         diagnostic.RouteID,
			Kind:             diagnostic.Kind,
			Message:          diagnostic.Message,
		}
	}
	sortDiagnostics(result)
	return result
}

func cacheDiagnostics(
	diagnostics []compilercache.Diagnostic,
) []Diagnostic {
	result := make([]Diagnostic, len(diagnostics))
	for index, diagnostic := range diagnostics {
		result[index] = Diagnostic{
			Stage:            StageCache,
			Position:         diagnostic.Position,
			PhysicalPosition: diagnostic.PhysicalPosition,
			SymbolID:         diagnostic.RouteID,
			Kind:             diagnostic.Kind,
			Message:          diagnostic.Message,
		}
	}
	sortDiagnostics(result)
	return result
}

func eventDiagnostics(
	diagnostics []compilerevent.Diagnostic,
) []Diagnostic {
	result := make([]Diagnostic, len(diagnostics))
	for index, diagnostic := range diagnostics {
		result[index] = Diagnostic{
			Stage:            StageEvent,
			Position:         diagnostic.Position,
			PhysicalPosition: diagnostic.PhysicalPosition,
			SymbolID:         diagnostic.SymbolID,
			Kind:             diagnostic.Kind,
			Message:          diagnostic.Message,
		}
	}
	sortDiagnostics(result)
	return result
}

func moduleDiagnostics(diagnostics []modulith.Diagnostic) []Diagnostic {
	result := make([]Diagnostic, len(diagnostics))
	for index, diagnostic := range diagnostics {
		result[index] = Diagnostic{
			Stage:            StageModule,
			Position:         diagnostic.Position,
			PhysicalPosition: diagnostic.PhysicalPosition,
			SymbolID:         diagnostic.ModuleID,
			Kind:             diagnostic.Kind,
			Message:          diagnostic.Message,
		}
	}
	sortDiagnostics(result)
	return result
}

func bootstrapDiagnostics(diagnostics []compilerbootstrap.Diagnostic) []Diagnostic {
	result := make([]Diagnostic, len(diagnostics))
	for index, diagnostic := range diagnostics {
		result[index] = Diagnostic{
			Stage:            StageBootstrap,
			Position:         diagnostic.Position,
			PhysicalPosition: diagnostic.PhysicalPosition,
			SymbolID:         diagnostic.SymbolID,
			Kind:             diagnostic.Kind,
			Message:          diagnostic.Message,
		}
	}
	sortDiagnostics(result)
	return result
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
		if left.Stage != right.Stage {
			return left.Stage < right.Stage
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

func cloneProvider(item provider.Provider) provider.Provider {
	item.Dependencies = append([]provider.Dependency(nil), item.Dependencies...)
	item.Interfaces = append(
		[]provider.InterfaceBinding(nil),
		item.Interfaces...,
	)
	return item
}

func cloneComponent(component lifecycle.Component) lifecycle.Component {
	component.Provider = cloneProvider(component.Provider)
	if component.Start != nil {
		start := *component.Start
		component.Start = &start
	}
	if component.Stop != nil {
		stop := *component.Stop
		component.Stop = &stop
	}
	return component
}

func symbolLabel(symbol load.Symbol) string {
	if symbol.DisplayLabel != "" {
		return symbol.DisplayLabel
	}
	if symbol.ID != "" {
		return symbol.ID
	}
	return symbol.Name
}

func parameterLabel(root Root) string {
	if root.Name == "" {
		return fmt.Sprintf("parameter %d", root.Index)
	}
	return fmt.Sprintf("parameter %d %q", root.Index, root.Name)
}

func renderPosition(position token.Position) string {
	if position.Filename == "" {
		return "<unknown>:1:1"
	}
	line, column := position.Line, position.Column
	if line <= 0 {
		line = 1
	}
	if column <= 0 {
		column = 1
	}
	return fmt.Sprintf("%s:%d:%d", position.Filename, line, column)
}
