//go:build windows

package wexec

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
	}
}
