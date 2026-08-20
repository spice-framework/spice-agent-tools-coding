// Package identity owns the canonical module and executable identities that
// cross the public Spice core and toolchain boundary.
package identity

import "strings"

const (
	// CoreModule is the public runtime, annotation descriptor, and SDK module.
	CoreModule = "github.com/spice-framework/spice"
	// CoreVersion is the exact core revision validated by this toolchain slice.
	CoreVersion = "v0.1.0-preview.1.0.20260807031220-45e4f9d3e12d"
	// ToolchainModule is the compiler, CLI, LSP, and annotation-tool module.
	ToolchainModule = "github.com/spice-framework/toolchain"
	// CLITool is the Spice command package applications invoke through go tool.
	CLITool = ToolchainModule + "/cmd/spice"
	// AnnotationTool is the official Go tool package applications authorize.
	AnnotationTool = ToolchainModule + "/cmd/spice-annotation-core"
	// LegacyAnnotationTool is accepted only while applications migrate from the
	// former monorepository command path.
	LegacyAnnotationTool = CoreModule + "/cmd/spice-annotation-core"
)

// OfficialDescriptorPackage reports whether path belongs to the public core
// annotation descriptor tree intentionally served by the official toolchain.
func OfficialDescriptorPackage(path string) bool {
	return strings.HasPrefix(path, CoreModule+"/annotation/")
}

// NormalizeDescriptorTool maps the former official command identity to its
// extracted toolchain identity. Third-party descriptor tools are untouched.
func NormalizeDescriptorTool(descriptorPackage, tool string) string {
	if OfficialDescriptorPackage(descriptorPackage) && tool == LegacyAnnotationTool {
		return AnnotationTool
	}
	return tool
}
