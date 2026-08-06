//go:build !windows

package annotationhost

import (
	"errors"
	"os/exec"
	"syscall"
)

type processContainment struct {
	pid int
}

func configureToolProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func containToolProcess(command *exec.Cmd) (processContainment, error) {
	return processContainment{pid: command.Process.Pid}, nil
}

func (containment *processContainment) terminate() error {
	if containment == nil || containment.pid <= 0 {
		return nil
	}
	err := syscall.Kill(-containment.pid, syscall.SIGKILL)
	containment.pid = 0
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func (containment *processContainment) release() error {
	if containment != nil {
		containment.pid = 0
	}
	return nil
}
