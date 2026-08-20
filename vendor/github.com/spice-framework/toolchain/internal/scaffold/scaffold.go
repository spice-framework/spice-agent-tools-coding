// @import { Module } from "github.com/spice-framework/spice/annotation/modulith"

// Package scaffold creates minimal, valid-Go Spice applications without
// downloading dependencies or overwriting developer-owned files.
//
// @Module(allowedDependencies=["github.com/spice-framework/toolchain/compiler::style", "github.com/spice-framework/toolchain/compiler::targetid"])
package scaffold

import (
	"context"
	"errors"
	"fmt"
	"go/format"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	compilerstyle "github.com/spice-framework/toolchain/compiler/style"
	"github.com/spice-framework/toolchain/compiler/targetid"
	"github.com/spice-framework/toolchain/internal/identity"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

const (
	// FrameworkModule is the canonical public Spice core module.
	FrameworkModule = identity.CoreModule
	// ToolchainModule is the independently versioned compiler and CLI module.
	ToolchainModule = identity.ToolchainModule
	// CLITool is the canonical Spice command package.
	CLITool = identity.CLITool
	// AnnotationTool is the canonical official annotation tool package.
	AnnotationTool = identity.AnnotationTool
	goVersion      = "1.26.0"
	toolchain      = "go1.26.5"
)

// Config is one fail-closed application scaffold request.
type Config struct {
	Directory    string
	Module       string
	SpiceVersion string
	// ToolchainVersion selects the independently released compiler/tool line.
	ToolchainVersion string
	Replace          string
	// ToolchainReplace is an explicit local development replacement. Scaffold
	// performs no implicit sibling or workspace discovery.
	ToolchainReplace string
	// Profile selects an opt-in source-organization contract.
	Profile compilerstyle.Profile
}

// DeclarationKind identifies one conservative typed source scaffold.
type DeclarationKind string

const (
	DeclarationModule     DeclarationKind = "module"
	DeclarationService    DeclarationKind = "service"
	DeclarationRepository DeclarationKind = "repository"
	DeclarationController DeclarationKind = "controller"
	DeclarationComponent  DeclarationKind = "component"
	DeclarationEnum       DeclarationKind = "enum"
)

// DeclarationConfig is one source declaration scaffold request.
type DeclarationConfig struct {
	Directory string
	Package   string
	Kind      DeclarationKind
	Name      string
}

// Result identifies the created application and its deterministic files.
type Result struct {
	Directory string
	Files     []string
}

type plannedFile struct {
	name    string
	content []byte
	mode    os.FileMode
}

// Create writes a complete scaffold only into a new or empty directory. It
// performs no Go command, module resolution, dependency download, or VCS work.
func Create(ctx context.Context, config Config) (Result, error) {
	if ctx == nil {
		return Result{}, errors.New("scaffold context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	validated, err := validateConfig(config)
	if err != nil {
		return Result{}, err
	}
	files, err := render(validated)
	if err != nil {
		return Result{}, err
	}
	return apply(ctx, validated.Directory, files)
}

// CreateDeclaration writes one deterministic source declaration without
// overwriting an existing file or invoking external tools.
func CreateDeclaration(ctx context.Context, config DeclarationConfig) (Result, error) {
	if ctx == nil {
		return Result{}, errors.New("declaration scaffold context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	validated, err := validateDeclarationConfig(config)
	if err != nil {
		return Result{}, err
	}
	planned, err := renderDeclaration(validated)
	if err != nil {
		return Result{}, err
	}
	createdDirectories, err := prepareDeclarationDirectory(validated.Directory)
	if err != nil {
		return Result{}, err
	}
	root, err := os.OpenRoot(validated.Directory)
	if err != nil {
		return Result{}, errors.Join(
			fmt.Errorf("open declaration scaffold directory: %w", err),
			cleanupDirectories(createdDirectories),
		)
	}
	created, writeErr := writeDeclarationFile(ctx, root, planned)
	closeErr := root.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		rollbackErr := rollback(validated.Directory, created, false)
		cleanupErr := cleanupDirectories(createdDirectories)
		return Result{}, errors.Join(err, rollbackErr, cleanupErr)
	}
	return Result{
		Directory: validated.Directory,
		Files:     []string{planned.name},
	}, nil
}

func writeDeclarationFile(
	ctx context.Context,
	root *os.Root,
	planned plannedFile,
) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name := filepath.Clean(filepath.FromSlash(planned.name))
	if filepath.Base(name) != name || name == "." {
		return nil, fmt.Errorf("invalid declaration scaffold file path %q", planned.name)
	}
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, planned.mode)
	if err != nil {
		return nil, fmt.Errorf("create declaration scaffold file %s: %w", name, err)
	}
	created := []string{name}
	writeErr := writeAll(file, planned.content)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return created, fmt.Errorf("write declaration scaffold file %s: %w", name, err)
	}
	return created, nil
}

func validateConfig(config Config) (Config, error) {
	if strings.TrimSpace(config.Directory) == "" {
		return Config{}, errors.New("scaffold directory is required")
	}
	if err := module.CheckPath(config.Module); err != nil {
		return Config{}, fmt.Errorf("validate application module %q: %w", config.Module, err)
	}
	if !semver.IsValid(config.SpiceVersion) {
		return Config{}, fmt.Errorf(
			"spice version %q must be an exact semantic version such as v0.2.0",
			config.SpiceVersion,
		)
	}
	if !semver.IsValid(config.ToolchainVersion) {
		return Config{}, fmt.Errorf(
			"toolchain version %q must be an exact semantic version such as v0.2.0",
			config.ToolchainVersion,
		)
	}
	if err := compilerstyle.ValidateProfile(config.Profile); err != nil {
		return Config{}, err
	}
	absolute, err := filepath.Abs(config.Directory)
	if err != nil {
		return Config{}, fmt.Errorf("resolve scaffold directory: %w", err)
	}
	config.Directory = filepath.Clean(absolute)
	if config.Replace != "" {
		config.Replace, err = validateReplacement("Spice", config.Replace)
		if err != nil {
			return Config{}, err
		}
	}
	if config.ToolchainReplace != "" {
		config.ToolchainReplace, err = validateReplacement(
			"toolchain",
			config.ToolchainReplace,
		)
		if err != nil {
			return Config{}, err
		}
	}
	return config, nil
}

func validateReplacement(name, value string) (string, error) {
	replacement, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve local %s replacement: %w", name, err)
	}
	info, err := os.Stat(replacement)
	if err != nil {
		return "", fmt.Errorf("inspect local %s replacement: %w", name, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf(
			"local %s replacement %q is not a directory",
			name,
			replacement,
		)
	}
	if _, err = os.Stat(filepath.Join(replacement, "go.mod")); err != nil {
		return "", fmt.Errorf("inspect local %s replacement go.mod: %w", name, err)
	}
	return filepath.Clean(replacement), nil
}

func render(config Config) ([]plannedFile, error) {
	moduleContent, err := renderModule(config)
	if err != nil {
		return nil, err
	}
	targetName := applicationTargetName(config.Module)
	targetID := targetid.Default(targetName)
	mainContent, err := format.Source([]byte(fmt.Sprintf(`package main

import (
	"os"

	spiceapp %q
)

// @import { Application } from %q

// @Application
func main() {
	// This package-main boundary alone owns the process status.
	os.Exit(spiceapp.Main(os.Args[1:]))
}
`, config.Module+"/internal/spicegen/"+targetID, FrameworkModule+"/annotation/core")))
	if err != nil {
		return nil, fmt.Errorf("format scaffold main.go: %w", err)
	}
	mainName := "main.go"
	patterns := "."
	profileOption := ""
	files := []plannedFile{
		{name: ".gitignore", content: []byte("/bin/\n"), mode: 0o600},
		{name: "go.mod", content: moduleContent, mode: 0o600},
	}
	if config.Profile == compilerstyle.ProfileJavaStructured {
		packageName := strings.ToLower(targetName[:1]) + targetName[1:]
		if !token.IsIdentifier(packageName) {
			packageName = "application"
		}
		mainName = filepath.ToSlash(filepath.Join("cmd", packageName, "main.go"))
		patterns = "./..."
		profileOption = " --profile=java-structured"
		packageContent := fmt.Sprintf(`// @import { Module } from %q

// Package %s is the root application module.
//
// @Module
package %s
`, FrameworkModule+"/annotation/modulith", packageName, packageName)
		files = append(files, plannedFile{
			name:    filepath.ToSlash(filepath.Join("internal", packageName, "package.go")),
			content: []byte(packageContent),
			mode:    0o600,
		})
	}
	readme := fmt.Sprintf(`# %s

This application uses valid Go source and committed, inspectable Spice output.
The scaffold did not download dependencies. Resolve the declared module graph,
then verify and generate the application:

`+"```text"+`
go mod download
go tool %s generate --target %s %s
go tool %s verify%s %s
go tool %s run --target %s %s
`+"```"+`
`, config.Module, CLITool, targetName, patterns, CLITool, profileOption, patterns, CLITool, targetName, patterns)
	files = append(
		files,
		plannedFile{name: "README.md", content: []byte(readme), mode: 0o600},
		plannedFile{name: mainName, content: mainContent, mode: 0o600},
	)
	return files, nil
}

func validateDeclarationConfig(config DeclarationConfig) (DeclarationConfig, error) {
	if strings.TrimSpace(config.Directory) == "" {
		return DeclarationConfig{}, errors.New("declaration scaffold directory is required")
	}
	if !slices.Contains([]DeclarationKind{
		DeclarationModule,
		DeclarationService,
		DeclarationRepository,
		DeclarationController,
		DeclarationComponent,
		DeclarationEnum,
	}, config.Kind) {
		return DeclarationConfig{}, fmt.Errorf("unsupported declaration scaffold kind %q", config.Kind)
	}
	absolute, err := filepath.Abs(config.Directory)
	if err != nil {
		return DeclarationConfig{}, fmt.Errorf("resolve declaration scaffold directory: %w", err)
	}
	config.Directory = filepath.Clean(absolute)
	if config.Package == "" {
		config.Package = filepath.Base(config.Directory)
	}
	if !token.IsIdentifier(config.Package) {
		return DeclarationConfig{}, fmt.Errorf("package name %q is not a valid Go identifier", config.Package)
	}
	if config.Kind == DeclarationModule {
		if config.Name == "" {
			config.Name = config.Package
		}
		return config, nil
	}
	if !token.IsIdentifier(config.Name) || !unicode.IsUpper([]rune(config.Name)[0]) {
		return DeclarationConfig{}, fmt.Errorf(
			"%s name %q must be an exported Go identifier",
			config.Kind,
			config.Name,
		)
	}
	return config, nil
}

func renderDeclaration(config DeclarationConfig) (plannedFile, error) {
	if config.Kind == DeclarationModule {
		content := fmt.Sprintf(`// @import { Module } from %q

// Package %s is an application module.
//
// @Module
package %s
`, FrameworkModule+"/annotation/modulith", config.Package, config.Package)
		return plannedFile{name: "package.go", content: []byte(content), mode: 0o600}, nil
	}
	annotationName := map[DeclarationKind]string{
		DeclarationService:    "Service",
		DeclarationRepository: "Repository",
		DeclarationController: "Controller",
		DeclarationComponent:  "Component",
		DeclarationEnum:       "Enum",
	}[config.Kind]
	annotationPackage := FrameworkModule + "/annotation/core"
	declaration := fmt.Sprintf(`// @import { %s } from %q

package %s

// @%s
type %s struct{}

// New%s constructs %s.
func New%s() *%s {
	return &%s{}
}
`, annotationName, annotationPackage, config.Package, annotationName, config.Name, config.Name, config.Name, config.Name, config.Name, config.Name)
	if config.Kind == DeclarationController {
		annotationPackage = FrameworkModule + "/annotation/web"
		declaration = fmt.Sprintf(`// @import { Controller, Get } from %q

package %s

import "net/http"

// @Controller
type %s struct{}

// New%s constructs %s.
func New%s() *%s {
	return &%s{}
}

// @Get("/")
func (*%s) Index(http.ResponseWriter, *http.Request) {}
`, annotationPackage, config.Package, config.Name, config.Name, config.Name, config.Name, config.Name, config.Name, config.Name)
	}
	if config.Kind == DeclarationEnum {
		declaration = fmt.Sprintf(`// @import { Enum } from %q

package %s

// @Enum
type %s string

const %sUnknown %s = "unknown"
`, annotationPackage, config.Package, config.Name, config.Name, config.Name)
	}
	content, err := format.Source([]byte(declaration))
	if err != nil {
		return plannedFile{}, fmt.Errorf("format %s scaffold: %w", config.Kind, err)
	}
	return plannedFile{
		name:    goFileName(config.Name) + ".go",
		content: content,
		mode:    0o600,
	}, nil
}

func goFileName(name string) string {
	runes := []rune(name)
	var result strings.Builder
	for index, current := range runes {
		if unicode.IsUpper(current) && index > 0 &&
			(unicode.IsLower(runes[index-1]) ||
				(index+1 < len(runes) && unicode.IsLower(runes[index+1]))) {
			result.WriteByte('_')
		}
		result.WriteRune(unicode.ToLower(current))
	}
	return result.String()
}

func applicationTargetName(modulePath string) string {
	name := filepath.Base(modulePath)
	if name == "" || name == "." || name == string(filepath.Separator) {
		return "application"
	}
	if name[0] >= 'a' && name[0] <= 'z' {
		name = string(name[0]-('a'-'A')) + name[1:]
	}
	return name
}

func renderModule(config Config) ([]byte, error) {
	file := new(modfile.File)
	operations := []func() error{
		func() error { return file.AddModuleStmt(config.Module) },
		func() error { return file.AddGoStmt(goVersion) },
		func() error { return file.AddToolchainStmt(toolchain) },
		func() error { return file.AddTool(CLITool) },
		func() error { return file.AddTool(AnnotationTool) },
		func() error { return file.AddRequire(FrameworkModule, config.SpiceVersion) },
		func() error {
			return file.AddRequire(ToolchainModule, config.ToolchainVersion)
		},
	}
	if config.ToolchainReplace != "" {
		operations = append(operations, func() error {
			return file.AddReplace(
				ToolchainModule,
				"",
				filepath.ToSlash(config.ToolchainReplace),
				"",
			)
		})
	}
	if config.Replace != "" {
		operations = append(operations, func() error {
			return file.AddReplace(
				FrameworkModule,
				"",
				filepath.ToSlash(config.Replace),
				"",
			)
		})
	}
	for _, operation := range operations {
		if err := operation(); err != nil {
			return nil, fmt.Errorf("render scaffold go.mod: %w", err)
		}
	}
	content, err := file.Format()
	if err != nil {
		return nil, fmt.Errorf("format scaffold go.mod: %w", err)
	}
	return content, nil
}

func apply(ctx context.Context, directory string, files []plannedFile) (Result, error) {
	createdDirectory, err := prepareDirectory(directory)
	if err != nil {
		return Result{}, err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		openErr := fmt.Errorf("open scaffold directory: %w", err)
		if createdDirectory {
			return Result{}, errors.Join(openErr, os.Remove(directory))
		}
		return Result{}, openErr
	}
	created, applyErr := writePlannedFiles(ctx, root, files)
	closeErr := root.Close()
	if err := errors.Join(applyErr, closeErr); err != nil {
		rollbackErr := rollback(directory, created, createdDirectory)
		return Result{}, errors.Join(err, rollbackErr)
	}
	names := make([]string, len(files))
	for index, file := range files {
		names[index] = file.name
	}
	slices.Sort(names)
	return Result{Directory: directory, Files: names}, nil
}

func prepareDirectory(directory string) (bool, error) {
	info, err := os.Lstat(directory)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return false, fmt.Errorf("scaffold destination %q is not a real directory", directory)
		}
		entries, readErr := os.ReadDir(directory)
		if readErr != nil {
			return false, fmt.Errorf("inspect scaffold destination: %w", readErr)
		}
		if len(entries) != 0 {
			return false, fmt.Errorf("scaffold destination %q is not empty", directory)
		}
		return false, nil
	case errors.Is(err, os.ErrNotExist):
		parent := filepath.Dir(directory)
		parentInfo, parentErr := os.Stat(parent)
		if parentErr != nil {
			return false, fmt.Errorf("inspect scaffold parent: %w", parentErr)
		}
		if !parentInfo.IsDir() {
			return false, fmt.Errorf("scaffold parent %q is not a directory", parent)
		}
		if mkdirErr := os.Mkdir(directory, 0o750); mkdirErr != nil {
			return false, fmt.Errorf("create scaffold destination: %w", mkdirErr)
		}
		return true, nil
	default:
		return false, fmt.Errorf("inspect scaffold destination: %w", err)
	}
}

