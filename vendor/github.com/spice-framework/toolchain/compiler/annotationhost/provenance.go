package annotationhost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/spice-framework/spice/annotation"
	"github.com/spice-framework/spice/annotation/sdk"
	"github.com/spice-framework/toolchain/internal/identity"
	"github.com/spice-framework/toolchain/internal/moduleenv"
)

const (
	maximumGoCommandStderr           = 256 << 10
	maximumImplementationSourceBytes = 4 << 20
)

// ModuleIdentity is one selected Go module and its visible replacement.
type ModuleIdentity struct {
	Path        string
	Version     string
	Directory   string
	Replacement *ModuleIdentity
}

// PackageIdentity records standard Go provenance for one package.
type PackageIdentity struct {
	Path   string
	Module ModuleIdentity
}

type goListPackage struct {
	ImportPath string        `json:"ImportPath"`
	Dir        string        `json:"Dir"`
	GoFiles    []string      `json:"GoFiles"`
	CgoFiles   []string      `json:"CgoFiles"`
	Error      *goListError  `json:"Error"`
	Module     *goListModule `json:"Module"`
}

type goListError struct {
	Err string `json:"Err"`
}

type goListModule struct {
	Path    string        `json:"Path"`
	Version string        `json:"Version"`
	Dir     string        `json:"Dir"`
	Replace *goListModule `json:"Replace"`
}

// ResolvePackage asks the standard Go command for selected module provenance.
// It is always offline and never edits go.mod or go.sum.
func ResolvePackage(
	ctx context.Context,
	module TargetModule,
	packagePath string,
	environment []string,
) (PackageIdentity, error) {
	if ctx == nil {
		return PackageIdentity{}, errors.New(
			"resolve annotation package context must not be nil",
		)
	}
	listed, err := listPackage(ctx, module, packagePath, environment)
	if err != nil {
		return PackageIdentity{}, err
	}
	if listed.Error != nil {
		return PackageIdentity{}, fmt.Errorf(
			"go list could not resolve annotation package %q: %s",
			packagePath,
			listed.Error.Err,
		)
	}
	if listed.ImportPath != packagePath || listed.Module == nil {
		return PackageIdentity{}, fmt.Errorf(
			"go list returned unexpected annotation package identity %q for %q",
			listed.ImportPath,
			packagePath,
		)
	}
	return PackageIdentity{
		Path:   listed.ImportPath,
		Module: moduleIdentity(listed.Module),
	}, nil
}

// ResolveSourceSymbols locates real package-level Go declarations through the
// target module's offline standard Go resolution. It never executes package
// code and never edits module files.
func ResolveSourceSymbols(
	ctx context.Context,
	module TargetModule,
	symbols []sdk.Symbol,
	environment []string,
) (map[sdk.Symbol]token.Position, error) {
	if ctx == nil {
		return nil, errors.New(
			"resolve annotation implementation context must not be nil",
		)
	}
	requested := make(map[string][]sdk.Symbol)
	for _, symbol := range symbols {
		requested[symbol.Package] = append(
			requested[symbol.Package],
			symbol,
		)
	}
	packages := make([]string, 0, len(requested))
	for packagePath := range requested {
		packages = append(packages, packagePath)
	}
	slices.Sort(packages)
	result := make(map[sdk.Symbol]token.Position, len(symbols))
	for _, packagePath := range packages {
		listed, err := listPackage(
			ctx,
			module,
			packagePath,
			environment,
		)
		if err != nil {
			return nil, err
		}
		positions, err := sourceSymbolPositions(
			listed,
			requested[packagePath],
		)
		if err != nil {
			return nil, err
		}
		maps.Copy(result, positions)
	}
	return result, nil
}

