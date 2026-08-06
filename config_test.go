package coding

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	spicestarter "github.com/spice-framework/spice/annotation/sdk/starter"
)

func TestNewNormalizesDefaultsAndDisclosesCapabilities(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "worktree")
	suite, err := New(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	config := suite.Config()
	if config.Root != filepath.Clean(root) || config.MaxReadBytes != defaultMaxReadBytes ||
		config.MaxWriteBytes != defaultMaxWriteBytes || config.MaxOutputBytes != defaultMaxOutput ||
		config.CommandTimeout != defaultTimeout || len(config.EnvironmentAllowlist) == 0 {
		t.Fatalf("Suite.Config() = %#v", config)
	}
	config.EnvironmentAllowlist[0] = "CHANGED"
	if suite.Config().EnvironmentAllowlist[0] == "CHANGED" {
		t.Fatal("Suite.Config() did not return a defensive environment copy")
	}
	capabilities := suite.Capabilities()
	if len(capabilities) != 3 || capabilities[0].Name != "read" ||
		capabilities[1].Name != "replace" || capabilities[2].Name != "shell" {
		t.Fatalf("Suite.Capabilities() = %#v", capabilities)
	}
	capabilities[0].Name = "changed"
	if suite.Capabilities()[0].Name != "read" {
		t.Fatal("Suite.Capabilities() did not return a defensive copy")
	}
}

func TestNewRejectsInvalidBounds(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "worktree")
	tests := []struct {
		name   string
		config Config
		want   string
	}{
		{name: "relative root", config: Config{Root: "relative"}, want: "absolute path"},
		{name: "read low", config: Config{Root: root, MaxReadBytes: -1}, want: "max read bytes"},
		{name: "write high", config: Config{Root: root, MaxWriteBytes: maximumBytes + 1}, want: "max write bytes"},
		{name: "output high", config: Config{Root: root, MaxOutputBytes: maximumBytes + 1}, want: "max output bytes"},
		{name: "timeout low", config: Config{Root: root, CommandTimeout: -time.Second}, want: "command timeout"},
		{name: "timeout high", config: Config{Root: root, CommandTimeout: 31 * time.Minute}, want: "command timeout"},
		{name: "environment empty", config: Config{Root: root, EnvironmentAllowlist: []string{"PATH", " "}}, want: "environment name"},
		{name: "environment equals", config: Config{Root: root, EnvironmentAllowlist: []string{"BAD=VALUE"}}, want: "environment name"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(test.config)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("New() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestSecurityWarningAndManifest(t *testing.T) {
	t.Parallel()
	if !strings.Contains(SecurityWarning, "no sandbox") || !strings.Contains(SecurityWarning, "operating-system privileges") {
		t.Fatalf("SecurityWarning = %q", SecurityWarning)
	}
	spec := Manifest().Spec()
	if spec.Module != "github.com/spice-framework/spice-agent-tools-coding" || len(spec.Capabilities) != 7 ||
		spec.MinimumGo != "1.26.5" || len(spec.Activation.EntryPoints) != 3 || len(spec.Dependencies) != 2 {
		t.Fatalf("Manifest().Spec() = %#v", spec)
	}
	wantDependencies := []spicestarter.Dependency{
		{
			Module:  "github.com/spice-framework/spice-agent",
			Version: "v0.0.0-20260806225954-af79fc7fe4ad",
			License: "Apache-2.0",
		},
		{
			Module:  "golang.org/x/sys",
			Version: "v0.47.0",
			License: "BSD-3-Clause",
		},
	}
	if !slices.Equal(spec.Dependencies, wantDependencies) {
		t.Fatalf("Manifest().Spec().Dependencies = %#v", spec.Dependencies)
	}
}
