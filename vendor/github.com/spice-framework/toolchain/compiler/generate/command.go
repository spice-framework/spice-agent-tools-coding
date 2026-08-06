package generate

import (
	"bytes"
	"fmt"
	"slices"
	"strconv"

	"github.com/spice-framework/spice/annotation"
	"github.com/spice-framework/toolchain/compiler/application"
	compilerbootstrap "github.com/spice-framework/toolchain/compiler/bootstrap"
	"github.com/spice-framework/toolchain/compiler/provider"
)

type modelHashBootstrapOption struct {
	Name  string           `json:"name"`
	Value annotation.Value `json:"value"`
}

type modelHashBootstrapFeature struct {
	Annotation    string                         `json:"annotation"`
	Capability    string                         `json:"capability"`
	SourceID      string                         `json:"source_id,omitempty"`
	SourceVersion string                         `json:"source_version,omitempty"`
	Options       []modelHashBootstrapOption     `json:"options"`
	Requirements  []string                       `json:"requirements"`
	EntryPoints   []modelHashBootstrapEntryPoint `json:"entry_points,omitempty"`
}

type modelHashBootstrapEntryPoint struct {
	Package string `json:"package"`
	Symbol  string `json:"symbol"`
}

type commandFeatures struct {
	endpoints        []compilerbootstrap.Endpoint
	managementAccess string
	management       bool
	logging          bool
	metrics          bool
	configProps      bool
	modules          bool
	hasMux           bool
	authorization    bool
	scheduling       bool
	asynchronous     bool
	transactions     bool
	events           bool
	caching          bool
	httpObservation  bool
	requestScope     bool
}

func commandFeaturesFor(
	target application.Target,
	hasControllers bool,
) commandFeatures {
	metadata := target.Bootstrap()
	management, managementEnabled := metadata.Management()
	endpoints := management.Endpoints()
	httpObservation := metadata.Enabled(
		compilerbootstrap.CapabilityHTTPObservation,
	)
	return commandFeatures{
		endpoints:        endpoints,
		managementAccess: management.Access(),
		management:       managementEnabled,
		logging:          metadata.Enabled(compilerbootstrap.CapabilityLogging),
		metrics:          slices.Contains(endpoints, compilerbootstrap.EndpointMetrics),
		configProps:      slices.Contains(endpoints, compilerbootstrap.EndpointConfigProps),
		modules:          slices.Contains(endpoints, compilerbootstrap.EndpointModules),
		hasMux:           hasControllers || managementEnabled || httpObservation,
		httpObservation:  httpObservation,
	}
}

func bootstrapHashInput(target application.Target) []modelHashBootstrapFeature {
	var result []modelHashBootstrapFeature
	for _, feature := range target.Bootstrap().Features() {
		item := modelHashBootstrapFeature{
			Annotation:    feature.Annotation,
			Capability:    string(feature.Capability),
			SourceID:      feature.SourceID,
			SourceVersion: feature.SourceVersion,
		}
		for _, option := range feature.Options() {
			item.Options = append(item.Options, modelHashBootstrapOption{
				Name:  option.Name,
				Value: option.Value(),
			})
		}
		for _, requirement := range feature.Requirements() {
			item.Requirements = append(item.Requirements, string(requirement))
		}
		for _, entryPoint := range feature.EntryPoints() {
			item.EntryPoints = append(item.EntryPoints, modelHashBootstrapEntryPoint{
				Package: entryPoint.Package,
				Symbol:  entryPoint.Symbol,
			})
		}
		result = append(result, item)
	}
	return result
}

func writeGeneratedConstants(
	source *bytes.Buffer,
	targetIDExpression string,
) {
	fmt.Fprintf(source, "const TargetID = %s\n\n", targetIDExpression)
	source.WriteString(`const (
	ExitSuccess = 0
	ExitFailure = 1
	ExitUsage = 2
)

`)
}

