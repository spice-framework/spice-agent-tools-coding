//go:build !windows

package cli

import (
	"os"
	"os/exec"
	"syscall"
)

func configureApplicationProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func applicationTerminationSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}

func interruptApplicationProcess(
	process *os.Process,
	signal os.Signal,
) error {
	systemSignal, ok := signal.(syscall.Signal)
	if !ok {
		systemSignal = syscall.SIGINT
	}
	return syscall.Kill(-process.Pid, systemSignal)
}

func killApplicationProcess(process *os.Process) error {
	return syscall.Kill(-process.Pid, syscall.SIGKILL)
}
