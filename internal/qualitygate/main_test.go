package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	writeGateFile(t, root, "go.mod", "require github.com/spice-framework/spice-agent "+agentVersion+"\n")
	writeGateFile(t, root, "tools/go.mod", "require google.golang.org/grpc v1.82.1 // indirect\n")
	writeGateFile(t, root, "spice-compatibility.json", `{"schema":1,"minimum":"v0.1.0-preview.1","current":"v0.1.0-preview.1","spice_agent":"`+agentVersion+`","toolchain":"v0.1.0-preview.1","go":"1.26.5"}`)
	if err := checkContracts(root); err != nil {
		t.Fatal(err)
	}
	writeGateFile(t, root, "go.mod", "require github.com/spice-framework/spice-agent "+agentVersion+"\nreplace github.com/spice-framework/spice-agent => ../spice-agent\n")
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
