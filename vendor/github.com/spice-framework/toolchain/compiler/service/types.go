// @import { NamedInterface } from "github.com/spice-framework/spice/annotation/modulith"

// Package service exposes Spice's typed compiler pipeline as an isolated,
// overlay-aware analysis service for commands and editor integrations.
//
// @NamedInterface("service")
package service

import (
	"context"
	"errors"
	"slices"

	"github.com/spice-framework/spice/annotation"
	"github.com/spice-framework/spice/annotation/sdk"
	"github.com/spice-framework/toolchain/compiler/application"
	compilerbootstrap "github.com/spice-framework/toolchain/compiler/bootstrap"
	"github.com/spice-framework/toolchain/compiler/diagnostic"
	"github.com/spice-framework/toolchain/compiler/generate"
	"github.com/spice-framework/toolchain/compiler/load"
	"github.com/spice-framework/toolchain/compiler/modulith"
	"github.com/spice-framework/toolchain/compiler/provider"
	compilerstarter "github.com/spice-framework/toolchain/compiler/starter"
)

const (
	defaultCacheEntries = 8
	defaultOverlayFiles = 1_024
	defaultOverlayBytes = 16 << 20
)

// ErrStaleAnalysis reports that a newer sequenced request superseded a result.
var ErrStaleAnalysis = errors.New("spice analysis request is stale")

// AnalysisMode controls whether a successful analysis also selects and renders
// one application target. The zero value preserves generation behavior.
type AnalysisMode uint8

const (
	// AnalysisGenerate builds the complete application IR and a generation plan.
	AnalysisGenerate AnalysisMode = iota
	// AnalysisValidate builds and validates the complete application IR without
	// requiring an @Application target or rendering generated files.
	AnalysisValidate
)

// Loader is the cancellable typed-program loading boundary.
type Loader func(
	context.Context,
	load.Options,
	...string,
) (*load.Program, error)

// ModuleVersionLoader returns the selected Go module graph used to enforce
// exact reviewed starter dependencies.
type ModuleVersionLoader func(
	context.Context,
	load.Options,
) ([]compilerstarter.ModuleVersion, error)

// Config constructs one isolated service instance.
type Config struct {
	Loader               Loader
	ModuleVersions       ModuleVersionLoader
	LoadOptions          load.Options
	Registry             annotation.Registry
	StarterCatalog       compilerstarter.Catalog
	BootstrapDefinitions []compilerbootstrap.Definition
	ProviderEntrypoints  []provider.Entrypoint
	CacheNamespace       string
	MaxCacheEntries      int
	MaxOverlayFiles      int
	MaxOverlayBytes      int
	// SpiceVersion is sent during annotation-tool compatibility negotiation.
	SpiceVersion string
}

// Close gracefully terminates annotation-tool processes owned by the service.
func (service *Service) Close(ctx context.Context) error {
	if service == nil || service.config.annotationTools == nil {
		return nil
	}
	return service.config.annotationTools.Close(ctx)
}

// Document is one versioned in-memory source overlay.
type Document struct {
	Version int
	Content []byte
}

// Request describes one read-only compiler analysis.
type Request struct {
	WorkspaceRoot string
	Target        string
	Patterns      []string
	Overlay       map[string]Document
	Mode          AnalysisMode
	// ContentHash is a caller-owned hash of all relevant workspace and overlay
	// content. Caching is disabled when it is empty, preventing stale disk
	// results from being reused without a complete content identity.
	ContentHash string
	// Sequence enables per-workspace stale-result rejection. Zero disables
	// sequencing for one-shot command callers.
	Sequence uint64
}

// Annotation is one resolved declaration annotation summary.
type Annotation struct {
	Name              string
	Spelling          string
	Raw               string
	Target            annotation.Target
	Declaration       string
	SymbolID          string
	PackagePath       string
	DefinitionPackage string
	DefinitionSymbol  string
	Location          diagnostic.Location
}

// ProviderDependency is one exact provider input.
type ProviderDependency struct {
	Index         int
	Name          string
	TypeID        string
	Kind          provider.DependencyKind
	ElementTypeID string
	Qualifiers    []string
}

// Provider is one dependency-first provider summary.
type Provider struct {
	ID             string
	Name           string
	ExplicitName   bool
	PackagePath    string
	OutputTypeID   string
	Source         provider.Source
	Aliases        []string
	Qualifiers     []string
	Primary        bool
	Fallback       bool
	Order          int64
	Scope          sdk.BeanScope
	Dependencies   []ProviderDependency
	ReturnsCleanup bool
	ReturnsError   bool
}

