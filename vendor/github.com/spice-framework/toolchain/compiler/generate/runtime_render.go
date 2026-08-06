package generate

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/spice-framework/toolchain/compiler/application"
	compilerasync "github.com/spice-framework/toolchain/compiler/async"
	"github.com/spice-framework/toolchain/compiler/provider"
	compilerschedule "github.com/spice-framework/toolchain/compiler/schedule"
)

func writeAsyncApplicationFields(
	source *bytes.Buffer,
	tasks []compilerasync.Task,
	aliases map[string]string,
) {
	for _, task := range tasks {
		fmt.Fprintf(
			source,
			"\t%s func(%s) error\n",
			asyncFieldName(task),
			strings.Join(asyncParameterTypes(task, aliases), ", "),
		)
	}
}

func writeAsyncSetup(
	source *bytes.Buffer,
	tasks []compilerasync.Task,
	providerVariables map[string]string,
	providerModules map[string]string,
	applicationModule string,
	aliases map[string]string,
) {
	if len(tasks) == 0 {
		return
	}
	source.WriteString("\tasyncConcurrency, err := configurationSnapshot.Integer(")
	source.WriteString(strconv.Quote(asyncConcurrencyKey))
	source.WriteString(")\n")
	source.WriteString("\tif err != nil {\n")
	source.WriteString("\t\treturn nil, application.coordinator.Abort(ctx, fmt.Errorf(\"decode generated async concurrency: %w\", err))\n")
	source.WriteString("\t}\n")
	source.WriteString("\tif asyncConcurrency < 1 || uint64(asyncConcurrency) > uint64(^uint(0)>>1) {\n")
	source.WriteString("\t\treturn nil, application.coordinator.Abort(ctx, fmt.Errorf(\"decode generated async concurrency: value must fit a positive int\"))\n")
	source.WriteString("\t}\n")
	source.WriteString("\tasyncContext := options.AsyncContext\n")
	source.WriteString("\tif asyncContext == nil {\n")
	source.WriteString("\t\tasyncContext = context.WithoutCancel(ctx)\n")
	source.WriteString("\t}\n")
	source.WriteString("\tgeneratedAsyncExecutor, err := spiceasync.NewExecutor(\n")
	source.WriteString("\t\tasyncContext,\n")
	source.WriteString("\t\tint(asyncConcurrency),\n")
	source.WriteString("\t\toptions.AsyncObservers...,\n")
	source.WriteString("\t)\n")
	source.WriteString("\tif err != nil {\n")
	source.WriteString("\t\treturn nil, application.coordinator.Abort(ctx, fmt.Errorf(\"construct generated async executor: %w\", err))\n")
	source.WriteString("\t}\n")
	fmt.Fprintf(
		source,
		"\tif err := application.coordinator.RegisterModuleCleanup(%s, \"spice.async\", generatedAsyncExecutor.Shutdown); err != nil {\n",
		strconv.Quote(applicationModule),
	)
	source.WriteString("\t\treturn nil, application.coordinator.Abort(ctx, fmt.Errorf(\"register generated async cleanup: %w\", err))\n")
	source.WriteString("\t}\n")
	source.WriteString("\tapplication.asyncExecutor = generatedAsyncExecutor\n")
	for _, task := range tasks {
		writeAsyncSubmitClosure(
			source,
			task,
			providerVariables[task.ProviderID],
			asyncTaskModule(task, providerModules),
			aliases,
		)
	}
}

func writeAsyncSubmitClosure(
	source *bytes.Buffer,
	task compilerasync.Task,
	providerVariable string,
	module string,
	aliases map[string]string,
) {
	field := asyncFieldName(task)
	declarations := asyncParameterDeclarations(task, aliases)
	arguments := asyncArgumentNames(task)
	fmt.Fprintf(
		source,
		"\tapplication.%s = func(%s) error {\n",
		field,
		strings.Join(declarations, ", "),
	)
	source.WriteString("\t\treturn generatedAsyncExecutor.Submit(\n")
	source.WriteString("\t\t\tadmission,\n")
	source.WriteString("\t\t\tspiceasync.Definition{\n")
	fmt.Fprintf(source, "\t\t\t\tID: %s,\n", strconv.Quote(task.MethodID))
	fmt.Fprintf(source, "\t\t\t\tModule: %s,\n", strconv.Quote(module))
	source.WriteString("\t\t\t},\n")
	source.WriteString("\t\t\tfunc(taskContext context.Context) error {\n")
	fmt.Fprintf(
		source,
		"\t\t\t\treturn %s.%s(taskContext%s)\n",
		providerVariable,
		task.Method.Name,
		asyncInvocationArguments(arguments),
	)
	source.WriteString("\t\t\t},\n")
	source.WriteString("\t\t)\n")
	source.WriteString("\t}\n")
}

