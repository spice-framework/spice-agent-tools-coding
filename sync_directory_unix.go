//go:build !windows

package coding

import "os"

func syncDirectory(workspace *os.Root, name string) error {
	directory, err := workspace.Open(name)
	if err != nil {
		return err
	}
	defer directory.Close() //nolint:errcheck // Sync error is the durability signal.
	return directory.Sync()
}