func writePlannedFiles(
	ctx context.Context,
	root *os.Root,
	files []plannedFile,
) ([]string, error) {
	created := make([]string, 0, len(files))
	for _, planned := range files {
		if err := ctx.Err(); err != nil {
			return created, err
		}
		cleanName := filepath.Clean(filepath.FromSlash(planned.name))
		if cleanName == "." || filepath.IsAbs(cleanName) || cleanName == ".." ||
			strings.HasPrefix(cleanName, ".."+string(filepath.Separator)) {
			return created, fmt.Errorf("invalid scaffold file path %q", planned.name)
		}
		parent := filepath.Dir(cleanName)
		if parent != "." {
			if err := root.MkdirAll(parent, 0o750); err != nil {
				return created, fmt.Errorf("create scaffold directory %s: %w", parent, err)
			}
		}
		file, err := root.OpenFile(
			cleanName,
			os.O_WRONLY|os.O_CREATE|os.O_EXCL,
			planned.mode,
		)
		if err != nil {
			return created, fmt.Errorf("create scaffold file %s: %w", planned.name, err)
		}
		created = append(created, cleanName)
		writeErr := writeAll(file, planned.content)
		closeErr := file.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			return created, fmt.Errorf("write scaffold file %s: %w", planned.name, err)
		}
	}
	actual, err := scaffoldEntries(root.Name())
	if err != nil {
		return created, fmt.Errorf("verify scaffold destination: %w", err)
	}
	expected := plannedEntries(files)
	if !slices.Equal(actual, expected) {
		return created, errors.New("scaffold destination changed while files were being created")
	}
	return created, nil
}