func listPackage(
	ctx context.Context,
	module TargetModule,
	packagePath string,
	environment []string,
) (goListPackage, error) {
	mode := moduleenv.OfflineMode(module.Root, environment)
	command := exec.CommandContext( // #nosec G204 -- executable and flags are fixed; packagePath is one argument.
		ctx,
		"go",
		"list",
		"-json",
		"-mod="+mode,
		packagePath,
	)
	command.Dir = module.Root
	command.Env = offlineEnvironment(environment, mode)
	var stdout bytes.Buffer
	stderr := newBoundedBuffer(maximumGoCommandStderr)
	command.Stdout = &stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		return goListPackage{}, fmt.Errorf(
			"resolve annotation package %q with offline go list: %w%s",
			packagePath,
			err,
			renderStderr(stderr.String()),
		)
	}
	var listed goListPackage
	decoder := json.NewDecoder(&stdout)
	if err := decoder.Decode(&listed); err != nil {
		return goListPackage{}, fmt.Errorf(
			"decode go list provenance for %q: %w",
			packagePath,
			err,
		)
	}
	return listed, nil
}

func sourceSymbolPositions(
	listed goListPackage,
	symbols []sdk.Symbol,
) (map[sdk.Symbol]token.Position, error) {
	if listed.Error != nil {
		return nil, fmt.Errorf(
			"go list could not resolve annotation implementation package %q: %s",
			listed.ImportPath,
			listed.Error.Err,
		)
	}
	if listed.Dir == "" {
		return nil, fmt.Errorf(
			"annotation implementation package %q has no source directory",
			listed.ImportPath,
		)
	}
	requested, err := requestedSymbolIndex(listed.ImportPath, symbols)
	if err != nil {
		return nil, err
	}
	files := append(
		append([]string(nil), listed.GoFiles...),
		listed.CgoFiles...,
	)
	slices.Sort(files)
	result := make(map[sdk.Symbol]token.Position, len(symbols))
	fileSet := token.NewFileSet()
	for _, file := range files {
		positions, err := parseSourceSymbolPositions(
			fileSet,
			listed.Dir,
			file,
			requested,
		)
		if err != nil {
			return nil, err
		}
		maps.Copy(result, positions)
	}
	for _, symbol := range symbols {
		if _, found := result[symbol]; !found {
			return nil, fmt.Errorf(
				"annotation implementation symbol %s.%s was not found",
				symbol.Package,
				symbol.Name,
			)
		}
	}
	return result, nil
}

func requestedSymbolIndex(
	packagePath string,
	symbols []sdk.Symbol,
) (map[string]sdk.Symbol, error) {
	requested := make(map[string]sdk.Symbol, len(symbols))
	for _, symbol := range symbols {
		if symbol.Package != packagePath {
			return nil, fmt.Errorf(
				"annotation implementation symbol %s.%s resolved as package %q",
				symbol.Package,
				symbol.Name,
				packagePath,
			)
		}
		requested[symbol.Name] = symbol
	}
	return requested, nil
}

func parseSourceSymbolPositions(
	fileSet *token.FileSet,
	directory string,
	file string,
	requested map[string]sdk.Symbol,
) (map[sdk.Symbol]token.Position, error) {
	path, err := safeSourcePath(directory, file)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf(
			"inspect annotation implementation source %q: %w",
			path,
			err,
		)
	}
	if !info.Mode().IsRegular() ||
		info.Size() > maximumImplementationSourceBytes {
		return nil, fmt.Errorf(
			"annotation implementation source %q must be a regular file no larger than %d bytes",
			path,
			maximumImplementationSourceBytes,
		)
	}
	source, err := parser.ParseFile(
		fileSet,
		path,
		nil,
		parser.SkipObjectResolution,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"parse annotation implementation source %q: %w",
			path,
			err,
		)
	}
	result := make(map[sdk.Symbol]token.Position)
	for _, declaration := range source.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Recv != nil {
			continue
		}
		symbol, found := requested[function.Name.Name]
		if found {
			result[symbol] = fileSet.Position(function.Name.Pos())
		}
	}
	return result, nil
}

