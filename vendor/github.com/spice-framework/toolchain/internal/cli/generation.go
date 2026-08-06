package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spice-framework/toolchain/compiler/application"
	"github.com/spice-framework/toolchain/compiler/diagnostic"
	codegen "github.com/spice-framework/toolchain/compiler/generate"
	"github.com/spice-framework/toolchain/compiler/load"
	compilerservice "github.com/spice-framework/toolchain/compiler/service"
	"github.com/spice-framework/toolchain/internal/genfs"
)

type generationArguments struct {
	check              bool
	diff               bool
	target             string
	relocateModuleFrom string
	patterns           []string
}

// NewGenerateHandler constructs the guarded generation command handler.
func NewGenerateHandler(runtime *Runtime) (Handler, error) {
	return newLoaderCommandHandler(
		runtime,
		[]string{"generate"},
		func(runtime *Runtime, invocation Invocation) int {
			return generateCommand(
				invocation.Arguments,
				invocation.Stdout,
				invocation.Stderr,
				runtime.options,
				runtime.loader,
			)
		},
	)
}

// NewBuildHandler constructs the generated-application build command handler.
func NewBuildHandler(runtime *Runtime) (Handler, error) {
	return newBuildCommandHandler(
		runtime,
		[]string{"build"},
		func(runtime *Runtime, invocation Invocation) int {
			return buildCommand(
				invocation.Arguments,
				invocation.Stdout,
				invocation.Stderr,
				runtime.options,
				runtime.loader,
				runtime.builder,
			)
		},
	)
}

func generateCommand(
	arguments []string,
	stdout io.Writer,
	stderr io.Writer,
	options load.Options,
	loader programLoader,
) int {
	parsed, err := parseGenerationArguments(arguments, true)
	if err != nil {
		if writeErr := writef(stderr, "Spice generation failed: %v\n", err); writeErr != nil {
			return 1
		}
		return 2
	}
	plan, targetName, ok := prepareGeneration(parsed, stderr, options, loader)
	if !ok {
		return 1
	}
	if parsed.check || parsed.diff {
		return checkGeneration(plan, targetName, parsed.diff, stdout, stderr)
	}
	result, err := genfs.ApplyWithOptions(
		plan,
		genfs.ApplyOptions{RelocateModuleFrom: parsed.relocateModuleFrom},
	)
	if err != nil {
		if writeErr := writef(stderr, "Spice generation failed for target %s: %v\n", targetName, err); writeErr != nil {
			return 1
		}
		return 1
	}
	if !result.Changed() {
		if err := writef(stdout, "Spice generation is current for target %s.\n", targetName); err != nil {
			return 1
		}
		return 0
	}
	if err := writef(
		stdout,
		"Spice generated target %s: wrote %d file(s), removed %d stale file(s), manifest updated=%t.\n",
		targetName,
		len(result.Written),
		len(result.Removed),
		result.ManifestUpdated,
	); err != nil {
		return 1
	}
	return 0
}

func buildCommand(
	arguments []string,
	stdout io.Writer,
	stderr io.Writer,
	options load.Options,
	loader programLoader,
	builder buildExecutor,
) int {
	parsed, err := parseGenerationArguments(arguments, false)
	if err != nil {
		if writeErr := writef(stderr, "Spice build failed: %v\n", err); writeErr != nil {
			return 1
		}
		return 2
	}
	plan, targetName, ok := prepareGeneration(parsed, stderr, options, loader)
	if !ok {
		return 1
	}
	result, err := genfs.Apply(plan)
	if err != nil {
		if writeErr := writef(stderr, "Spice build generation failed for target %s: %v\n", targetName, err); writeErr != nil {
			return 1
		}
		return 1
	}
	if result.Changed() {
		if err := writef(
			stdout,
			"Spice generated target %s: wrote %d file(s), removed %d stale file(s), manifest updated=%t.\n",
			targetName,
			len(result.Written),
			len(result.Removed),
			result.ManifestUpdated,
		); err != nil {
			return 1
		}
	}
	if err := builder(context.Background(), plan.Target().ModuleRoot, stdout, stderr); err != nil {
		if writeErr := writef(stderr, "Spice build failed for target %s: %v\n", targetName, err); writeErr != nil {
			return 1
		}
		return 1
	}
	if err := writef(stdout, "Spice build passed for target %s.\n", targetName); err != nil {
		return 1
	}
	return 0
}

