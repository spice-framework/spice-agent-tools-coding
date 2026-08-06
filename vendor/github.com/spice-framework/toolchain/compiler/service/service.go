package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"go/token"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spice-framework/spice/annotation"
	"github.com/spice-framework/spice/annotation/sdk"
	"github.com/spice-framework/toolchain/compiler/annotationcatalog"
	"github.com/spice-framework/toolchain/compiler/annotationhost"
	"github.com/spice-framework/toolchain/compiler/annotationimport"
	"github.com/spice-framework/toolchain/compiler/application"
	compilerauto "github.com/spice-framework/toolchain/compiler/autoconfigure"
	compilerbootstrap "github.com/spice-framework/toolchain/compiler/bootstrap"
	"github.com/spice-framework/toolchain/compiler/descriptor"
	"github.com/spice-framework/toolchain/compiler/diagnostic"
	diagnosticadapt "github.com/spice-framework/toolchain/compiler/diagnostic/adapt"
	"github.com/spice-framework/toolchain/compiler/generate"
	"github.com/spice-framework/toolchain/compiler/load"
	"github.com/spice-framework/toolchain/compiler/modulith"
	annotationparser "github.com/spice-framework/toolchain/compiler/parser"
	"github.com/spice-framework/toolchain/compiler/provider"
	"github.com/spice-framework/toolchain/compiler/resolve"
	"github.com/spice-framework/toolchain/compiler/scan"
	compilerstarter "github.com/spice-framework/toolchain/compiler/starter"
	"github.com/spice-framework/toolchain/compiler/validate"
	"github.com/spice-framework/toolchain/internal/moduleenv"
)

// Service owns bounded analysis state for independent workspaces.
type Service struct {
	config serviceConfig

	mu           sync.Mutex
	cache        map[[sha256.Size]byte]Result
	order        [][sha256.Size]byte
	latest       map[string]uint64
	running      map[string]runningRequest
	catalogCache map[string]catalogCacheEntry
}

type runningRequest struct {
	sequence uint64
	cancel   context.CancelFunc
}

type catalogCacheEntry struct {
	expires     time.Time
	definitions []AnnotationDefinition
}

const annotationCatalogCacheDuration = 5 * time.Second

type serviceConfig struct {
	loader               Loader
	moduleVersions       ModuleVersionLoader
	loadOptions          load.Options
	registry             annotation.Registry
	starterCatalog       compilerstarter.Catalog
	bootstrapDefinitions []compilerbootstrap.Definition
	providerEntrypoints  []provider.Entrypoint
	cacheNamespace       string
	maxCacheEntries      int
	maxOverlayFiles      int
	maxOverlayBytes      int
	cacheExtensions      bool
	annotationTools      *annotationhost.Manager
	spiceVersion         string
}

// New creates an isolated bounded compiler service.
func New(config Config) (*Service, error) {
	if config.Loader == nil {
		config.Loader = load.Load
	}
	hasStarters := len(config.StarterCatalog.Manifests()) != 0
	if hasStarters {
		registry, err := config.StarterCatalog.Registry(config.Registry)
		if err != nil {
			return nil, err
		}
		config.Registry = registry
		config.BootstrapDefinitions = append(
			slices.Clone(config.BootstrapDefinitions),
			config.StarterCatalog.BootstrapDefinitions()...,
		)
	}
	if config.MaxCacheEntries == 0 {
		config.MaxCacheEntries = defaultCacheEntries
	}
	if config.MaxCacheEntries < 0 {
		return nil, errors.New("compiler service cache size must not be negative")
	}
	if config.MaxOverlayFiles == 0 {
		config.MaxOverlayFiles = defaultOverlayFiles
	}
	if config.MaxOverlayFiles <= 0 {
		return nil, errors.New("compiler service overlay file limit must be positive")
	}
	if config.MaxOverlayBytes == 0 {
		config.MaxOverlayBytes = defaultOverlayBytes
	}
	if config.MaxOverlayBytes <= 0 {
		return nil, errors.New("compiler service overlay byte limit must be positive")
	}
	cacheExtensions := len(config.BootstrapDefinitions) == 0 &&
		len(config.ProviderEntrypoints) == 0 &&
		!hasStarters
	if config.CacheNamespace != "" {
		cacheExtensions = true
	}
	return &Service{
		config: serviceConfig{
			loader:               config.Loader,
			moduleVersions:       config.ModuleVersions,
			loadOptions:          cloneLoadOptions(config.LoadOptions),
			registry:             config.Registry,
			starterCatalog:       config.StarterCatalog,
			bootstrapDefinitions: cloneBootstrapDefinitions(config.BootstrapDefinitions),
			providerEntrypoints:  slices.Clone(config.ProviderEntrypoints),
			cacheNamespace:       config.CacheNamespace,
			maxCacheEntries:      config.MaxCacheEntries,
			maxOverlayFiles:      config.MaxOverlayFiles,
			maxOverlayBytes:      config.MaxOverlayBytes,
			cacheExtensions:      cacheExtensions,
			annotationTools:      annotationhost.NewManager(),
			spiceVersion:         normalizedSpiceVersion(config.SpiceVersion),
		},
		cache:   make(map[[sha256.Size]byte]Result),
		latest:  make(map[string]uint64),
		running: make(map[string]runningRequest),
		catalogCache: make(
			map[string]catalogCacheEntry,
		),
	}, nil
}

