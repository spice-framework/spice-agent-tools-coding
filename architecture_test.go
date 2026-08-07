package coding_test

import (
	"bytes"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestProductDoesNotOwnAParallelProcessLauncher(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Clean(entry.Name())
		content, err := os.ReadFile(path) // #nosec G304 -- package-local source selected by ReadDir.
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range [][]byte{
			[]byte("exec." + "Command"),
			[]byte("prepare" + "ProcessTree"),
			[]byte("type process" + "Tree"),
			[]byte("Create" + "JobObject"),
			[]byte("Sys" + "ProcAttr"),
		} {
			if bytes.Contains(content, forbidden) {
				t.Fatalf("%s contains forbidden launcher implementation %q", path, forbidden)
			}
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, content, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imported := range file.Imports {
			name, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if name == "os/exec" || name == "syscall" {
				t.Fatalf("%s imports forbidden launcher package %q", path, name)
			}
		}
	}
}
