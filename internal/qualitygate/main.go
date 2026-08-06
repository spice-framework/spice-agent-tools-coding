// Command qualitygate runs this repository's cross-platform quality contract.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	modulePath      = "github.com/spice-framework/spice-agent-tools-coding"
	requiredGo      = "go1.26.5"
	minimumCoverage = 85.0
	agentVersion    = "v0.0.0-20260806191411-841edd3d47ad"
)

func main() {
	os.Exit(execute())
}

func execute() int {
	mode := flag.String("mode", "check", "tools-bootstrap, fast, check, fmt, or verify")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	root, err := repositoryRoot()
	if err == nil {
		err = run(ctx, root, *mode)
	}
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "qualitygate:", err)
		return 1
	}
	return 0
}

func run(ctx context.Context, root, mode string) error {
	if runtime.Version() != requiredGo {
		return fmt.Errorf("requires %s, got %s", requiredGo, runtime.Version())
	}
	if networkAllowed(mode) {
		return bootstrapDependencies(ctx, root, networkCommand)
	}
	switch mode {
	case "fast":
		return command(ctx, root, nil, "go", "test", "-shuffle=on", "-count=1", "./...")
	case "fmt":
		return format(ctx, root, true)
	case "check":
		return check(ctx, root)
	case "verify":
		if err := check(ctx, root); err != nil {
			return err
		}
		for _, gate := range []func(context.Context, string) error{
			lint, security, raceTests, coverage, vendor, offline,
		} {
			if err := gate(ctx, root); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported mode %q", mode)
	}
}

func networkAllowed(mode string) bool { return mode == "tools-bootstrap" }

type bootstrapRunner func(context.Context, string, ...string) error

type moduleGraph struct {
	directory string
	optional  bool
}

func bootstrapDependencies(ctx context.Context, root string, runner bootstrapRunner) (returnErr error) {
	before, err := sourceTreeDigests(root)
	if err != nil {
		return fmt.Errorf("snapshot repository before bootstrap: %w", err)
	}
	defer func() {
		after, snapshotErr := sourceTreeDigests(root)
		if snapshotErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("snapshot repository after bootstrap: %w", snapshotErr))
			return
		}
		if !maps.Equal(before, after) {
			returnErr = errors.Join(returnErr, errors.New("dependency bootstrap modified the repository"))
		}
	}()

	graphs := []moduleGraph{{directory: root}, {directory: filepath.Join(root, "tools"), optional: true}}
	for _, graph := range graphs {
		if err := bootstrapModuleGraph(ctx, graph, runner); err != nil {
			return err
		}
	}
	return nil
}