// AnnotationCatalog returns statically discoverable descriptors from source
// already present in the target application's offline Go module graph. It does
// not load packages, execute tools, download modules, or mutate module files.
func (service *Service) AnnotationCatalog(
	ctx context.Context,
	workspaceRoot string,
) ([]AnnotationDefinition, error) {
	if ctx == nil {
		return nil, errors.New(
			"annotation catalog context must not be nil",
		)
	}
	root, err := normalizedCatalogRoot(workspaceRoot)
	if err != nil {
		return nil, err
	}
	key := normalizedWorkspaceKey(root)
	now := time.Now()
	service.mu.Lock()
	cached, found := service.catalogCache[key]
	service.mu.Unlock()
	if found && now.Before(cached.expires) {
		return cloneDefinitions(cached.definitions), nil
	}
	candidates, err := annotationcatalog.Discover(
		ctx,
		root,
		service.config.loadOptions.Env,
	)
	if err != nil {
		return nil, err
	}
	definitions := catalogDefinitions(root, candidates)
	service.mu.Lock()
	service.catalogCache[key] = catalogCacheEntry{
		expires:     time.Now().Add(annotationCatalogCacheDuration),
		definitions: cloneDefinitions(definitions),
	}
	service.mu.Unlock()
	return cloneDefinitions(definitions), nil
}

// InvalidateAnnotationCatalog drops a workspace's brief completion cache after
// an explicitly confirmed module-file change.
func (service *Service) InvalidateAnnotationCatalog(
	workspaceRoot string,
) error {
	root, err := normalizedCatalogRoot(workspaceRoot)
	if err != nil {
		return err
	}
	service.mu.Lock()
	delete(service.catalogCache, normalizedWorkspaceKey(root))
	service.mu.Unlock()
	return nil
}

func normalizedCatalogRoot(root string) (string, error) {
	if root == "" {
		return "", errors.New(
			"annotation catalog workspace root must not be empty",
		)
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf(
			"resolve annotation catalog workspace root: %w",
			err,
		)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf(
			"inspect annotation catalog workspace root: %w",
			err,
		)
	}
	if !info.IsDir() {
		return "", errors.New(
			"annotation catalog workspace root must be a directory",
		)
	}
	return filepath.Clean(absolute), nil
}

func catalogDefinitions(
	workspaceRoot string,
	candidates []annotationcatalog.Candidate,
) []AnnotationDefinition {
	result := make([]AnnotationDefinition, len(candidates))
	for index, candidate := range candidates {
		result[index] = AnnotationDefinition{
			Name:              candidate.Symbol,
			Summary:           candidate.Summary,
			DescriptorPackage: candidate.Package,
			DescriptorSymbol:  candidate.Symbol,
			DescriptorLocation: symbolLocation(
				workspaceRoot,
				candidate.DescriptorPosition,
				candidate.Symbol,
			),
			HasDescriptorLocation: candidate.DescriptorPosition.Filename != "",
			Implementation: AnnotationImplementation{
				Tool:               candidate.Tool,
				Handler:            candidate.Handler,
				Protocol:           candidate.Protocol,
				Authorized:         candidate.ToolAuthorized,
				AuthorizationKnown: true,
			},
			Provenance: AnnotationProvenance{
				Module:             candidate.Module,
				Version:            candidate.Version,
				ReplacementModule:  candidate.ReplacementModule,
				ReplacementVersion: candidate.ReplacementVersion,
				ReplacementDir:     candidate.ReplacementDir,
				LocalReplacement:   candidate.LocalReplacement,
			},
		}
	}
	return result
}

func normalizedSpiceVersion(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "development"
	}
	return value
}

type normalizedRequest struct {
	root     string
	target   string
	patterns []string
	overlay  map[string]Document
	mode     AnalysisMode
	content  string
	sequence uint64
}

// Analyze executes one read-only typed compiler analysis.
func (service *Service) Analyze(
	ctx context.Context,
	request Request,
) (Result, error) {
	if ctx == nil {
		return Result{}, errors.New("compiler service context must not be nil")
	}
	normalized, err := service.normalizeRequest(request)
	if err != nil {
		return Result{}, err
	}
	analysisCtx, cancel, err := service.begin(ctx, normalized)
	if err != nil {
		return Result{}, err
	}
	defer cancel()
	defer service.finish(normalized)

	key, cacheable, err := service.cacheKey(normalized)
	if err != nil {
		return Result{}, err
	}
	if cacheable {
		if cached, found := service.cached(key); found {
			if staleErr := service.rejectStale(normalized); staleErr != nil {
				return Result{}, staleErr
			}
			cached.sequence = normalized.sequence
			return cached, nil
		}
	}

	result, err := service.analyze(analysisCtx, normalized)
	if err != nil {
		if staleErr := service.rejectStale(normalized); staleErr != nil {
			return Result{}, staleErr
		}
		return Result{}, err
	}
	if err := service.rejectStale(normalized); err != nil {
		return Result{}, err
	}
	if cacheable {
		service.store(key, result)
	}
	return cloneResult(result), nil
}

func (service *Service) begin(
	ctx context.Context,
	request normalizedRequest,
) (context.Context, context.CancelFunc, error) {
	analysisCtx, cancel := context.WithCancel(ctx)
	if request.sequence == 0 {
		return analysisCtx, cancel, nil
	}
	workspaceKey := normalizedWorkspaceKey(request.root)
	service.mu.Lock()
	defer service.mu.Unlock()
	if request.sequence < service.latest[workspaceKey] {
		cancel()
		return nil, nil, ErrStaleAnalysis
	}
	service.latest[workspaceKey] = request.sequence
	if previous, found := service.running[workspaceKey]; found {
		previous.cancel()
	}
	service.running[workspaceKey] = runningRequest{
		sequence: request.sequence,
		cancel:   cancel,
	}
	return analysisCtx, cancel, nil
}

