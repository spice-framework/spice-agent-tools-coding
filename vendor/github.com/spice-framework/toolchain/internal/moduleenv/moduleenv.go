// Package moduleenv derives fail-closed Go module execution settings.
package moduleenv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
)

const (
	maximumWorkspaceBytes = 4 << 20
	maximumVendorBytes    = 16 << 20
	maximumVendorModules  = 10_000
	maximumGoStderrBytes  = 256 << 10
)

// WorkspaceModule is one module selected by the active Go workspace.
type WorkspaceModule struct {
	Path string
	Root string
}

// VendoredModule is one versioned module that owns at least one package in the
// Go-validated vendor graph.
type VendoredModule struct {
	Path                 string
	Version              string
	Directory            string
	ReplacementPath      string
	ReplacementVersion   string
	ReplacementDirectory string
	LocalReplacement     bool
}

// OfflineMode selects the vendor tree Go itself can use for root. A module's
// vendor directory is not valid while a parent go.work is active; workspace
// mode requires a consistent vendor directory beside that go.work.
func OfflineMode(root string, environment []string) string {
	if _, found := VendorRoot(root, environment); found {
		return "vendor"
	}
	return "readonly"
}

// VendorRoot returns the module or workspace root whose vendor graph Go can
// use for root. It rejects invalid workspace selection, workspaces that do not
// contain root, and workspace manifests without Go's workspace marker.
func VendorRoot(root string, environment []string) (string, bool) {
	targetRoot, err := findModuleRoot(root)
	if err != nil {
		return "", false
	}
	vendorRoot := targetRoot
	workspace, required := workspaceSelection(targetRoot, environment)
	if workspace != "" {
		modules, parseErr := workspaceModules(context.Background(), workspace)
		if parseErr != nil || !containsModuleRoot(modules, targetRoot) {
			return "", false
		}
		vendorRoot = filepath.Dir(workspace)
	} else if required {
		return "", false
	}
	manifestRoot := filepath.Join(vendorRoot, "vendor")
	content, err := readBoundedFile(manifestRoot, "modules.txt", maximumVendorBytes)
	if err != nil {
		return "", false
	}
	if workspace != "" && !hasWorkspaceMarker(content) {
		return "", false
	}
	return vendorRoot, true
}

// WorkspaceFile returns the active, valid absolute go.work path for root.
func WorkspaceFile(root string, environment []string) string {
	workspace, _ := workspaceSelection(root, environment)
	return workspace
}

// WorkspaceModules returns the target module in module mode or every use
// module in workspace mode. An active workspace must contain root.
func WorkspaceModules(
	ctx context.Context,
	root string,
	environment []string,
) ([]WorkspaceModule, error) {
	if ctx == nil {
		return nil, errors.New("resolve workspace modules: context is nil")
	}
	targetRoot, err := findModuleRoot(root)
	if err != nil {
		return nil, err
	}
	workspace, required := workspaceSelection(targetRoot, environment)
	if workspace == "" {
		if required {
			return nil, errors.New("resolve workspace modules: GOWORK does not identify an absolute regular file")
		}
		path, readErr := readModulePath(targetRoot)
		if readErr != nil {
			return nil, readErr
		}
		return []WorkspaceModule{{Path: path, Root: targetRoot}}, nil
	}
	modules, err := workspaceModules(ctx, workspace)
	if err != nil {
		return nil, err
	}
	if !containsModuleRoot(modules, targetRoot) {
		return nil, fmt.Errorf(
			"resolve workspace modules: target module %q is not listed by %s",
			targetRoot,
			workspace,
		)
	}
	return modules, nil
}

