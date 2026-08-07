// Package autoconfigure contributes the default coding suite only when an
// application explicitly blank-imports this package.
package autoconfigure

import (
	coding "github.com/spice-framework/spice-agent-tools-coding"
	"github.com/spice-framework/spice-agent/process"
	"github.com/spice-framework/spice-agent/tool"
	"github.com/spice-framework/spice/lifecycle"
	"github.com/spice-framework/spice/starter"
)

// DefaultRead constructs the fallback exact read binding.
func DefaultRead(config coding.Config) (tool.Tool, error) {
	return coding.NewRead(config)
}

// DefaultReplace constructs the fallback exact replace binding.
func DefaultReplace(config coding.Config) (tool.Tool, error) {
	return coding.NewReplace(config)
}

// DefaultShell constructs the fallback exact shell binding.
func DefaultShell(
	config coding.Config,
	resolver process.ExecutableResolver,
	launcher process.Launcher,
) (tool.Tool, lifecycle.Cleanup, error) {
	return coding.NewShell(config, resolver, launcher)
}

// SpiceAutoConfiguration is statically decoded by Spice and never executed
// during analysis.
func SpiceAutoConfiguration() starter.AutoConfiguration {
	return starter.AutoConfiguration{
		Review: "docs/dependency-review.md",
		Beans: []starter.AutoBean{
			{Factory: DefaultRead, Name: "read", Fallback: true},
			{Factory: DefaultReplace, Name: "replace", Fallback: true},
			{Factory: DefaultShell, Name: "shell", Fallback: true},
		},
	}
}