// ProviderEdge is one exact-type provider dependency edge.
type ProviderEdge struct {
	ConsumerID      string
	DependencyID    string
	RequiredTypeID  string
	ParameterIndex  int
	ParameterName   string
	DependencyKind  provider.DependencyKind
	CollectionIndex int
}

// ProviderGraph is the immutable exact-type construction graph.
type ProviderGraph struct {
	Providers []Provider
	Edges     []ProviderEdge
}

// AutoConfiguration is one imported library default and its compiler-owned
// activation decision.
type AutoConfiguration struct {
	PackagePath     string                           `json:"package"`
	Factory         string                           `json:"factory"`
	OutputTypeID    string                           `json:"output"`
	Status          provider.AutoConfigurationStatus `json:"status"`
	Reason          string                           `json:"reason"`
	ModulePath      string                           `json:"module"`
	ModuleVersion   string                           `json:"version"`
	ReplacementPath string                           `json:"replacement,omitempty"`
	Review          string                           `json:"review"`
}

// NamedInterface is one explicitly exposed module descendant.
type NamedInterface struct {
	Name        string
	PackagePath string
}

// ModuleDependency is one explicitly allowed module API.
type ModuleDependency struct {
	ModuleID string
	API      string
}

// Module is one compile-time application-module summary.
type Module struct {
	ID                  string
	RootPackage         string
	Packages            []string
	NamedInterfaces     []NamedInterface
	AllowedDependencies []ModuleDependency
}

// ModuleEdge is one observed cross-module import edge.
type ModuleEdge struct {
	FromModule  string
	ToModule    string
	FromPackage string
	ToPackage   string
	API         string
	Exported    bool
	Allowed     bool
}

// ModuleGraph is the immutable module architecture summary.
type ModuleGraph struct {
	Modules            []Module
	Edges              []ModuleEdge
	UnassignedPackages []string
}

// ConfigurationField is one generated configuration property.
type ConfigurationField struct {
	Name        string
	Key         string
	TypeID      string
	Environment string
	Default     string
	HasDefault  bool
	Required    bool
	Secret      bool
}

// Configuration is one generated typed configuration declaration.
type Configuration struct {
	SymbolID    string
	Name        string
	PackagePath string
	TypeID      string
	Prefix      string
	Module      string
	Fields      []ConfigurationField
}

// AnnotationArgument describes one completion-safe annotation argument.
type AnnotationArgument struct {
	Name             string
	Kinds            []annotation.Kind
	ListElementKinds []annotation.Kind
	ValueDomain      sdk.ValueDomain
	AllowedStrings   []string
	Description      string
	Default          string
	Required         bool
	Positional       bool
	Variadic         bool
}

// GoInterfaceMethod is one method in the complete method set of a named
// runtime Go interface.
type GoInterfaceMethod struct {
	Name      string
	Signature string
}

// GoInterface is one compiler-resolved named runtime interface available to
// annotation arguments whose SDK value domain is ValueDomainGoInterface.
type GoInterface struct {
	Name           string
	PackageName    string
	PackagePath    string
	TypeID         string
	TypeParameters []string
	Methods        []GoInterfaceMethod
	Exported       bool
	Location       diagnostic.Location
	HasLocation    bool
}

// GoInterfacePackage groups interfaces by their exact Go package identity and
// records the physical files that establish same-package visibility.
type GoInterfacePackage struct {
	Name       string
	Path       string
	Files      []string
	Interfaces []GoInterface
}

// GoInterfaceCatalog is the immutable type-aware interface view produced by
// the same loaded Go program used for diagnostics, DI, and generation.
type GoInterfaceCatalog struct {
	Packages []GoInterfacePackage
}

// AnnotationExample is one descriptor-owned editor example.
type AnnotationExample struct {
	Title string
	Code  string
}

// AnnotationCompatibility records the declared lifecycle of one descriptor.
type AnnotationCompatibility struct {
	Since        string
	MinimumSpice string
}

// AnnotationProvenance is the exact Go-selected descriptor module identity.
type AnnotationProvenance struct {
	Module             string
	Version            string
	ReplacementModule  string
	ReplacementVersion string
	ReplacementDir     string
	LocalReplacement   bool
}

