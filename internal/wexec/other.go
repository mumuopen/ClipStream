//go:build !windows

package wexec

import "os/exec"

func isolate(*exec.Cmd) {} // 非 Windows 无控制台 Ctrl 事件传播语义