func (service *Service) finish(request normalizedRequest) {
	if request.sequence == 0 {
		return
	}
	workspaceKey := normalizedWorkspaceKey(request.root)
	service.mu.Lock()
	if running, found := service.running[workspaceKey]; found &&
		running.sequence == request.sequence {
		delete(service.running, workspaceKey)
	}
	service.mu.Unlock()
}

func (service *Service) rejectStale(request normalizedRequest) error {
	if request.sequence == 0 {
		return nil
	}
	service.mu.Lock()
	latest := service.latest[normalizedWorkspaceKey(request.root)]
	service.mu.Unlock()
	if request.sequence < latest {
		return ErrStaleAnalysis
	}
	return nil
}

func (service *Service) analyze(
	ctx context.Context,
	request normalizedRequest,
) (Result, error) {
	result := Result{
		workspaceRoot: request.root,
		sequence:      request.sequence,
		definitions: summarizeDefinitions(
			request.root,
			service.config.registry,
			service.config.bootstrapDefinitions,
			nil,
			nil,
			nil,
		),
	}
	if preflight := rawAnnotationDiagnostics(
		request.root,
		request.overlay,
	); !preflight.Empty() {
		result.diagnostics = preflight
		result.actions = actionsFromDiagnostics(result.diagnostics)
		return result, nil
	}
	program, discovery, autoDiscovery, loadDiagnostics, err := service.loadAnalysisProgram(
		ctx,
		request,
	)
	if err != nil {
		return Result{}, err
	}
	if program != nil {
		result.goInterfaces = summarizeGoInterfaces(request.root, program)
	}
	if !loadDiagnostics.Empty() {
		result.diagnostics = loadDiagnostics
		result.actions = actionsFromDiagnostics(result.diagnostics)
		return result, nil
	}

	descriptorState, definitions, descriptorDiagnostics := service.prepareAnalysisDescriptors(
		ctx,
		request,
		program,
		discovery,
	)
	if !descriptorDiagnostics.Empty() {
		result.diagnostics = descriptorDiagnostics
		return result, nil
	}
	result.definitions = definitions
	resolution := resolve.AnnotationsWithDefinitions(
		program,
		descriptorState.index,
	)
	result.files = resolution.Files
	result.annotations = summarizeAnnotations(request.root, resolution)
	if len(resolution.Diagnostics) != 0 {
		result.diagnostics = versionDiagnostics(
			legacyImportFixes(
				diagnosticadapt.Resolution(
					request.root,
					resolution.Diagnostics,
				),
				request.overlay,
			),
			request.overlay,
		)
		result.actions = actionsFromDiagnostics(result.diagnostics)
		return result, nil
	}

	validationDiagnostics := validateOccurrences(
		resolution.Occurrences,
		descriptorState.registry,
	)
	if len(validationDiagnostics) != 0 {
		result.diagnostics = versionDiagnostics(
			diagnosticadapt.Validation(
				request.root,
				validationDiagnostics,
			),
			request.overlay,
		)
		result.actions = actionsFromDiagnostics(result.diagnostics)
		return result, nil
	}

	resolution, contributionDiagnostics, err := service.applyToolContributions(
		ctx,
		request,
		program,
		resolution,
		descriptorState,
	)
	if err != nil {
		return Result{}, err
	}
	if !contributionDiagnostics.Empty() {
		result.diagnostics = contributionDiagnostics
		result.actions = actionsFromDiagnostics(result.diagnostics)
		return result, nil
	}

	starterDiagnostics, err := service.starterDependencyDiagnostics(
		ctx,
		request,
		resolution.Occurrences,
	)
	if err != nil {
		return Result{}, err
	}
	if !starterDiagnostics.Empty() {
		result.diagnostics = starterDiagnostics
		result.actions = actionsFromDiagnostics(result.diagnostics)
		return result, nil
	}

	buildOptions := application.BuildOptions{
		BootstrapDefinitions: cloneBootstrapDefinitions(
			service.config.bootstrapDefinitions,
		),
	}
	primaryProviderCatalog := provider.Build(program, resolution)
	buildOptions.PrimaryProviderCatalog = &primaryProviderCatalog
	result.providerGraph.Providers = summarizeProviders(
		primaryProviderCatalog.Providers(),
	)
	providerCatalogs, autoConfigurations, catalogDiagnostics := service.prepareProviderCatalogs(
		request,
		program,
		resolution,
		primaryProviderCatalog,
		autoDiscovery,
	)
	result.autoConfigs = autoConfigurations
	if !catalogDiagnostics.Empty() {
		result.diagnostics = catalogDiagnostics
		result.actions = actionsFromDiagnostics(result.diagnostics)
		return result, nil
	}
	buildOptions.ProviderCatalogs = providerCatalogs

	moduleModel := modulith.Build(program, resolution)
	result.moduleModel = moduleModel
	result.moduleGraph = summarizeModuleGraph(moduleModel)
	model := application.BuildWithOptions(
		program,
		resolution,
		buildOptions,
	)
	result.application = model
	if graph := summarizeProviderGraph(model); len(graph.Providers) != 0 {
		result.providerGraph = graph
	}
	result.configurations = summarizeConfigurations(model)
	if diagnostics := model.Diagnostics(); len(diagnostics) != 0 {
		result.diagnostics = versionDiagnostics(
			diagnosticadapt.Application(request.root, diagnostics),
			request.overlay,
		)
		result.actions = actionsFromDiagnostics(result.diagnostics)
		return result, nil
	}
	return completeAnalysis(request, program, model, result)
}