func writeAsyncApplicationMethods(
	source *bytes.Buffer,
	tasks []compilerasync.Task,
	aliases map[string]string,
) {
	if len(tasks) != 0 {
		source.WriteString("\n")
	}
	for _, task := range tasks {
		declarations := asyncParameterDeclarations(task, aliases)
		arguments := asyncArgumentNames(task)
		fmt.Fprintf(
			source,
			"func (application *Application) %s(%s) error {\n",
			task.SubmitMethod,
			strings.Join(declarations, ", "),
		)
		fmt.Fprintf(
			source,
			"\tif application == nil || application.%s == nil {\n",
			asyncFieldName(task),
		)
		fmt.Fprintf(
			source,
			"\t\treturn fmt.Errorf(%s)\n",
			strconv.Quote(
				"submit asynchronous task "+task.MethodID+
					": application is nil",
			),
		)
		source.WriteString("\t}\n")
		source.WriteString("\tif state := application.State(); state != spicelifecycle.StateReady {\n")
		fmt.Fprintf(
			source,
			"\t\treturn fmt.Errorf(%s, state)\n",
			strconv.Quote(
				"submit asynchronous task "+task.MethodID+
					": application state %s is not ready",
			),
		)
		source.WriteString("\t}\n")
		fmt.Fprintf(
			source,
			"\treturn application.%s(%s)\n",
			asyncFieldName(task),
			strings.Join(arguments, ", "),
		)
		source.WriteString("}\n\n")
	}
	if len(tasks) == 0 {
		return
	}
	source.WriteString("func (application *Application) AsyncSnapshot() spiceasync.Snapshot {\n")
	source.WriteString("\tif application == nil || application.asyncExecutor == nil {\n")
	source.WriteString("\t\treturn spiceasync.Snapshot{Closed: true}\n")
	source.WriteString("\t}\n")
	source.WriteString("\treturn application.asyncExecutor.Snapshot()\n")
	source.WriteString("}\n\n")
}

func asyncParameterTypes(
	task compilerasync.Task,
	aliases map[string]string,
) []string {
	result := []string{"context.Context"}
	for _, parameter := range task.Parameters() {
		result = append(result, renderedType(parameter.Type, aliases))
	}
	return result
}

func asyncParameterDeclarations(
	task compilerasync.Task,
	aliases map[string]string,
) []string {
	result := []string{"admission context.Context"}
	for _, parameter := range task.Parameters() {
		result = append(
			result,
			fmt.Sprintf(
				"argument%d %s",
				parameter.Index,
				renderedType(parameter.Type, aliases),
			),
		)
	}
	return result
}

func asyncArgumentNames(task compilerasync.Task) []string {
	result := []string{"admission"}
	for _, parameter := range task.Parameters() {
		result = append(result, "argument"+strconv.Itoa(parameter.Index))
	}
	return result
}

func asyncInvocationArguments(arguments []string) string {
	if len(arguments) < 2 {
		return ""
	}
	return ", " + strings.Join(arguments[1:], ", ")
}

func asyncFieldName(task compilerasync.Task) string {
	return "submit" + strings.TrimPrefix(task.SubmitMethod, "Submit")
}

func asyncTaskModule(
	task compilerasync.Task,
	providerModules map[string]string,
) string {
	if module := providerModules[task.ProviderID]; module != "" {
		return module
	}
	return task.Method.PackagePath
}

func writeHooks(
	source *bytes.Buffer,
	model application.Model,
	providerVariables map[string]string,
	providerModules map[string]string,
	applicationModule string,
) {
	components := model.Components()
	jobs := model.Jobs()
	if len(components) == 0 && len(jobs) == 0 {
		return
	}
	source.WriteString("\tapplication.hooks = []spicelifecycle.Hook{\n")
	for _, component := range components {
		variable := providerVariables[component.Provider.SymbolID]
		source.WriteString("\t\t{\n")
		fmt.Fprintf(source, "\t\t\tID: %s,\n", strconv.Quote(component.Provider.SymbolID))
		fmt.Fprintf(
			source,
			"\t\t\tModule: %s,\n",
			strconv.Quote(providerModules[component.Provider.SymbolID]),
		)
		fmt.Fprintf(source, "\t\t\tStart: %s.%s,\n", variable, component.Start.Method.Name)
		if component.Stop != nil {
			fmt.Fprintf(source, "\t\t\tStop: %s.%s,\n", variable, component.Stop.Method.Name)
		}
		source.WriteString("\t\t},\n")
	}
	if len(jobs) != 0 {
		source.WriteString("\t\t{\n")
		source.WriteString("\t\t\tID: \"spice.schedule\",\n")
		fmt.Fprintf(
			source,
			"\t\t\tModule: %s,\n",
			strconv.Quote(applicationModule),
		)
		source.WriteString("\t\t\tStart: generatedScheduler.Start,\n")
		source.WriteString("\t\t\tStop: generatedScheduler.Shutdown,\n")
		source.WriteString("\t\t},\n")
	}
	source.WriteString("\t}\n")
}