func writeBootstrapObservers(source *bytes.Buffer, features commandFeatures) {
	source.WriteString("\tobservers := append([]spicelifecycle.Observer(nil), options.Observers...)\n")
	if features.hasMux {
		source.WriteString("\thttpObservers := append([]spiceweb.HTTPObserver(nil), options.HTTPObservers...)\n")
		source.WriteString("\t_ = httpObservers\n")
	}
	if features.metrics {
		source.WriteString("\tmanagementMetrics := spicemanagement.NewHTTPMetrics()\n")
		source.WriteString("\thttpObservers = append([]spiceweb.HTTPObserver{managementMetrics}, httpObservers...)\n")
	}
	if !features.logging {
		return
	}
	source.WriteString("\tlogger := options.Logger\n")
	source.WriteString("\tif logger == nil {\n")
	source.WriteString("\t\tlogger = slog.New(slog.NewJSONHandler(os.Stderr, nil))\n")
	source.WriteString("\t}\n")
	source.WriteString("\tlifecycleLogs, err := spiceobservability.NewSlogLifecycleObserver(logger)\n")
	source.WriteString("\tif err != nil {\n")
	source.WriteString("\t\treturn nil, application.coordinator.Abort(ctx, fmt.Errorf(\"configure lifecycle logging: %w\", err))\n")
	source.WriteString("\t}\n")
	source.WriteString("\tobservers = append([]spicelifecycle.Observer{lifecycleLogs}, observers...)\n")
	if features.hasMux {
		source.WriteString("\thttpLogs, err := spiceobservability.NewSlogHTTPObserver(logger)\n")
		source.WriteString("\tif err != nil {\n")
		source.WriteString("\t\treturn nil, application.coordinator.Abort(ctx, fmt.Errorf(\"configure HTTP logging: %w\", err))\n")
		source.WriteString("\t}\n")
		if features.metrics {
			source.WriteString("\thttpObservers = append(httpObservers[:1], append([]spiceweb.HTTPObserver{httpLogs}, httpObservers[1:]...)...)\n")
		} else {
			source.WriteString("\thttpObservers = append([]spiceweb.HTTPObserver{httpLogs}, httpObservers...)\n")
		}
	}
}

func writeFeatureHTTPObservers(
	source *bytes.Buffer,
	target application.Target,
	providers []provider.Provider,
	providerVariables map[string]string,
) error {
	for _, feature := range target.Bootstrap().Features() {
		if feature.Capability != compilerbootstrap.CapabilityHTTPObservation {
			continue
		}
		for _, entrypoint := range feature.EntryPoints() {
			var matches []provider.Provider
			for _, item := range providers {
				if item.Source == provider.SourceStarter &&
					item.SourceID == feature.SourceID &&
					item.SourceVersion == feature.SourceVersion &&
					item.PackagePath == entrypoint.Package &&
					item.Name == entrypoint.Symbol {
					matches = append(matches, item)
				}
			}
			if len(matches) != 1 {
				return fmt.Errorf(
					"HTTP observation feature @%s entrypoint %s.%s has %d selected providers",
					feature.Annotation,
					entrypoint.Package,
					entrypoint.Symbol,
					len(matches),
				)
			}
			variable := providerVariables[matches[0].SymbolID]
			if variable == "" {
				return fmt.Errorf(
					"HTTP observation feature @%s entrypoint %s.%s has no generated provider variable",
					feature.Annotation,
					entrypoint.Package,
					entrypoint.Symbol,
				)
			}
			fmt.Fprintf(
				source,
				"\thttpObservers = append(httpObservers, %s)\n",
				variable,
			)
		}
	}
	return nil
}

func writeAuthorizationSetup(
	source *bytes.Buffer,
	features commandFeatures,
) {
	if !features.authorization {
		return
	}
	source.WriteString("\tauthorizer, authorizationErr := spicesecurity.NewAuthorizer(options.AuthorizationObservers...)\n")
	source.WriteString("\tif authorizationErr != nil {\n")
	source.WriteString("\t\treturn nil, application.coordinator.Abort(ctx, fmt.Errorf(\"construct generated authorizer: %w\", authorizationErr))\n")
	source.WriteString("\t}\n")
}

