package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNetworkAllowedOnlyForBootstrap(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"fast", "check", "fmt", "verify", "unknown"} {
		if networkAllowed(mode) {
			t.Fatalf("networkAllowed(%q) = true", mode)
		}
	}
	if !networkAllowed("tools-bootstrap") {
		t.Fatal("networkAllowed(tools-bootstrap) = false")
	}
}

func TestRepositoryPortabilityRequiresLFAndExplicitToolBootstrap(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeGateFile(t, root, ".gitattributes", "* text=auto eol=lf\n*.pb -text\n*.png -text\n")
	writeGateFile(t, root, ".github/workflows/ci.yml", `steps:
  - run: go run ./internal/qualitygate -mode=tools-bootstrap
  - run: go run ./internal/qualitygate -mode=verify
`)
	if err := checkRepositoryPortability(root); err != nil {
		t.Fatal(err)
	}

	writeGateFile(t, root, ".gitattributes", "* text=auto\n")
	if err := checkRepositoryPortability(root); err == nil || !strings.Contains(err.Error(), ".gitattributes") {
		t.Fatalf("invalid attributes error = %v", err)
	}

	writeGateFile(t, root, ".gitattributes", "* text=auto eol=lf\n*.pb -text\n*.png -text\n")
	writeGateFile(t, root, ".github/workflows/ci.yml", `steps:
  - run: go run ./internal/qualitygate -mode=verify
`)
	if err := checkRepositoryPortability(root); err == nil || !strings.Contains(err.Error(), "bootstrap") {
		t.Fatalf("missing bootstrap error = %v", err)
	}
}

func TestExactGoExecutable(t *testing.T) {
	t.Parallel()
	if goExecutableName("windows") != "go.exe" || goExecutableName("linux") != "go" {
		t.Fatal("go executable name is not platform-correct")
	}
	actualName := filepath.Base(exactGoExecutable())
	if (actualName != "go" && actualName != "go.exe") || filepath.Base(filepath.Dir(exactGoExecutable())) != "bin" ||
		qualityExecutable("go") != exactGoExecutable() ||
		qualityExecutable("gofumpt") != "gofumpt" {
		t.Fatalf("exact Go executable = %q", exactGoExecutable())
	}
}

func TestBootstrapDownloadArguments(t *testing.T) {
	t.Parallel()
	moduleFile := filepath.Join("private", "graph.mod")
	want := "mod download -modfile=" + moduleFile + " all"
	if got := strings.Join(bootstrapDownloadArguments(moduleFile), " "); got != want {
		t.Fatalf("bootstrapDownloadArguments() = %q, want %q", got, want)
	}
}

func TestBootstrapPreservesRepositoryOnSuccessAndFailure(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		runnerErr error
	}{
		{name: "success"},
		{name: "failure", runnerErr: errors.New("download failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := bootstrapFixture(t, true)
			before, err := sourceTreeDigests(root)
			if err != nil {
				t.Fatal(err)
			}
			var calls [][]string
			runner := func(_ context.Context, directory string, arguments ...string) error {
				if directory != root && directory != filepath.Join(root, "tools") {
					t.Fatalf("unexpected directory %q", directory)
				}
				calls = append(calls, append([]string(nil), arguments...))
				return test.runnerErr
			}
			err = bootstrapDependencies(context.Background(), root, runner)
			if !errors.Is(err, test.runnerErr) {
				t.Fatalf("bootstrapDependencies() error = %v, want %v", err, test.runnerErr)
			}
			after, digestErr := sourceTreeDigests(root)
			if digestErr != nil {
				t.Fatal(digestErr)
			}
			if len(before) != len(after) {
				t.Fatalf("repository file count changed: %d != %d", len(before), len(after))
			}
			for name, digest := range before {
				if after[name] != digest {
					t.Fatalf("repository file %q changed", name)
				}
			}
			wantCalls := 2
			if test.runnerErr != nil {
				wantCalls = 1
			}
			if len(calls) != wantCalls {
				t.Fatalf("bootstrap calls = %d, want %d", len(calls), wantCalls)
			}
			for _, arguments := range calls {
				if len(arguments) != 4 || arguments[0] != "mod" || arguments[1] != "download" ||
					!strings.HasPrefix(arguments[2], "-modfile=") || arguments[3] != "all" {
					t.Fatalf("unexpected bootstrap arguments: %q", arguments)
				}
				if strings.HasPrefix(strings.TrimPrefix(arguments[2], "-modfile="), root) {
					t.Fatalf("temporary modfile is inside repository: %q", arguments[2])
				}
			}
		})
	}
}

func TestBootstrapAllowsMissingToolsModule(t *testing.T) {
	t.Parallel()
	root := bootstrapFixture(t, false)
	calls := 0
	err := bootstrapDependencies(context.Background(), root, func(_ context.Context, _ string, _ ...string) error {
		calls++
		return nil
	})
	if err != nil || calls != 1 {
		t.Fatalf("bootstrapDependencies() = calls %d, error %v", calls, err)
	}
}