func (service *Service) prepareAnalysisDescriptors(
	ctx context.Context,
	request normalizedRequest,
	program *load.Program,
	discovery annotationimport.Discovery,
) (preparedDescriptors, []AnnotationDefinition, diagnostic.Set) {
	references, err := selectedDescriptorReferences(program, discovery)
	if err != nil {
		return preparedDescriptors{}, nil, diagnosticadapt.Failure(
			"annotation",
			"descriptor",
			err.Error(),
		)
	}
	state, err := prepareDescriptors(
		service.config.registry,
		program,
		references,
	)
	if err != nil {
		return preparedDescriptors{}, nil, diagnosticadapt.Failure(
			"annotation",
			"descriptor",
			err.Error(),
		)
	}
	implementationPositions, err := resolveImplementationPositions(
		ctx,
		request.root,
		service.config.loadOptions.Env,
		state.items,
	)
	if err != nil {
		return preparedDescriptors{}, nil, diagnosticadapt.Failure(
			"annotation",
			"implementation-source",
			err.Error(),
		)
	}
	return state, summarizeDefinitions(
		request.root,
		state.registry,
		service.config.bootstrapDefinitions,
		state.items,
		program,
		implementationPositions,
	), diagnostic.NewSet()
}

func (service *Service) buildProviderCatalogs(
	request normalizedRequest,
	program *load.Program,
	resolution resolve.Result,
) ([]provider.Catalog, diagnostic.Set) {
	var catalogs []provider.Catalog
	if len(service.config.providerEntrypoints) != 0 {
		catalog := provider.BuildEntrypoints(
			program,
			service.config.providerEntrypoints,
		)
		if diagnostics := catalog.Diagnostics(); len(diagnostics) != 0 {
			return nil, versionDiagnostics(
				diagnosticadapt.Provider(request.root, diagnostics),
				request.overlay,
			)
		}
		catalogs = append(catalogs, catalog)
	}
	starterEntrypoints := service.config.starterCatalog.ProviderEntrypoints(
		resolution.Occurrences,
	)
	if len(starterEntrypoints) == 0 {
		return catalogs, diagnostic.NewSet()
	}
	catalog := provider.BuildEntrypoints(program, starterEntrypoints)
	if diagnostics := catalog.Diagnostics(); len(diagnostics) != 0 {
		return nil, versionDiagnostics(
			diagnosticadapt.StarterProviders(
				request.root,
				diagnostics,
			),
			request.overlay,
		)
	}
	return append(catalogs, catalog), diagnostic.NewSet()
}

func completeAnalysis(
	request normalizedRequest,
	program *load.Program,
	model application.Model,
	result Result,
) (Result, error) {
	if request.mode == AnalysisValidate {
		result.diagnostics = diagnostic.NewSet()
		return result, nil
	}

	target, err := selectTarget(model.Targets(), request.target)
	if err != nil {
		result.diagnostics = diagnosticadapt.Failure(
			"application",
			"target-selection",
			err.Error(),
		)
		result.actions = actionsFromDiagnostics(result.diagnostics)
		return result, nil
	}
	result.targetName = target.Name
	generationTarget, generationDiagnostics := generate.DefaultTarget(
		program,
		target,
	)
	if len(generationDiagnostics) != 0 {
		result.diagnostics = versionDiagnostics(
			diagnosticadapt.Generation(
				request.root,
				generationDiagnostics,
			),
			request.overlay,
		)
		result.actions = actionsFromDiagnostics(result.diagnostics)
		return result, nil
	}
	plan, generationDiagnostics := generate.Render(
		program,
		model,
		target,
		generationTarget,
	)
	if len(generationDiagnostics) != 0 {
		result.diagnostics = versionDiagnostics(
			diagnosticadapt.Generation(
				request.root,
				generationDiagnostics,
			),
			request.overlay,
		)
		result.actions = actionsFromDiagnostics(result.diagnostics)
		return result, nil
	}
	result.diagnostics = diagnostic.NewSet()
	result.plan = plan
	result.hasPlan = true
	return result, nil
}

func (service *Service) loadAnalysisProgram(
	ctx context.Context,
	request normalizedRequest,
) (
	*load.Program,
	annotationimport.Discovery,
	compilerauto.Discovery,
	diagnostic.Set,
	error,
) {
	options := service.analysisLoadOptions(request)
	discovery, err := annotationimport.Discover(
		request.root,
		options.Overlay,
	)
	if err != nil {
		return nil, annotationimport.Discovery{}, compilerauto.Discovery{}, diagnostic.Set{}, err
	}
	autoDiscovery, err := compilerauto.Discover(
		request.root,
		options.Overlay,
	)
	if err != nil {
		return nil, annotationimport.Discovery{}, compilerauto.Discovery{}, diagnostic.Set{}, err
	}
	options.AuxiliaryPackages = append(
		options.AuxiliaryPackages,
		discovery.Packages...,
	)
	options.AuxiliaryPackages = append(
		options.AuxiliaryPackages,
		autoDiscovery.Packages...,
	)
	program, loadErr := service.config.loader(
		ctx,
		options,
		request.patterns...,
	)
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, annotationimport.Discovery{}, compilerauto.Discovery{}, diagnostic.Set{}, contextErr
	}
	if loadErr == nil {
		return program, discovery, autoDiscovery, diagnostic.Set{}, nil
	}
	if program == nil {
		return nil, discovery, autoDiscovery, diagnosticadapt.Failure(
			"load",
			"operation",
			loadErr.Error(),
		), nil
	}
	diagnostics := versionDiagnostics(
		overlaySafeFixes(
			diagnosticadapt.Load(request.root, program.Diagnostics()),
			request.overlay,
		),
		request.overlay,
	)
	return program, discovery, autoDiscovery, diagnostics, nil
}

