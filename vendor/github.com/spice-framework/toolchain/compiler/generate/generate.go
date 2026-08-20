// @import { NamedInterface } from "github.com/spice-framework/spice/annotation/modulith"

// Package generate renders deterministic generated application plans from one
// validated Spice application model. It performs no filesystem writes.
//
// @NamedInterface("generate")
package generate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/spice-framework/spice/annotation/sdk"
	"github.com/spice-framework/toolchain/compiler/application"
	compilerasync "github.com/spice-framework/toolchain/compiler/async"
	compilercache "github.com/spice-framework/toolchain/compiler/cache"
	"github.com/spice-framework/toolchain/compiler/configuration"
	"github.com/spice-framework/toolchain/compiler/controller"
	compilerevent "github.com/spice-framework/toolchain/compiler/event"
	compilerlifecycle "github.com/spice-framework/toolchain/compiler/lifecycle"
	"github.com/spice-framework/toolchain/compiler/load"
	compilerpolicy "github.com/spice-framework/toolchain/compiler/policy"
	"github.com/spice-framework/toolchain/compiler/provider"
	compilerschedule "github.com/spice-framework/toolchain/compiler/schedule"
	"github.com/spice-framework/toolchain/compiler/targetid"
	compilertransaction "github.com/spice-framework/toolchain/compiler/transaction"
)

const (
	// SchemaVersion is the current generated ownership manifest schema.
	SchemaVersion = 6
	// GeneratorVersion is recorded in manifests to make generator compatibility
	// explicit during freshness checks.
	GeneratorVersion = "v0.1.0-preview.2"
	// GoFormatLine is the supported Go formatter compatibility line.
	GoFormatLine = "1.26"
	// AnalysisBuildTag excludes committed generated source while Spice analyzes
	// source, allowing stale output to be regenerated safely.
	AnalysisBuildTag = "spice_generate"

	contractsFilename     = "spice_contracts_gen.go"
	configurationFilename = "spice_configuration_gen.go"
	providersFilename     = "spice_providers_gen.go"
	assemblyFilename      = "spice_assembly_gen.go"
	featuresFilename      = "spice_features_gen.go"
	httpFilename          = "spice_http_gen.go"
	lifecycleFilename     = "spice_lifecycle_gen.go"
	commandFilename       = "spice_command_gen.go"
	// bridgeFilename is retained for ownership migration tests. New plans never
	// emit a same-package bridge.
	bridgeFilename    = "zz_spice_bridge_gen.go"
	asyncPath         = "github.com/spice-framework/spice/async"
	beanPath          = "github.com/spice-framework/spice/bean"
	configPath        = "github.com/spice-framework/spice/config"
	cachePath         = "github.com/spice-framework/spice/cache"
	dataPath          = "github.com/spice-framework/spice/data"
	eventPath         = "github.com/spice-framework/spice/event"
	interceptPath     = "github.com/spice-framework/spice/intercept"
	lifecyclePath     = "github.com/spice-framework/spice/lifecycle"
	managementPath    = "github.com/spice-framework/spice/management"
	observabilityPath = "github.com/spice-framework/spice/observability"
	retryPath         = "github.com/spice-framework/spice/retry"
	schedulePath      = "github.com/spice-framework/spice/schedule"
	securityPath      = "github.com/spice-framework/spice/security"
	viewPath          = "github.com/spice-framework/spice/view"
	webPath           = "github.com/spice-framework/spice/web"

	shutdownConfigurationKey = "spice.shutdown-timeout"
	asyncConcurrencyKey      = "spice.async.max-concurrency"
)

var targetIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Layout identifies where generated Go is compiled.
type Layout string

const (
	// LayoutGeneratedPackage is the legacy importable
	// internal/spicegen/<target> package layout.
	LayoutGeneratedPackage Layout = "generated-package"
	// LayoutApplicationPackage emits the complete generated application in an
	// importable target-scoped package. The physical package-main source imports
	// that package explicitly; generation never writes beside handwritten code.
	LayoutApplicationPackage Layout = "application-package"
)

// Target identifies one module-scoped generated application output.
type Target struct {
	ID                    string
	Layout                Layout
	ModulePath            string
	ModuleRoot            string
	PackagePath           string
	EntrypointPackagePath string
	OutputDir             string
	BridgeDir             string
	ManifestPath          string
}

// TargetSummary is the safe, serializable subset of a generation target.
type TargetSummary struct {
	ID                    string `json:"id"`
	Layout                Layout `json:"layout"`
	ModulePath            string `json:"module"`
	PackagePath           string `json:"package"`
	EntrypointPackagePath string `json:"entrypoint_package,omitempty"`
	OutputDir             string `json:"output_dir"`
	BridgeDir             string `json:"bridge_dir,omitempty"`
	ManifestPath          string `json:"manifest_path"`
}

// FileRole identifies the purpose of one generated artifact.
type FileRole string

const (
	// FileRoleTargetOrchestrator is target-wide application coordination that
	// cannot truthfully belong to one source file. It is retained for guarded
	// migration from schema-4 monolithic output; new plans do not emit it.
	FileRoleTargetOrchestrator FileRole = "target-orchestrator"
	// FileRoleTargetContracts contains the stable public generated types.
	FileRoleTargetContracts FileRole = "target-contracts"
	// FileRoleTargetConfiguration contains generated configuration metadata.
	FileRoleTargetConfiguration FileRole = "target-configuration"
	// FileRoleTargetProviders contains dependency-ordered graph construction.
	FileRoleTargetProviders FileRole = "target-providers"
	// FileRoleTargetAssembly coordinates bounded generated construction phases.
	FileRoleTargetAssembly FileRole = "target-assembly"
	// FileRoleTargetFeatures contains lifecycle, schedule, and async wiring.
	FileRoleTargetFeatures FileRole = "target-features"
	// FileRoleTargetHTTP contains generated HTTP and management wiring.
	FileRoleTargetHTTP FileRole = "target-http"
	// FileRoleTargetHTTPRoute contains one source-linked generated route.
	FileRoleTargetHTTPRoute FileRole = "target-http-route"
	// FileRoleTargetLifecycle contains the public generated runtime methods.
	FileRoleTargetLifecycle FileRole = "target-lifecycle"
	// FileRoleTargetCommand contains the process-level generated command API.
	FileRoleTargetCommand FileRole = "target-command"
	// FileRoleCommandBridge is retained only to migrate previously owned
	// same-package bridges. New plans never emit this role.
	FileRoleCommandBridge FileRole = "command-bridge"
	// FileRoleSourceUnit is annotation-derived code owned by exactly one
	// handwritten source file in the mirrored generated tree.
	FileRoleSourceUnit FileRole = "source-unit"
	// FileRoleArtifact is a non-Go generated contract such as OpenAPI.
	FileRoleArtifact FileRole = "artifact"

	// FileRoleApplication identifies the generated construction entrypoint.
	FileRoleApplication = FileRoleTargetAssembly
	// FileRoleSourceShard is retained as a source-compatible alias while
	// callers migrate to the source-unit name.
	FileRoleSourceShard = FileRoleSourceUnit
)

