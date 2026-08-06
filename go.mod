module github.com/spice-framework/spice-agent-tools-coding

go 1.26.0

toolchain go1.26.5

require (
	github.com/spice-framework/spice v0.1.0-preview.1
	github.com/spice-framework/spice-agent v0.0.0-20260806183953-eaf19180429a
	golang.org/x/sys v0.47.0
)

require (
	github.com/spice-framework/toolchain v0.1.0-preview.1 // indirect
	golang.org/x/mod v0.38.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/tools v0.48.0 // indirect
)

tool github.com/spice-framework/toolchain/cmd/spice
