//go:build !windows

package detach

import "os/exec"

// 非 Windows 无控制台事件传播，直接原地运行（不 re-exec）
// 实现为"假启动成功"不合适——这里返回 nil 让调用方前台继续跑。
func applyDetached(*exec.Cmd) *exec.Cmd { return nil }