func writeRouteMux(
	source *bytes.Buffer,
	providers []provider.Provider,
	providerVariables map[string]string,
) {
	muxVariable := ""
	for _, item := range providers {
		if pointerNamedType(item.Output, "net/http", "ServeMux") {
			muxVariable = providerVariables[item.SymbolID]
			break
		}
	}
	if muxVariable == "" {
		source.WriteString("\trouteMux := http.NewServeMux()\n")
	} else {
		fmt.Fprintf(source, "\trouteMux := %s\n", muxVariable)
	}
	source.WriteString("\tapplication.mux = routeMux\n")
	source.WriteString("\tapplication.handler = routeMux\n")
}

func writeManagementSetup(
	source *bytes.Buffer,
	model application.Model,
	target application.Target,
	features commandFeatures,
) {
	if !features.management {
		return
	}
	fmt.Fprintf(
		source,
		"\tmanagementChecks, err := spicemanagement.LifecycleChecks(TargetID, %s, application.State)\n",
		strconv.Quote(target.PackagePath),
	)
	source.WriteString("\tif err != nil {\n")
	source.WriteString("\t\treturn nil, application.coordinator.Abort(ctx, fmt.Errorf(\"configure management lifecycle checks: %w\", err))\n")
	source.WriteString("\t}\n")
	source.WriteString("\tmanagementManager, err := spicemanagement.New(managementChecks...)\n")
	source.WriteString("\tif err != nil {\n")
	source.WriteString("\t\treturn nil, application.coordinator.Abort(ctx, fmt.Errorf(\"configure management checks: %w\", err))\n")
	source.WriteString("\t}\n")
	if features.configProps {
		source.WriteString("\tmanagementConfiguration, err := spicemanagement.NewConfigurationReport(configurationSchema, configurationSnapshot)\n")
		source.WriteString("\tif err != nil {\n")
		source.WriteString("\t\treturn nil, application.coordinator.Abort(ctx, fmt.Errorf(\"configure management configuration report: %w\", err))\n")
		source.WriteString("\t}\n")
	}
	if features.modules {
		writeManagementModuleReport(source, model)
	}
	source.WriteString("\tmanagementHandler, err := spicemanagement.NewHandler(spicemanagement.HandlerOptions{\n")
	source.WriteString("\t\tManager: managementManager,\n")
	source.WriteString("\t\tInfo: map[string]string{\n")
	source.WriteString("\t\t\t\"application\": TargetID,\n")
	fmt.Fprintf(source, "\t\t\t\"module\": %s,\n", strconv.Quote(target.PackagePath))
	source.WriteString("\t\t\t\"framework\": \"Spice\",\n")
	source.WriteString("\t\t},\n")
	if features.metrics {
		source.WriteString("\t\tMetrics: managementMetrics,\n")
	}
	if features.configProps {
		source.WriteString("\t\tConfiguration: &managementConfiguration,\n")
	}
	if features.modules {
		source.WriteString("\t\tModules: &managementModules,\n")
	}
	source.WriteString("\t\tExpose: []spicemanagement.Endpoint{\n")
	for _, endpoint := range features.endpoints {
		fmt.Fprintf(source, "\t\t\t%s,\n", managementEndpointName(endpoint))
	}
	source.WriteString("\t\t},\n")
	fmt.Fprintf(
		source,
		"\t\tAccess: spicemanagement.Access(%s),\n",
		strconv.Quote(features.managementAccess),
	)
	source.WriteString("\t})\n")
	source.WriteString("\tif err != nil {\n")
	source.WriteString("\t\treturn nil, application.coordinator.Abort(ctx, fmt.Errorf(\"configure management handler: %w\", err))\n")
	source.WriteString("\t}\n")
	source.WriteString("\tif err := spiceweb.Register(routeMux, managementHandler.Pattern(), managementHandler); err != nil {\n")
	source.WriteString("\t\treturn nil, application.coordinator.Abort(ctx, fmt.Errorf(\"register management routes: %w\", err))\n")
	source.WriteString("\t}\n")
}

