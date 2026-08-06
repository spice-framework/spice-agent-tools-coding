// Package autoconfigure contributes the default coding suite only when an
// application explicitly blank-imports this package.
package autoconfigure

import (
	coding "github.com/spice-framework/spice-agent-tools-coding"
	"github.com/spice-framework/spice/starter"
)

// Default constructs the fallback suite from application-owned typed
// configuration without touching the filesystem or starting a process.
func Default(config coding.Config) (*coding.Suite, error) {
	return coding.New(config)
}

// SpiceAutoConfiguration is statically decoded by Spice and never executed
// during analysis.
func SpiceAutoConfiguration() starter.AutoConfiguration {
	return starter.AutoConfiguration{
		Review: "docs/dependency-review.md",
		Beans: []starter.AutoBean{{
			Factory:  Default,
			Name:     "codingTools",
			Fallback: true,
		}},
	}
}