func (service *Service) starterDependencyDiagnostics(
	ctx context.Context,
	request normalizedRequest,
	occurrences []resolve.Occurrence,
) (diagnostic.Set, error) {
	requirements := service.config.starterCatalog.ActiveDependencies(
		occurrences,
	)
	if len(requirements) == 0 {
		return diagnostic.NewSet(), nil
	}
	if service.config.moduleVersions == nil {
		return diagnosticadapt.Failure(
			"starter",
			"dependency-inspection",
			"selected starters require Go module version inspection",
		), nil
	}
	moduleOptions := cloneLoadOptions(service.config.loadOptions)
	moduleOptions.Dir = request.root
	modules, err := service.config.moduleVersions(ctx, moduleOptions)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return diagnostic.Set{}, contextErr
		}
		return diagnosticadapt.Failure(
			"starter",
			"dependency-inspection",
			fmt.Sprintf(
				"inspect selected starter dependencies: %v",
				err,
			),
		), nil
	}
	dependencyDiagnostics := service.config.starterCatalog.
		ValidateActiveModuleVersions(occurrences, modules)
	return diagnosticadapt.StarterDependencies(dependencyDiagnostics), nil
}

func (service *Service) analysisLoadOptions(
	request normalizedRequest,
) load.Options {
	options := cloneLoadOptions(service.config.loadOptions)
	options.Dir = request.root
	if options.Overlay == nil {
		options.Overlay = make(map[string][]byte, len(request.overlay))
	}
	for filePath, document := range request.overlay {
		options.Overlay[filePath] = slices.Clone(document.Content)
	}
	options.AuxiliaryPackages = append(
		options.AuxiliaryPackages,
		entrypointPackages(service.config.providerEntrypoints)...,
	)
	options.AuxiliaryPackages = append(
		options.AuxiliaryPackages,
		service.config.starterCatalog.EntryPointPackages()...,
	)
	if request.mode == AnalysisGenerate {
		options.PrepareGeneratedApplicationEntrypoints = true
		options = withAnalysisBuildTag(options)
	}
	return withOfflineModuleResolution(options, request.root)
}

func withOfflineModuleResolution(
	options load.Options,
	root string,
) load.Options {
	result := cloneLoadOptions(options)
	if result.Env == nil {
		result.Env = os.Environ()
	}
	result.Env = replaceEnvironment(result.Env, "GOPROXY", "off")
	flags := make([]string, 0, len(result.BuildFlags)+1)
	for index := 0; index < len(result.BuildFlags); index++ {
		flag := result.BuildFlags[index]
		if flag == "-mod" {
			if index+1 < len(result.BuildFlags) {
				index++
			}
			continue
		}
		if strings.HasPrefix(flag, "-mod=") {
			continue
		}
		flags = append(flags, flag)
	}
	flags = append(flags, "-mod="+moduleenv.OfflineMode(root, result.Env))
	result.BuildFlags = flags
	return result
}

func replaceEnvironment(
	environment []string,
	name string,
	value string,
) []string {
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		key, _, found := strings.Cut(entry, "=")
		if found && strings.EqualFold(key, name) {
			continue
		}
		result = append(result, entry)
	}
	return append(result, name+"="+value)
}

func (service *Service) normalizeRequest(
	request Request,
) (normalizedRequest, error) {
	if request.WorkspaceRoot == "" {
		return normalizedRequest{}, errors.New("compiler service workspace root must not be empty")
	}
	if request.Mode != AnalysisGenerate && request.Mode != AnalysisValidate {
		return normalizedRequest{}, fmt.Errorf(
			"compiler service analysis mode %d is unsupported",
			request.Mode,
		)
	}
	if request.Mode == AnalysisValidate && request.Target != "" {
		return normalizedRequest{}, errors.New(
			"compiler service validation analysis must not select a target",
		)
	}
	root, err := filepath.Abs(request.WorkspaceRoot)
	if err != nil {
		return normalizedRequest{}, fmt.Errorf("resolve compiler service workspace root: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return normalizedRequest{}, fmt.Errorf("inspect compiler service workspace root: %w", err)
	}
	if !info.IsDir() {
		return normalizedRequest{}, errors.New("compiler service workspace root must be a directory")
	}
	patterns := slices.Clone(request.Patterns)
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}
	overlay, err := normalizeOverlay(
		root,
		request.Overlay,
		service.config.maxOverlayFiles,
		service.config.maxOverlayBytes,
	)
	if err != nil {
		return normalizedRequest{}, err
	}
	return normalizedRequest{
		root:     filepath.Clean(root),
		target:   request.Target,
		patterns: patterns,
		overlay:  overlay,
		mode:     request.Mode,
		content:  request.ContentHash,
		sequence: request.Sequence,
	}, nil
}