// VendoredModules validates the selected vendor tree with Go 1.26 before
// returning a bounded, strictly parsed module catalog.
func VendoredModules(
	ctx context.Context,
	root string,
	environment []string,
) ([]VendoredModule, error) {
	if ctx == nil {
		return nil, errors.New("resolve vendored modules: context is nil")
	}
	vendorRoot, found := VendorRoot(root, environment)
	if !found {
		return nil, errors.New("resolve vendored modules: no usable vendor graph")
	}
	if err := validateVendor(ctx, root, environment); err != nil {
		return nil, err
	}
	vendorDirectory := filepath.Join(vendorRoot, "vendor")
	content, err := readBoundedFile(vendorDirectory, "modules.txt", maximumVendorBytes)
	if err != nil {
		return nil, fmt.Errorf("read vendored Go module graph: %w", err)
	}
	return parseVendoredModules(ctx, vendorRoot, vendorDirectory, content)
}

func workspaceSelection(root string, environment []string) (string, bool) {
	if environment == nil {
		environment = os.Environ()
	}
	value := ""
	found := false
	for _, entry := range environment {
		name, candidate, valid := strings.Cut(entry, "=")
		if valid && strings.EqualFold(name, "GOWORK") {
			value = candidate
			found = true
		}
	}
	if found {
		switch {
		case strings.EqualFold(value, "off"):
			return "", false
		case value != "" && !strings.EqualFold(value, "auto"):
			if !filepath.IsAbs(value) {
				return "", true
			}
			workspace, err := canonicalFile(value)
			if err != nil {
				return "", true
			}
			return workspace, true
		}
	}
	current, err := canonicalDirectory(root)
	if err != nil {
		return "", false
	}
	for {
		workspace := filepath.Join(current, "go.work")
		if regularFile(workspace) {
			return workspace, true
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", false
		}
		current = parent
	}
}

func workspaceModules(ctx context.Context, workspace string) ([]WorkspaceModule, error) {
	content, err := readBoundedFile(filepath.Dir(workspace), filepath.Base(workspace), maximumWorkspaceBytes)
	if err != nil {
		return nil, fmt.Errorf("read workspace %q: %w", workspace, err)
	}
	work, err := modfile.ParseWork(workspace, content, nil)
	if err != nil {
		return nil, fmt.Errorf("parse workspace %q: %w", workspace, err)
	}
	result := make([]WorkspaceModule, 0, len(work.Use))
	seen := make(map[string]struct{}, len(work.Use))
	for _, used := range work.Use {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		root := filepath.FromSlash(used.Path)
		if !filepath.IsAbs(root) {
			root = filepath.Join(filepath.Dir(workspace), root)
		}
		root, err = canonicalDirectory(root)
		if err != nil {
			return nil, fmt.Errorf("resolve workspace use %q: %w", used.Path, err)
		}
		path, readErr := readModulePath(root)
		if readErr != nil {
			return nil, fmt.Errorf("read workspace use %q: %w", used.Path, readErr)
		}
		if _, duplicate := seen[path]; duplicate {
			return nil, fmt.Errorf("workspace contains duplicate module path %q", path)
		}
		seen[path] = struct{}{}
		result = append(result, WorkspaceModule{Path: path, Root: root})
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("workspace %q contains no use modules", workspace)
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].Path != result[right].Path {
			return result[left].Path < result[right].Path
		}
		return result[left].Root < result[right].Root
	})
	return result, nil
}

func validateVendor(ctx context.Context, root string, environment []string) error {
	// #nosec G204 -- executable and arguments are fixed; no shell is involved.
	command := exec.CommandContext(ctx, "go", "list", "-mod=vendor", "-m", "-json")
	command.Dir = root
	command.Env = offlineEnvironment(environment)
	command.Stdout = io.Discard
	stderr := &boundedBuffer{limit: maximumGoStderrBytes}
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			detail = ": " + detail
		}
		return fmt.Errorf("validate vendored Go module graph: %w%s", err, detail)
	}
	return nil
}

