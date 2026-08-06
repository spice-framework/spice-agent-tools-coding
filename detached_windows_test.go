//go:build windows

package coding

import "os/exec"

func configureDetachedChild(_ *exec.Cmd) {}
