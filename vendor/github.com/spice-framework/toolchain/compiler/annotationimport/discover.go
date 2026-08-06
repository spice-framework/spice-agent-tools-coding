package annotationimport

import (
	"cmp"
	"errors"
	"fmt"
	goscanner "go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/spice-framework/spice/annotation"
	annotationparser "github.com/spice-framework/toolchain/compiler/parser"
)

// Discovery is the deterministic lexical preload input for one typed compiler
// operation. Semantic resolution still uses only files selected by Go.
type Discovery struct {
	Directives []annotation.ImportDirective
	Packages   []string
	References []annotation.DefinitionReference
}

// NamespacePackages returns the stable package paths whose complete descriptor
// surface was explicitly imported with "* as".
func (discovery Discovery) NamespacePackages() []string {
	packages := make(map[string]struct{})
	for _, directive := range discovery.Directives {
		if directive.Kind == annotation.ImportNamespace {
			packages[directive.Package] = struct{}{}
		}
	}
	return sortedKeys(packages)
}

// Discover lexically finds annotation import comments before the one typed
// package load. Overlays replace matching disk content and no Go code runs.
func Discover(
	root string,
	overlay map[string][]byte,
) (Discovery, error) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return Discovery{}, fmt.Errorf(
			"resolve annotation import discovery root: %w",
			err,
		)
	}
	overlay = cleanOverlay(overlay)
	files, err := discoveryFiles(absoluteRoot, overlay)
	if err != nil {
		return Discovery{}, err
	}
	sourceRoot, err := os.OpenRoot(absoluteRoot)
	if err != nil {
		return Discovery{}, fmt.Errorf(
			"open annotation import discovery root: %w",
			err,
		)
	}
	directives, readErr := readImportDirectives(
		sourceRoot,
		absoluteRoot,
		files,
		overlay,
	)
	if err := errors.Join(readErr, sourceRoot.Close()); err != nil {
		return Discovery{}, err
	}
	sort.Slice(directives, func(i, j int) bool {
		left, right := directives[i], directives[j]
		if left.Position.Filename != right.Position.Filename {
			return left.Position.Filename < right.Position.Filename
		}
		return left.Position.Offset < right.Position.Offset
	})
	packages := make(map[string]struct{})
	references := make(map[annotation.DefinitionReference]struct{})
	for _, directive := range directives {
		packages[directive.Package] = struct{}{}
		for _, binding := range directive.Bindings {
			references[annotation.DefinitionReference{
				Package: directive.Package,
				Symbol:  binding.Imported,
			}] = struct{}{}
		}
	}
	return Discovery{
		Directives: directives,
		Packages:   sortedKeys(packages),
		References: sortedReferences(references),
	}, nil
}

func readImportDirectives(
	sourceRoot *os.Root,
	absoluteRoot string,
	files []string,
	overlay map[string][]byte,
) ([]annotation.ImportDirective, error) {
	var directives []annotation.ImportDirective
	for _, file := range files {
		content, found := overlay[file]
		if !found {
			relative, err := filepath.Rel(absoluteRoot, file)
			if err != nil {
				return nil, fmt.Errorf(
					"resolve annotation import source %q: %w",
					file,
					err,
				)
			}
			content, err = sourceRoot.ReadFile(relative)
			if err != nil {
				return nil, fmt.Errorf(
					"read annotation import source %q: %w",
					file,
					err,
				)
			}
		}
		directives = append(
			directives,
			importsInSource(file, content)...,
		)
	}
	return directives, nil
}

func cleanOverlay(values map[string][]byte) map[string][]byte {
	result := make(map[string][]byte, len(values))
	for file, content := range values {
		result[filepath.Clean(file)] = content
	}
	return result
}

func discoveryFiles(
	root string,
	overlay map[string][]byte,
) ([]string, error) {
	files := make(map[string]struct{})
	err := filepath.WalkDir(root, func(
		path string,
		entry fs.DirEntry,
		walkErr error,
	) error {
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
		return nil, fmt.Errorf("discover annotation import sources: %w", err)
	}
	for path := range overlay {
		if discoverySource(path) &&
			withinRoot(root, path) &&
			!insideNestedModule(root, path) {
			files[path] = struct{}{}
		}
	}
	return sortedKeys(files), nil
}

func nestedModuleDirectory(directory string) bool {
	info, err := os.Stat(filepath.Join(directory, "go.mod"))
	if err != nil || info == nil {
		return false
	}
	return !info.IsDir()
}

func insideNestedModule(root, file string) bool {
	directory := filepath.Dir(file)
	for directory != root {
		if nestedModuleDirectory(directory) {
			return true
		}
		parent := filepath.Dir(directory)
		if parent == directory || !withinRoot(root, parent) {
			return false
		}
		directory = parent
	}
	return false
}

func excludedDirectory(name string) bool {
	switch name {
	case ".git", ".spice", "testdata", "vendor":
		return true
	default:
		return strings.HasPrefix(name, ".")
	}
}

func discoverySource(path string) bool {
	base := filepath.Base(path)
	return strings.EqualFold(filepath.Ext(base), ".go") &&
		!strings.HasSuffix(base, "_test.go") &&
		!strings.HasSuffix(base, "_spice_gen.go") &&
		(!strings.HasPrefix(base, "spice_") ||
			!strings.HasSuffix(base, "_gen.go"))
}

func withinRoot(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && (relative == "." || filepath.IsLocal(relative))
}

func importsInSource(
	file string,
	content []byte,
) []annotation.ImportDirective {
	fileSet := token.NewFileSet()
	sourceFile := fileSet.AddFile(file, -1, len(content))
	var scanner goscanner.Scanner
	scanner.Init(sourceFile, content, nil, goscanner.ScanComments)
	var directives []annotation.ImportDirective
	for {
		position, kind, literal := scanner.Scan()
		if kind == token.EOF {
			return directives
		}
		if kind != token.COMMENT ||
			!strings.HasPrefix(strings.TrimSpace(literal), "//") {
			continue
		}
		sourcePosition := fileSet.Position(position)
		directive, recognized, err := annotationparser.ParseImportComment(
			literal,
			sourcePosition,
		)
		if !recognized || err != nil {
			continue
		}
		directive.PhysicalPosition = sourcePosition
		directives = append(directives, directive)
	}
}

func sortedKeys[T ~string](values map[T]struct{}) []T {
	result := make([]T, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	slices.Sort(result)
	return result
}

func sortedReferences(
	values map[annotation.DefinitionReference]struct{},
) []annotation.DefinitionReference {
	result := make([]annotation.DefinitionReference, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	slices.SortFunc(result, func(left, right annotation.DefinitionReference) int {
		if order := cmp.Compare(left.Package, right.Package); order != 0 {
			return order
		}
		return cmp.Compare(left.Symbol, right.Symbol)
	})
	return result
}
