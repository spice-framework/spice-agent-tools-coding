package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spice-framework/toolchain/compiler/load"
	compilerservice "github.com/spice-framework/toolchain/compiler/service"
)

type beansArguments struct {
	explain  bool
	format   string
	patterns []string
}

type beansDocument struct {
	Schema            string                              `json:"schema"`
	Providers         []beanProviderDocument              `json:"providers"`
	AutoConfiguration []compilerservice.AutoConfiguration `json:"auto_configuration"`
}

type beanProviderDocument struct {
	Name       string   `json:"name"`
	Output     string   `json:"output"`
	Source     string   `json:"source"`
	Package    string   `json:"package"`
	Qualifiers []string `json:"qualifiers,omitempty"`
	Primary    bool     `json:"primary,omitempty"`
	Fallback   bool     `json:"fallback,omitempty"`
}

// NewBeansHandler constructs the bean-selection explanation command handler.
func NewBeansHandler(runtime *Runtime) (Handler, error) {
	return newLoaderCommandHandler(
		runtime,
		[]string{"beans"},
		func(runtime *Runtime, invocation Invocation) int {
			return beansCommand(
				invocation.Arguments,
				invocation.Stdout,
				invocation.Stderr,
				runtime.options,
				runtime.loader,
			)
		},
	)
}

func beansCommand(
	arguments []string,
	stdout io.Writer,
	stderr io.Writer,
	options load.Options,
	loader programLoader,
) int {
	parsed, err := parseBeansArguments(arguments)
	if err != nil {
		if writeErr := writef(stderr, "Spice bean explanation failed: %v\n", err); writeErr != nil {
			return 1
		}
		return 2
	}
	root := options.Dir
	if root == "" {
		root = "."
	}
	service, err := newCompilerAnalysisService(options, loader)
	if err != nil {
		if writeErr := writef(stderr, "Spice bean explanation failed: %v\n", err); writeErr != nil {
			return 1
		}
		return 1
	}
	defer func() {
		if closeErr := closeCompilerAnalysisService(service); closeErr != nil {
			if writeErr := writef(
				stderr,
				"Spice bean explanation shutdown failed: %v\n",
				closeErr,
			); writeErr != nil {
				return
			}
		}
	}()
	result, err := service.Analyze(context.Background(), compilerservice.Request{
		WorkspaceRoot: root,
		Patterns:      packagePatterns(parsed.patterns),
		Mode:          compilerservice.AnalysisValidate,
	})
	if err != nil {
		if writeErr := writef(stderr, "Spice bean explanation failed: %v\n", err); writeErr != nil {
			return 1
		}
		return 1
	}
	if diagnostics := result.Diagnostics().Items(); len(diagnostics) != 0 {
		if reportErr := reportDiagnostics(
			stderr,
			diagnostics,
			fmt.Sprintf("Spice bean explanation failed: %d compiler error(s).", len(diagnostics)),
		); reportErr != nil {
			return 1
		}
		return 1
	}
	if parsed.format == "json" {
		return writeBeansJSON(stdout, result)
	}
	return writeBeansText(stdout, result)
}

func parseBeansArguments(arguments []string) (beansArguments, error) {
	result := beansArguments{format: "text"}
	for index := 0; index < len(arguments); index++ {
		switch argument := arguments[index]; {
		case argument == "--explain":
			result.explain = true
		case argument == "--format":
			index++
			if index >= len(arguments) {
				return beansArguments{}, errors.New("--format requires text or json")
			}
			result.format = arguments[index]
		case strings.HasPrefix(argument, "--format="):
			result.format = strings.TrimPrefix(argument, "--format=")
		case strings.HasPrefix(argument, "-"):
			return beansArguments{}, fmt.Errorf("unknown beans option %q", argument)
		default:
			result.patterns = append(result.patterns, argument)
		}
	}
	if !result.explain {
		return beansArguments{}, errors.New("the current command requires --explain")
	}
	if result.format != "text" && result.format != "json" {
		return beansArguments{}, fmt.Errorf("unsupported beans format %q", result.format)
	}
	return result, nil
}

func writeBeansJSON(stdout io.Writer, result compilerservice.Result) int {
	graph := result.ProviderGraph()
	providers := make([]beanProviderDocument, len(graph.Providers))
	for index, item := range graph.Providers {
		providers[index] = beanProviderDocument{
			Name:       item.Name,
			Output:     item.OutputTypeID,
			Source:     string(item.Source),
			Package:    item.PackagePath,
			Qualifiers: item.Qualifiers,
			Primary:    item.Primary,
			Fallback:   item.Fallback,
		}
	}
	content, err := json.MarshalIndent(beansDocument{
		Schema:            "spice.beans/v1",
		Providers:         providers,
		AutoConfiguration: result.AutoConfigurations(),
	}, "", "  ")
	if err != nil {
		return 1
	}
	if _, err := stdout.Write(append(content, '\n')); err != nil {
		return 1
	}
	return 0
}

func writeBeansText(stdout io.Writer, result compilerservice.Result) int {
	graph := result.ProviderGraph()
	if _, err := fmt.Fprintln(stdout, "Beans:"); err != nil {
		return 1
	}
	for _, item := range graph.Providers {
		metadata := []string{string(item.Source)}
		if item.Primary {
			metadata = append(metadata, "primary")
		}
		if item.Fallback {
			metadata = append(metadata, "fallback")
		}
		if _, err := fmt.Fprintf(
			stdout,
			"  %s -> %s [%s]\n",
			item.Name,
			item.OutputTypeID,
			strings.Join(metadata, ", "),
		); err != nil {
			return 1
		}
	}
	decisions := result.AutoConfigurations()
	sort.Slice(decisions, func(i, j int) bool {
		if decisions[i].PackagePath != decisions[j].PackagePath {
			return decisions[i].PackagePath < decisions[j].PackagePath
		}
		return decisions[i].Factory < decisions[j].Factory
	})
	if _, err := fmt.Fprintln(stdout, "Auto-configuration:"); err != nil {
		return 1
	}
	if len(decisions) == 0 {
		if _, err := fmt.Fprintln(stdout, "  none explicitly imported"); err != nil {
			return 1
		}
		return 0
	}
	for _, item := range decisions {
		provenance := item.ModulePath + "@" + item.ModuleVersion
		if item.ReplacementPath != "" {
			provenance += " => " + item.ReplacementPath
		}
		if _, err := fmt.Fprintf(
			stdout,
			"  %s.%s -> %s: %s\n    %s\n    provenance: %s; review: %s\n",
			item.PackagePath,
			item.Factory,
			item.OutputTypeID,
			item.Status,
			item.Reason,
			provenance,
			item.Review,
		); err != nil {
			return 1
		}
	}
	return 0
}