func prepareGeneration(
	arguments generationArguments,
	stderr io.Writer,
	options load.Options,
	loader programLoader,
) (codegen.Plan, string, bool) {
	return prepareGenerationContext(
		context.Background(),
		arguments,
		stderr,
		options,
		loader,
	)
}

func prepareGenerationContext(
	ctx context.Context,
	arguments generationArguments,
	stderr io.Writer,
	options load.Options,
	loader programLoader,
) (codegen.Plan, string, bool) {
	service, err := newCompilerAnalysisService(options, loader)
	if err != nil {
		if writeErr := writef(stderr, "Spice generation failed: %v\n", err); writeErr != nil {
			return codegen.Plan{}, "", false
		}
		return codegen.Plan{}, "", false
	}
	root := options.Dir
	if root == "" {
		root = "."
	}
	plan, target, ready := prepareGenerationAnalysis(
		ctx,
		arguments,
		stderr,
		root,
		service,
		0,
	)
	if closeErr := closeCompilerAnalysisService(service); closeErr != nil {
		if writeErr := writef(
			stderr,
			"Spice generation failed: close annotation tools: %v\n",
			closeErr,
		); writeErr != nil {
			return codegen.Plan{}, "", false
		}
		return codegen.Plan{}, "", false
	}
	return plan, target, ready
}

func prepareGenerationAnalysis(
	ctx context.Context,
	arguments generationArguments,
	stderr io.Writer,
	root string,
	service *compilerservice.Service,
	sequence uint64,
) (codegen.Plan, string, bool) {
	patterns := arguments.patterns
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}
	result, err := service.Analyze(
		ctx,
		compilerservice.Request{
			WorkspaceRoot: root,
			Target:        arguments.target,
			Patterns:      patterns,
			Sequence:      sequence,
		},
	)
	if err != nil {
		if ctx.Err() != nil {
			return codegen.Plan{}, "", false
		}
		if writeErr := writef(stderr, "Spice generation failed: %v\n", err); writeErr != nil {
			return codegen.Plan{}, "", false
		}
		return codegen.Plan{}, "", false
	}
	if diagnostics := result.Diagnostics().Items(); len(diagnostics) != 0 {
		if err := reportDiagnostics(
			stderr,
			diagnostics,
			generationDiagnosticSummary(diagnostics),
		); err != nil {
			return codegen.Plan{}, "", false
		}
		return codegen.Plan{}, "", false
	}
	plan, found := result.GenerationPlan()
	if !found {
		if err := writef(
			stderr,
			"Spice generation failed: compiler analysis produced no generation plan.\n",
		); err != nil {
			return codegen.Plan{}, "", false
		}
		return codegen.Plan{}, "", false
	}
	return plan, result.TargetName(), true
}

func newCompilerAnalysisService(
	options load.Options,
	loader programLoader,
) (*compilerservice.Service, error) {
	if err := rejectLegacyStarterSelection(options); err != nil {
		return nil, err
	}
	return compilerservice.New(compilerservice.Config{
		Loader:         compilerservice.Loader(loader),
		ModuleVersions: loadModuleVersions,
		LoadOptions:    options,
		SpiceVersion:   Version,
	})
}

func closeCompilerAnalysisService(
	service *compilerservice.Service,
) error {
	ctx, cancel := context.WithTimeout(
		context.Background(),
		10*time.Second,
	)
	defer cancel()
	return service.Close(ctx)
}

