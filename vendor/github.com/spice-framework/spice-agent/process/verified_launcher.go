package process

import "context"

// VerifiedLauncher starts a child from a previously verified executable lease.
// Implementations must validate the Spec against the lease and must prevent a
// pathname substitution from selecting a different image. On a partial launch,
// Process ownership follows the same rules as Launcher.Start.
//
// This interface is intentionally separate from Launcher: a security-sensitive
// caller must not silently fall back to pathname-only launch.
type VerifiedLauncher interface {
	StartVerified(context.Context, *ExecutableLease, Spec) (Process, error)
}

// VerifiedLauncherFunc adapts a verified launch function for constructor
// injection and conformance tests.
type VerifiedLauncherFunc func(context.Context, *ExecutableLease, Spec) (Process, error)

func (launcher VerifiedLauncherFunc) StartVerified(
	ctx context.Context,
	lease *ExecutableLease,
	spec Spec,
) (Process, error) {
	return launcher(ctx, lease, spec)
}
