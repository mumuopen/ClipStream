// guanclip-launch: 干净的进程启动器（无重依赖，隔离 serve 进程的 runtime 状态对子进程网络的影响）
// 用法: guanclip-launch <录制器路径> <FLV_URL> <输出目录> <分段秒数>
package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func main() {
	if len(os.Args) < 5 {
		fmt.Fprintf(os.Stderr, "用法: guanclip-launch <录制器> <URL> <目录> <分段秒>\n")
		os.Exit(1)
	}
	recordBin := os.Args[1]
	args := os.Args[2:]

	cmd := exec.Command(recordBin, args...)
	// 关键：CREATE_NEW_PROCESS_GROUP 让子进程脱离父进程组
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}

	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		fmt.Fprintf(os.Stderr, "%s", string(out))
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode()) // 透传退出码
		}
		fmt.Fprintf(os.Stderr, "启动失败: %v\n", err)
		os.Exit(1)
	}
}
