package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spice-framework/toolchain/compiler/load"
	"github.com/spice-framework/toolchain/compiler/modulith"
	compilerservice "github.com/spice-framework/toolchain/compiler/service"
)

type moduleArguments struct {
	format    modulith.Format
	formatSet bool
	focus     string
	patterns  []string
}

// NewModulesHandler constructs the application-module command handler.
func NewModulesHandler(runtime *Runtime) (Handler, error) {
	return newLoaderCommandHandler(
		runtime,
		[]string{"modules"},
		func(runtime *Runtime, invocation Invocation) int {
			return modulesCommand(
				invocation.Arguments,
				invocation.Stdout,
				invocation.Stderr,
				runtime.options,
				runtime.loader,
			)
		},
	)
}

func modulesCommand(
	arguments []string,
	stdout io.Writer,
	stderr io.Writer,
	options load.Options,
	loader programLoader,
) int {
	parsed, parseErr := parseModuleArguments(arguments)
	if parseErr != nil {
		if writeErr := writef(stderr, "Spice module documentation failed: %v\n", parseErr); writeErr != nil {
			return 1
		}
		return 2
	}
	if len(parsed.patterns) == 0 {
		parsed.patterns = []string{"./..."}
	}
	model, ok := prepareModuleModel(
		parsed.patterns,
		stderr,
		options,
		loader,
		"Spice module documentation failed",
	)
	if !ok {
		return 1
	}
	if parsed.focus != "" {
		focused, focusErr := model.Focus(parsed.focus)
		if focusErr != nil {
			if writeErr := writef(stderr, "Spice module documentation failed: %v\n", focusErr); writeErr != nil {
				return 1
			}
			return 1
		}
		model = focused
	}
	content, err := modulith.Render(model, parsed.format)
	if err != nil {
		if writeErr := writef(stderr, "Spice module documentation failed: %v\n", err); writeErr != nil {
			return 1
		}
		return 1
	}
	if _, err := stdout.Write(content); err != nil {
		return 1
	}
	return 0
}

func prepareModuleModel(
	patterns []string,
	stderr io.Writer,
	options load.Options,
	loader programLoader,
	failurePrefix string,
) (modulith.Model, bool) {
	service, err := newCompilerAnalysisService(options, loader)
	if err != nil {
		if writeErr := writef(
			stderr,
			"%s: %v\n",
			failurePrefix,
			err,
		); writeErr != nil {
			return modulith.Model{}, false
		}
		return modulith.Model{}, false
	}
	root := options.Dir
	if root == "" {
		root = "."
	}
	result, analysisErr := service.Analyze(
		context.Background(),
		compilerservice.Request{
			WorkspaceRoot: root,
			Patterns:      patterns,
			Mode:          compilerservice.AnalysisValidate,
		},
	)
	closeErr := closeCompilerAnalysisService(service)
	if analysisErr != nil {
		if writeErr := writef(
			stderr,
			"%s: %v\n",
			failurePrefix,
			analysisErr,
		); writeErr != nil {
			return modulith.Model{}, false
		}
		return modulith.Model{}, false
	}
	if closeErr != nil {
		if writeErr := writef(
			stderr,
			"%s: close annotation tools: %v\n",
			failurePrefix,
			closeErr,
		); writeErr != nil {
			return modulith.Model{}, false
		}
		return modulith.Model{}, false
	}
	if diagnostics := result.Diagnostics().Items(); len(diagnostics) != 0 {
		if reportErr := reportDiagnostics(
			stderr,
			diagnostics,
			fmt.Sprintf(
				"%s: %d module architecture error(s).",
				failurePrefix,
				len(diagnostics),
			),
		); reportErr != nil {
			return modulith.Model{}, false
		}
		return modulith.Model{}, false
	}
	return result.ModuleModel(), true
}

func parseModuleArguments(arguments []string) (moduleArguments, error) {
	result := moduleArguments{format: modulith.FormatJSON}
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		switch {
		case argument == "--format" || strings.HasPrefix(argument, "--format="):
			value, next, err := moduleOptionValue(
				arguments,
				index,
				"--format",
				result.formatSet,
				"json, mermaid, or plantuml",
			)
			if err != nil {
				return moduleArguments{}, err
			}
			index = next
			result.format = modulith.Format(value)
			result.formatSet = true
		case argument == "--focus" || strings.HasPrefix(argument, "--focus="):
			value, next, err := moduleOptionValue(
				arguments,
				index,
				"--focus",
				result.focus != "",
				"a full module import path",
			)
			if err != nil {
				return moduleArguments{}, err
			}
			index = next
			result.focus = value
		case strings.HasPrefix(argument, "-"):
			return moduleArguments{}, fmt.Errorf("unknown module option %q", argument)
		default:
			result.patterns = append(result.patterns, argument)
		}
	}
	switch result.format {
	case modulith.FormatJSON, modulith.FormatMermaid, modulith.FormatPlantUML:
		return result, nil
	default:
		return moduleArguments{}, fmt.Errorf(
			"unsupported module format %q; expected json, mermaid, or plantuml",
			result.format,
		)
	}
}

func moduleOptionValue(
	arguments []string,
	index int,
	name string,
	alreadySet bool,
	expected string,
) (string, int, error) {
	if alreadySet {
		return "", index, fmt.Errorf("%s may be specified only once", name)
	}
	argument := arguments[index]
	if argument == name {
		next := index + 1
		if next >= len(arguments) || strings.HasPrefix(arguments[next], "-") {
			return "", index, fmt.Errorf("%s requires %s", name, expected)
		}
		return arguments[next], next, nil
	}
	value := strings.TrimPrefix(argument, name+"=")
	if value == "" {
		return "", index, fmt.Errorf("%s requires %s", name, expected)
	}
	return value, index, nil
}