// SourceOrigin identifies handwritten source that owns generated behavior.
// Paths are module-relative and never absolute.
type SourceOrigin struct {
	Path   string `json:"path"`
	Line   int    `json:"line,omitempty"`
	Column int    `json:"column,omitempty"`
	Symbol string `json:"symbol,omitempty"`
}

// GeneratedRange identifies a half-open line/column range in generated output.
// Generated paths and coordinates remain physical so debuggers never pretend
// generated execution occurred in handwritten source.
type GeneratedRange struct {
	StartLine   int `json:"start_line"`
	StartColumn int `json:"start_column"`
	EndLine     int `json:"end_line"`
	EndColumn   int `json:"end_column"`
}

// SourceMapping connects one generated declaration or statement group to the
// handwritten contribution that caused it.
type SourceMapping struct {
	Kind          string         `json:"kind"`
	Contribution  string         `json:"contribution"`
	Source        SourceOrigin   `json:"source"`
	Generated     GeneratedRange `json:"generated"`
	RelatedSource []SourceOrigin `json:"related_sources,omitempty"`
}

// ManifestFile records one generated file owned by a target.
type ManifestFile struct {
	Path           string          `json:"path"`
	SHA256         string          `json:"sha256"`
	Role           FileRole        `json:"role"`
	PrimarySource  *SourceOrigin   `json:"primary_source,omitempty"`
	RelatedSources []SourceOrigin  `json:"related_sources,omitempty"`
	Mappings       []SourceMapping `json:"mappings,omitempty"`
}

// Manifest is the deterministic generated-file ownership record.
type Manifest struct {
	Schema           int            `json:"schema"`
	Target           TargetSummary  `json:"target"`
	GeneratorVersion string         `json:"generator_version"`
	GoFormatLine     string         `json:"go_format_line"`
	InputSHA256      string         `json:"input_sha256"`
	Files            []ManifestFile `json:"files"`
}

// File is one generated file in a render plan.
type File struct {
	Path           string
	Mode           fs.FileMode
	SHA256         string
	Role           FileRole
	PrimarySource  *SourceOrigin
	RelatedSources []SourceOrigin
	Mappings       []SourceMapping
	Sources        []SourceOrigin
	content        []byte
}

// Content returns a defensive copy of the generated bytes.
func (f File) Content() []byte {
	return append([]byte(nil), f.content...)
}

// Plan is a deterministic in-memory generation result.
type Plan struct {
	target          Target
	files           []File
	manifest        Manifest
	manifestContent []byte
}

// Target returns the plan's target.
func (p Plan) Target() Target {
	return p.target
}

// Files returns defensive copies sorted by module-relative path.
func (p Plan) Files() []File {
	result := make([]File, len(p.files))
	copy(result, p.files)
	for index := range result {
		result[index].content = append([]byte(nil), p.files[index].content...)
		result[index].PrimarySource = cloneSourceOrigin(
			p.files[index].PrimarySource,
		)
		result[index].RelatedSources = append(
			[]SourceOrigin(nil),
			p.files[index].RelatedSources...,
		)
		result[index].Mappings = cloneSourceMappings(
			p.files[index].Mappings,
		)
		result[index].Sources = append(
			[]SourceOrigin(nil),
			p.files[index].Sources...,
		)
	}
	return result
}

// Manifest returns a defensive copy of the ownership manifest.
func (p Plan) Manifest() Manifest {
	result := p.manifest
	result.Files = make([]ManifestFile, len(p.manifest.Files))
	for index, file := range p.manifest.Files {
		result.Files[index] = file
		result.Files[index].PrimarySource = cloneSourceOrigin(
			file.PrimarySource,
		)
		result.Files[index].RelatedSources = append(
			[]SourceOrigin(nil),
			file.RelatedSources...,
		)
		result.Files[index].Mappings = cloneSourceMappings(file.Mappings)
	}
	return result
}

func cloneSourceOrigin(origin *SourceOrigin) *SourceOrigin {
	if origin == nil {
		return nil
	}
	result := *origin
	return &result
}

func cloneSourceMappings(mappings []SourceMapping) []SourceMapping {
	if len(mappings) == 0 {
		return nil
	}
	result := make([]SourceMapping, len(mappings))
	copy(result, mappings)
	for index := range result {
		result[index].RelatedSource = append(
			[]SourceOrigin(nil),
			mappings[index].RelatedSource...,
		)
	}
	return result
}

// ManifestContent returns the exact canonical manifest bytes.
func (p Plan) ManifestContent() []byte {
	return append([]byte(nil), p.manifestContent...)
}

// Diagnostic is one deterministic generation failure.
type Diagnostic struct {
	Position         token.Position
	PhysicalPosition token.Position
	TargetID         string
	Kind             string
	Message          string
}

// Error renders a compiler-style diagnostic.
func (d Diagnostic) Error() string {
	position := d.Position
	if position.Filename == "" {
		position.Filename = "<generation>"
	}
	if position.Line <= 0 {
		position.Line = 1
	}
	if position.Column <= 0 {
		position.Column = 1
	}
	return fmt.Sprintf("%s:%d:%d: %s", position.Filename, position.Line, position.Column, d.Message)
}

// DefaultTarget derives the standard module-scoped output for one application
// marker. All generated output lives below internal/spicegen; BridgeDir is
// retained as schema compatibility metadata for the handwritten entrypoint
// package. Every target uses .spice/<target>.manifest.json.
func DefaultTarget(program *load.Program, applicationTarget application.Target) (Target, []Diagnostic) {
	if program == nil {
		return Target{}, []Diagnostic{{
			Kind:    "invalid-program",
			Message: "derive generation target: loaded program is nil",
		}}
	}
	pkg, ok := packageByPath(program.Packages(), applicationTarget.PackagePath)
	if !ok || pkg.Raw == nil || pkg.Raw.Module == nil {
		return Target{}, []Diagnostic{targetDiagnostic(
			applicationTarget,
			"module",
			fmt.Sprintf(
				"@Application target %s is not owned by a Go module; select a module-backed package",
				applicationTarget.SymbolID,
			),
		)}
	}
	id := defaultTargetID(applicationTarget.Name)
	layout := LayoutGeneratedPackage
	outputDir := path.Join("internal", "spicegen", id)
	packagePath := path.Join(pkg.Raw.Module.Path, outputDir)
	bridgeDir := ""
	entrypointPackagePath := ""
	if applicationTarget.AutomaticDiscovery() {
		layout = LayoutApplicationPackage
		relative, err := filepath.Rel(pkg.Raw.Module.Dir, pkg.Dir)
		if err != nil ||
			(!filepath.IsLocal(relative) && relative != ".") {
			return Target{}, []Diagnostic{targetDiagnostic(
				applicationTarget,
				"package-directory",
				fmt.Sprintf(
					"derive package-main output for %s inside module %s",
					applicationTarget.PackagePath,
					pkg.Raw.Module.Path,
				),
			)}
		}
		bridgeDir = filepath.ToSlash(relative)
		entrypointPackagePath = applicationTarget.PackagePath
	}
	target := Target{
		ID:                    id,
		Layout:                layout,
		ModulePath:            pkg.Raw.Module.Path,
		ModuleRoot:            pkg.Raw.Module.Dir,
		PackagePath:           packagePath,
		EntrypointPackagePath: entrypointPackagePath,
		OutputDir:             outputDir,
		BridgeDir:             bridgeDir,
		ManifestPath:          path.Join(".spice", id+".manifest.json"),
	}
	if diagnostics := validateTarget(target, applicationTarget); len(diagnostics) != 0 {
		return Target{}, diagnostics
	}
	return target, nil
}