func safeSourcePath(directory, file string) (string, error) {
	if !filepath.IsLocal(file) || filepath.Ext(file) != ".go" {
		return "", fmt.Errorf(
			"annotation implementation source name %q is unsafe",
			file,
		)
	}
	path := filepath.Join(directory, file)
	relative, err := filepath.Rel(directory, path)
	if err != nil || !filepath.IsLocal(relative) {
		return "", fmt.Errorf(
			"annotation implementation source %q escapes package directory",
			file,
		)
	}
	return filepath.Clean(path), nil
}

func moduleIdentity(module *goListModule) ModuleIdentity {
	if module == nil {
		return ModuleIdentity{}
	}
	result := ModuleIdentity{
		Path:      module.Path,
		Version:   module.Version,
		Directory: filepath.Clean(module.Dir),
	}
	if module.Replace != nil {
		replacement := moduleIdentity(module.Replace)
		result.Replacement = &replacement
	}
	return result
}

// ValidateDescriptorToolModule requires descriptor and executable packages to
// resolve from the same module version and replacement identity. The official
// extracted annotation tool is the sole exception: it serves the descriptors
// owned by the independently versioned public core module.
func ValidateDescriptorToolModule(
	descriptor annotation.ModuleProvenance,
	tool PackageIdentity,
) error {
	resolved := tool.Module
	if descriptor.Path == identity.CoreModule &&
		tool.Path == identity.AnnotationTool &&
		resolved.Path == identity.ToolchainModule {
		return nil
	}
	if descriptor.Path != resolved.Path ||
		descriptor.Version != resolved.Version {
		return fmt.Errorf(
			"annotation descriptor module %s@%s does not match tool module %s@%s",
			descriptor.Path,
			descriptor.Version,
			resolved.Path,
			resolved.Version,
		)
	}
	if descriptor.Version == "" &&
		descriptor.ReplacementPath == "" &&
		resolved.Replacement == nil &&
		!sameDirectory(descriptor.Directory, resolved.Directory) {
		return fmt.Errorf(
			"annotation descriptor and tool use different local source for module %s",
			descriptor.Path,
		)
	}
	replacementPath := ""
	replacementVersion := ""
	replacementDirectory := ""
	if resolved.Replacement != nil {
		replacementPath = resolved.Replacement.Path
		replacementVersion = resolved.Replacement.Version
		replacementDirectory = resolved.Replacement.Directory
	}
	if descriptor.ReplacementPath != replacementPath ||
		descriptor.ReplacementVersion != replacementVersion ||
		!sameDirectory(descriptor.ReplacementDir, replacementDirectory) {
		return fmt.Errorf(
			"annotation descriptor and tool use different replacements for module %s@%s",
			descriptor.Path,
			descriptor.Version,
		)
	}
	return nil
}

func sameDirectory(left, right string) bool {
	if left == "" || right == "" {
		return left == right
	}
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func offlineEnvironment(environment []string, mode string) []string {
	if environment == nil {
		environment = os.Environ()
	}
	result := replaceEnvironmentValue(environment, "GOPROXY", "off")
	flags := environmentValue(result, "GOFLAGS")
	fields := strings.Fields(flags)
	filtered := make([]string, 0, len(fields)+1)
	for index := 0; index < len(fields); index++ {
		if fields[index] == "-mod" {
			index++
			continue
		}
		if strings.HasPrefix(fields[index], "-mod=") {
			continue
		}
		filtered = append(filtered, fields[index])
	}
	filtered = append(filtered, "-mod="+mode)
	return replaceEnvironmentValue(
		result,
		"GOFLAGS",
		strings.Join(filtered, " "),
	)
}

func replaceEnvironmentValue(
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

func environmentValue(environment []string, name string) string {
	for _, entry := range environment {
		key, value, found := strings.Cut(entry, "=")
		if found && strings.EqualFold(key, name) {
			return value
		}
	}
	return ""
}

func renderStderr(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return ": " + value
}
