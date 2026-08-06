package autoconfigure

import (
	"path/filepath"
	"testing"

	coding "github.com/spice-framework/spice-agent-tools-coding"
)

func TestDefaultAndDescriptor(t *testing.T) {
	t.Parallel()
	suite, err := Default(coding.Config{Root: filepath.Join(t.TempDir(), "worktree")})
	if err != nil || len(suite.Capabilities()) != 3 {
		t.Fatalf("Default() = %#v, %v", suite, err)
	}
	descriptor := SpiceAutoConfiguration()
	if descriptor.Review != "docs/dependency-review.md" || len(descriptor.Beans) != 1 {
		t.Fatalf("SpiceAutoConfiguration() = %#v", descriptor)
	}
}
