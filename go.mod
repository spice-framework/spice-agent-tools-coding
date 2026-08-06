module github.com/spice-framework/spice-agent-tools-coding

go 1.26.0

toolchain go1.26.5

require (
	github.com/spice-framework/spice v0.1.0-preview.1.0.20260806200749-524424a04df0
	github.com/spice-framework/spice-agent v0.0.0-20260806225954-af79fc7fe4ad
	golang.org/x/sys v0.47.0
)

require (
	github.com/spice-framework/toolchain v0.1.0-preview.1.0.20260806203056-d0b9ac086bd6 // indirect
	golang.org/x/mod v0.38.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/tools v0.48.0 // indirect
)

tool github.com/spice-framework/toolchain/cmd/spice