func defaultTargetID(name string) string {
	return targetid.Default(name)
}

// Render creates canonical generated Go and ownership manifest bytes entirely
// in memory. It consumes the application model's existing order and edges and
// never reloads packages, rebuilds dependency resolution, or executes source.
func Render(
	program *load.Program,
	model application.Model,
	applicationTarget application.Target,
	target Target,
) (Plan, []Diagnostic) {
	if program == nil {
		return Plan{}, []Diagnostic{{
			Kind:    "invalid-program",
			Message: "render application: loaded program is nil",
		}}
	}
	if diagnostics := model.Diagnostics(); len(diagnostics) != 0 {
		return Plan{}, []Diagnostic{targetDiagnostic(
			applicationTarget,
			"invalid-model",
			fmt.Sprintf("render application: model has %d diagnostic(s)", len(diagnostics)),
		)}
	}
	if diagnostics := validateTarget(target, applicationTarget); len(diagnostics) != 0 {
		return Plan{}, diagnostics
	}
	if !containsTarget(model.Targets(), applicationTarget.SymbolID) {
		return Plan{}, []Diagnostic{targetDiagnostic(
			applicationTarget,
			"unknown-target",
			fmt.Sprintf("application target %s is not present in the supplied model", applicationTarget.SymbolID),
		)}
	}
	if diagnostics := validateRenderable(program, model, applicationTarget, target); len(diagnostics) != 0 {
		return Plan{}, diagnostics
	}

	modelOrigins := modelSourceOrigins(
		program,
		model,
		applicationTarget,
		target,
	)
	targetFiles, renderErr := renderTargetFiles(
		model,
		applicationTarget,
		target,
		modelOrigins,
	)
	if renderErr != nil {
		return Plan{}, []Diagnostic{targetDiagnostic(
			applicationTarget,
			"render",
			fmt.Sprintf("render generated Go: %v", renderErr),
		)}
	}
	files := append([]File(nil), targetFiles...)
	sourceUnits, sourceUnitErr := renderSourceUnits(
		program,
		applicationTarget,
		model.Providers(),
		model.Configurations(),
		target,
	)
	if sourceUnitErr != nil {
		return Plan{}, []Diagnostic{targetDiagnostic(
			applicationTarget,
			"render-source-units",
			fmt.Sprintf("render generated source units: %v", sourceUnitErr),
		)}
	}
	files = append(files, sourceUnits...)
	if len(model.Controllers()) != 0 {
		openAPIContent, openAPIErr := renderOpenAPI(model, applicationTarget)
		if openAPIErr != nil {
			return Plan{}, []Diagnostic{targetDiagnostic(
				applicationTarget,
				"openapi",
				fmt.Sprintf("render OpenAPI document: %v", openAPIErr),
			)}
		}
		openAPIOrigins := controllerSourceOrigins(
			modelOrigins,
			model.Controllers(),
		)
		files = append(files, File{
			Path:           path.Join(target.OutputDir, "artifacts", openAPIFilename),
			Mode:           0o644,
			SHA256:         contentHash(openAPIContent),
			Role:           FileRoleArtifact,
			RelatedSources: openAPIOrigins,
			Sources:        openAPIOrigins,
			content:        openAPIContent,
		})
	}
	sort.Slice(files, func(left, right int) bool { return files[left].Path < files[right].Path })
	inputHash, hashErr := modelHash(model, applicationTarget, target)
	if hashErr != nil {
		return Plan{}, []Diagnostic{targetDiagnostic(
			applicationTarget,
			"input-hash",
			fmt.Sprintf("hash application model: %v", hashErr),
		)}
	}
	manifest := Manifest{
		Schema:           SchemaVersion,
		Target:           summarizeTarget(target),
		GeneratorVersion: GeneratorVersion,
		GoFormatLine:     GoFormatLine,
		InputSHA256:      inputHash,
	}
	for _, file := range files {
		manifest.Files = append(manifest.Files, ManifestFile{
			Path:           file.Path,
			SHA256:         file.SHA256,
			Role:           file.Role,
			PrimarySource:  cloneSourceOrigin(file.PrimarySource),
			RelatedSources: append([]SourceOrigin(nil), file.RelatedSources...),
			Mappings:       cloneSourceMappings(file.Mappings),
		})
	}
	manifestContent, manifestErr := json.MarshalIndent(manifest, "", "  ")
	if manifestErr != nil {
		return Plan{}, []Diagnostic{targetDiagnostic(
			applicationTarget,
			"manifest",
			fmt.Sprintf("encode generation manifest: %v", manifestErr),
		)}
	}
	manifestContent = append(manifestContent, '\n')
	return Plan{
		target:          target,
		files:           files,
		manifest:        manifest,
		manifestContent: manifestContent,
	}, nil
}

