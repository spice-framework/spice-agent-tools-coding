//go:build windows

package coding

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const createNewProcessGroup = 0x00000200

type processTree struct {
	job     windows.Handle
	process *os.Process
}

func prepareProcessTree(command *exec.Cmd) (*processTree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	information := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	information.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, err = windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&information)), // #nosec G103 -- Windows requires the documented Job Object struct pointer.
		uint32(unsafe.Sizeof(information)),
	)
	if err != nil {
		return nil, errors.Join(err, windows.CloseHandle(job))
	}
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup}
	return &processTree{job: job}, nil
}

func (tree *processTree) attach(process *os.Process) error {
	tree.process = process
	handle, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(process.Pid), // #nosec G115 -- Windows process IDs are unsigned 32-bit values.
	)
	if err != nil {
		return err
	}
	defer func() {
		if err := windows.CloseHandle(handle); err != nil {
			return
		}
	}()
	return windows.AssignProcessToJobObject(tree.job, handle)
}

func (tree *processTree) requestStop() error {
	return tree.terminate()
}

func (tree *processTree) forceStop() error {
	return tree.terminate()
}

func (tree *processTree) terminate() error {
	if tree.job == 0 {
		return nil
	}
	err := windows.TerminateJobObject(tree.job, 1)
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) && tree.process != nil {
		killErr := tree.process.Kill()
		if errors.Is(killErr, os.ErrProcessDone) {
			killErr = nil
		}
		return errors.Join(err, killErr)
	}
	return err
}

func (tree *processTree) close() error {
	if tree.job == 0 {
		return nil
	}
	err := windows.CloseHandle(tree.job)
	tree.job = 0
	return err
}