func TestBootstrapPropagatesCancellation(t *testing.T) {
	t.Parallel()
	root := bootstrapFixture(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := bootstrapDependencies(ctx, root, func(callContext context.Context, _ string, _ ...string) error {
		calls++
		return callContext.Err()
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("bootstrapDependencies() = calls %d, error %v", calls, err)
	}
}

func TestBootstrapDetectsRepositoryMutation(t *testing.T) {
	t.Parallel()
	root := bootstrapFixture(t, false)
	err := bootstrapDependencies(context.Background(), root, func(_ context.Context, directory string, _ ...string) error {
		return os.WriteFile(filepath.Join(directory, "unexpected"), []byte("mutation"), 0o600)
	})
	if err == nil || !strings.Contains(err.Error(), "modified the repository") {
		t.Fatalf("bootstrapDependencies() error = %v", err)
	}
}

func TestCommandEnvironmentSeparatesNetworkAndSecrets(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "must-not-leak")
	t.Setenv("SPICE_TEST_TOKEN", "must-not-leak")
	for _, test := range []struct {
		name    string
		network bool
		proxy   string
	}{
		{name: "offline", proxy: "off"},
		{name: "bootstrap", network: true, proxy: "https://proxy.golang.org"},
	} {
		t.Run(test.name, func(t *testing.T) {
			environment := strings.Join(commandEnvironment(test.network, nil), "\n")
			if strings.Contains(environment, "must-not-leak") || !strings.Contains(environment, "GOPROXY="+test.proxy) {
				t.Fatalf("unsafe command environment:\n%s", environment)
			}
			if test.network && !strings.Contains(environment, "GOAUTH=off") {
				t.Fatalf("bootstrap environment enables Go authentication:\n%s", environment)
			}
		})
	}
}

func bootstrapFixture(t *testing.T, tools bool) string {
	t.Helper()
	root := t.TempDir()
	modules := []string{root}
	if tools {
		modules = append(modules, filepath.Join(root, "tools"))
	}
	for _, directory := range modules {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "go.mod"), []byte("module example.com/fixture\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "go.sum"), []byte("fixture sum\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestTotalCoverage(t *testing.T) {
	t.Parallel()
	percentage, err := totalCoverage("value.go:1:\tFunction\t100.0%\ntotal:\t(statements)\t86.0%\n")
	if err != nil || percentage != 86 {
		t.Fatalf("totalCoverage() = %v, %v", percentage, err)
	}
	for _, content := range []string{"", "total: invalid"} {
		if _, err := totalCoverage(content); err == nil {
			t.Fatalf("totalCoverage(%q) error = nil", content)
		}
	}
}

func TestGoFilesExcludesToolsAndVendor(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, name := range []string{"main.go", "nested/value.go", "vendor/ignored.go", "tools/ignored.go"} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("package fixture\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, err := goFiles(root)
	if err != nil || len(files) != 2 {
		t.Fatalf("goFiles() = %v, %v", files, err)
	}
	if strings.Contains(strings.Join(files, " "), "ignored.go") {
		t.Fatalf("goFiles() included excluded files: %v", files)
	}
}

func TestTreeDigests(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "value")
	if err := os.WriteFile(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := treeDigests(root)
	if err != nil {
		t.Fatal(err)
	}
	if writeErr := os.WriteFile(path, []byte("two"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	second, err := treeDigests(root)
	if err != nil || first["value"] == second["value"] {
		t.Fatalf("treeDigests() did not detect change: %v", err)
	}
}

func TestCheckContracts(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeGateFile(t, root, ".gitattributes", "* text=auto eol=lf\n*.pb -text\n*.png -text\n")
	writeGateFile(t, root, ".github/workflows/ci.yml", `steps:
  - run: go run ./internal/qualitygate -mode=tools-bootstrap
  - run: go run ./internal/qualitygate -mode=verify
`)
	module := "require (\n" +
		"github.com/spice-framework/spice " + coreVersion + "\n" +
		"github.com/spice-framework/spice-agent " + agentVersion + "\n" +
		"github.com/spice-framework/toolchain " + toolchainVersion + "\n" +
		")\ntool github.com/spice-framework/toolchain/cmd/spice\n"
	writeGateFile(t, root, "go.mod", module)
	writeGateFile(t, root, "tools/go.mod", "require google.golang.org/grpc v1.82.1 // indirect\n")
	writeGateFile(t, root, "spice-compatibility.json", `{"schema":1,"minimum":"`+coreVersion+`","current":"`+coreVersion+`","spice_agent":"`+agentVersion+`","toolchain":"`+toolchainVersion+`","go":"1.26.5"}`)
	writeGateFile(t, root, "spice-release.json", "{\n"+
		"  \"schema\": 1,\n"+
		"  \"profile\": \""+releaseProfile+"\",\n"+
		"  \"repository\": \""+releaseRepository+"\",\n"+
		"  \"module\": \""+modulePath+"\",\n"+
		"  \"version\": \""+releaseVersion+"\"\n"+
		"}\n")
	if err := checkContracts(root); err != nil {
		t.Fatal(err)
	}
	writeGateFile(t, root, "go.mod", module+"replace github.com/spice-framework/spice-agent => ../spice-agent\n")
	if err := checkContracts(root); err == nil || !strings.Contains(err.Error(), "without a replace") {
		t.Fatalf("checkContracts() error = %v", err)
	}
}

func writeGateFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