func renderTargetFiles(
	model application.Model,
	applicationTarget application.Target,
	target Target,
	modelOrigins []SourceOrigin,
) ([]File, error) {
	providers := model.Providers()
	configTypes := model.Configurations()
	controllers := model.Controllers()
	jobs := model.Jobs()
	asyncTasks := model.AsyncTasks()
	policies := model.Policies()
	events := model.Events()
	transactions := model.Transactions()
	caches := model.Caches()
	features := commandFeaturesFor(applicationTarget, len(controllers) != 0)
	features.authorization = hasAuthorization(controllers)
	features.scheduling = len(jobs) != 0
	features.asynchronous = len(asyncTasks) != 0
	features.transactions = len(transactions) != 0
	features.events = len(events) != 0
	features.caching = len(caches) != 0
	features.methodPolicies = len(policies) != 0
	features.authorization = features.authorization || policiesUseAuthorization(policies)
	features.transactions = features.transactions || policiesUseTransactions(policies)
	features.caching = features.caching || policiesUseCache(policies)
	features.retry = policiesUseRetry(policies)
	features.methodObservation = policiesUseObservation(policies)
	features.requestScope = hasProviderScope(
		providers,
		sdk.BeanScopeRequest,
	)
	aliases := importAliasesWithPolicies(
		providers,
		controllers,
		asyncTasks,
		events,
		caches,
		policies,
		features,
	)
	providerAdapters, adapterErr := providerSourceAdapters(
		providers,
		target,
		aliases,
	)
	if adapterErr != nil {
		return nil, adapterErr
	}
	applicationAdapter, adapterErr := buildApplicationSourceAdapter(
		applicationTarget,
		target,
		aliases,
	)
	if adapterErr != nil {
		return nil, adapterErr
	}
	providerModules := providerModuleIDs(model, providers)
	componentFields := generatedComponentFields(providers, policies)
	routeInterceptorFields := generatedRouteInterceptorFields(controllers)
	if hasOverridableProviders(componentFields) {
		aliases[beanPath] = "spicebean"
		aliases["strings"] = "strings"
	}
	if len(routeInterceptorFields) != 0 {
		aliases[interceptPath] = "spiceintercept"
	}
	applicationOrigins := sourceOriginsForSymbolFamilies(
		modelOrigins,
		applicationTarget.SymbolID,
	)
	providerOrigins := providerSourceOrigins(
		modelOrigins,
		providers,
		events,
	)
	for _, service := range policies {
		for _, method := range service.Methods() {
			providerOrigins = mergeSourceOrigins(
				providerOrigins,
				sourceOriginsForSymbolFamilies(modelOrigins, method.MethodID),
			)
		}
	}
	configurationOrigins := configurationSourceOrigins(
		modelOrigins,
		configTypes,
	)
	featureOrigins := featureSourceOrigins(
		modelOrigins,
		model.Components(),
		jobs,
		asyncTasks,
	)
	controllerOrigins := controllerSourceOrigins(
		modelOrigins,
		controllers,
	)

	contracts := renderContractsTargetSource(
		applicationAdapter,
		componentFields,
		aliases,
		features,
		asyncTasks,
		routeInterceptorFields,
	)

	var configurationSource bytes.Buffer
	writeConfigurationAPI(
		&configurationSource,
		configTypes,
		append(caches, serviceCacheBoundaries(policies)...),
		features.asynchronous,
	)

	localProviderVariables, dependencyProviderVariables := targetProviderVariables(providers)
	localExposedVariables, dependencyExposedVariables := policyExposureVariables(
		providers,
		policies,
		localProviderVariables,
		dependencyProviderVariables,
	)
	dependencies, err := dependencyVariablesWithExposure(
		model,
		providers,
		aliases,
		localExposedVariables,
	)
	if err != nil {
		return nil, err
	}
	providerSource, providerErr := renderProvidersTargetSource(
		providers,
		componentFields,
		configTypes,
		aliases,
		dependencies,
		providerModules,
		localProviderVariables,
		localExposedVariables,
		events,
		policies,
		features,
		providerAdapters,
	)
	if providerErr != nil {
		return nil, providerErr
	}

	hasLifecycleFeatures := len(model.Components()) != 0 || len(jobs) != 0
	assembly := renderAssemblyTargetSource(
		target,
		features,
		componentFields,
		dependencyExposedVariables,
		hasLifecycleFeatures,
	)

	featureSource := renderFeaturesTargetSource(
		model,
		applicationTarget,
		features,
		asyncTasks,
		jobs,
		dependencyProviderVariables,
		providerModules,
		aliases,
	)

	httpSource, routeSpecifications, httpErr := renderHTTPTargetSources(
		model,
		applicationTarget,
		features,
		providers,
		controllers,
		transactions,
		caches,
		dependencyProviderVariables,
		aliases,
		modelOrigins,
		routeInterceptorFields,
	)
	if httpErr != nil {
		return nil, httpErr
	}

	var lifecycleSource bytes.Buffer
	writeLifecycleMethods(&lifecycleSource)
	writeComponentsMethod(&lifecycleSource)
	writeAsyncApplicationMethods(&lifecycleSource, asyncTasks, aliases)
	if features.hasMux {
		writeHandlerMethod(&lifecycleSource)
	}

	var commandSource bytes.Buffer
	writeCommandAPI(&commandSource, features)

	specifications := targetFileSpecifications(
		targetSourceBodies{
			contracts:     contracts,
			configuration: configurationSource.Bytes(),
			providers:     providerSource,
			assembly:      assembly,
			features:      featureSource,
			http:          httpSource,
			lifecycle:     lifecycleSource.Bytes(),
			command:       commandSource.Bytes(),
		},
		targetSourceOrigins{
			application:   applicationOrigins,
			providers:     providerOrigins,
			configuration: configurationOrigins,
			features:      featureOrigins,
			controllers:   controllerOrigins,
		},
		routeSpecifications,
	)

	return materializeTargetFiles(
		specifications,
		aliases,
		target,
		applicationTarget,
	)
}

type targetSourceBodies struct {
	contracts     []byte
	configuration []byte
	providers     []byte
	assembly      []byte
	features      []byte
	http          []byte
	lifecycle     []byte
	command       []byte
}

type targetSourceOrigins struct {
	application   []SourceOrigin
	providers     []SourceOrigin
	configuration []SourceOrigin
	features      []SourceOrigin
	controllers   []SourceOrigin
}

func targetFileSpecifications(
	bodies targetSourceBodies,
	origins targetSourceOrigins,
	routes []targetFileSpecification,
) []targetFileSpecification {
	specifications := []targetFileSpecification{
		{
			filename: contractsFilename,
			role:     FileRoleTargetContracts,
			body:     bodies.contracts,
			relatedSources: mergeSourceOrigins(
				origins.application,
				origins.providers,
				origins.features,
			),
		},
		{
			filename: configurationFilename,
			role:     FileRoleTargetConfiguration,
			body:     bodies.configuration,
			relatedSources: mergeSourceOrigins(
				origins.application,
				origins.configuration,
			),
		},
		{
			filename: providersFilename,
			role:     FileRoleTargetProviders,
			body:     bodies.providers,
			relatedSources: mergeSourceOrigins(
				origins.application,
				origins.providers,
			),
		},
		{
			filename: assemblyFilename,
			role:     FileRoleTargetAssembly,
			body:     bodies.assembly,
			relatedSources: mergeSourceOrigins(
				origins.application,
				origins.providers,
				origins.features,
				origins.controllers,
			),
		},
	}
	if len(bodies.features) != 0 {
		specifications = append(specifications, targetFileSpecification{
			filename: featuresFilename,
			role:     FileRoleTargetFeatures,
			body:     bodies.features,
			relatedSources: mergeSourceOrigins(
				origins.application,
				origins.features,
			),
		})
	}
	if len(bodies.http) != 0 {
		specifications = append(specifications, targetFileSpecification{
			filename: httpFilename,
			role:     FileRoleTargetHTTP,
			body:     bodies.http,
			relatedSources: mergeSourceOrigins(
				origins.application,
				origins.controllers,
			),
		})
		specifications = append(specifications, routes...)
	}
	return append(
		specifications,
		targetFileSpecification{
			filename: lifecycleFilename,
			role:     FileRoleTargetLifecycle,
			body:     bodies.lifecycle,
			relatedSources: mergeSourceOrigins(
				origins.application,
				origins.features,
			),
		},
		targetFileSpecification{
			filename:       commandFilename,
			role:           FileRoleTargetCommand,
			body:           bodies.command,
			relatedSources: origins.application,
		},
	)
}

