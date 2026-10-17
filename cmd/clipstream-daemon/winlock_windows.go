//go:build windows

package main

// daemonAlive 判断 PID 是否是一个活着的 clipstream-daemon 进程。
// 用途：daemon.lock 残留复核——心跳新鲜但持有者已被强杀（断电/任务管理器/
// 控制台事件）时避免误判"已有实例"拒绝启动（2026-10-16 本地实测踩坑）。
// PID 复用由"可执行文件名匹配"防误判：只有指向 daemon 本体才挡，
// 被系统复用成无关进程则放行接管。
import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func daemonAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false // 进程不存在/无权限（大概率已死）
	}
	defer windows.CloseHandle(h)
	var buf [windows.MAX_PATH]uint16
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return true // 打得开但拿不到名字：保守视为活（宁可拒启不重复录制）
	}
	exe := strings.ToLower(filepath.Base(windows.UTF16ToString(buf[:size])))
	return exe == "clipstream-daemon.exe"
}