func normalizeOverlay(
	root string,
	documents map[string]Document,
	maximumFiles int,
	maximumBytes int,
) (map[string]Document, error) {
	if len(documents) > maximumFiles {
		return nil, fmt.Errorf(
			"compiler service overlay has %d files; maximum is %d",
			len(documents),
			maximumFiles,
		)
	}
	result := make(map[string]Document, len(documents))
	total := 0
	for identity, document := range documents {
		filePath, err := overlayPath(root, identity)
		if err != nil {
			return nil, err
		}
		total += len(document.Content)
		if total > maximumBytes {
			return nil, fmt.Errorf(
				"compiler service overlay exceeds %d bytes",
				maximumBytes,
			)
		}
		document.Content = slices.Clone(document.Content)
		result[filePath] = document
	}
	return result, nil
}

func overlayPath(root, identity string) (string, error) {
	if identity == "" {
		return "", errors.New("compiler service overlay path must not be empty")
	}
	filePath, err := overlayIdentityPath(identity)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(filePath) {
		filePath = filepath.Join(root, filePath)
	}
	absolute, err := filepath.Abs(filePath)
	if err != nil {
		return "", fmt.Errorf("resolve compiler service overlay %q: %w", identity, err)
	}
	relative, err := filepath.Rel(root, absolute)
	if err != nil || (relative != "." && !filepath.IsLocal(relative)) {
		return "", fmt.Errorf(
			"compiler service overlay %q must remain inside workspace",
			identity,
		)
	}
	return filepath.Clean(absolute), nil
}

func overlayIdentityPath(identity string) (string, error) {
	isURI := strings.HasPrefix(strings.ToLower(identity), "file:") ||
		strings.Contains(identity, "://")
	if !isURI {
		return identity, nil
	}
	parsed, err := url.Parse(identity)
	if err != nil {
		return "", fmt.Errorf(
			"parse compiler service overlay URI %q: %w",
			identity,
			err,
		)
	}
	if parsed.Scheme != "file" {
		return "", fmt.Errorf(
			"compiler service overlay URI %q must use file scheme",
			identity,
		)
	}
	filePath := parsed.Path
	if parsed.Host != "" && parsed.Host != "localhost" {
		if runtime.GOOS != "windows" ||
			len(parsed.Host) != 2 ||
			parsed.Host[1] != ':' {
			return "", fmt.Errorf(
				"compiler service overlay URI %q must not name a remote host",
				identity,
			)
		}
		filePath = "/" + parsed.Host + parsed.Path
	}
	if runtime.GOOS == "windows" &&
		len(filePath) >= 3 &&
		filePath[0] == '/' &&
		filePath[2] == ':' {
		filePath = filePath[1:]
	}
	return filepath.FromSlash(filePath), nil
}

func validateOccurrences(
	occurrences []resolve.Occurrence,
	registry annotation.Registry,
) []validate.Diagnostic {
	var diagnostics []validate.Diagnostic
	for _, occurrence := range occurrences {
		items := validate.Occurrences([]scan.Occurrence{{
			Annotation: occurrence.Annotation,
			Target:     occurrence.Target,
			Name:       occurrence.Name,
			File:       occurrence.PhysicalFile,
		}}, registry)
		for index := range items {
			physical := occurrence.PhysicalPosition
			if physical.Filename == "" {
				physical = token.Position{
					Filename: occurrence.PhysicalFile,
					Offset:   occurrence.PhysicalOffset,
				}
			}
			items[index].PhysicalPosition = physical
		}
		diagnostics = append(diagnostics, items...)
	}
	return diagnostics
}

func descriptorDefinitionIndex(
	descriptors []descriptor.Descriptor,
) resolve.DefinitionIndex {
	result := make(resolve.DefinitionIndex, len(descriptors))
	for _, item := range descriptors {
		result[annotation.DefinitionReference{
			Package: item.Package,
			Symbol:  item.Symbol,
		}] = item.Definition.Name
	}
	return result
}

func resolveImplementationPositions(
	ctx context.Context,
	root string,
	environment []string,
	descriptors []descriptor.Descriptor,
) (map[sdk.Symbol]token.Position, error) {
	if len(descriptors) == 0 {
		return map[sdk.Symbol]token.Position{}, nil
	}
	symbols := make([]sdk.Symbol, 0, len(descriptors))
	seen := make(map[sdk.Symbol]struct{}, len(descriptors))
	for _, item := range descriptors {
		symbol := item.Handler
		if _, duplicate := seen[symbol]; duplicate {
			continue
		}
		seen[symbol] = struct{}{}
		symbols = append(symbols, symbol)
	}
	sort.Slice(symbols, func(left, right int) bool {
		if symbols[left].Package != symbols[right].Package {
			return symbols[left].Package < symbols[right].Package
		}
		return symbols[left].Name < symbols[right].Name
	})
	module, err := annotationhost.ReadTargetModule(root)
	if err != nil {
		return nil, err
	}
	return annotationhost.ResolveSourceSymbols(
		ctx,
		module,
		symbols,
		environment,
	)
}

type preparedDescriptors struct {
	index       resolve.DefinitionIndex
	registry    annotation.Registry
	items       []descriptor.Descriptor
	descriptors map[annotation.DefinitionReference]descriptor.Descriptor
}

func selectedDescriptorReferences(
	program *load.Program,
	discovery annotationimport.Discovery,
) ([]annotation.DefinitionReference, error) {
	namespacePackages := usedAnnotationNamespacePackages(
		program,
		discovery.Directives,
	)
	namespaceReferences, err := descriptor.NamespaceReferences(
		program,
		namespacePackages,
	)
	if err != nil {
		return nil, err
	}
	return append(
		append(
			[]annotation.DefinitionReference(nil),
			discovery.References...,
		),
		namespaceReferences...,
	), nil
}

