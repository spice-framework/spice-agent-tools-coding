//go:build windows

package coding

import "os"

// Windows does not expose a portable directory-fsync operation. The committed
// target itself is opened through os.Root and flushed after its atomic link or
// rename; the pre-commit temporary file was also flushed before close.
func syncDirectory(_ *os.Root, _ string) error { return nil }