func parseVendoredModules(
	ctx context.Context,
	vendorRoot string,
	vendorDirectory string,
	content []byte,
) ([]VendoredModule, error) {
	var current *VendoredModule
	currentHasPackage := false
	result := make([]VendoredModule, 0)
	seen := make(map[string]string)
	flush := func() error {
		if current == nil || !currentHasPackage {
			return nil
		}
		if previous, duplicate := seen[current.Path]; duplicate {
			return fmt.Errorf(
				"vendored module path %q occurs with versions %q and %q",
				current.Path,
				previous,
				current.Version,
			)
		}
		if len(result) == maximumVendorModules {
			return fmt.Errorf("vendored module graph exceeds %d entries", maximumVendorModules)
		}
		relative := filepath.FromSlash(current.Path)
		if !filepath.IsLocal(relative) {
			return fmt.Errorf("vendored module path %q escapes vendor", current.Path)
		}
		directory := filepath.Join(vendorDirectory, relative)
		canonicalVendor, err := canonicalDirectory(vendorDirectory)
		if err != nil {
			return fmt.Errorf("resolve vendor directory: %w", err)
		}
		canonicalModule, err := canonicalDirectory(directory)
		if err != nil {
			return fmt.Errorf("resolve vendored module %s@%s: %w", current.Path, current.Version, err)
		}
		contained, err := containedPath(canonicalVendor, canonicalModule)
		if err != nil || !contained {
			return fmt.Errorf("vendored module path %q escapes vendor", current.Path)
		}
		info, err := os.Stat(canonicalModule)
		if err != nil {
			return fmt.Errorf("inspect vendored module %s@%s: %w", current.Path, current.Version, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("vendored module %s@%s is not a directory", current.Path, current.Version)
		}
		current.Directory = canonicalModule
		if current.LocalReplacement {
			replacement := filepath.FromSlash(current.ReplacementPath)
			if !filepath.IsAbs(replacement) {
				replacement = filepath.Join(vendorRoot, replacement)
			}
			current.ReplacementDirectory = filepath.Clean(replacement)
		}
		seen[current.Path] = current.Version
		result = append(result, *current)
		return nil
	}

	for line := range strings.SplitSeq(strings.ReplaceAll(string(content), "\r\n", "\n"), "\n") {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if strings.HasPrefix(line, "# ") && !strings.HasPrefix(line, "## ") {
			if err := flush(); err != nil {
				return nil, err
			}
			parsed, versioned, err := parseVendorHeader(line)
			if err != nil {
				return nil, err
			}
			current = nil
			currentHasPackage = false
			if versioned {
				current = &parsed
			}
			continue
		}
		if current == nil || strings.HasPrefix(line, "## ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 1 && module.CheckImportPath(fields[0]) == nil {
			currentHasPackage = true
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return result, nil
}

func parseVendorHeader(line string) (VendoredModule, bool, error) {
	fields := strings.Fields(line)
	if len(fields) < 3 || fields[0] != "#" {
		return VendoredModule{}, false, fmt.Errorf("invalid vendor module header %q", line)
	}
	path := fields[1]
	if err := module.CheckPath(path); err != nil {
		return VendoredModule{}, false, fmt.Errorf("invalid vendored module path %q: %w", path, err)
	}
	if fields[2] == "=>" {
		if _, err := parseReplacement(path, fields[2:]); err != nil {
			return VendoredModule{}, false, err
		}
		return VendoredModule{}, false, nil
	}
	version := fields[2]
	if err := module.Check(path, version); err != nil {
		return VendoredModule{}, false, fmt.Errorf("invalid vendored module %s@%s: %w", path, version, err)
	}
	result := VendoredModule{Path: path, Version: version}
	if len(fields) > 3 {
		replacement, err := parseReplacement(path, fields[3:])
		if err != nil {
			return VendoredModule{}, false, err
		}
		result.ReplacementPath = replacement.Path
		result.ReplacementVersion = replacement.Version
		result.LocalReplacement = replacement.Local
	}
	return result, true, nil
}

type replacement struct {
	Path    string
	Version string
	Local   bool
}

func parseReplacement(oldPath string, fields []string) (replacement, error) {
	if len(fields) < 2 || len(fields) > 3 || fields[0] != "=>" {
		return replacement{}, fmt.Errorf("invalid replacement for vendored module %q", oldPath)
	}
	result := replacement{Path: fields[1]}
	if result.Path == "" {
		return replacement{}, fmt.Errorf("empty replacement for vendored module %q", oldPath)
	}
	if len(fields) == 2 {
		result.Local = true
		return result, nil
	}
	result.Version = fields[2]
	if err := module.Check(result.Path, result.Version); err != nil {
		return replacement{}, fmt.Errorf(
			"invalid replacement %s@%s for vendored module %q: %w",
			result.Path,
			result.Version,
			oldPath,
			err,
		)
	}
	return result, nil
}

func findModuleRoot(root string) (string, error) {
	current, err := canonicalDirectory(root)
	if err != nil {
		return "", fmt.Errorf("resolve target module root: %w", err)
	}
	for {
		if regularFile(filepath.Join(current, "go.mod")) {
			return current, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("resolve target module root: no go.mod above %q", root)
		}
		current = parent
	}
}

func readModulePath(root string) (string, error) {
	content, err := readBoundedFile(root, "go.mod", maximumWorkspaceBytes)
	if err != nil {
		return "", fmt.Errorf("read target go.mod: %w", err)
	}
	path := modfile.ModulePath(content)
	if path == "" {
		return "", errors.New("read target go.mod: module directive is missing")
	}
	if err := module.CheckPath(path); err != nil {
		return "", fmt.Errorf("read target go.mod: invalid module path %q: %w", path, err)
	}
	return path, nil
}

func readBoundedFile(root, name string, maximum int64) ([]byte, error) {
	directory, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	info, statErr := directory.Stat(name)
	if statErr != nil {
		return nil, errors.Join(statErr, directory.Close())
	}
	if !info.Mode().IsRegular() || info.Size() > maximum {
		return nil, errors.Join(
			fmt.Errorf("%s is not a regular file of at most %d bytes", name, maximum),
			directory.Close(),
		)
	}
	content, readErr := directory.ReadFile(name)
	if err := errors.Join(readErr, directory.Close()); err != nil {
		return nil, err
	}
	if int64(len(content)) > maximum {
		return nil, fmt.Errorf("%s exceeds %d bytes", name, maximum)
	}
	return content, nil
}

func canonicalDirectory(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(filepath.Clean(absolute))
}

func canonicalFile(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	if !regularFile(resolved) {
		return "", fmt.Errorf("%q is not a regular file", path)
	}
	return resolved, nil
}

func containsModuleRoot(modules []WorkspaceModule, target string) bool {
	for _, item := range modules {
		if samePath(item.Root, target) {
			return true
		}
	}
	return false
}

func samePath(left, right string) bool {
	left, leftErr := canonicalDirectory(left)
	right, rightErr := canonicalDirectory(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func containedPath(root, candidate string) (bool, error) {
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return false, err
	}
	return relative != "." && filepath.IsLocal(relative), nil
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular()
}

func hasWorkspaceMarker(content []byte) bool {
	for line := range bytes.Lines(content) {
		if strings.TrimSpace(string(line)) == "## workspace" {
			return true
		}
	}
	return false
}

func offlineEnvironment(environment []string) []string {
	if environment == nil {
		environment = os.Environ()
	}
	result := make([]string, 0, len(environment)+2)
	for _, entry := range environment {
		name, _, found := strings.Cut(entry, "=")
		if found && (strings.EqualFold(name, "GOPROXY") || strings.EqualFold(name, "GOSUMDB")) {
			continue
		}
		result = append(result, entry)
	}
	return append(result, "GOPROXY=off", "GOSUMDB=off")
}

type boundedBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (buffer *boundedBuffer) Write(value []byte) (int, error) {
	written := len(value)
	remaining := buffer.limit - buffer.buffer.Len()
	if remaining > 0 {
		_, _ = buffer.buffer.Write(value[:min(remaining, len(value))])
	}
	return written, nil
}

func (buffer *boundedBuffer) String() string {
	return buffer.buffer.String()
}