func generationDiagnosticSummary(
	diagnostics []diagnostic.Diagnostic,
) string {
	stage := "compiler"
	if len(diagnostics) != 0 {
		segments := strings.Split(diagnostics[0].Code, ".")
		if len(segments) >= 3 {
			stage = segments[1]
		}
	}
	description := map[string]string{
		"load":        "package loading",
		"resolution":  "annotation resolution",
		"validation":  "annotation validation",
		"starter":     "starter dependency alignment",
		"provider":    "provider catalog",
		"application": "application model",
		"modulith":    "module architecture",
		"generation":  "generation",
	}[stage]
	if description == "" {
		description = "compiler"
	}
	return fmt.Sprintf(
		"Spice generation failed: %d %s error(s).",
		len(diagnostics),
		description,
	)
}

func checkGeneration(
	plan codegen.Plan,
	targetName string,
	withDiff bool,
	stdout io.Writer,
	stderr io.Writer,
) int {
	status, err := genfs.Check(plan)
	if err != nil {
		if writeErr := writef(stderr, "Spice generation check failed for target %s: %v\n", targetName, err); writeErr != nil {
			return 1
		}
		return 1
	}
	if status.Current {
		if err := writef(stdout, "Spice generation is current for target %s.\n", targetName); err != nil {
			return 1
		}
		return 0
	}
	for _, difference := range status.Differences {
		if err := writef(stderr, "%s: %s\n", difference.Path, difference.Message); err != nil {
			return 1
		}
	}
	if withDiff {
		diff, diffErr := genfs.Diff(plan)
		if diffErr != nil {
			if err := writef(stderr, "render generation diff: %v\n", diffErr); err != nil {
				return 1
			}
			return 1
		}
		if _, err := io.WriteString(stdout, diff); err != nil {
			return 1
		}
	}
	if err := writef(
		stderr,
		"Spice generation is stale for target %s: %d difference(s).\n",
		targetName,
		len(status.Differences),
	); err != nil {
		return 1
	}
	return 1
}

func parseGenerationArguments(arguments []string, allowReadOnly bool) (generationArguments, error) {
	result := generationArguments{}
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if !strings.HasPrefix(argument, "-") {
			result.patterns = append(result.patterns, argument)
			continue
		}
		name, inlineValue, hasInlineValue := strings.Cut(argument, "=")
		var err error
		switch name {
		case "--check", "--diff":
			err = parseGenerationReadOnlyFlag(
				name,
				hasInlineValue,
				allowReadOnly,
				&result,
			)
		case "--target":
			index, err = parseGenerationTarget(
				arguments,
				index,
				inlineValue,
				hasInlineValue,
				&result,
			)
		case "--relocate-module-from":
			index, err = parseGenerationRelocation(
				arguments,
				index,
				inlineValue,
				hasInlineValue,
				allowReadOnly,
				&result,
			)
		default:
			return generationArguments{}, fmt.Errorf("unknown generation option %q", argument)
		}
		if err != nil {
			return generationArguments{}, err
		}
	}
	if result.relocateModuleFrom != "" && (result.check || result.diff) {
		return generationArguments{}, errors.New("--relocate-module-from performs a guarded write and cannot be combined with --check or --diff")
	}
	return result, nil
}

func parseGenerationReadOnlyFlag(
	name string,
	hasInlineValue bool,
	allowReadOnly bool,
	result *generationArguments,
) error {
	if hasInlineValue {
		return fmt.Errorf("unknown generation option %q", name+"=")
	}
	if !allowReadOnly {
		return fmt.Errorf("%s is supported by spice generate, not spice build", name)
	}
	if name == "--check" {
		result.check = true
	} else {
		result.diff = true
	}
	return nil
}

func parseGenerationTarget(
	arguments []string,
	index int,
	inlineValue string,
	hasInlineValue bool,
	result *generationArguments,
) (int, error) {
	if result.target != "" {
		return index, errors.New("--target may be specified only once")
	}
	value, next, err := generationOptionValue(
		arguments,
		index,
		inlineValue,
		hasInlineValue,
		"--target requires an application name",
	)
	if err != nil {
		return index, err
	}
	result.target = value
	return next, nil
}

