package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spice-framework/toolchain/compiler/load"
	"github.com/spice-framework/toolchain/compiler/modulith"
)

type moduleTestArguments struct {
	module   string
	race     bool
	count    string
	run      string
	timeout  string
	patterns []string
}

// NewTestHandler constructs the focused module-test command handler.
func NewTestHandler(runtime *Runtime) (Handler, error) {
	return newTestCommandHandler(
		runtime,
		[]string{"test"},
		func(runtime *Runtime, invocation Invocation) int {
			return moduleTestCommand(
				invocation.Arguments,
				invocation.Stdout,
				invocation.Stderr,
				runtime.options,
				runtime.loader,
				runtime.tester,
			)
		},
	)
}

func moduleTestCommand(
	arguments []string,
	stdout io.Writer,
	stderr io.Writer,
	options load.Options,
	loader programLoader,
	tester testExecutor,
) int {
	parsed, err := parseModuleTestArguments(arguments)
	if err != nil {
		if writeErr := writef(stderr, "Spice module test failed: %v\n", err); writeErr != nil {
			return 1
		}
		return 2
	}
	if len(parsed.patterns) == 0 {
		parsed.patterns = []string{"./..."}
	}
	focused, ok := prepareFocusedModuleTest(parsed, stderr, options, loader)
	if !ok {
		return 1
	}
	packages := focusedModulePackages(focused)
	if len(packages) == 0 {
		if writeErr := writef(
			stderr,
			"Spice module test failed: focused module %q has no owned packages\n",
			parsed.module,
		); writeErr != nil {
			return 1
		}
		return 1
	}
	directory := options.Dir
	if directory == "" {
		directory = "."
	}
	if err := tester(
		context.Background(),
		directory,
		parsed.goTestArguments(packages),
		stdout,
		stderr,
	); err != nil {
		if writeErr := writef(stderr, "Spice module test failed for %s: %v\n", parsed.module, err); writeErr != nil {
			return 1
		}
		return 1
	}
	if err := writef(
		stdout,
		"Spice module tests passed for %s: %d package(s) across %d module(s).\n",
		parsed.module,
		len(packages),
		len(focused.Modules()),
	); err != nil {
		return 1
	}
	return 0
}

func prepareFocusedModuleTest(
	arguments moduleTestArguments,
	stderr io.Writer,
	options load.Options,
	loader programLoader,
) (modulith.Model, bool) {
	model, ok := prepareModuleModel(
		arguments.patterns,
		stderr,
		options,
		loader,
		"Spice module test failed",
	)
	if !ok {
		return modulith.Model{}, false
	}
	focused, err := model.Focus(arguments.module)
	if err != nil {
		if writeErr := writef(stderr, "Spice module test failed: %v\n", err); writeErr != nil {
			return modulith.Model{}, false
		}
		return modulith.Model{}, false
	}
	return focused, true
}

func parseModuleTestArguments(arguments []string) (moduleTestArguments, error) {
	var result moduleTestArguments
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if !strings.HasPrefix(argument, "-") {
			result.patterns = append(result.patterns, argument)
			continue
		}
		next, err := parseModuleTestOption(arguments, index, &result)
		if err != nil {
			return moduleTestArguments{}, err
		}
		index = next
	}
	if result.module == "" {
		return moduleTestArguments{}, errors.New("--module requires a full module import path")
	}
	return result, nil
}

func parseModuleTestOption(
	arguments []string,
	index int,
	result *moduleTestArguments,
) (int, error) {
	argument := arguments[index]
	switch {
	case argument == "--module" || strings.HasPrefix(argument, "--module="):
		return setModuleTestOption(
			arguments,
			index,
			"--module",
			"a full module import path",
			&result.module,
			nil,
		)
	case argument == "--race":
		if result.race {
			return index, errors.New("--race may be specified only once")
		}
		result.race = true
		return index, nil
	case argument == "--count" || strings.HasPrefix(argument, "--count="):
		return setModuleTestOption(
			arguments,
			index,
			"--count",
			"a positive integer",
			&result.count,
			validateModuleTestCount,
		)
	case argument == "--run" || strings.HasPrefix(argument, "--run="):
		return setModuleTestOption(
			arguments,
			index,
			"--run",
			"a non-empty Go test expression",
			&result.run,
			nil,
		)
	case argument == "--timeout" || strings.HasPrefix(argument, "--timeout="):
		return setModuleTestOption(
			arguments,
			index,
			"--timeout",
			"a positive Go duration",
			&result.timeout,
			validateModuleTestTimeout,
		)
	default:
		return index, fmt.Errorf("unknown module test option %q", argument)
	}
}

func setModuleTestOption(
	arguments []string,
	index int,
	name string,
	expected string,
	target *string,
	validate func(string) error,
) (int, error) {
	value, next, err := moduleOptionValue(arguments, index, name, *target != "", expected)
	if err != nil {
		return index, err
	}
	if validate != nil {
		if err := validate(value); err != nil {
			return index, err
		}
	}
	*target = value
	return next, nil
}

func validateModuleTestCount(value string) error {
	count, err := strconv.Atoi(value)
	if err != nil || count <= 0 {
		return fmt.Errorf("--count requires a positive integer, got %q", value)
	}
	return nil
}

func validateModuleTestTimeout(value string) error {
	timeout, err := time.ParseDuration(value)
	if err != nil || timeout <= 0 {
		return fmt.Errorf("--timeout requires a positive Go duration, got %q", value)
	}
	return nil
}

func (arguments moduleTestArguments) goTestArguments(packages []string) []string {
	result := []string{"-trimpath"}
	if arguments.race {
		result = append(result, "-race")
	}
	if arguments.count != "" {
		result = append(result, "-count="+arguments.count)
	}
	if arguments.run != "" {
		result = append(result, "-run="+arguments.run)
	}
	if arguments.timeout != "" {
		result = append(result, "-timeout="+arguments.timeout)
	}
	return append(result, packages...)
}

func focusedModulePackages(model modulith.Model) []string {
	modules := make(map[string]modulith.Module, len(model.Modules()))
	for _, module := range model.Modules() {
		modules[module.ID] = module
	}
	var result []string
	for _, moduleID := range model.DependencyOrder() {
		module, ok := modules[moduleID]
		if !ok {
			continue
		}
		for _, pkg := range module.Packages() {
			result = append(result, pkg.Path)
		}
	}
	return result
}
