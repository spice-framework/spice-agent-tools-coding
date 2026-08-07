// Package lifecycle defines explicit cleanup and lifecycle coordination types
// used by Spice compiler phases and generated application code.
package lifecycle

import "context"

// Cleanup releases one successfully constructed provider resource.
// Generated code invokes it with a caller-owned rollback or shutdown context.
type Cleanup func(context.Context) error
