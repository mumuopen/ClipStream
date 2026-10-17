//go:build windows

package detach

import (
	"fmt"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func applyDetached(cmd *exec.Cmd) *exec.Cmd {
	// DETACHED_PROCESS：无控制台进程，任何控制台事件（CTRL_CLOSE/LOGOFF/C）都
	// 找不到它；CREATE_NEW_PROCESS_GROUP：与父进程组隔离。stdout/stderr 已是
	// 文件句柄，不依赖控制台。
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
	}
	if err := cmd.Start(); err != nil {
		fmt.Printf("[detach] re-exec 失败（退化为前台）: %v\n", err)
		return nil
	}
	return cmd
}