func usedAnnotationNamespacePackages(
	program *load.Program,
	directives []annotation.ImportDirective,
) []string {
	bindings := annotationNamespaceBindings(directives)
	used := make(map[string]struct{})
	for _, pkg := range program.Packages() {
		for _, source := range pkg.Files {
			namespaces := bindings[filepath.Clean(source.PhysicalPath)]
			for _, packagePath := range usedAnnotationNamespacesInSource(
				pkg,
				source,
				namespaces,
			) {
				used[packagePath] = struct{}{}
			}
		}
	}
	result := make([]string, 0, len(used))
	for packagePath := range used {
		result = append(result, packagePath)
	}
	sort.Strings(result)
	return result
}

func annotationNamespaceBindings(
	directives []annotation.ImportDirective,
) map[string]map[string]string {
	bindings := make(map[string]map[string]string)
	for _, directive := range directives {
		if directive.Kind != annotation.ImportNamespace {
			continue
		}
		file := filepath.Clean(directive.Position.Filename)
		if bindings[file] == nil {
			bindings[file] = make(map[string]string)
		}
		bindings[file][directive.Namespace] = directive.Package
	}
	return bindings
}

func usedAnnotationNamespacesInSource(
	pkg load.Package,
	source load.SourceFile,
	namespaces map[string]string,
) []string {
	if pkg.Raw == nil ||
		pkg.Raw.Fset == nil ||
		len(namespaces) == 0 ||
		source.Syntax == nil {
		return nil
	}
	var result []string
	for _, group := range source.Syntax.Comments {
		for _, comment := range group.List {
			position := pkg.Raw.Fset.PositionFor(comment.Pos(), true)
			invocation, recognized, err := annotationparser.ParseComment(
				comment.Text,
				position,
			)
			if err != nil || !recognized {
				continue
			}
			namespace, _, qualified := strings.Cut(invocation.Name, ".")
			if !qualified {
				continue
			}
			if packagePath, found := namespaces[namespace]; found {
				result = append(result, packagePath)
			}
		}
	}
	return result
}

func prepareDescriptors(
	base annotation.Registry,
	program *load.Program,
	references []annotation.DefinitionReference,
) (preparedDescriptors, error) {
	descriptors, err := descriptor.DecodeAll(program, references)
	if err != nil {
		return preparedDescriptors{}, err
	}
	registry, err := registryWithDescriptors(base, descriptors)
	if err != nil {
		return preparedDescriptors{}, err
	}
	return preparedDescriptors{
		index:       descriptorDefinitionIndex(descriptors),
		registry:    registry,
		items:       descriptors,
		descriptors: descriptorIndex(descriptors),
	}, nil
}

func descriptorIndex(
	descriptors []descriptor.Descriptor,
) map[annotation.DefinitionReference]descriptor.Descriptor {
	result := make(
		map[annotation.DefinitionReference]descriptor.Descriptor,
		len(descriptors),
	)
	for _, item := range descriptors {
		result[annotation.DefinitionReference{
			Package: item.Package,
			Symbol:  item.Symbol,
		}] = item
	}
	return result
}

func registryWithDescriptors(
	base annotation.Registry,
	descriptors []descriptor.Descriptor,
) (annotation.Registry, error) {
	definitions := make(map[string]annotation.Definition)
	for _, definition := range base.Definitions() {
		definitions[definition.Name] = definition
	}
	for _, item := range descriptors {
		definition, err := item.RegistryDefinition()
		if err != nil {
			return annotation.Registry{}, err
		}
		definitions[definition.Name] = definition
	}
	names := make([]string, 0, len(definitions))
	for name := range definitions {
		names = append(names, name)
	}
	sort.Strings(names)
	merged := make([]annotation.Definition, 0, len(names))
	for _, name := range names {
		merged = append(merged, definitions[name])
	}
	return annotation.NewRegistry(merged...)
}

func selectTarget(
	targets []application.Target,
	selector string,
) (application.Target, error) {
	if len(targets) == 0 {
		return application.Target{}, errors.New(
			"no @Application marker was found in the selected packages",
		)
	}
	if selector == "" {
		if len(targets) == 1 {
			return targets[0], nil
		}
		names := make([]string, len(targets))
		for index, target := range targets {
			names[index] = target.Name
		}
		return application.Target{}, fmt.Errorf(
			"multiple @Application targets were found (%s); select one with --target",
			strings.Join(names, ", "),
		)
	}
	var matches []application.Target
	for _, target := range targets {
		if target.Name == selector ||
			target.PackagePath == selector ||
			target.SymbolID == selector ||
			strings.EqualFold(target.Name, selector) {
			matches = append(matches, target)
		}
	}
	if len(matches) == 0 {
		return application.Target{}, fmt.Errorf(
			"@Application target %q was not found",
			selector,
		)
	}
	if len(matches) > 1 {
		return application.Target{}, fmt.Errorf(
			"@Application target %q is ambiguous; select by stable symbol ID",
			selector,
		)
	}
	return matches[0], nil
}

func entrypointPackages(items []provider.Entrypoint) []string {
	result := make([]string, 0, len(items))
	for _, item := range items {
		result = append(result, item.PackagePath)
	}
	sort.Strings(result)
	return slices.Compact(result)
}

func cloneLoadOptions(options load.Options) load.Options {
	options.Env = slices.Clone(options.Env)
	options.BuildFlags = slices.Clone(options.BuildFlags)
	options.AuxiliaryPackages = slices.Clone(options.AuxiliaryPackages)
	if options.Overlay != nil {
		overlay := make(map[string][]byte, len(options.Overlay))
		for filePath, content := range options.Overlay {
			overlay[filePath] = slices.Clone(content)
		}
		options.Overlay = overlay
	}
	return options
}

