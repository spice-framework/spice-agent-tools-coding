package autoconfigure

import (
	"path/filepath"
	"testing"

	coding "github.com/spice-framework/spice-agent-tools-coding"
	"github.com/spice-framework/spice-agent/tool"
)

func TestExactFallbackFactoriesAndDescriptor(t *testing.T) {
	t.Parallel()
	config := coding.Config{Root: filepath.Join(t.TempDir(), "worktree")}
	factories := []func(coding.Config) (tool.Tool, error){DefaultRead, DefaultReplace, DefaultShell}
	wantNames := []string{"read", "replace", "shell"}
	for index, factory := range factories {
		value, err := factory(config)
		if err != nil || value.Definition().Name() != wantNames[index] {
			t.Fatalf("factory %d = %#v, %v", index, value, err)
		}
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
