package autoconfigure

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	coding "github.com/spice-framework/spice-agent-tools-coding"
	"github.com/spice-framework/spice-agent/process"
	"github.com/spice-framework/spice-agent/tool"
)

func TestExactFallbackFactoriesAndDescriptor(t *testing.T) {
	t.Parallel()
	config := coding.Config{Root: filepath.Join(t.TempDir(), "worktree")}
	factories := []func(coding.Config) (tool.Tool, error){DefaultRead, DefaultReplace}
	wantNames := []string{"read", "replace", "shell"}
	for index, factory := range factories {
		value, err := factory(config)
		if err != nil || value.Definition().Name() != wantNames[index] {
			t.Fatalf("factory %d = %#v, %v", index, value, err)
		}
	}
	shell, cleanup, err := DefaultShell(
		config,
		process.ResolverFunc(func(context.Context, process.Lookup) (string, error) {
			return filepath.Join(config.Root, "tool"), nil
		}),
		process.LauncherFunc(func(context.Context, process.Spec) (process.Process, error) {
			return nil, errors.New("autoconfiguration constructor fixture must not launch")
		}),
	)
	if err != nil || cleanup == nil || shell.Definition().Name() != "shell" {
		t.Fatalf("DefaultShell() = %#v, %v, %v", shell, cleanup, err)
	}
	descriptor := SpiceAutoConfiguration()
	if descriptor.Review != "docs/dependency-review.md" || len(descriptor.Beans) != 3 {
		t.Fatalf("SpiceAutoConfiguration() = %#v", descriptor)
	}
	for index, bean := range descriptor.Beans {
		if bean.Name != wantNames[index] || !bean.Fallback || bean.Primary {
			t.Fatalf("SpiceAutoConfiguration().Beans[%d] = %#v", index, bean)
		}
	}
}