func targetProviderVariables(
	providers []provider.Provider,
) (map[string]string, map[string]string) {
	local := make(map[string]string, len(providers))
	dependencies := make(map[string]string, len(providers))
	names := providerVariableNames(providers)
	for index, item := range providers {
		local[item.SymbolID] = names[index]
		dependencies[item.SymbolID] = "dependencies." + names[index]
	}
	return local, dependencies
}

func renderProvidersTargetSource(
	providers []provider.Provider,
	componentFields []generatedComponentField,
	configTypes []configuration.Type,
	aliases map[string]string,
	dependencies map[string][]string,
	providerModules map[string]string,
	providerVariables map[string]string,
	exposedVariables map[string]string,
	events []compilerevent.Topic,
	policies []compilerpolicy.Service,
	features commandFeatures,
	adapters map[string]providerSourceAdapter,
) ([]byte, error) {
	var source bytes.Buffer
	writeApplicationDependenciesType(
		&source,
		providers,
		aliases,
		providerVariables,
		exposedVariables,
		policies,
	)
	writeServicePolicyDeclarations(&source, policies, aliases)
	source.WriteString("func constructApplicationDependencies(\n")
	source.WriteString("\tctx context.Context,\n")
	source.WriteString("\tapplication *Application,\n")
	source.WriteString("\toptions ApplicationOptions,\n")
	source.WriteString("\tconfigurationSnapshot spiceconfig.Snapshot,\n")
	if features.authorization {
		source.WriteString("\tauthorizer *spicesecurity.Authorizer,\n")
	}
	source.WriteString(") (*applicationDependencies, error) {\n")
	source.WriteString("\t_ = ctx\n")
	source.WriteString("\t_ = application\n")
	source.WriteString("\t_ = options\n")
	source.WriteString("\t_ = configurationSnapshot\n")
	if err := writeProviders(
		&source,
		providers,
		componentFields,
		configTypes,
		aliases,
		dependencies,
		providerModules,
		providerVariables,
		events,
		adapters,
		policies,
		exposedVariables,
		features,
	); err != nil {
		return nil, err
	}
	writeApplicationDependenciesReturn(
		&source,
		providers,
		providerVariables,
		exposedVariables,
		policies,
	)
	source.WriteString("}\n")
	return source.Bytes(), nil
}

func renderAssemblyTargetSource(
	target Target,
	features commandFeatures,
	componentFields []generatedComponentField,
	providerVariables map[string]string,
	hasLifecycleFeatures bool,
) []byte {
	var source bytes.Buffer
	source.WriteString("func NewApplication(ctx context.Context, observers ...spicelifecycle.Observer) (*Application, error) {\n")
	source.WriteString("\treturn NewApplicationWithOptions(ctx, ApplicationOptions{Observers: observers})\n")
	source.WriteString("}\n\n")
	source.WriteString("func NewApplicationWithOptions(ctx context.Context, options ApplicationOptions) (*Application, error) {\n")
	fmt.Fprintf(
		&source,
		"\tif ctx == nil {\n\t\treturn nil, fmt.Errorf(%s)\n\t}\n",
		strconv.Quote("construct application "+target.ID+": context is nil"),
	)
	source.WriteString("\tapplication := &Application{coordinator: spicelifecycle.NewCoordinator()}\n")
	writeBootstrapObservers(&source, features)
	writeAuthorizationSetup(&source, features)
	source.WriteString("\tfor index, observer := range observers {\n")
	source.WriteString("\t\tif err := application.coordinator.RegisterObserver(observer); err != nil {\n")
	source.WriteString("\t\t\treturn nil, fmt.Errorf(\"register lifecycle observer %d: %w\", index, err)\n")
	source.WriteString("\t\t}\n")
	source.WriteString("\t}\n")
	writeConfigurationResolution(&source, target)
	source.WriteString("\tdependencies, err := constructApplicationDependencies(ctx, application, options, configurationSnapshot")
	if features.authorization {
		source.WriteString(", authorizer")
	}
	source.WriteString(")\n")
	source.WriteString("\tif err != nil {\n")
	source.WriteString("\t\treturn nil, err\n")
	source.WriteString("\t}\n")
	source.WriteString("\t_ = dependencies\n")
	writeComponentAssignments(
		&source,
		componentFields,
		providerVariables,
	)
	if features.asynchronous {
		source.WriteString("\tif _, err := configureGeneratedAsync(ctx, application, options, configurationSnapshot, dependencies); err != nil {\n")
		source.WriteString("\t\treturn nil, err\n")
		source.WriteString("\t}\n")
	}
	if hasLifecycleFeatures {
		source.WriteString("\tif _, err := configureGeneratedLifecycle(ctx, application, options, dependencies); err != nil {\n")
		source.WriteString("\t\treturn nil, err\n")
		source.WriteString("\t}\n")
	}
	if features.hasMux {
		writeHTTPSetupCall(&source, features)
	}
	source.WriteString("\treturn application, nil\n")
	source.WriteString("}\n")
	return source.Bytes()
}

func renderContractsTargetSource(
	applicationAdapter applicationSourceAdapter,
	componentFields []generatedComponentField,
	aliases map[string]string,
	features commandFeatures,
	asyncTasks []compilerasync.Task,
	routeInterceptorFields []generatedRouteInterceptorField,
) []byte {
	var source bytes.Buffer
	writeGeneratedConstants(
		&source,
		applicationAdapter.alias+"."+applicationAdapter.identifier,
	)
	writeComponentsType(&source, componentFields, aliases)
	writeBeanOverridesType(&source, componentFields, aliases)
	writeRouteInterceptorsType(&source, routeInterceptorFields, aliases)
	source.WriteString("type Application struct {\n")
	source.WriteString("\tcoordinator *spicelifecycle.Coordinator\n")
	source.WriteString("\thooks []spicelifecycle.Hook\n")
	source.WriteString("\tshutdownTimeout time.Duration\n")
	source.WriteString("\tcomponents Components\n")
	if features.asynchronous {
		source.WriteString("\tasyncExecutor *spiceasync.Executor\n")
		writeAsyncApplicationFields(&source, asyncTasks, aliases)
	}
	if features.hasMux {
		source.WriteString("\tmux *http.ServeMux\n")
		source.WriteString("\thandler http.Handler\n")
	}
	source.WriteString("}\n\n")
	writeApplicationOptions(
		&source,
		features,
		componentFields,
		routeInterceptorFields,
	)
	return source.Bytes()
}

