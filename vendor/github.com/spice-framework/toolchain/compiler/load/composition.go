package load

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"
)

// compositionCandidate records an untyped, explicit blank import discovered
// from a requested root. The imported package is added to the one subsequent
// typed load, then admitted only when Go resolves it to the same module.
type compositionCandidate struct {
	importPath  string
	ownerModule string
}

func discoverCompositionCandidates(
	options Options,
	patterns []string,
	overlay map[string][]byte,
) ([]compositionCandidate, map[string]struct{}) {
	directories, requested := compositionPatternDirectories(options.Dir, patterns)
	buildContext := compositionBuildContext(options)
	values := make(map[compositionCandidate]struct{})
	for _, directory := range directories {
		_, ownerModule, found, err := enclosingModule(directory)
		if err != nil || !found {
			continue
		}
		loaded, importErr := buildContext.ImportDir(directory, build.ImportComment)
		if importErr != nil || loaded == nil {
			continue
		}
		filenames := append([]string(nil), loaded.GoFiles...)
		filenames = append(filenames, loaded.CgoFiles...)
		sort.Strings(filenames)
		for _, name := range filenames {
			filename := filepath.Join(directory, name)
			var source any
			if content, found := overlayFileContent(overlay, filename); found {
				source = content
			}
			file, err := parser.ParseFile(
				token.NewFileSet(),
				filename,
				source,
				parser.ImportsOnly,
			)
			if err != nil {
				continue
			}
			for _, importPath := range blankImportPaths([]*ast.File{file}) {
				if potentialCompositionImport(ownerModule, importPath) {
					values[compositionCandidate{
						importPath:  importPath,
						ownerModule: ownerModule,
					}] = struct{}{}
				}
			}
		}
	}
	result := make([]compositionCandidate, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].importPath != result[j].importPath {
			return result[i].importPath < result[j].importPath
		}
		return result[i].ownerModule < result[j].ownerModule
	})
	return result, requested
}

func compositionPatternDirectories(
	directory string,
	patterns []string,
) ([]string, map[string]struct{}) {
	if directory == "" {
		directory = "."
	}
	base, err := filepath.Abs(directory)
	if err != nil {
		return nil, nil
	}
	moduleRoot, modulePath, moduleFound, moduleErr := enclosingModule(base)
	if moduleErr != nil {
		moduleFound = false
	}

	directories := make(map[string]struct{})
	requested := make(map[string]struct{})
	for _, pattern := range patterns {
		start, recursive, resolved := compositionPatternDirectory(
			base,
			moduleRoot,
			modulePath,
			moduleFound,
			pattern,
		)
		if !resolved {
			if !strings.Contains(pattern, "...") &&
				!strings.HasPrefix(pattern, "file=") {
				requested[pattern] = struct{}{}
			}
			continue
		}
		if recursive {
			walked, walkErr := walkCompositionDirectories(start)
			if walkErr != nil {
				continue
			}
			for _, candidate := range walked {
				directories[candidate] = struct{}{}
			}
		} else {
			directories[filepath.Clean(start)] = struct{}{}
		}
	}

	result := make([]string, 0, len(directories))
	for candidate := range directories {
		result = append(result, candidate)
		if importPath, found := compositionDirectoryImportPath(candidate); found {
			requested[importPath] = struct{}{}
		}
	}
	sort.Strings(result)
	return result, requested
}

func compositionPatternDirectory(
	base string,
	moduleRoot string,
	modulePath string,
	moduleFound bool,
	pattern string,
) (string, bool, bool) {
	recursive := strings.HasSuffix(pattern, "/...") || pattern == "..."
	trimmed := strings.TrimSuffix(pattern, "/...")
	if pattern == "..." {
		trimmed = "."
	}
	switch {
	case trimmed == ".":
		return base, recursive, true
	case filepath.IsAbs(trimmed):
		return filepath.Clean(trimmed), recursive, true
	case strings.HasPrefix(trimmed, "./") ||
		strings.HasPrefix(trimmed, "../"):
		return filepath.Clean(filepath.Join(base, filepath.FromSlash(trimmed))), recursive, true
	case moduleFound &&
		(trimmed == modulePath || strings.HasPrefix(trimmed, modulePath+"/")):
		relative := strings.TrimPrefix(trimmed, modulePath)
		relative = strings.TrimPrefix(relative, "/")
		return filepath.Join(moduleRoot, filepath.FromSlash(relative)), recursive, true
	default:
		return "", false, false
	}
}

func walkCompositionDirectories(root string) ([]string, error) {
	root = filepath.Clean(root)
	var result []string
	err := filepath.WalkDir(root, func(
		current string,
		entry fs.DirEntry,
		walkErr error,
	) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		if current != root {
			name := entry.Name()
			if name == "vendor" || name == "testdata" ||
				strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return filepath.SkipDir
			}
			if _, statErr := os.Stat(filepath.Join(current, "go.mod")); statErr == nil {
				return filepath.SkipDir
			}
		}
		result = append(result, current)
		return nil
	})
	sort.Strings(result)
	return result, err
}