func parseGenerationRelocation(
	arguments []string,
	index int,
	inlineValue string,
	hasInlineValue bool,
	allow bool,
	result *generationArguments,
) (int, error) {
	if !allow {
		return index, errors.New("--relocate-module-from is supported by spice generate")
	}
	if result.relocateModuleFrom != "" {
		return index, errors.New("--relocate-module-from may be specified only once")
	}
	value, next, err := generationOptionValue(
		arguments,
		index,
		inlineValue,
		hasInlineValue,
		"--relocate-module-from requires a previous Go module path",
	)
	if err != nil {
		return index, err
	}
	result.relocateModuleFrom = value
	return next, nil
}

func generationOptionValue(
	arguments []string,
	index int,
	inlineValue string,
	hasInlineValue bool,
	missingMessage string,
) (string, int, error) {
	if hasInlineValue {
		if inlineValue == "" {
			return "", index, errors.New(missingMessage)
		}
		return inlineValue, index, nil
	}
	next := index + 1
	if next >= len(arguments) || strings.HasPrefix(arguments[next], "--") {
		return "", index, errors.New(missingMessage)
	}
	return arguments[next], next, nil
}

func selectApplicationTarget(
	targets []application.Target,
	selector string,
) (application.Target, error) {
	if len(targets) == 0 {
		return application.Target{}, errors.New("no @Application marker was found in the selected packages")
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
		return application.Target{}, fmt.Errorf("@Application target %q was not found", selector)
	}
	if len(matches) > 1 {
		return application.Target{}, fmt.Errorf(
			"@Application target %q is ambiguous; select by stable symbol ID",
			selector,
		)
	}
	return matches[0], nil
}

func executeGoBuild(
	ctx context.Context,
	directory string,
	stdout io.Writer,
	stderr io.Writer,
) error {
	command := exec.CommandContext(ctx, "go", "build", "-trimpath", "./...")
	command.Dir = directory
	command.Env = os.Environ()
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("go build -trimpath ./...: %w", err)
	}
	return nil
}

func executeGoTest(
	ctx context.Context,
	directory string,
	arguments []string,
	stdout io.Writer,
	stderr io.Writer,
) error {
	commandArguments := append([]string{"test"}, arguments...)
	// #nosec G204 -- arguments are validated flags or compiler-owned import paths passed directly to the fixed Go executable without a shell.
	command := exec.CommandContext(ctx, "go", commandArguments...)
	command.Dir = directory
	command.Env = os.Environ()
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("go %s: %w", strings.Join(commandArguments, " "), err)
	}
	return nil
}

func verificationSummary(diagnostics []application.Diagnostic) string {
	label := "application model"
	if len(diagnostics) != 0 {
		label = verificationStageLabel(diagnostics[0].Stage)
	}
	return fmt.Sprintf("Spice verification failed: %d %s error(s).", len(diagnostics), label)
}

func verificationStageLabel(stage application.Stage) string {
	labels := map[application.Stage]string{
		application.StageResolution:    "annotation resolution",
		application.StageProvider:      "provider catalog",
		application.StageGraph:         "provider graph",
		application.StageLifecycle:     "lifecycle hook",
		application.StageSchedule:      "scheduled method",
		application.StageAsync:         "asynchronous method",
		application.StageTransaction:   "transaction boundary",
		application.StageCache:         "cache boundary",
		application.StageEvent:         "event contract",
		application.StageModule:        "module architecture",
		application.StageConfiguration: "configuration declaration",
		application.StageController:    "HTTP controller",
		application.StageBootstrap:     "application bootstrap",
		application.StageApplication:   "application model",
	}
	if label, found := labels[stage]; found {
		return label
	}
	return "application model"
}
