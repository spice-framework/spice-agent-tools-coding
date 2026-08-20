//go:build windows

package process

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

type executableFileIdentity struct {
	volumeSerial uint32
	fileIndexHi  uint32
	fileIndexLo  uint32
}

func (identity executableFileIdentity) equal(other executableFileIdentity) bool {
	return identity == other
}

func openExecutableFile(path string) (*os.File, executableFileIdentity, error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, executableFileIdentity{}, err
	}
	// Denying FILE_SHARE_WRITE and FILE_SHARE_DELETE prevents ordinary in-place
	// mutation or pathname replacement while the launch lease is held.
	handle, err := windows.CreateFile(
		pointer,
		windows.GENERIC_READ|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, executableFileIdentity{}, err
	}
	file := os.NewFile(uintptr(handle), "[verified-executable]")
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, executableFileIdentity{}, errors.New("cannot own executable handle")
	}
	identity, err := inspectExecutableHandle(handle)
	if err != nil {
		_ = file.Close()
		return nil, executableFileIdentity{}, err
	}
	return file, identity, nil
}

func inspectExecutableHandle(handle windows.Handle) (executableFileIdentity, error) {
	fileType, err := windows.GetFileType(handle)
	if err != nil {
		return executableFileIdentity{}, err
	}
	if fileType != windows.FILE_TYPE_DISK {
		return executableFileIdentity{}, errors.New("executable is not a disk file")
	}
	var information windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(handle, &information); err != nil {
		return executableFileIdentity{}, err
	}
	if information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return executableFileIdentity{}, errors.New("executable is a reparse point")
	}
	if information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return executableFileIdentity{}, errors.New("executable is not a regular file")
	}
	return executableFileIdentity{
		volumeSerial: information.VolumeSerialNumber,
		fileIndexHi:  information.FileIndexHigh,
		fileIndexLo:  information.FileIndexLow,
	}, nil
}

func duplicateExecutableFile(file *os.File) (*os.File, error) {
	if file == nil {
		return nil, errExecutableLeaseClosed
	}
	current := windows.CurrentProcess()
	var duplicate windows.Handle
	if err := windows.DuplicateHandle(
		current,
		windows.Handle(file.Fd()),
		current,
		&duplicate,
		0,
		false,
		windows.DUPLICATE_SAME_ACCESS,
	); err != nil {
		return nil, err
	}
	result := os.NewFile(uintptr(duplicate), "[verified-executable-launch]")
	if result == nil {
		_ = windows.CloseHandle(duplicate)
		return nil, errors.New("cannot own duplicated executable handle")
	}
	return result, nil
}