func compositionDirectoryImportPath(directory string) (string, bool) {
	root, modulePath, found, err := enclosingModule(directory)
	if err != nil || !found {
		return "", false
	}
	relative, err := filepath.Rel(root, directory)
	if err != nil || (relative != "." && !filepath.IsLocal(relative)) {
		return "", false
	}
	if relative == "." {
		return modulePath, true
	}
	return path.Join(modulePath, filepath.ToSlash(relative)), true
}

func compositionBuildContext(options Options) build.Context {
	result := build.Default
	environment := make(map[string]string)
	for _, value := range options.Env {
		name, setting, found := strings.Cut(value, "=")
		if found {
			environment[name] = setting
		}
	}
	if value := environment["GOOS"]; value != "" {
		result.GOOS = value
	}
	if value := environment["GOARCH"]; value != "" {
		result.GOARCH = value
	}
	if value := environment["CGO_ENABLED"]; value != "" {
		result.CgoEnabled = value == "1"
	}
	flags := append([]string(nil), options.BuildFlags...)
	flags = append(flags, strings.Fields(environment["GOFLAGS"])...)
	result.BuildTags = compositionBuildTags(flags)
	return result
}

func compositionBuildTags(flags []string) []string {
	var tags []string
	for index := 0; index < len(flags); index++ {
		value := ""
		switch {
		case flags[index] == "-tags" && index+1 < len(flags):
			index++
			value = flags[index]
		case strings.HasPrefix(flags[index], "-tags="):
			value = strings.TrimPrefix(flags[index], "-tags=")
		}
		for tag := range strings.FieldsFuncSeq(value, func(character rune) bool {
			return character == ',' || character == ' ' || character == '\t'
		}) {
			if tag != "" {
				tags = append(tags, tag)
			}
		}
	}
	sort.Strings(tags)
	return slicesCompact(tags)
}

func overlayFileContent(
	overlay map[string][]byte,
	filename string,
) ([]byte, bool) {
	if content, found := overlay[filename]; found {
		return content, true
	}
	cleanFilename := filepath.Clean(filename)
	for overlayFilename, content := range overlay {
		cleanOverlayFilename := filepath.Clean(overlayFilename)
		if cleanOverlayFilename == cleanFilename ||
			equalFoldWindowsPath(cleanOverlayFilename, cleanFilename) {
			return content, true
		}
	}
	return nil, false
}

func equalFoldWindowsPath(left, right string) bool {
	return filepath.Separator == '\\' && strings.EqualFold(left, right)
}

func potentialCompositionImport(modulePath, importPath string) bool {
	if importPath != modulePath &&
		!strings.HasPrefix(importPath, modulePath+"/") {
		return false
	}
	return path.Base(importPath) != "autoconfigure" &&
		!strings.Contains(importPath, "/internal/spicegen/")
}

func compositionCandidatePaths(candidates []compositionCandidate) []string {
	paths := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		paths = append(paths, candidate.importPath)
	}
	sort.Strings(paths)
	return slicesCompact(paths)
}

func selectProgramRoots(
	roots []*packages.Package,
	requested map[string]struct{},
	auxiliary []string,
	candidates []compositionCandidate,
) []*packages.Package {
	auxiliaryPaths := make(map[string]struct{}, len(auxiliary))
	for _, importPath := range auxiliary {
		auxiliaryPaths[importPath] = struct{}{}
	}
	candidateModules := make(map[string]map[string]struct{})
	for _, candidate := range candidates {
		modules := candidateModules[candidate.importPath]
		if modules == nil {
			modules = make(map[string]struct{})
			candidateModules[candidate.importPath] = modules
		}
		modules[candidate.ownerModule] = struct{}{}
	}

	result := make([]*packages.Package, 0, len(roots))
	seen := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		if root == nil {
			continue
		}
		_, isRequested := requested[root.PkgPath]
		_, isAuxiliary := auxiliaryPaths[root.PkgPath]
		modules, isCandidate := candidateModules[root.PkgPath]
		_, isComposition := modules[modulePathOf(root)]
		if isCandidate && !isRequested && !isAuxiliary && !isComposition {
			continue
		}
		identity := packageIdentity(root)
		if _, duplicate := seen[identity]; duplicate {
			continue
		}
		seen[identity] = struct{}{}
		result = append(result, root)
	}
	return result
}

func modulePathOf(pkg *packages.Package) string {
	if pkg == nil || pkg.Module == nil {
		return ""
	}
	return pkg.Module.Path
}

func blankImportPaths(files []*ast.File) []string {
	values := make(map[string]struct{})
	for _, file := range files {
		if file == nil {
			continue
		}
		for _, importSpec := range file.Imports {
			if importSpec.Name == nil || importSpec.Name.Name != "_" {
				continue
			}
			importPath, err := strconv.Unquote(importSpec.Path.Value)
			if err == nil {
				values[importPath] = struct{}{}
			}
		}
	}
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
