//go:build unix

package process

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

type executableFileIdentity struct {
	device uint64
	inode  uint64
}

func (identity executableFileIdentity) equal(other executableFileIdentity) bool {
	return identity == other
}

func openExecutableFile(path string) (*os.File, executableFileIdentity, error) {
	descriptor, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, executableFileIdentity{}, err
	}
	file := os.NewFile(uintptr(descriptor), "[verified-executable]")
	if file == nil {
		_ = unix.Close(descriptor)
		return nil, executableFileIdentity{}, errors.New("cannot own executable descriptor")
	}
	identity, err := inspectExecutableDescriptor(descriptor)
	if err != nil {
		_ = file.Close()
		return nil, executableFileIdentity{}, err
	}
	return file, identity, nil
}

func inspectExecutableDescriptor(descriptor int) (executableFileIdentity, error) {
	var status unix.Stat_t
	if err := unix.Fstat(descriptor, &status); err != nil {
		return executableFileIdentity{}, err
	}
	if status.Mode&unix.S_IFMT != unix.S_IFREG {
		return executableFileIdentity{}, errors.New("executable is not a regular file")
	}
	if status.Mode&0o111 == 0 {
		return executableFileIdentity{}, errors.New("executable has no execute permission")
	}
	return executableFileIdentity{
		device: uint64(status.Dev), //nolint:unconvert // Required on Darwin; redundant only on Linux.
		inode:  uint64(status.Ino), //nolint:unconvert // Required on Darwin; redundant only on Linux.
	}, nil
}

func duplicateExecutableFile(file *os.File) (*os.File, error) {
	if file == nil {
		return nil, errExecutableLeaseClosed
	}
	descriptor, err := unix.Dup(int(file.Fd()))
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(descriptor)
	duplicate := os.NewFile(uintptr(descriptor), "[verified-executable-launch]")
	if duplicate == nil {
		_ = unix.Close(descriptor)
		return nil, errors.New("cannot own duplicated executable descriptor")
	}
	return duplicate, nil
}
