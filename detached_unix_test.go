//go:build !windows

package coding

import (
	"os/exec"
	"syscall"
)

func configureDetachedChild(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
