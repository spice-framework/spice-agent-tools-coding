package cli

import (
	"context"
	"errors"
	"io"

	"github.com/spice-framework/toolchain/compiler/load"
	compilerservice "github.com/spice-framework/toolchain/compiler/service"
	"github.com/spice-framework/toolchain/internal/lsp"
)

// NewLSPHandler constructs the editor-neutral language-server handler.
func NewLSPHandler(runtime *Runtime) (Handler, error) {
	return newLoaderCommandHandler(
		runtime,
		[]string{"lsp"},
		func(runtime *Runtime, invocation Invocation) int {
			return lspCommandContext(
				context.Background(),
				invocation.Arguments,
				invocation.Stdin,
				invocation.Stdout,
				invocation.Stderr,
				runtime.options,
				runtime.loader,
			)
		},
	)
}

func lspCommandContext(
	ctx context.Context,
	arguments []string,
	input io.Reader,
	stdout io.Writer,
	stderr io.Writer,
	options load.Options,
	loader programLoader,
) int {
	if len(arguments) != 0 {
		if err := writef(
			stderr,
			"Spice lsp failed: lsp does not accept arguments\n",
		); err != nil {
			return 1
		}
		return 2
	}
	server, err := lsp.New(lsp.Config{
		NewService: func(root string) (*compilerservice.Service, error) {
			workspaceOptions := options
			workspaceOptions.Dir = root
			return newCompilerAnalysisService(workspaceOptions, loader)
		},
	})
	if err != nil {
		if writeErr := writef(stderr, "Spice lsp failed: %v\n", err); writeErr != nil {
			return 1
		}
		return 1
	}
	if err := server.Run(ctx, input, stdout); err != nil &&
		!errors.Is(err, context.Canceled) {
		if writeErr := writef(stderr, "Spice lsp failed: %v\n", err); writeErr != nil {
			return 1
		}
		return 1
	}
	return 0
}
