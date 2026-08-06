// Package autoconfigure discovers and statically decodes Go-native library
// auto-configuration. It never imports or executes application code.
package autoconfigure

import (
	"errors"
	"fmt"
	"go/ast"
	goparser "go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/spice-framework/toolchain/compiler/load"
)

const packageName = "autoconfigure"

// Discovery is the deterministic lexical preload input for one compiler
// operation. Packages are only candidates; Selected resolves actual imports
// from the typed primary program.
type Discovery struct {
	Packages []string
}

// Discover finds explicit blank imports of canonical autoconfigure packages
// before the single typed package load. Overlays replace matching disk files.
func Discover(root string, overlay map[string][]byte) (Discovery, error) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return Discovery{}, fmt.Errorf("resolve auto-configuration discovery root: %w", err)
	}
	cleanedOverlay := make(map[string][]byte, len(overlay))
	for file, content := range overlay {
		cleanedOverlay[filepath.Clean(file)] = content
	}
	files, err := discoveryFiles(absoluteRoot, cleanedOverlay)
	if err != nil {
		return Discovery{}, err
	}
	sourceRoot, err := os.OpenRoot(absoluteRoot)
	if err != nil {
		return Discovery{}, fmt.Errorf("open auto-configuration discovery root: %w", err)
	}
	packages := make(map[string]struct{})
	var readErr error
	for _, file := range files {
		content, found := cleanedOverlay[file]
		if !found {
			relative, relativeErr := filepath.Rel(absoluteRoot, file)
			if relativeErr != nil {
				readErr = fmt.Errorf("resolve auto-configuration source %q: %w", file, relativeErr)
				break
			}
			content, readErr = sourceRoot.ReadFile(relative)
			if readErr != nil {
				readErr = fmt.Errorf("read auto-configuration source %q: %w", file, readErr)
				break
			}
		}
		for _, packagePath := range blankAutoConfigurationImports(file, content) {
			packages[packagePath] = struct{}{}
		}
	}
	if closeErr := sourceRoot.Close(); closeErr != nil {
		readErr = errors.Join(readErr, closeErr)
	}
	if readErr != nil {
		return Discovery{}, readErr
	}
	result := make([]string, 0, len(packages))
	for packagePath := range packages {
		result = append(result, packagePath)
	}
	slices.Sort(result)
	return Discovery{Packages: result}, nil
}

// Selected returns canonical blank imports that occur in typed primary source.
// Candidate imports from unselected workspace packages therefore cannot
// activate library behavior.
func (discovery Discovery) Selected(program *load.Program) []string {
	candidates := make(map[string]struct{}, len(discovery.Packages))
	for _, packagePath := range discovery.Packages {
		candidates[packagePath] = struct{}{}
	}
	selected := make(map[string]struct{})
	for _, pkg := range program.PrimaryPackages() {
		for _, file := range pkg.Syntax {
			for _, declaration := range file.Decls {
				imports, ok := declaration.(*ast.GenDecl)
				if !ok || imports.Tok != token.IMPORT {
					continue
				}
				for _, spec := range imports.Specs {
					importSpec, ok := spec.(*ast.ImportSpec)
					if !ok || importSpec.Name == nil || importSpec.Name.Name != "_" {
						continue
					}
					packagePath, err := strconv.Unquote(importSpec.Path.Value)
					if err != nil {
						continue
					}
					if _, candidate := candidates[packagePath]; candidate {
						selected[packagePath] = struct{}{}
					}
				}
			}
		}
	}
	result := make([]string, 0, len(selected))
	for packagePath := range selected {
		result = append(result, packagePath)
	}
	slices.Sort(result)
	return result
}

func blankAutoConfigurationImports(filename string, content []byte) []string {
	file, err := goparser.ParseFile(
		token.NewFileSet(),
		filename,
		content,
		goparser.ImportsOnly,
	)
	if err != nil {
		return nil
	}
	var result []string
	for _, importSpec := range file.Imports {
		if importSpec.Name == nil || importSpec.Name.Name != "_" {
			continue
		}
		packagePath, err := strconv.Unquote(importSpec.Path.Value)
		if err != nil || path.Base(packagePath) != packageName {
			continue
		}
		result = append(result, packagePath)
	}
	slices.Sort(result)
	return slices.Compact(result)
}

func discoveryFiles(root string, overlay map[string][]byte) ([]string, error) {
	files := make(map[string]struct{})
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && excludedDirectory(entry.Name()) {
				return filepath.SkipDir
			}
			if path != root && nestedModuleDirectory(path) {
				return filepath.SkipDir
			}
			return nil
		}
		path = filepath.Clean(path)
		if discoverySource(path) {
			files[path] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("discover auto-configuration sources: %w", err)
	}
	for file := range overlay {
		if discoverySource(file) && withinRoot(root, file) && !insideNestedModule(root, file) {
			files[file] = struct{}{}
		}
	}
	result := make([]string, 0, len(files))
	for file := range files {
		result = append(result, file)
	}
	sort.Strings(result)
	return result, nil
}

func excludedDirectory(name string) bool {
	switch name {
	case ".git", ".spice", "testdata", "vendor":
		return true
	default:
		return strings.HasPrefix(name, ".")
	}
}

func nestedModuleDirectory(directory string) bool {
	info, err := os.Stat(filepath.Join(directory, "go.mod"))
	return err == nil && info != nil && !info.IsDir()
}

func insideNestedModule(root, file string) bool {
	for directory := filepath.Dir(file); directory != root; directory = filepath.Dir(directory) {
		if nestedModuleDirectory(directory) {
			return true
		}
		parent := filepath.Dir(directory)
		if parent == directory || !withinRoot(root, parent) {
			return false
		}
	}
	return false
}

func discoverySource(file string) bool {
	base := filepath.Base(file)
	return strings.EqualFold(filepath.Ext(base), ".go") &&
		!strings.HasSuffix(base, "_test.go") &&
		!strings.HasSuffix(base, "_spice_gen.go") &&
		(!strings.HasPrefix(base, "spice_") || !strings.HasSuffix(base, "_gen.go"))
}

func withinRoot(root, file string) bool {
	relative, err := filepath.Rel(root, file)
	return err == nil && (relative == "." || filepath.IsLocal(relative))
}