func cloneBootstrapDefinitions(
	items []compilerbootstrap.Definition,
) []compilerbootstrap.Definition {
	result := make([]compilerbootstrap.Definition, len(items))
	for index, item := range items {
		result[index] = item
		result[index].Options = slices.Clone(item.Options)
		for optionIndex := range result[index].Options {
			result[index].Options[optionIndex].ListItemKinds = slices.Clone(
				item.Options[optionIndex].ListItemKinds,
			)
			result[index].Options[optionIndex].AllowedStrings = slices.Clone(
				item.Options[optionIndex].AllowedStrings,
			)
		}
		result[index].Requirements = slices.Clone(item.Requirements)
		result[index].EntryPoints = slices.Clone(item.EntryPoints)
	}
	return result
}

func withAnalysisBuildTag(options load.Options) load.Options {
	result := cloneLoadOptions(options)
	result.BuildFlags = nil
	tags := map[string]struct{}{generate.AnalysisBuildTag: {}}
	addTagsFromFlags(tags, goFlags(options.Env))
	for index := 0; index < len(options.BuildFlags); index++ {
		flag := options.BuildFlags[index]
		switch {
		case flag == "-tags" && index+1 < len(options.BuildFlags):
			index++
			addTagValue(tags, options.BuildFlags[index])
		case strings.HasPrefix(flag, "-tags="):
			addTagValue(tags, strings.TrimPrefix(flag, "-tags="))
		default:
			result.BuildFlags = append(result.BuildFlags, flag)
		}
	}
	ordered := make([]string, 0, len(tags))
	for tag := range tags {
		ordered = append(ordered, tag)
	}
	sort.Strings(ordered)
	result.BuildFlags = append(
		result.BuildFlags,
		"-tags="+strings.Join(ordered, ","),
	)
	return result
}

func goFlags(environment []string) string {
	if environment == nil {
		return os.Getenv("GOFLAGS")
	}
	for _, value := range environment {
		name, flagValue, found := strings.Cut(value, "=")
		if found && strings.EqualFold(name, "GOFLAGS") {
			return flagValue
		}
	}
	return ""
}

func addTagsFromFlags(tags map[string]struct{}, flags string) {
	fields := strings.Fields(flags)
	for index := 0; index < len(fields); index++ {
		switch {
		case fields[index] == "-tags" && index+1 < len(fields):
			index++
			addTagValue(tags, fields[index])
		case strings.HasPrefix(fields[index], "-tags="):
			addTagValue(tags, strings.TrimPrefix(fields[index], "-tags="))
		}
	}
}

func addTagValue(tags map[string]struct{}, value string) {
	for tag := range strings.FieldsFuncSeq(value, func(character rune) bool {
		return character == ',' || character == ' '
	}) {
		if tag != "" {
			tags[tag] = struct{}{}
		}
	}
}

func normalizedWorkspaceKey(root string) string {
	root = filepath.Clean(root)
	if runtime.GOOS == "windows" {
		return strings.ToLower(root)
	}
	return root
}

func (service *Service) cacheKey(
	request normalizedRequest,
) ([sha256.Size]byte, bool, error) {
	if service.config.maxCacheEntries == 0 ||
		!service.config.cacheExtensions ||
		request.content == "" {
		return [sha256.Size]byte{}, false, nil
	}
	options := service.analysisLoadOptions(request)
	payload := struct {
		Root              string
		Target            string
		Mode              AnalysisMode
		Patterns          []string
		Overlay           map[string]Document
		Environment       []string
		BuildFlags        []string
		AuxiliaryPackages []string
		Namespace         string
		Definitions       []annotation.Definition
		ContentHash       string
	}{
		Root:              normalizedWorkspaceKey(request.root),
		Target:            request.target,
		Mode:              request.mode,
		Patterns:          request.patterns,
		Overlay:           request.overlay,
		Environment:       options.Env,
		BuildFlags:        options.BuildFlags,
		AuxiliaryPackages: options.AuxiliaryPackages,
		Namespace:         service.config.cacheNamespace,
		Definitions:       service.config.registry.Definitions(),
		ContentHash:       request.content,
	}
	content, err := json.Marshal(payload)
	if err != nil {
		return [sha256.Size]byte{}, false, fmt.Errorf(
			"hash compiler service request: %w",
			err,
		)
	}
	return sha256.Sum256(content), true, nil
}

func (service *Service) cached(
	key [sha256.Size]byte,
) (Result, bool) {
	service.mu.Lock()
	defer service.mu.Unlock()
	result, found := service.cache[key]
	if !found {
		return Result{}, false
	}
	service.touch(key)
	return cloneResult(result), true
}

func (service *Service) store(key [sha256.Size]byte, result Result) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if _, found := service.cache[key]; found {
		service.cache[key] = cloneResult(result)
		service.touch(key)
		return
	}
	service.cache[key] = cloneResult(result)
	service.order = append(service.order, key)
	for len(service.order) > service.config.maxCacheEntries {
		oldest := service.order[0]
		service.order = service.order[1:]
		delete(service.cache, oldest)
	}
}

func (service *Service) touch(key [sha256.Size]byte) {
	for index, item := range service.order {
		if item == key {
			service.order = append(
				append(service.order[:index], service.order[index+1:]...),
				key,
			)
			return
		}
	}
}
