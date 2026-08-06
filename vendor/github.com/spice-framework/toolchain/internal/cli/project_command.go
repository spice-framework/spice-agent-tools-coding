package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/spice-framework/toolchain/compiler/annotationinstall"
	"github.com/spice-framework/toolchain/compiler/load"
	"github.com/spice-framework/toolchain/internal/identity"
	"github.com/spice-framework/toolchain/internal/scaffold"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

const moduleChangeTimeout = 2 * time.Minute

type scaffoldArguments struct {
	directory        string
	module           string
	spiceVersion     string
	toolchainVersion string
	replace          string
	toolchainReplace string
}

type addArguments struct {
	root    string
	path    string
	version string
	tool    bool
	apply   bool
}

// NewScaffoldHandler constructs the clean-room application scaffold handler.
func NewScaffoldHandler(runtime *Runtime) (Handler, error) {
	return newCommandHandler(
		runtime,
		[]string{"new"},
		func(_ *Runtime, invocation Invocation) int {
			return scaffoldCommand(
				invocation.Arguments,
				invocation.Stdout,
				invocation.Stderr,
			)
		},
	)
}

// NewAddHandler constructs the guarded Go module dependency handler.
func NewAddHandler(runtime *Runtime) (Handler, error) {
	return newCommandHandler(
		runtime,
		[]string{"add"},
		func(runtime *Runtime, invocation Invocation) int {
			return addCommand(
				invocation.Arguments,
				invocation.Stdout,
				invocation.Stderr,
				runtime.options,
			)
		},
	)
}

func scaffoldCommand(arguments []string, stdout, stderr io.Writer) int {
	parsed, err := parseScaffoldArguments(arguments)
	if err != nil {
		if writeErr := writef(stderr, "Spice new failed: %v\n", err); writeErr != nil {
			return 1
		}
		return 2
	}
	result, err := scaffold.Create(context.Background(), scaffold.Config{
		Directory:        parsed.directory,
		Module:           parsed.module,
		SpiceVersion:     parsed.spiceVersion,
		ToolchainVersion: parsed.toolchainVersion,
		Replace:          parsed.replace,
		ToolchainReplace: parsed.toolchainReplace,
	})
	if err != nil {
		if writeErr := writef(stderr, "Spice new failed: %v\n", err); writeErr != nil {
			return 1
		}
		return 1
	}
	if err := writef(
		stdout,
		"Created Spice application %s in %s (%d files).\n",
		parsed.module,
		result.Directory,
		len(result.Files),
	); err != nil {
		return 1
	}
	if err := writef(
		stdout,
		"No dependencies were downloaded; run go mod download explicitly.\n",
	); err != nil {
		return 1
	}
	return 0
}

func parseScaffoldArguments(arguments []string) (scaffoldArguments, error) {
	result := scaffoldArguments{
		spiceVersion:     identity.CoreVersion,
		toolchainVersion: currentToolchainModuleVersion(),
	}
	for index := 0; index < len(arguments); index++ {
		name := arguments[index]
		switch name {
		case "--module", "--directory", "--spice-version", "--toolchain-version", "--replace", "--toolchain-replace":
		default:
			return scaffoldArguments{}, fmt.Errorf("unknown new argument %q", name)
		}
		if index+1 >= len(arguments) || strings.TrimSpace(arguments[index+1]) == "" {
			return scaffoldArguments{}, fmt.Errorf("%s requires a value", name)
		}
		value := arguments[index+1]
		index++
		switch name {
		case "--module":
			result.module = value
		case "--directory":
			result.directory = value
		case "--spice-version":
			result.spiceVersion = value
		case "--toolchain-version":
			result.toolchainVersion = value
		case "--replace":
			result.replace = value
		case "--toolchain-replace":
			result.toolchainReplace = value
		}
	}
	if result.module == "" {
		return scaffoldArguments{}, errors.New("--module is required")
	}
	if result.directory == "" {
		result.directory = filepath.Base(result.module)
	}
	if result.toolchainVersion == "" {
		return scaffoldArguments{}, errors.New(
			"the CLI version cannot select a toolchain module version; pass --toolchain-version",
		)
	}
	return result, nil
}

