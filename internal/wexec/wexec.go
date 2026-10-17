// Package wexec 无人值守场景下的子进程构造器。
// 所有长跑引擎子进程（whisper/ffmpeg/yt-dlp/biliup/tts）统一走本包：在 Windows 上
// 以 DETACHED_PROCESS + 新进程组启动，彻底脱离父进程控制台——RDP 断开/注销/误关
// cmd 黑窗时 Windows 发出的 CTRL_CLOSE/CTRL_LOGOFF 事件不再波及子进程。
// 背景（2026-10-17 服务器实战）：serve 挂在 启动-挂机.cmd 的最小化 cmd 控制台上，
// 一次控制台事件让 whisper-cli exit 0xC000013A（STATUS_CONTROL_C_EXIT）当场被杀、
// 转写全灭（serve 自身因 Go signal.Notify 拦截存活）。子进程输出全部走 Go pipe，
// 不依赖控制台，DETACHED 无副作用。
package wexec

import (
	"context"
	"os/exec"
)

// Command 构造已隔离的子进程（ctx/参数语义同 exec.CommandContext）
func Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	isolate(cmd)
	return cmd
}
