package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spice-framework/spice/annotation"
	"github.com/spice-framework/toolchain/compiler/annotationhost"
	"github.com/spice-framework/toolchain/compiler/annotationimport"
	"github.com/spice-framework/toolchain/compiler/descriptor"
	"github.com/spice-framework/toolchain/compiler/load"
)

// NewAnnotationsHandler constructs the annotation inspection command handler.
func NewAnnotationsHandler(runtime *Runtime) (Handler, error) {
	return newLoaderCommandHandler(
		runtime,
		[]string{"annotations"},
		func(runtime *Runtime, invocation Invocation) int {
			return annotationsCommand(
				invocation.Arguments,
				invocation.Stdout,
				invocation.Stderr,
				runtime.options,
				runtime.loader,
			)
		},
	)
}

func annotationsCommand(
	arguments []string,
	stdout io.Writer,
	stderr io.Writer,
	options load.Options,
	loader programLoader,
) int {
	if len(arguments) == 0 ||
		arguments[0] != "list" && arguments[0] != "doctor" {
		return annotations(
			packagePatterns(arguments),
			stdout,
			stderr,
			options,
			loader,
		)
	}
	command := arguments[0]
	descriptors, root, err := inspectAnnotationDescriptors(
		context.Background(),
		packagePatterns(arguments[1:]),
		options,
		loader,
	)
	if err != nil {
		if writeErr := writef(
			stderr,
			"Spice annotations %s failed: %v\n",
			command,
			err,
		); writeErr != nil {
			return 1
		}
		return 1
	}
	switch command {
	case "list":
		return listAnnotationDescriptors(root, descriptors, stdout, stderr)
	case "doctor":
		return doctorAnnotationDescriptors(
			root,
			descriptors,
			options.Env,
			stdout,
			stderr,
		)
	default:
		return 2
	}
}

func inspectAnnotationDescriptors(
	ctx context.Context,
	patterns []string,
	options load.Options,
	loader programLoader,
) ([]descriptor.Descriptor, string, error) {
	root := options.Dir
	if root == "" {
		root = "."
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, "", fmt.Errorf("resolve annotation workspace: %w", err)
	}
	discovery, err := annotationimport.Discover(absolute, options.Overlay)
	if err != nil {
		return nil, "", err
	}
	options.Dir = absolute
	options.AuxiliaryPackages = append(
		options.AuxiliaryPackages,
		discovery.Packages...,
	)
	options.Env = moduleGraphEnvironment(options.Env)
	options.BuildFlags = annotationInspectionBuildFlags(
		options.BuildFlags,
		absolute,
	)
	program, err := loader(ctx, options, patterns...)
	if err != nil {
		return nil, "", err
	}
	namespaceReferences, err := descriptor.NamespaceReferences(
		program,
		discovery.NamespacePackages(),
	)
	if err != nil {
		return nil, "", err
	}
	references := append(
		append(
			[]annotation.DefinitionReference(nil),
			discovery.References...,
		),
		namespaceReferences...,
	)
	decoded, err := descriptor.DecodeAll(program, references)
	if err != nil {
		return nil, "", err
	}
	return decoded, absolute, nil
}

func annotationInspectionBuildFlags(values []string, root string) []string {
	result := make([]string, 0, len(values)+1)
	for index := 0; index < len(values); index++ {
		if values[index] == "-mod" {
			index++
			continue
		}
		if strings.HasPrefix(values[index], "-mod=") {
			continue
		}
		result = append(result, values[index])
	}
	mode := "readonly"
	if _, err := os.Stat(
		filepath.Join(root, "vendor", "modules.txt"),
	); err == nil {
		mode = "vendor"
	}
	return append(result, "-mod="+mode)
}

func listAnnotationDescriptors(
	root string,
	descriptors []descriptor.Descriptor,
	stdout io.Writer,
	stderr io.Writer,
) int {
	module, err := annotationhost.ReadTargetModule(root)
	if err != nil {
		if writeErr := writef(stderr, "Spice annotations list failed: %v\n", err); writeErr != nil {
			return 1
		}
		return 1
	}
	for _, item := range descriptors {
		status := "authorized"
		if err := module.AuthorizeTool(
			item.Definition.Implementation.Tool,
		); err != nil {
			status = "unauthorized"
		}
		if err := writef(
			stdout,
			"%s\t%s.%s\t%s\t%s\t%s\n",
			item.Definition.Name,
			item.Package,
			item.Symbol,
			item.Definition.Implementation.Tool,
			item.Handler.Package+"."+item.Handler.Name,
			status,
		); err != nil {
			return 1
		}
	}
	if err := writef(
		stdout,
		"Found %d explicit annotation descriptors.\n",
		len(descriptors),
	); err != nil {
		return 1
	}
	return 0
}

func doctorAnnotationDescriptors(
	root string,
	descriptors []descriptor.Descriptor,
	environment []string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	manager := annotationhost.NewManager()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var problems []error
	clients := make(map[string]*annotationhost.Client)
	for _, item := range descriptors {
		tool := item.Definition.Implementation.Tool
		client := clients[tool]
		if client == nil {
			var err error
			client, err = manager.Client(ctx, annotationhost.Config{
				Root:         root,
				ToolPath:     tool,
				SpiceVersion: Version,
				Environment:  environment,
			})
			if err != nil {
				problems = append(problems, err)
				continue
			}
			clients[tool] = client
		}
		if err := client.ValidateDescriptor(
			item.Package,
			item.Symbol,
			item.Definition,
			item.Provenance,
		); err != nil {
			problems = append(problems, err)
			continue
		}
	}
	closeErr := manager.Close(ctx)
	problems = append(problems, closeErr)
	problems = compactErrors(problems)
	if len(problems) != 0 {
		sort.Slice(problems, func(i, j int) bool {
			return problems[i].Error() < problems[j].Error()
		})
		for _, problem := range problems {
			if err := writef(stderr, "- %v\n", problem); err != nil {
				return 1
			}
		}
		if err := writef(
			stderr,
			"Spice annotations doctor found %d problem(s).\n",
			len(problems),
		); err != nil {
			return 1
		}
		return 1
	}
	if err := writef(
		stdout,
		"Spice annotations doctor passed: %d descriptor(s), %d tool(s).\n",
		len(descriptors),
		len(clients),
	); err != nil {
		return 1
	}
	return 0
}

func compactErrors(values []error) []error {
	result := values[:0]
	for _, value := range values {
		if value != nil {
			result = append(result, value)
		}
	}
	return result
}
