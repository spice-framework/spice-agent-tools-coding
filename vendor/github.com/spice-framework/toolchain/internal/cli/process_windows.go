//go:build windows

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func configureApplicationProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
}

func applicationTerminationSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}

func interruptApplicationProcess(
	process *os.Process,
	_ os.Signal,
) error {
	if process.Pid <= 0 {
		return fmt.Errorf("invalid Windows process ID %d", process.Pid)
	}
	// #nosec G115 -- Windows process IDs originate as 32-bit DWORD values;
	// os.Process exposes that same positive value as int.
	processGroupID := uint32(process.Pid)
	return windows.GenerateConsoleCtrlEvent(
		windows.CTRL_BREAK_EVENT,
		processGroupID,
	)
}

func killApplicationProcess(process *os.Process) error {
	return process.Kill()
}
