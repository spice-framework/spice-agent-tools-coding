// @import { Module } from "github.com/spice-framework/spice/annotation/modulith"

// Package scaffold creates minimal, valid-Go Spice applications without
// downloading dependencies or overwriting developer-owned files.
//
// @Module(allowedDependencies=["github.com/spice-framework/toolchain/compiler::targetid"])
package scaffold

import (
	"context"
	"errors"
	"fmt"
	"go/format"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

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
	readme := fmt.Sprintf(`# %s

This application uses valid Go source and committed, inspectable Spice output.
The scaffold did not download dependencies. Resolve the declared module graph,
then verify and generate the application:

`+"```text"+`
go mod download
go tool %s generate --target %s .
go tool %s verify .
go tool %s run --target %s .
`+"```"+`
`, config.Module, CLITool, targetName, CLITool, CLITool, targetName)
	return []plannedFile{
		{name: ".gitignore", content: []byte("/bin/\n"), mode: 0o600},
		{name: "README.md", content: []byte(readme), mode: 0o600},
		{name: "go.mod", content: moduleContent, mode: 0o600},
		{name: "main.go", content: mainContent, mode: 0o600},
	}, nil
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
		file, err := root.OpenFile(
			planned.name,
			os.O_WRONLY|os.O_CREATE|os.O_EXCL,
			planned.mode,
		)
		if err != nil {
			return created, fmt.Errorf("create scaffold file %s: %w", planned.name, err)
		}
		created = append(created, planned.name)
		writeErr := writeAll(file, planned.content)
		closeErr := file.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			return created, fmt.Errorf("write scaffold file %s: %w", planned.name, err)
		}
	}
	entries, err := os.ReadDir(root.Name())
	if err != nil {
		return created, fmt.Errorf("verify scaffold destination: %w", err)
	}
	if len(entries) != len(files) {
		return created, errors.New("scaffold destination changed while files were being created")
	}
	return created, nil
}

func rollback(directory string, names []string, removeDirectory bool) error {
	var problems []error
	for _, name := range slices.Backward(names) {
		if err := os.Remove(filepath.Join(directory, name)); err != nil &&
			!errors.Is(err, os.ErrNotExist) {
			problems = append(problems, fmt.Errorf("remove %s: %w", name, err))
		}
	}
	if removeDirectory {
		if err := os.Remove(directory); err != nil && !errors.Is(err, os.ErrNotExist) {
			problems = append(problems, fmt.Errorf("remove scaffold directory: %w", err))
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
