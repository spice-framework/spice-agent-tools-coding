//go:build windows

package coding

import (
	"errors"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

var replaceFile = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReplaceFileW")

func renameWithinRoot(workspace *os.Root, oldName, newName string) error {
	oldParent := filepath.Dir(oldName)
	if oldParent != filepath.Dir(newName) {
		return windows.ERROR_NOT_SAME_DEVICE
	}
	parent, err := workspace.OpenRoot(oldParent)
	if err != nil {
		return err
	}
	defer closeBestEffort(parent)
	target, err := windows.UTF16PtrFromString(filepath.Join(parent.Name(), filepath.Base(newName)))
	if err != nil {
		return err
	}
	replacement, err := windows.UTF16PtrFromString(filepath.Join(parent.Name(), filepath.Base(oldName)))
	if err != nil {
		return err
	}
	result, _, callErr := replaceFile.Call(
		uintptr(unsafe.Pointer(target)),      // #nosec G103 -- Win32 ReplaceFileW requires UTF-16 pointers.
		uintptr(unsafe.Pointer(replacement)), // #nosec G103 -- Win32 ReplaceFileW requires UTF-16 pointers.
		0,
		0,
		0,
		0,
	)
	if result == 0 {
		return callErr
	}
	return nil
}

func isTransientRenameError(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
