//go:build !windows

package coding

import "os"

func renameWithinRoot(workspace *os.Root, oldName, newName string) error {
	return workspace.Rename(oldName, newName)
}

func isRetryableRenameError(error) bool { return false }
