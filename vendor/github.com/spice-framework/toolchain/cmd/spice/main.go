// Command spice is the standalone Spice compiler and developer-tool entrypoint.
//
// During the repository split this command intentionally delegates directly to
// the handwritten CLI. Production self-hosting remains a later integration
// milestone and is not required to build or run this released tool identity.
package main

import (
	"io"
	"os"

	"github.com/spice-framework/toolchain/internal/cli"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(arguments []string, stdout, stderr io.Writer) int {
	return cli.Run(arguments, stdout, stderr)
}