func currentToolchainModuleVersion() string {
	value := strings.TrimSpace(Version)
	if !strings.HasPrefix(value, "v") {
		value = "v" + value
	}
	if !semver.IsValid(value) {
		return ""
	}
	return value
}

func addCommand(
	arguments []string,
	stdout io.Writer,
	stderr io.Writer,
	options load.Options,
) int {
	parsed, err := parseAddArguments(arguments, options.Dir)
	if err != nil {
		if writeErr := writef(stderr, "Spice add failed: %v\n", err); writeErr != nil {
			return 1
		}
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), moduleChangeTimeout)
	defer cancel()
	var preview annotationinstall.Preview
	if parsed.tool {
		preview, err = annotationinstall.PreviewTool(
			ctx,
			parsed.root,
			parsed.path,
			parsed.version,
			options.Env,
		)
	} else {
		preview, err = annotationinstall.PreviewDependency(
			ctx,
			parsed.root,
			parsed.path,
			parsed.version,
			options.Env,
		)
	}
	if err != nil {
		if writeErr := writef(stderr, "Spice add failed: %v\n", err); writeErr != nil {
			return 1
		}
		return 1
	}
	if err := writef(stdout, "Command: %s\n%s", preview.Command(), preview.Diff()); err != nil {
		return 1
	}
	if !parsed.apply {
		if err := writef(
			stdout,
			"Preview only; re-run with --apply to apply a freshly validated plan.\n",
		); err != nil {
			return 1
		}
		return 0
	}
	if err := annotationinstall.Apply(ctx, preview); err != nil {
		if writeErr := writef(stderr, "Spice add apply failed: %v\n", err); writeErr != nil {
			return 1
		}
		return 1
	}
	if err := writef(stdout, "Applied the previewed module changes.\n"); err != nil {
		return 1
	}
	return 0
}

func parseAddArguments(arguments []string, defaultRoot string) (addArguments, error) {
	result, selector, err := collectAddArguments(arguments, defaultRoot)
	if err != nil {
		return addArguments{}, err
	}
	path, version, err := parseDependencySelector(selector)
	if err != nil {
		return addArguments{}, err
	}
	result.path = path
	result.version = version
	return result, nil
}

func collectAddArguments(
	arguments []string,
	defaultRoot string,
) (addArguments, string, error) {
	result := addArguments{root: defaultRoot}
	if result.root == "" {
		result.root = "."
	}
	var selector string
	for index := 0; index < len(arguments); index++ {
		switch arguments[index] {
		case "--tool":
			result.tool = true
		case "--apply":
			result.apply = true
		case "--directory":
			if index+1 >= len(arguments) || strings.TrimSpace(arguments[index+1]) == "" {
				return addArguments{}, "", errors.New("--directory requires a value")
			}
			result.root = arguments[index+1]
			index++
		default:
			if strings.HasPrefix(arguments[index], "-") {
				return addArguments{}, "", fmt.Errorf("unknown add argument %q", arguments[index])
			}
			if selector != "" {
				return addArguments{}, "", errors.New("add accepts exactly one dependency selector")
			}
			selector = arguments[index]
		}
	}
	return result, selector, nil
}

func parseDependencySelector(selector string) (string, string, error) {
	path, version, found := strings.Cut(selector, "@")
	if !found || path == "" || version == "" || strings.Contains(version, "@") {
		return "", "", errors.New(
			"dependency selector must be package@vX.Y.Z with an exact semantic version",
		)
	}
	if err := module.CheckImportPath(path); err != nil {
		return "", "", fmt.Errorf("validate dependency package %q: %w", path, err)
	}
	if !semver.IsValid(version) {
		return "", "", fmt.Errorf("dependency version %q is not an exact semantic version", version)
	}
	return path, version, nil
}