func renderFeaturesTargetSource(
	model application.Model,
	applicationTarget application.Target,
	features commandFeatures,
	asyncTasks []compilerasync.Task,
	jobs []compilerschedule.Job,
	providerVariables map[string]string,
	providerModules map[string]string,
	aliases map[string]string,
) []byte {
	var source bytes.Buffer
	if features.asynchronous {
		source.WriteString("func configureGeneratedAsync(\n")
		source.WriteString("\tctx context.Context,\n")
		source.WriteString("\tapplication *Application,\n")
		source.WriteString("\toptions ApplicationOptions,\n")
		source.WriteString("\tconfigurationSnapshot spiceconfig.Snapshot,\n")
		source.WriteString("\tdependencies *applicationDependencies,\n")
		source.WriteString(") (*Application, error) {\n")
		source.WriteString("\t_ = dependencies\n")
		writeAsyncSetup(
			&source,
			asyncTasks,
			providerVariables,
			providerModules,
			applicationTarget.PackagePath,
			aliases,
		)
		source.WriteString("\treturn application, nil\n")
		source.WriteString("}\n\n")
	}
	if len(model.Components()) != 0 || len(jobs) != 0 {
		source.WriteString("func configureGeneratedLifecycle(\n")
		source.WriteString("\tctx context.Context,\n")
		source.WriteString("\tapplication *Application,\n")
		source.WriteString("\toptions ApplicationOptions,\n")
		source.WriteString("\tdependencies *applicationDependencies,\n")
		source.WriteString(") (*Application, error) {\n")
		source.WriteString("\t_ = ctx\n")
		source.WriteString("\t_ = options\n")
		source.WriteString("\t_ = dependencies\n")
		writeScheduleSetup(
			&source,
			jobs,
			providerVariables,
			providerModules,
		)
		writeHooks(
			&source,
			model,
			providerVariables,
			providerModules,
			applicationTarget.PackagePath,
		)
		source.WriteString("\treturn application, nil\n")
		source.WriteString("}\n")
	}
	return source.Bytes()
}

func renderHTTPTargetSources(
	model application.Model,
	applicationTarget application.Target,
	features commandFeatures,
	providers []provider.Provider,
	controllers []controller.Controller,
	transactions []compilertransaction.Boundary,
	caches []compilercache.Boundary,
	providerVariables map[string]string,
	aliases map[string]string,
	modelOrigins []SourceOrigin,
	routeInterceptorFields []generatedRouteInterceptorField,
) ([]byte, []targetFileSpecification, error) {
	if !features.hasMux {
		return nil, nil, nil
	}
	var source bytes.Buffer
	writeHTTPSetupSignature(&source, features)
	if features.httpObservation {
		if err := writeFeatureHTTPObservers(
			&source,
			applicationTarget,
			providers,
			providerVariables,
		); err != nil {
			return nil, nil, err
		}
	}
	writeRouteMux(&source, providers, providerVariables)
	var routeSpecifications []targetFileSpecification
	if len(controllers) != 0 {
		var err error
		routeSpecifications, err = renderRouteSpecifications(
			controllers,
			transactions,
			caches,
			providerVariables,
			aliases,
			features,
			modelOrigins,
			routeInterceptorFields,
		)
		if err != nil {
			return nil, nil, err
		}
		for _, item := range routeSpecifications {
			writeRouteSetupCall(
				&source,
				item.generatedIdentifier,
				features,
			)
		}
	}
	writeManagementSetup(
		&source,
		model,
		applicationTarget,
		features,
	)
	writeRequestScopeSetup(&source, features)
	source.WriteString("\treturn application, nil\n")
	source.WriteString("}\n")
	return source.Bytes(), routeSpecifications, nil
}

func materializeTargetFiles(
	specifications []targetFileSpecification,
	aliases map[string]string,
	target Target,
	applicationTarget application.Target,
) ([]File, error) {
	files := make([]File, 0, len(specifications))
	for _, specification := range specifications {
		content, err := renderTargetFile(specification.body, aliases)
		if err != nil {
			return nil, fmt.Errorf(
				"render %s for %s: %w",
				specification.filename,
				applicationTarget.SymbolID,
				err,
			)
		}
		relatedSources := specification.relatedSources
		var mappings []SourceMapping
		if specification.generatedIdentifier != "" &&
			len(specification.relatedSources) != 0 {
			line, column, found := generatedIdentifierPosition(
				content,
				specification.generatedIdentifier,
			)
			if !found {
				return nil, fmt.Errorf(
					"generated %s has no position for %s",
					specification.filename,
					specification.generatedIdentifier,
				)
			}
			mappings = append(mappings, SourceMapping{
				Kind:         specification.mappingKind,
				Contribution: specification.contribution,
				Source:       specification.relatedSources[0],
				Generated: generatedIdentifierRange(
					line,
					column,
					specification.generatedIdentifier,
				),
			})
		}
		files = append(files, File{
			Path:   path.Join(target.OutputDir, specification.filename),
			Mode:   0o644,
			SHA256: contentHash(content),
			Role:   specification.role,
			RelatedSources: append(
				[]SourceOrigin(nil),
				relatedSources...,
			),
			Mappings: mappings,
			Sources:  append([]SourceOrigin(nil), relatedSources...),
			content:  content,
		})
	}
	return files, nil
}

type targetFileSpecification struct {
	filename            string
	role                FileRole
	body                []byte
	relatedSources      []SourceOrigin
	mappingKind         string
	contribution        string
	generatedIdentifier string
}

func renderTargetFile(
	body []byte,
	aliases map[string]string,
) ([]byte, error) {
	used, err := selectorAliases(body)
	if err != nil {
		return nil, err
	}
	fileAliases := make(map[string]string)
	for importPath, alias := range aliases {
		if _, found := used[alias]; found {
			fileAliases[importPath] = alias
		}
	}
	var source bytes.Buffer
	source.WriteString("//go:build !" + AnalysisBuildTag + "\n\n")
	source.WriteString("// Code generated by Spice. DO NOT EDIT.\n\n")
	source.WriteString("package spicegen\n\n")
	writeImports(&source, fileAliases)
	source.Write(body)
	formatted, err := format.Source(source.Bytes())
	if err != nil {
		return nil, err
	}
	return formatted, nil
}

func selectorAliases(body []byte) (map[string]struct{}, error) {
	source := append([]byte("package spicegen\n"), body...)
	parsed, err := parser.ParseFile(
		token.NewFileSet(),
		"generated.go",
		source,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("parse generated declarations: %w", err)
	}
	result := make(map[string]struct{})
	ast.Inspect(parsed, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		identifier, ok := selector.X.(*ast.Ident)
		if ok {
			result[identifier.Name] = struct{}{}
		}
		return true
	})
	return result, nil
}

