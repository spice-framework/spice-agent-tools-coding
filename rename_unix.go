//go:build !windows

package coding

import "os"

func renameWithinRoot(workspace *os.Root, oldName, newName string) error {
	return workspace.Rename(oldName, newName)
}

func isTransientRenameError(error) bool { return false }
