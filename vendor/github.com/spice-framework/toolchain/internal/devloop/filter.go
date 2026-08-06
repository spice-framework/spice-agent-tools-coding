package devloop

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

var defaultExcludedDirectories = map[string]struct{}{
	".cache":       {},
	".git":         {},
	".idea":        {},
	".tools":       {},
	".vscode":      {},
	"bin":          {},
	"dist":         {},
	"node_modules": {},
	"vendor":       {},
}

var defaultRelevantExtensions = map[string]struct{}{
	".go":   {},
	".html": {},
	".json": {},
	".sql":  {},
	".tmpl": {},
	".toml": {},
	".tpl":  {},
	".yaml": {},
	".yml":  {},
}

// PathRules adds workspace-relative include and exclude patterns to the
// development watch policy. Patterns support path.Match syntax and ** segments.
type PathRules struct {
	Include []string
	Exclude []string
}

// PathFilter applies normalized, deterministic development watch rules.
type PathFilter struct {
	includes []string
	excludes []string
}

// NewPathFilter validates and defensively copies configurable path rules.
func NewPathFilter(rules PathRules) (PathFilter, error) {
	includes, err := validatePatterns(rules.Include)
	if err != nil {
		return PathFilter{}, fmt.Errorf("validate development include rules: %w", err)
	}
	excludes, err := validatePatterns(rules.Exclude)
	if err != nil {
		return PathFilter{}, fmt.Errorf("validate development exclude rules: %w", err)
	}
	return PathFilter{
		includes: includes,
		excludes: excludes,
	}, nil
}

func validatePatterns(patterns []string) ([]string, error) {
	result := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		normalized := filepath.ToSlash(filepath.Clean(pattern))
		if pattern == "" ||
			normalized == "." ||
			!filepath.IsLocal(filepath.FromSlash(normalized)) {
			return nil, fmt.Errorf("path pattern %q must be workspace-relative", pattern)
		}
		if _, err := path.Match(strings.ReplaceAll(normalized, "**", "*"), "probe"); err != nil {
			return nil, fmt.Errorf("invalid path pattern %q: %w", pattern, err)
		}
		result = append(result, normalized)
	}
	slices.Sort(result)
	return slices.Compact(result), nil
}

// Match reports whether a normalized workspace-relative file is relevant.
func (filter PathFilter) Match(filePath string) bool {
	normalized, err := normalizeRelativePath(filePath)
	if err != nil || filter.excluded(normalized) || temporaryEditorPath(normalized) {
		return false
	}
	if defaultGeneratedPath(normalized) {
		return false
	}
	for _, pattern := range filter.includes {
		if matchPath(pattern, normalized) {
			return true
		}
	}
	base := path.Base(normalized)
	switch base {
	case "go.mod", "go.sum", "go.work", "go.work.sum":
		return true
	}
	_, found := defaultRelevantExtensions[path.Ext(base)]
	return found
}

// SkipDirectory reports whether a normalized directory subtree is ignored.
func (filter PathFilter) SkipDirectory(directoryPath string) bool {
	normalized, err := normalizeRelativePath(directoryPath)
	if err != nil {
		return true
	}
	if normalized == "." {
		return false
	}
	for segment := range strings.SplitSeq(normalized, "/") {
		if _, found := defaultExcludedDirectories[segment]; found {
			return true
		}
	}
	if normalized == ".spice/build" ||
		normalized == ".spice/dev" ||
		strings.HasPrefix(normalized, ".spice/build/") ||
		strings.HasPrefix(normalized, ".spice/dev/") ||
		normalized == "internal/spicegen" ||
		strings.HasPrefix(normalized, "internal/spicegen/") {
		return true
	}
	return filter.excluded(normalized) ||
		filter.excluded(normalized+"/placeholder")
}

func (filter PathFilter) excluded(filePath string) bool {
	for _, pattern := range filter.excludes {
		if matchPath(pattern, filePath) {
			return true
		}
	}
	return false
}

func normalizeRelativePath(filePath string) (string, error) {
	normalized := filepath.ToSlash(filepath.Clean(filePath))
	if normalized == "." {
		return normalized, nil
	}
	if !filepath.IsLocal(filepath.FromSlash(normalized)) {
		return "", errors.New("development path must remain inside the workspace")
	}
	return normalized, nil
}

func temporaryEditorPath(filePath string) bool {
	base := path.Base(filePath)
	return strings.HasPrefix(base, ".#") ||
		(strings.HasPrefix(base, "#") && strings.HasSuffix(base, "#")) ||
		strings.HasSuffix(base, "~") ||
		strings.HasSuffix(base, ".swp") ||
		strings.HasSuffix(base, ".swo") ||
		strings.HasSuffix(base, ".tmp") ||
		strings.HasSuffix(base, ".temp")
}

func defaultGeneratedPath(filePath string) bool {
	base := path.Base(filePath)
	return strings.HasSuffix(base, "_spice_gen.go") ||
		(strings.HasPrefix(base, "spice_") &&
			strings.HasSuffix(base, "_gen.go")) ||
		base == "zz_spice_bridge_gen.go" ||
		base == "openapi.json" ||
		(strings.HasPrefix(filePath, ".spice/") &&
			strings.HasSuffix(base, ".manifest.json"))
}

func matchPath(pattern, filePath string) bool {
	return matchPathSegments(
		strings.Split(pattern, "/"),
		strings.Split(filePath, "/"),
	)
}

func matchPathSegments(pattern, value []string) bool {
	if len(pattern) == 0 {
		return len(value) == 0
	}
	if pattern[0] == "**" {
		return matchPathSegments(pattern[1:], value) ||
			(len(value) != 0 && matchPathSegments(pattern, value[1:]))
	}
	if len(value) == 0 {
		return false
	}
	matched, err := path.Match(pattern[0], value[0])
	return err == nil && matched && matchPathSegments(pattern[1:], value[1:])
}