func writeApplicationDependenciesType(
	source *bytes.Buffer,
	providers []provider.Provider,
	aliases map[string]string,
	providerVariables map[string]string,
	exposedVariables map[string]string,
	policies []compilerpolicy.Service,
) {
	source.WriteString("type applicationDependencies struct {\n")
	for _, item := range providers {
		output := renderedType(item.Output, aliases)
		if item.Scope != sdk.BeanScopeSingleton {
			output = "spicebean.Provider[" + output + "]"
		}
		fmt.Fprintf(
			source,
			"\t%s %s\n",
			providerVariables[item.SymbolID],
			output,
		)
	}
	for _, service := range policies {
		fmt.Fprintf(
			source,
			"\t%s %s\n",
			exposedVariables[service.Provider.SymbolID],
			renderedType(service.Interface.Type, aliases),
		)
	}
	source.WriteString("}\n\n")
}

func writeApplicationDependenciesReturn(
	source *bytes.Buffer,
	providers []provider.Provider,
	providerVariables map[string]string,
	exposedVariables map[string]string,
	policies []compilerpolicy.Service,
) {
	source.WriteString("\treturn &applicationDependencies{\n")
	for _, item := range providers {
		variable := providerVariables[item.SymbolID]
		fmt.Fprintf(source, "\t\t%s: %s,\n", variable, variable)
	}
	for _, service := range policies {
		variable := exposedVariables[service.Provider.SymbolID]
		fmt.Fprintf(source, "\t\t%s: %s,\n", variable, variable)
	}
	source.WriteString("\t}, nil\n")
}

func writeHTTPSetupCall(
	source *bytes.Buffer,
	features commandFeatures,
) {
	source.WriteString("\tif _, err := configureGeneratedHTTP(\n")
	source.WriteString("\t\tctx,\n")
	source.WriteString("\t\tapplication,\n")
	source.WriteString("\t\toptions,\n")
	source.WriteString("\t\tconfigurationSchema,\n")
	source.WriteString("\t\tconfigurationSnapshot,\n")
	source.WriteString("\t\tdependencies,\n")
	source.WriteString("\t\thttpObservers,\n")
	if features.metrics {
		source.WriteString("\t\tmanagementMetrics,\n")
	}
	if features.authorization {
		source.WriteString("\t\tauthorizer,\n")
	}
	source.WriteString("\t); err != nil {\n")
	source.WriteString("\t\treturn nil, err\n")
	source.WriteString("\t}\n")
}

func writeHTTPSetupSignature(
	source *bytes.Buffer,
	features commandFeatures,
) {
	source.WriteString("func configureGeneratedHTTP(\n")
	source.WriteString("\tctx context.Context,\n")
	source.WriteString("\tapplication *Application,\n")
	source.WriteString("\toptions ApplicationOptions,\n")
	source.WriteString("\tconfigurationSchema spiceconfig.Schema,\n")
	source.WriteString("\tconfigurationSnapshot spiceconfig.Snapshot,\n")
	source.WriteString("\tdependencies *applicationDependencies,\n")
	source.WriteString("\thttpObservers []spiceweb.HTTPObserver,\n")
	if features.metrics {
		source.WriteString("\tmanagementMetrics *spicemanagement.HTTPMetrics,\n")
	}
	if features.authorization {
		source.WriteString("\tauthorizer *spicesecurity.Authorizer,\n")
	}
	source.WriteString(") (*Application, error) {\n")
	source.WriteString("\t_ = configurationSchema\n")
	source.WriteString("\t_ = configurationSnapshot\n")
	source.WriteString("\t_ = dependencies\n")
	source.WriteString("\t_ = httpObservers\n")
}

func renderRouteSpecifications(
	controllers []controller.Controller,
	transactions []compilertransaction.Boundary,
	caches []compilercache.Boundary,
	providerVariables map[string]string,
	aliases map[string]string,
	features commandFeatures,
	applicationOrigins []SourceOrigin,
	routeInterceptorFields []generatedRouteInterceptorField,
) ([]targetFileSpecification, error) {
	transactionIndex := make(
		map[string]compilertransaction.Boundary,
		len(transactions),
	)
	for _, boundary := range transactions {
		transactionIndex[boundary.RouteID] = boundary
	}
	cacheIndex := make(map[string]compilercache.Boundary, len(caches))
	for _, boundary := range caches {
		cacheIndex[boundary.RouteID] = boundary
	}
	interceptorFields := routeInterceptorFieldIndex(routeInterceptorFields)

	var result []targetFileSpecification
	for _, item := range controllers {
		receiver := providerVariables[item.ProviderID]
		for _, route := range item.Routes() {
			functionName, filename := generatedRouteIdentity(route)
			var source bytes.Buffer
			writeRouteSetupSignature(
				&source,
				functionName,
				features,
			)
			var routeCaches []compilercache.Boundary
			if boundary, found := cacheIndex[route.SymbolID]; found {
				routeCaches = append(routeCaches, boundary)
			}
			cacheRuntimes := writeCacheSetup(
				&source,
				routeCaches,
				aliases,
			)
			middleware := "options.Middleware"
			if authorization, protected := route.Authorization(); protected {
				middleware = writeRouteAuthorization(
					&source,
					authorization,
					route.HTTPMethod+" "+route.Path,
					0,
				)
			}
			if err := writeControllerRoute(
				&source,
				route,
				transactionIndex,
				cacheRuntimes,
				providerVariables,
				receiver,
				middleware,
				aliases,
				0,
				interceptorFields[route.SymbolID],
			); err != nil {
				return nil, err
			}
			source.WriteString("\treturn application, nil\n")
			source.WriteString("}\n")
			related := sourceOriginsForSymbol(
				applicationOrigins,
				route.SymbolID,
			)
			result = append(result, targetFileSpecification{
				filename:            filename,
				role:                FileRoleTargetHTTPRoute,
				body:                source.Bytes(),
				relatedSources:      related,
				mappingKind:         "http-route-wiring",
				contribution:        route.SymbolID,
				generatedIdentifier: functionName,
			})
		}
	}
	return result, nil
}

func generatedRouteIdentity(route controller.Route) (string, string) {
	digest := sha256.Sum256([]byte(route.SymbolID))
	suffix := hex.EncodeToString(digest[:6])
	functionSuffix := hex.EncodeToString(digest[:4])
	label := targetid.Default(
		path.Base(route.Symbol.PackagePath) + "_" +
			route.Symbol.Receiver + "_" +
			route.Name,
	)
	const maximumRouteLabelLength = 64
	if len(label) > maximumRouteLabelLength {
		label = strings.TrimSuffix(
			label[:maximumRouteLabelLength],
			"_",
		)
	}
	functionLabel := exportedGeneratedIdentifier(
		path.Base(route.Symbol.PackagePath),
		"Route",
	) + exportedGeneratedIdentifier(
		route.Symbol.Receiver,
		"Controller",
	) + exportedGeneratedIdentifier(route.Name, "Handler")
	return "registerGeneratedRoute" + functionLabel + "_" + functionSuffix,
		"spice_http_route_" + label + "_" + suffix + "_gen.go"
}

