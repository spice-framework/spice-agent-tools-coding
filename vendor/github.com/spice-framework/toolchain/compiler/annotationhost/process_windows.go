//go:build windows

package annotationhost

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type processContainment struct {
	job windows.Handle
}

func configureToolProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
}

func containToolProcess(command *exec.Cmd) (processContainment, error) {
	if command == nil || command.Process == nil || command.Process.Pid <= 0 {
		return processContainment{}, fmt.Errorf(
			"contain annotation tool: invalid process",
		)
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return processContainment{}, fmt.Errorf(
			"create annotation tool job object: %w",
			err,
		)
	}
	information := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	information.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, err = windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&information)), // #nosec G103 -- Windows API requires a pointer to the fixed SDK structure.
		uint32(unsafe.Sizeof(information)),
	)
	if err != nil {
		closeErr := windows.CloseHandle(job)
		return processContainment{}, errors.Join(
			fmt.Errorf("configure annotation tool job object: %w", err),
			closeErr,
		)
	}
	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(command.Process.Pid), // #nosec G115 -- Windows PIDs are positive DWORD values.
	)
	if err != nil {
		closeErr := windows.CloseHandle(job)
		return processContainment{}, errors.Join(
			fmt.Errorf("open annotation tool process: %w", err),
			closeErr,
		)
	}
	assignErr := windows.AssignProcessToJobObject(job, process)
	closeProcessErr := windows.CloseHandle(process)
	if assignErr != nil || closeProcessErr != nil {
		closeJobErr := windows.CloseHandle(job)
		return processContainment{}, errors.Join(
			assignErr,
			closeProcessErr,
			closeJobErr,
		)
	}
	return processContainment{job: job}, nil
}

func (containment *processContainment) terminate() error {
	if containment == nil || containment.job == 0 {
		return nil
	}
	err := windows.CloseHandle(containment.job)
	containment.job = 0
	return err
}

func (containment *processContainment) release() error {
	return containment.terminate()
}
