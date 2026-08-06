//go:build windows

package coding

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func renameWithinRoot(workspace *os.Root, oldName, newName string) error {
	// Keep the commit handle-relative through os.Root. OpenRoot plus Root.Name
	// does not make a later absolute-path Win32 replacement handle-relative;
	// a same-user parent rename or reparse-point swap could escape the root.
	return workspace.Rename(oldName, newName)
}

func isRetryableRenameError(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