func writeManagementModuleReport(
	source *bytes.Buffer,
	model application.Model,
) {
	source.WriteString("\tmanagementModules, err := spicemanagement.NewModuleReport(\n")
	source.WriteString("\t\t[]spicemanagement.ModuleDefinition{\n")
	for _, module := range model.Modules() {
		source.WriteString("\t\t\t{\n")
		fmt.Fprintf(source, "\t\t\t\tID: %s,\n", strconv.Quote(module.ID))
		fmt.Fprintf(
			source,
			"\t\t\t\tRootPackage: %s,\n",
			strconv.Quote(module.RootPackage),
		)
		packages := module.Packages()
		packagePaths := make([]string, len(packages))
		for index, item := range packages {
			packagePaths[index] = item.Path
		}
		writeManagementStringSlice(source, "Packages", packagePaths)
		interfaces := module.NamedInterfaces()
		if len(interfaces) != 0 {
			source.WriteString("\t\t\t\tNamedInterfaces: []spicemanagement.NamedInterface{\n")
			for _, item := range interfaces {
				fmt.Fprintf(
					source,
					"\t\t\t\t\t{Name: %s, PackagePath: %s},\n",
					strconv.Quote(item.Name),
					strconv.Quote(item.PackagePath),
				)
			}
			source.WriteString("\t\t\t\t},\n")
		}
		dependencies := module.AllowedDependencies()
		allowed := make([]string, len(dependencies))
		for index, dependency := range dependencies {
			allowed[index] = dependency.String()
		}
		writeManagementStringSlice(source, "AllowedDependencies", allowed)
		source.WriteString("\t\t\t},\n")
	}
	source.WriteString("\t\t},\n")
	source.WriteString("\t\t[]spicemanagement.ModuleEdge{\n")
	for _, edge := range model.ModuleEdges() {
		api := "default"
		if edge.API != "" {
			api = edge.API
		}
		fmt.Fprintf(
			source,
			"\t\t\t{FromModule: %s, ToModule: %s, API: %s, FromPackage: %s, ToPackage: %s},\n",
			strconv.Quote(edge.FromModule),
			strconv.Quote(edge.ToModule),
			strconv.Quote(api),
			strconv.Quote(edge.FromPackage),
			strconv.Quote(edge.ToPackage),
		)
	}
	source.WriteString("\t\t},\n")
	source.WriteString("\t\t[]string{\n")
	for _, item := range model.UnassignedPackages() {
		fmt.Fprintf(source, "\t\t\t%s,\n", strconv.Quote(item.Path))
	}
	source.WriteString("\t\t},\n")
	source.WriteString("\t)\n")
	source.WriteString("\tif err != nil {\n")
	source.WriteString("\t\treturn nil, application.coordinator.Abort(ctx, fmt.Errorf(\"configure management module report: %w\", err))\n")
	source.WriteString("\t}\n")
}

func writeManagementStringSlice(
	source *bytes.Buffer,
	name string,
	values []string,
) {
	if len(values) == 0 {
		return
	}
	fmt.Fprintf(source, "\t\t\t\t%s: []string{\n", name)
	for _, value := range values {
		fmt.Fprintf(source, "\t\t\t\t\t%s,\n", strconv.Quote(value))
	}
	source.WriteString("\t\t\t\t},\n")
}

func managementEndpointName(endpoint compilerbootstrap.Endpoint) string {
	switch endpoint {
	case compilerbootstrap.EndpointHealth:
		return "spicemanagement.EndpointHealth"
	case compilerbootstrap.EndpointLiveness:
		return "spicemanagement.EndpointLiveness"
	case compilerbootstrap.EndpointReadiness:
		return "spicemanagement.EndpointReadiness"
	case compilerbootstrap.EndpointInfo:
		return "spicemanagement.EndpointInfo"
	case compilerbootstrap.EndpointMetrics:
		return "spicemanagement.EndpointMetrics"
	case compilerbootstrap.EndpointConfigProps:
		return "spicemanagement.EndpointConfigProps"
	case compilerbootstrap.EndpointModules:
		return "spicemanagement.EndpointModules"
	}
	return strconv.Quote(string(endpoint))
}