func scaffoldEntries(directory string) ([]string, error) {
	var result []string
	err := filepath.WalkDir(directory, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == directory {
			return nil
		}
		relative, err := filepath.Rel(directory, name)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			relative += string(filepath.Separator)
		}
		result = append(result, relative)
		return nil
	})
	slices.Sort(result)
	return result, err
}

func plannedEntries(files []plannedFile) []string {
	unique := make(map[string]struct{})
	for _, planned := range files {
		name := filepath.Clean(filepath.FromSlash(planned.name))
		unique[name] = struct{}{}
		for parent := filepath.Dir(name); parent != "."; parent = filepath.Dir(parent) {
			unique[parent+string(filepath.Separator)] = struct{}{}
		}
	}
	result := make([]string, 0, len(unique))
	for name := range unique {
		result = append(result, name)
	}
	slices.Sort(result)
	return result
}

func rollback(directory string, names []string, removeDirectory bool) error {
	var problems []error
	for _, name := range slices.Backward(names) {
		if err := os.Remove(filepath.Join(directory, name)); err != nil &&
			!errors.Is(err, os.ErrNotExist) {
			problems = append(problems, fmt.Errorf("remove %s: %w", name, err))
		}
	}
	parents := make(map[string]struct{})
	for _, name := range names {
		for parent := filepath.Dir(name); parent != "."; parent = filepath.Dir(parent) {
			parents[parent] = struct{}{}
		}
	}
	parentNames := make([]string, 0, len(parents))
	for parent := range parents {
		parentNames = append(parentNames, parent)
	}
	slices.SortFunc(parentNames, func(left, right string) int {
		return strings.Count(right, string(filepath.Separator)) - strings.Count(left, string(filepath.Separator))
	})
	for _, parent := range parentNames {
		if err := os.Remove(filepath.Join(directory, parent)); err != nil &&
			!errors.Is(err, os.ErrNotExist) {
			problems = append(problems, fmt.Errorf("remove %s: %w", parent, err))
		}
	}
	if removeDirectory {
		if err := os.Remove(directory); err != nil && !errors.Is(err, os.ErrNotExist) {
			problems = append(problems, fmt.Errorf("remove scaffold directory: %w", err))
		}
	}
	return errors.Join(problems...)
}