func writeScheduleSetup(
	source *bytes.Buffer,
	jobs []compilerschedule.Job,
	providerVariables map[string]string,
	providerModules map[string]string,
) {
	if len(jobs) == 0 {
		return
	}
	source.WriteString("\tscheduleContext := options.ScheduleContext\n")
	source.WriteString("\tif scheduleContext == nil {\n")
	source.WriteString("\t\tscheduleContext = context.WithoutCancel(ctx)\n")
	source.WriteString("\t}\n")
	source.WriteString("\tgeneratedScheduler, err := spiceschedule.New(\n")
	source.WriteString("\t\tscheduleContext,\n")
	source.WriteString("\t\t[]spiceschedule.Job{\n")
	for _, job := range jobs {
		module := providerModules[job.ProviderID]
		if module == "" {
			module = job.Method.PackagePath
		}
		source.WriteString("\t\t\t{\n")
		source.WriteString("\t\t\t\tDefinition: spiceschedule.Definition{\n")
		fmt.Fprintf(
			source,
			"\t\t\t\t\tID: %s,\n",
			strconv.Quote(job.MethodID),
		)
		fmt.Fprintf(
			source,
			"\t\t\t\t\tModule: %s,\n",
			strconv.Quote(module),
		)
		source.WriteString("\t\t\t\t},\n")
		if job.InitialDelay != 0 {
			fmt.Fprintf(
				source,
				"\t\t\t\tInitialDelay: %d,\n",
				job.InitialDelay,
			)
		}
		fmt.Fprintf(
			source,
			"\t\t\t\tDelay: %d,\n",
			job.Delay,
		)
		if job.ContinueOnError {
			source.WriteString("\t\t\t\tContinueOnError: true,\n")
		}
		fmt.Fprintf(
			source,
			"\t\t\t\tRun: %s.%s,\n",
			providerVariables[job.ProviderID],
			job.Method.Name,
		)
		source.WriteString("\t\t\t},\n")
	}
	source.WriteString("\t\t},\n")
	source.WriteString("\t\toptions.ScheduleWaiter,\n")
	source.WriteString("\t\toptions.ScheduleObservers...,\n")
	source.WriteString("\t)\n")
	source.WriteString("\tif err != nil {\n")
	source.WriteString("\t\treturn nil, application.coordinator.Abort(ctx, fmt.Errorf(\"construct generated scheduler: %w\", err))\n")
	source.WriteString("\t}\n")
}

func writeLifecycleMethods(source *bytes.Buffer) {
	source.WriteString(`func (application *Application) State() spicelifecycle.State {
	if application == nil || application.coordinator == nil {
		return spicelifecycle.StateInvalid
	}
	return application.coordinator.State()
}

func (application *Application) Start(ctx context.Context) error {
	if application == nil || application.coordinator == nil {
		return fmt.Errorf("start application: application is nil")
	}
	return application.coordinator.Start(ctx, application.hooks)
}

func (application *Application) Stop(ctx context.Context) error {
	if application == nil || application.coordinator == nil {
		return fmt.Errorf("stop application: application is nil")
	}
	return application.coordinator.Stop(ctx)
}

func (application *Application) ShutdownTimeout() time.Duration {
	if application == nil {
		return 0
	}
	return application.shutdownTimeout
}

func (application *Application) RegisterObserver(observer spicelifecycle.Observer) error {
	if application == nil || application.coordinator == nil {
		return fmt.Errorf("register lifecycle observer: application is nil")
	}
	return application.coordinator.RegisterObserver(observer)
}

func (application *Application) Run(ctx context.Context, shutdown spicelifecycle.ContextFactory) error {
	if application == nil || application.coordinator == nil {
		return fmt.Errorf("run application: application is nil")
	}
	return application.coordinator.Run(ctx, application.hooks, shutdown)
}
`)
}

func writeHandlerMethod(source *bytes.Buffer) {
	source.WriteString(`
func (application *Application) Handler() http.Handler {
	if application == nil {
		return nil
	}
	return application.handler
}
`)
}

func providerModuleIDs(
	model application.Model,
	providers []provider.Provider,
) map[string]string {
	packageModules := make(map[string]string)
	for _, module := range model.Modules() {
		for _, pkg := range module.Packages() {
			packageModules[pkg.Path] = module.ID
		}
	}
	result := make(map[string]string, len(providers))
	for _, item := range providers {
		result[item.SymbolID] = packageModules[item.PackagePath]
	}
	return result
}
