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
	compilerstyle "github.com/spice-framework/toolchain/compiler/style"
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
	profile          compilerstyle.Profile
	profileSet       bool
}

type declarationArguments struct {
	directory   string
	packageName string
	kind        scaffold.DeclarationKind
	name        string
	profile     compilerstyle.Profile
	profileSet  bool
}

type addArguments struct {
	root    string
	path    string
	version string
	tool    bool
	apply   bool
}

// NewInitHandler constructs the clean-room application initialization handler.
func NewInitHandler(runtime *Runtime) (Handler, error) {
	return newCommandHandler(
		runtime,
		[]string{"init"},
		func(_ *Runtime, invocation Invocation) int {
			return applicationScaffoldCommand(
				"init",
				invocation.Arguments,
				invocation.Stdout,
				invocation.Stderr,
			)
		},
	)
}

// NewScaffoldHandler constructs the clean-room declaration scaffold handler
// and preserves the original application form for compatibility.
func NewScaffoldHandler(runtime *Runtime) (Handler, error) {
	return newCommandHandler(
		runtime,
		[]string{"new"},
		func(_ *Runtime, invocation Invocation) int {
			if isDeclarationInvocation(invocation.Arguments) {
				return declarationScaffoldCommand(
					invocation.Arguments,
					invocation.Stdout,
					invocation.Stderr,
				)
			}
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
	return applicationScaffoldCommand("new", arguments, stdout, stderr)
}

func applicationScaffoldCommand(command string, arguments []string, stdout, stderr io.Writer) int {
	parsed, err := parseScaffoldArguments(arguments)
	if err != nil {
		if writeErr := writef(stderr, "Spice %s failed: %v\n", command, err); writeErr != nil {
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
		Profile:          parsed.profile,
	})
	if err != nil {
		if writeErr := writef(stderr, "Spice %s failed: %v\n", command, err); writeErr != nil {
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

func declarationScaffoldCommand(arguments []string, stdout, stderr io.Writer) int {
	parsed, err := parseDeclarationArguments(arguments)
	if err != nil {
		if writeErr := writef(stderr, "Spice new failed: %v\n", err); writeErr != nil {
			return 1
		}
		return 2
	}
	result, err := scaffold.CreateDeclaration(context.Background(), scaffold.DeclarationConfig{
		Directory: parsed.directory,
		Package:   parsed.packageName,
		Kind:      parsed.kind,
		Name:      parsed.name,
	})
	if err != nil {
		if writeErr := writef(stderr, "Spice new failed: %v\n", err); writeErr != nil {
			return 1
		}
		return 1
	}
	if err := writef(
		stdout,
		"Created Spice %s %s in %s (%s).\n",
		parsed.kind,
		parsed.name,
		result.Directory,
		result.Files[0],
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
		if strings.HasPrefix(name, "--profile=") {
			if result.profileSet {
				return scaffoldArguments{}, errors.New("--profile may be specified only once")
			}
			value := strings.TrimPrefix(name, "--profile=")
			if strings.TrimSpace(value) == "" {
				return scaffoldArguments{}, errors.New("--profile requires a value")
			}
			result.profile = compilerstyle.Profile(value)
			result.profileSet = true
			continue
		}
		switch name {
		case "--module", "--directory", "--spice-version", "--toolchain-version", "--replace", "--toolchain-replace", "--profile":
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
		case "--profile":
			if result.profileSet {
				return scaffoldArguments{}, errors.New("--profile may be specified only once")
			}
			result.profile = compilerstyle.Profile(value)
			result.profileSet = true
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
	if err := compilerstyle.ValidateProfile(result.profile); err != nil {
		return scaffoldArguments{}, err
	}
	return result, nil
}

func isDeclarationInvocation(arguments []string) bool {
	if len(arguments) == 0 {
		return false
	}
	_, err := parseDeclarationKind(arguments[0])
	return err == nil
}

func parseDeclarationArguments(arguments []string) (declarationArguments, error) {
	if len(arguments) < 2 {
		return declarationArguments{}, errors.New(
			"new declaration requires a kind and name",
		)
	}
	kind, err := parseDeclarationKind(arguments[0])
	if err != nil {
		return declarationArguments{}, err
	}
	result := declarationArguments{kind: kind, name: arguments[1]}
	for index := 2; index < len(arguments); index++ {
		option := arguments[index]
		if strings.HasPrefix(option, "--profile=") {
			if result.profileSet {
				return declarationArguments{}, errors.New("--profile may be specified only once")
			}
			value := strings.TrimPrefix(option, "--profile=")
			if strings.TrimSpace(value) == "" {
				return declarationArguments{}, errors.New("--profile requires a value")
			}
			result.profile = compilerstyle.Profile(value)
			result.profileSet = true
			continue
		}
		if option != "--directory" && option != "--package" && option != "--profile" {
			return declarationArguments{}, fmt.Errorf("unknown new argument %q", option)
		}
		if index+1 >= len(arguments) || strings.TrimSpace(arguments[index+1]) == "" {
			return declarationArguments{}, fmt.Errorf("%s requires a value", option)
		}
		value := arguments[index+1]
		index++
		switch option {
		case "--directory":
			result.directory = value
		case "--package":
			result.packageName = value
		case "--profile":
			if result.profileSet {
				return declarationArguments{}, errors.New("--profile may be specified only once")
			}
			result.profile = compilerstyle.Profile(value)
			result.profileSet = true
		}
	}
	if err := compilerstyle.ValidateProfile(result.profile); err != nil {
		return declarationArguments{}, err
	}
	if result.directory == "" {
		if kind == scaffold.DeclarationModule {
			result.directory = filepath.Join("internal", result.name)
		} else {
			result.directory = "."
		}
	}
	return result, nil
}

func parseDeclarationKind(value string) (scaffold.DeclarationKind, error) {
	kind := scaffold.DeclarationKind(value)
	switch kind {
	case scaffold.DeclarationModule,
		scaffold.DeclarationService,
		scaffold.DeclarationRepository,
		scaffold.DeclarationController,
		scaffold.DeclarationComponent,
		scaffold.DeclarationEnum:
		return kind, nil
	default:
		return "", fmt.Errorf(
			"unsupported declaration kind %q; expected module, service, repository, controller, component, or enum",
			value,
		)
	}
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
