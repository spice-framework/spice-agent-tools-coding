//go:build !windows

package coding

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

type processTree struct {
	process *os.Process
}

func prepareProcessTree(command *exec.Cmd) (*processTree, error) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return &processTree{}, nil
}

func (tree *processTree) attach(process *os.Process) error {
	tree.process = process
	return nil
}

func (tree *processTree) requestStop() error {
	return signalProcessGroup(tree.process, syscall.SIGTERM)
}

func (tree *processTree) forceStop() error {
	return signalProcessGroup(tree.process, syscall.SIGKILL)
}

func (tree *processTree) close() error {
	return tree.forceStop()
}

func signalProcessGroup(process *os.Process, signal syscall.Signal) error {
	if process == nil {
		return nil
	}
	err := syscall.Kill(-process.Pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