func sourceOriginsForSymbol(
	origins []SourceOrigin,
	symbolID string,
) []SourceOrigin {
	return sourceOriginsForSymbolFamilies(origins, symbolID)
}

func sourceOriginsForSymbolFamilies(
	origins []SourceOrigin,
	symbolIDs ...string,
) []SourceOrigin {
	var result []SourceOrigin
	for _, origin := range origins {
		for _, symbolID := range symbolIDs {
			if origin.Symbol == symbolID ||
				strings.HasPrefix(origin.Symbol, symbolID+".") ||
				strings.HasPrefix(origin.Symbol, symbolID+"#") {
				result = append(result, origin)
				break
			}
		}
	}
	return result
}

func mergeSourceOrigins(groups ...[]SourceOrigin) []SourceOrigin {
	var result []SourceOrigin
	for _, group := range groups {
		result = append(result, group...)
	}
	sortSourceOrigins(result)
	return slices.Compact(result)
}

func providerSourceOrigins(
	origins []SourceOrigin,
	providers []provider.Provider,
	events []compilerevent.Topic,
) []SourceOrigin {
	symbols := make([]string, 0, len(providers)+len(events))
	for _, item := range providers {
		symbols = append(symbols, item.SymbolID)
	}
	for _, topic := range events {
		symbols = append(symbols, topic.MarkerID)
		for _, listener := range topic.Listeners() {
			symbols = append(symbols, listener.MethodID)
		}
	}
	return sourceOriginsForSymbolFamilies(origins, symbols...)
}

func configurationSourceOrigins(
	origins []SourceOrigin,
	configTypes []configuration.Type,
) []SourceOrigin {
	symbols := make([]string, 0, len(configTypes))
	for _, item := range configTypes {
		symbols = append(symbols, item.SymbolID)
	}
	return sourceOriginsForSymbolFamilies(origins, symbols...)
}

func featureSourceOrigins(
	origins []SourceOrigin,
	components []compilerlifecycle.Component,
	jobs []compilerschedule.Job,
	tasks []compilerasync.Task,
) []SourceOrigin {
	var symbols []string
	for _, component := range components {
		if component.Start != nil {
			symbols = append(symbols, component.Start.MethodID)
		}
		if component.Stop != nil {
			symbols = append(symbols, component.Stop.MethodID)
		}
	}
	for _, job := range jobs {
		symbols = append(symbols, job.MethodID)
	}
	for _, task := range tasks {
		symbols = append(symbols, task.MethodID)
	}
	return sourceOriginsForSymbolFamilies(origins, symbols...)
}

func controllerSourceOrigins(
	origins []SourceOrigin,
	controllers []controller.Controller,
) []SourceOrigin {
	var symbols []string
	for _, item := range controllers {
		symbols = append(symbols, item.SymbolID)
		for _, route := range item.Routes() {
			symbols = append(symbols, route.SymbolID)
		}
	}
	return sourceOriginsForSymbolFamilies(origins, symbols...)
}

func writeRouteSetupSignature(
	source *bytes.Buffer,
	functionName string,
	features commandFeatures,
) {
	fmt.Fprintf(source, "func %s(\n", functionName)
	source.WriteString("\tctx context.Context,\n")
	source.WriteString("\tapplication *Application,\n")
	source.WriteString("\toptions ApplicationOptions,\n")
	source.WriteString("\tconfigurationSnapshot spiceconfig.Snapshot,\n")
	source.WriteString("\tdependencies *applicationDependencies,\n")
	source.WriteString("\thttpObservers []spiceweb.HTTPObserver,\n")
	source.WriteString("\trouteMux *http.ServeMux,\n")
	if features.authorization {
		source.WriteString("\tauthorizer *spicesecurity.Authorizer,\n")
	}
	source.WriteString(") (*Application, error) {\n")
	source.WriteString("\t_ = configurationSnapshot\n")
	source.WriteString("\t_ = dependencies\n")
	source.WriteString("\t_ = httpObservers\n")
	if features.authorization {
		source.WriteString("\t_ = authorizer\n")
	}
}

func writeRouteSetupCall(
	source *bytes.Buffer,
	functionName string,
	features commandFeatures,
) {
	fmt.Fprintf(source, "\tif _, err := %s(\n", functionName)
	source.WriteString("\t\tctx,\n")
	source.WriteString("\t\tapplication,\n")
	source.WriteString("\t\toptions,\n")
	source.WriteString("\t\tconfigurationSnapshot,\n")
	source.WriteString("\t\tdependencies,\n")
	source.WriteString("\t\thttpObservers,\n")
	source.WriteString("\t\trouteMux,\n")
	if features.authorization {
		source.WriteString("\t\tauthorizer,\n")
	}
	source.WriteString("\t); err != nil {\n")
	source.WriteString("\t\treturn nil, err\n")
	source.WriteString("\t}\n")
}

func contentHash(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func summarizeTarget(target Target) TargetSummary {
	return TargetSummary{
		ID:                    target.ID,
		Layout:                target.Layout,
		ModulePath:            target.ModulePath,
		PackagePath:           target.PackagePath,
		EntrypointPackagePath: target.EntrypointPackagePath,
		OutputDir:             target.OutputDir,
		BridgeDir:             target.BridgeDir,
		ManifestPath:          target.ManifestPath,
	}
}

func packageByPath(packages []load.Package, packagePath string) (load.Package, bool) {
	for _, pkg := range packages {
		if pkg.Path == packagePath {
			return pkg, true
		}
	}
	return load.Package{}, false
}

func containsTarget(targets []application.Target, symbolID string) bool {
	for _, target := range targets {
		if target.SymbolID == symbolID {
			return true
		}
	}
	return false
}

func providerRenderDiagnostic(
	applicationTarget application.Target,
	item provider.Provider,
	kind string,
	message string,
) Diagnostic {
	return Diagnostic{
		Position:         item.Position,
		PhysicalPosition: item.PhysicalPosition,
		TargetID:         applicationTarget.SymbolID,
		Kind:             kind,
		Message:          message,
	}
}

func targetDiagnostic(target application.Target, kind, message string) Diagnostic {
	return Diagnostic{
		Position:         target.Position,
		PhysicalPosition: target.PhysicalPosition,
		TargetID:         target.SymbolID,
		Kind:             kind,
		Message:          message,
	}
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
		if left.TargetID != right.TargetID {
			return left.TargetID < right.TargetID
		}
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		return left.Message < right.Message
	})
}