func prepareDeclarationDirectory(directory string) ([]string, error) {
	var missing []string
	current := directory
	for {
		info, err := os.Lstat(current)
		switch {
		case err == nil:
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return nil, fmt.Errorf("declaration scaffold parent %q is not a real directory", current)
			}
			created := make([]string, 0, len(missing))
			for _, name := range slices.Backward(missing) {
				if mkdirErr := os.Mkdir(name, 0o750); mkdirErr != nil {
					cleanupErr := cleanupDirectories(created)
					return nil, errors.Join(fmt.Errorf("create declaration scaffold directory: %w", mkdirErr), cleanupErr)
				}
				created = append(created, name)
			}
			return created, nil
		case errors.Is(err, os.ErrNotExist):
			missing = append(missing, current)
			parent := filepath.Dir(current)
			if parent == current {
				return nil, fmt.Errorf("find declaration scaffold parent for %q", directory)
			}
			current = parent
		default:
			return nil, fmt.Errorf("inspect declaration scaffold directory: %w", err)
		}
	}
}

func cleanupDirectories(directories []string) error {
	var problems []error
	for _, directory := range slices.Backward(directories) {
		if err := os.Remove(directory); err != nil && !errors.Is(err, os.ErrNotExist) {
			problems = append(problems, fmt.Errorf("remove directory %s: %w", directory, err))
		}
	}
	return errors.Join(problems...)
}

func writeAll(writer io.Writer, content []byte) error {
	for len(content) != 0 {
		written, err := writer.Write(content)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		content = content[written:]
	}
	return nil
}
