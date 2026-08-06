package coding

import spicestarter "github.com/spice-framework/spice/annotation/sdk/starter"

// Manifest returns coding-tool compatibility and review metadata.
func Manifest() spicestarter.Manifest {
	return spicestarter.Must(spicestarter.Spec{
		Schema:    spicestarter.Schema,
		ID:        "github.com/spice-framework/spice-agent-tools-coding",
		Version:   "0.1.0-dev",
		Module:    "github.com/spice-framework/spice-agent-tools-coding",
		SpiceAPI:  spicestarter.APIVersion,
		MinimumGo: "1.26",
		License:   "Apache-2.0",
		Review:    "docs/dependency-review.md",
		Activation: spicestarter.Activation{
			Mode: spicestarter.ActivationExplicitConstructor,
			EntryPoints: []spicestarter.EntryPoint{
				{Package: "github.com/spice-framework/spice-agent-tools-coding", Symbol: "NewRead"},
				{Package: "github.com/spice-framework/spice-agent-tools-coding", Symbol: "NewReplace"},
				{Package: "github.com/spice-framework/spice-agent-tools-coding", Symbol: "NewShell"},
			},
		},
		Capabilities: []string{
			"agent.tool.filesystem.read",
			"agent.tool.filesystem.write",
			"agent.tool.process.execute",
			"agent.tool.network.access",
			"agent.tool.secrets.read",
			"agent.tool.environment.read",
			"agent.tool.environment.write",
		},
	})
}
