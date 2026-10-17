//go:build !windows

package main

import (
	"os"
	"strconv"
)

// 非 Windows：proc 文件系统直查进程存在性（daemon 自身强杀后 /proc/<pid> 消失）
func daemonAlive(pid int) bool {
	_, err := os.Stat("/proc/" + strconv.Itoa(pid))
	return err == nil
}