// AnnotationImplementation identifies the trusted Go tool handler and its
// inspectable implementation symbol.
type AnnotationImplementation struct {
	Tool               string
	Handler            string
	Protocol           string
	Authorized         bool
	AuthorizationKnown bool
	Package            string
	Symbol             string
	Location           diagnostic.Location
	HasLocation        bool
}

// AnnotationDefinition is one available built-in or selected extension.
type AnnotationDefinition struct {
	Name                  string
	Summary               string
	Documentation         string
	DescriptorPackage     string
	DescriptorSymbol      string
	DescriptorLocation    diagnostic.Location
	HasDescriptorLocation bool
	Targets               []annotation.Target
	Repeatable            bool
	Arguments             []AnnotationArgument
	Examples              []AnnotationExample
	Compatibility         AnnotationCompatibility
	Implementation        AnnotationImplementation
	Provenance            AnnotationProvenance
}

// Result is an immutable-by-construction compiler analysis result.
type Result struct {
	workspaceRoot  string
	sequence       uint64
	diagnostics    diagnostic.Set
	annotations    []Annotation
	providerGraph  ProviderGraph
	autoConfigs    []AutoConfiguration
	moduleGraph    ModuleGraph
	moduleModel    modulith.Model
	configurations []Configuration
	goInterfaces   GoInterfaceCatalog
	definitions    []AnnotationDefinition
	actions        []diagnostic.SuggestedFix
	application    application.Model
	plan           generate.Plan
	targetName     string
	files          int
	hasPlan        bool
}

// WorkspaceRoot returns the normalized absolute analysis root.
func (result Result) WorkspaceRoot() string {
	return result.workspaceRoot
}

// Sequence returns the caller-supplied request sequence.
func (result Result) Sequence() uint64 {
	return result.sequence
}

// Diagnostics returns the immutable shared diagnostic set.
func (result Result) Diagnostics() diagnostic.Set {
	return diagnostic.NewSet(result.diagnostics.Items()...)
}

// Annotations returns defensive resolved annotation summaries.
func (result Result) Annotations() []Annotation {
	return slices.Clone(result.annotations)
}

// Files returns the number of primary Go files scanned for annotations.
func (result Result) Files() int {
	return result.files
}

// ProviderGraph returns a deep defensive graph summary.
func (result Result) ProviderGraph() ProviderGraph {
	return cloneProviderGraph(result.providerGraph)
}

// AutoConfigurations returns imported library-default activation decisions.
func (result Result) AutoConfigurations() []AutoConfiguration {
	return slices.Clone(result.autoConfigs)
}

// ModuleGraph returns a deep defensive module summary.
func (result Result) ModuleGraph() ModuleGraph {
	return cloneModuleGraph(result.moduleGraph)
}

// ModuleModel returns the immutable application-module model used by CLI and
// editor projections. Its public accessors return defensive values.
func (result Result) ModuleModel() modulith.Model {
	return result.moduleModel
}

// Configurations returns deep defensive configuration metadata.
func (result Result) Configurations() []Configuration {
	return cloneConfigurations(result.configurations)
}

// GoInterfaces returns the compiler-owned named runtime interface catalog used
// by LSP completion and navigation. Editors do not independently decide which
// Go types are valid Spice interface bindings.
func (result Result) GoInterfaces() GoInterfaceCatalog {
	return cloneGoInterfaceCatalog(result.goInterfaces)
}

// AnnotationDefinitions returns completion-safe available definitions.
func (result Result) AnnotationDefinitions() []AnnotationDefinition {
	return cloneDefinitions(result.definitions)
}

// CodeActions returns version-aware safe fixes carried by diagnostics.
func (result Result) CodeActions() []diagnostic.SuggestedFix {
	return cloneActions(result.actions)
}

// ApplicationModel returns the immutable application IR assembled from the
// same typed program. Its public accessors return defensive metadata copies.
func (result Result) ApplicationModel() application.Model {
	return result.application
}

// GenerationReady reports whether a pure guarded generation plan is available.
func (result Result) GenerationReady() bool {
	return result.hasPlan
}

// TargetName returns the selected application's stable developer-facing name.
// It is empty when analysis did not reach target selection.
func (result Result) TargetName() string {
	return result.targetName
}

// GenerationPlan returns the immutable pure plan when analysis succeeded.
func (result Result) GenerationPlan() (generate.Plan, bool) {
	return result.plan, result.hasPlan
}