func writeCommandAPI(
	source *bytes.Buffer,
	features commandFeatures,
) {
	source.WriteString(`
type ShutdownContextFactory func(time.Duration) (context.Context, context.CancelFunc)

type CommandOptions struct {
	Context context.Context
	Arguments []string
	Stdout io.Writer
	Stderr io.Writer
	Logger *slog.Logger
	ShutdownTimeout time.Duration
	ShutdownContext ShutdownContextFactory
	Application ApplicationOptions
}

`)
	source.WriteString("func Main(arguments []string) int {\n")
	source.WriteString(`	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	environment, err := spiceconfig.OSEnvironment("SPICE_")
	if err != nil {
		logger.Error("Spice command configuration failed", slog.Any("error", err))
		return ExitFailure
	}
	runContext, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	return RunCommand(CommandOptions{
		Context: runContext,
		Arguments: arguments,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Logger: logger,
		Application: ApplicationOptions{
			Sources: []spiceconfig.Source{environment},
		},
	})
}

func RunCommand(options CommandOptions) int {
	stdout := options.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	stderr := options.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(stderr, nil))
	}
	ctx := options.Context
	if ctx == nil {
		return commandFailure(context.Background(), logger, "Spice command context is nil", nil)
	}
	if options.ShutdownTimeout < 0 {
		return commandFailure(ctx, logger, "Spice shutdown timeout is invalid", nil)
	}

	flags := flag.NewFlagSet(TargetID, flag.ContinueOnError)
	flags.SetOutput(stderr)
	check := flags.Bool("check", false, "construct the generated application and exit")
	if err := flags.Parse(options.Arguments); err != nil {
		logger.ErrorContext(ctx, "Spice command arguments are invalid", slog.Any("error", err))
		return ExitUsage
	}
	if flags.NArg() != 0 {
		if _, err := fmt.Fprintf(stderr, "%s does not accept positional arguments\n", TargetID); err != nil {
			logger.ErrorContext(ctx, "Spice command usage output failed", slog.Any("error", err))
		}
		return ExitUsage
	}

	applicationOptions := options.Application
`)
	if features.logging {
		source.WriteString("\tapplicationOptions.Logger = logger\n")
	}
	source.WriteString(`	logger.InfoContext(ctx, "Spice application constructing", slog.String("application", TargetID))
	application, err := NewApplicationWithOptions(ctx, applicationOptions)
	if err != nil {
		return commandFailure(ctx, logger, "Spice application construction failed", err)
	}

	shutdownTimeout := application.shutdownTimeout
	if options.ShutdownTimeout > 0 {
		shutdownTimeout = options.ShutdownTimeout
	}
	shutdownContext := options.ShutdownContext
	if shutdownContext == nil {
		shutdownContext = func(timeout time.Duration) (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), timeout)
		}
	}
	shutdown := func() (context.Context, context.CancelFunc) {
		return shutdownContext(shutdownTimeout)
	}

	if *check {
		if err := stopCheckedApplication(application, shutdown); err != nil {
			return commandFailure(ctx, logger, "Spice application check cleanup failed", err)
		}
		if _, err := fmt.Fprintf(stdout, "Spice %s ready.\n", TargetID); err != nil {
			return commandFailure(ctx, logger, "Spice readiness output failed", err)
		}
		return ExitSuccess
	}

	logger.InfoContext(ctx, "Spice application starting", slog.String("application", TargetID))
	if err := application.Run(ctx, shutdown); err != nil {
		return commandFailure(ctx, logger, "Spice application run failed", err)
	}
	logger.InfoContext(context.Background(), "Spice application stopped", slog.String("application", TargetID))
	return ExitSuccess
}

func stopCheckedApplication(
	application *Application,
	shutdown spicelifecycle.ContextFactory,
) error {
	shutdownContext, cancel := shutdown()
	if shutdownContext == nil || cancel == nil {
		return fmt.Errorf("check application: shutdown context factory returned a nil context or cancel function")
	}
	defer cancel()
	return application.Stop(shutdownContext)
}

func commandFailure(
	ctx context.Context,
	logger *slog.Logger,
	message string,
	err error,
) int {
	if err == nil {
		logger.ErrorContext(ctx, message)
	} else {
		logger.ErrorContext(ctx, message, slog.Any("error", err))
	}
	return ExitFailure
}
`)
}