func bootstrapModuleGraph(ctx context.Context, graph moduleGraph, runner bootstrapRunner) (returnErr error) {
	moduleFile := filepath.Join(graph.directory, "go.mod")
	moduleContent, err := os.ReadFile(moduleFile) // #nosec G304 -- repository-owned module graph.
	if errors.Is(err, os.ErrNotExist) && graph.optional {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", moduleFile, err)
	}
	temporary, err := os.MkdirTemp("", "spice-tools-bootstrap-*")
	if err != nil {
		return fmt.Errorf("create dependency bootstrap directory: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, os.RemoveAll(temporary)) }()
	temporaryRoot, err := os.OpenRoot(temporary)
	if err != nil {
		return fmt.Errorf("open dependency bootstrap directory: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, temporaryRoot.Close()) }()

	temporaryModule := filepath.Join(temporary, "graph.mod")
	if writeErr := temporaryRoot.WriteFile("graph.mod", moduleContent, 0o600); writeErr != nil {
		return fmt.Errorf("write temporary module file: %w", writeErr)
	}
	sumFile := filepath.Join(graph.directory, "go.sum")
	sumContent, err := os.ReadFile(sumFile) // #nosec G304 -- repository-owned module graph.
	if err == nil {
		if writeErr := temporaryRoot.WriteFile("graph.sum", sumContent, 0o600); writeErr != nil {
			return fmt.Errorf("write temporary checksum file: %w", writeErr)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", sumFile, err)
	}
	return runner(ctx, graph.directory, bootstrapDownloadArguments(temporaryModule)...)
}

func bootstrapDownloadArguments(moduleFile string) []string {
	return []string{"mod", "download", "-modfile=" + moduleFile, "all"}
}

func check(ctx context.Context, root string) error {
	if err := checkContracts(root); err != nil {
		return err
	}
	if err := format(ctx, root, false); err != nil {
		return err
	}
	for _, arguments := range [][]string{
		{"mod", "tidy", "-diff"},
		{"-C", "tools", "mod", "tidy", "-diff"},
		{"vet", "./..."},
		{"test", "-shuffle=on", "-count=1", "./..."},
	} {
		if err := command(ctx, root, nil, "go", arguments...); err != nil {
			return err
		}
	}
	return nil
}

type compatibility struct {
	Schema     int    `json:"schema"`
	Minimum    string `json:"minimum"`
	Current    string `json:"current"`
	SpiceAgent string `json:"spice_agent"`
	Toolchain  string `json:"toolchain"`
	Go         string `json:"go"`
}

func checkContracts(root string) error {
	module, err := os.ReadFile(filepath.Join(root, "go.mod")) // #nosec G304 -- repository-owned fixed path.
	if err != nil {
		return fmt.Errorf("read go.mod: %w", err)
	}
	if !bytes.Contains(module, []byte("github.com/spice-framework/spice-agent "+agentVersion)) ||
		bytes.Contains(module, []byte("replace github.com/spice-framework/spice-agent")) {
		return errors.New("go.mod must require the exact Spice Agent core pin without a replace directive")
	}
	toolsModule, err := os.ReadFile(filepath.Join(root, "tools", "go.mod")) // #nosec G304 -- repository-owned fixed path.
	if err != nil {
		return fmt.Errorf("read tools/go.mod: %w", err)
	}
	if !bytes.Contains(toolsModule, []byte("google.golang.org/grpc v1.82.1")) {
		return errors.New("tools/go.mod must retain patched google.golang.org/grpc v1.82.1")
	}
	metadata, err := os.ReadFile(filepath.Join(root, "spice-compatibility.json")) // #nosec G304 -- fixed metadata path.
	if err != nil {
		return fmt.Errorf("read compatibility metadata: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(metadata))
	decoder.DisallowUnknownFields()
	var value compatibility
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("decode compatibility metadata: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("compatibility metadata contains trailing JSON values")
	}
	if value.Schema != 1 || value.Minimum != "v0.1.0-preview.1" || value.Current != "v0.1.0-preview.1" ||
		value.SpiceAgent != agentVersion || value.Toolchain != "v0.1.0-preview.1" || value.Go != "1.26.5" {
		return errors.New("compatibility metadata does not match the exact verified framework selections")
	}
	return nil
}

func format(ctx context.Context, root string, write bool) error {
	files, err := goFiles(root)
	if err != nil {
		return err
	}
	option := "-l"
	if write {
		option = "-w"
	}
	for _, name := range []string{"goimports", "gofumpt"} {
		executable, pathErr := toolPath(ctx, root, name)
		if pathErr != nil {
			return pathErr
		}
		stdout, runErr := capture(ctx, root, executable, append([]string{option}, files...)...)
		if runErr != nil {
			return runErr
		}
		if !write && strings.TrimSpace(stdout) != "" {
			return fmt.Errorf("%s requires formatting: %s", name, strings.Join(strings.Fields(stdout), ", "))
		}
	}
	return nil
}

func lint(ctx context.Context, root string) error {
	golangci, err := toolPath(ctx, root, "golangci-lint")
	if err != nil {
		return err
	}
	if runErr := command(ctx, root, nil, golangci, "run", "--timeout=10m"); runErr != nil {
		return runErr
	}
	nilaway, err := toolPath(ctx, root, "nilaway")
	if err != nil {
		return err
	}
	return command(ctx, root, nil, nilaway, "-include-pkgs="+modulePath, "./...")
}

func security(ctx context.Context, root string) error {
	gosec, err := toolPath(ctx, root, "gosec")
	if err != nil {
		return err
	}
	if runErr := command(ctx, root, nil, gosec, "-quiet", "-exclude-generated", "./..."); runErr != nil {
		return runErr
	}
	govulncheck, err := toolPath(ctx, root, "govulncheck")
	if err != nil {
		return err
	}
	return command(ctx, root, nil, govulncheck, "./...")
}

func raceTests(ctx context.Context, root string) error {
	return command(ctx, root, nil, "go", "test", "-race", "-shuffle=on", "-count=1", "./...")
}

func coverage(ctx context.Context, root string) (returnErr error) {
	packages, err := productPackages(ctx, root)
	if err != nil {
		return err
	}
	profile, err := os.CreateTemp("", "spice-agent-tools-coverage-*.out")
	if err != nil {
		return fmt.Errorf("create coverage profile: %w", err)
	}
	path := profile.Name()
	if closeErr := profile.Close(); closeErr != nil {
		return fmt.Errorf("close coverage profile: %w", closeErr)
	}
	defer func() { returnErr = errors.Join(returnErr, os.Remove(path)) }()
	arguments := append([]string{"test", "-covermode=atomic", "-coverprofile=" + path}, packages...)
	if runErr := command(ctx, root, nil, "go", arguments...); runErr != nil {
		return runErr
	}
	report, err := capture(ctx, root, "go", "tool", "cover", "-func="+path)
	if err != nil {
		return err
	}
	percentage, err := totalCoverage(report)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(os.Stdout, "coverage %.1f%% (minimum %.1f%%)\n", percentage, minimumCoverage); err != nil {
		return fmt.Errorf("write coverage result: %w", err)
	}
	if percentage < minimumCoverage {
		return fmt.Errorf("coverage %.1f%% is below %.1f%%", percentage, minimumCoverage)
	}
	return nil
}

func productPackages(ctx context.Context, root string) ([]string, error) {
	stdout, err := capture(ctx, root, "go", "list", "-f={{.ImportPath}}", "./...")
	if err != nil {
		return nil, err
	}
	qualityPackage := modulePath + "/internal/qualitygate"
	var result []string
	for candidate := range strings.FieldsSeq(stdout) {
		if candidate != qualityPackage {
			result = append(result, candidate)
		}
	}
	slices.Sort(result)
	if len(result) == 0 {
		return nil, errors.New("no product packages found")
	}
	return result, nil
}

func totalCoverage(report string) (float64, error) {
	lines := strings.Split(strings.TrimSpace(report), "\n")
	if len(lines) == 0 {
		return 0, errors.New("coverage report is empty")
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) == 0 || !strings.HasSuffix(fields[len(fields)-1], "%") {
		return 0, errors.New("coverage report has no total percentage")
	}
	return strconv.ParseFloat(strings.TrimSuffix(fields[len(fields)-1], "%"), 64)
}

func vendor(ctx context.Context, root string) (returnErr error) {
	temporary, err := os.MkdirTemp("", "spice-agent-tools-vendor-*")
	if err != nil {
		return fmt.Errorf("create vendor comparison directory: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, os.RemoveAll(temporary)) }()
	candidate := filepath.Join(temporary, "vendor")
	if vendorErr := command(ctx, root, nil, "go", "mod", "vendor", "-o", candidate); vendorErr != nil {
		return vendorErr
	}
	current, err := treeDigests(filepath.Join(root, "vendor"))
	if err != nil {
		return err
	}
	expected, err := treeDigests(candidate)
	if err != nil {
		return err
	}
	if !maps.Equal(current, expected) {
		return errors.New("vendor differs from a fresh go mod vendor result")
	}
	return nil
}

func offline(ctx context.Context, root string) error {
	environment := map[string]string{"GOFLAGS": "-mod=vendor"}
	if err := command(ctx, root, environment, "go", "test", "-count=1", "./..."); err != nil {
		return err
	}
	return command(ctx, root, environment, "go", "build", "-trimpath", "./...")
}

func goFiles(root string) ([]string, error) {
	var result []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && path != root && slices.Contains([]string{".git", "tools", "vendor"}, entry.Name()) {
			return filepath.SkipDir
		}
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".go" {
			result = append(result, path)
		}
		return nil
	})
	slices.Sort(result)
	return result, err
}

func treeDigests(root string) (_ map[string][sha256.Size]byte, returnErr error) {
	return digests(root, false)
}

func sourceTreeDigests(root string) (_ map[string][sha256.Size]byte, returnErr error) {
	return digests(root, true)
}

func digests(root string, excludeGit bool) (_ map[string][sha256.Size]byte, returnErr error) {
	opened, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("open tree %q: %w", root, err)
	}
	defer func() { returnErr = errors.Join(returnErr, opened.Close()) }()
	result := make(map[string][sha256.Size]byte)
	err = fs.WalkDir(opened.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if excludeGit && path == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		content, readErr := opened.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		result[filepath.ToSlash(path)] = sha256.Sum256(content)
		return nil
	})
	return result, err
}

func toolPath(ctx context.Context, root, name string) (string, error) {
	stdout, err := capture(ctx, root, "go", "tool", "-C", "tools", "-n", name)
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(stdout)
	if path == "" {
		return "", fmt.Errorf("resolve tool %q: empty path", name)
	}
	return path, nil
}

func repositoryRoot() (string, error) {
	current, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	for {
		content, readErr := os.ReadFile(filepath.Join(current, "go.mod")) // #nosec G304 -- candidates are bounded ancestors.
		if readErr == nil && bytes.Contains(content, []byte("module "+modulePath)) {
			return current, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New("repository go.mod not found")
		}
		current = parent
	}
}

func command(ctx context.Context, directory string, environment map[string]string, executable string, arguments ...string) error {
	executable = qualityExecutable(executable)
	// #nosec G204,G702 -- executable and arguments are repository-owned gate values.
	cmd := exec.CommandContext(ctx, executable, arguments...)
	cmd.Dir = directory
	cmd.Env = mergedEnvironment(environment)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", executable, strings.Join(arguments, " "), err)
	}
	return nil
}

func networkCommand(ctx context.Context, directory string, arguments ...string) error {
	// #nosec G204,G702 -- only the exact copied module graphs are downloaded.
	cmd := exec.CommandContext(ctx, exactGoExecutable(), arguments...)
	cmd.Dir = directory
	cmd.Env = commandEnvironment(true, nil)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go %s: %w", strings.Join(arguments, " "), err)
	}
	return nil
}

func capture(ctx context.Context, directory, executable string, arguments ...string) (string, error) {
	executable = qualityExecutable(executable)
	// #nosec G204,G702 -- executable and arguments are repository-owned gate values.
	cmd := exec.CommandContext(ctx, executable, arguments...)
	cmd.Dir = directory
	cmd.Env = mergedEnvironment(nil)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w\n%s", executable, strings.Join(arguments, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func qualityExecutable(executable string) string {
	if executable == "go" {
		return exactGoExecutable()
	}
	return executable
}

func exactGoExecutable() string {
	return filepath.Join(runtime.GOROOT(), "bin", goExecutableName(runtime.GOOS)) //nolint:staticcheck // Gate runs in place under the selected exact toolchain.
}

func goExecutableName(goos string) string {
	if goos == "windows" {
		return "go.exe"
	}
	return "go"
}

func mergedEnvironment(overrides map[string]string) []string {
	return commandEnvironment(false, overrides)
}

func commandEnvironment(network bool, overrides map[string]string) []string {
	values := map[string]string{"GOWORK": "off", "GOFLAGS": "", "GOTOOLCHAIN": "local"}
	if network {
		values["GOAUTH"] = "off"
		values["GONOPROXY"] = ""
		values["GONOSUMDB"] = ""
		values["GOPRIVATE"] = ""
		values["GOPROXY"] = "https://proxy.golang.org"
		values["GOSUMDB"] = "sum.golang.org"
	} else {
		values["GOPROXY"] = "off"
	}
	maps.Copy(values, overrides)
	result := make([]string, 0, len(os.Environ())+len(values))
	for _, entry := range os.Environ() {
		key, _, found := strings.Cut(entry, "=")
		if found {
			upperKey := strings.ToUpper(key)
			if sensitiveEnvironmentKey(upperKey) {
				continue
			}
			if _, replaced := values[upperKey]; !replaced {
				result = append(result, entry)
			}
		}
	}
	for key, value := range values {
		result = append(result, key+"="+value)
	}
	slices.Sort(result)
	return result
}

func sensitiveEnvironmentKey(key string) bool {
	return strings.Contains(key, "TOKEN") || strings.Contains(key, "PASSWORD") ||
		strings.Contains(key, "SECRET") || strings.HasSuffix(key, "API_KEY") ||
		strings.HasSuffix(key, "ACCESS_KEY") || strings.HasSuffix(key, "PRIVATE_KEY")
}
