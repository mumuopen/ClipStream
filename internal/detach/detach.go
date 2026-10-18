// Package detach 无人值守进程自脱离（Windows 控制台事件免疫）。
// 背景（2026-10-17 服务器实战）：启动-挂机.cmd 用 "start /min cmd /c <exe>" 拉起
// serve/daemon，进程 attach 到该最小化 cmd 控制台；RDP 断开/注销/误关黑窗时
// Windows 向该控制台所有进程发 CTRL_CLOSE/CTRL_LOGOFF 事件，限时强杀——挂机中断
//（当日事故：whisper-cli 子进程 exit 0xC000013A 转写全灭；serve 因 Go
// signal.Notify 拦截侥幸存活，但关窗场景它同样活不了）。
// 用法：进程入口先调 Self("xxx.log")，返回 true = 当前进程是一次性引导壳，
// 应立即退出（真正的工作进程已 DETACHED 后台运行，日志转 log\xxx.log）。
package detach

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
)

const envMark = "CLIPSTREAM_DETACHED"

// Self 以无控制台方式 re-exec 当前进程。
// 返回 true 表示调用方是引导壳，应立即正常退出。
// redirectFile：日志落盘名（相对 log\，如 "serve.log"）。返回 false = 已是
// 后台本体或脱离失败（退化为前台运行，行为同旧版）。
func Self(redirectFile string) bool {
	if os.Getenv(envMark) == "1" {
		return false // 后台本体，继续干活
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[detach] 无法定位自身（退化为前台）: %v\n", err)
		return false
	}
	// 运行日志统一收拢到根目录 log\（serve/daemon 一处可查；data\ 只放数据不混日志）
	logPath := filepath.Join("log", redirectFile)
	if err := os.MkdirAll("log", 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "[detach] 日志目录不可建（退化为前台）: %v\n", err)
		return false
	}
	logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[detach] 日志 %s 打不开（退化为前台）: %v\n", logPath, err)
		return false
	}
	defer logF.Close()

	reexec := newDetachedCmd(exe, logF)
	if reexec == nil {
		return false // 平台不支持/创建失败（已打印原因），前台跑
	}
	fmt.Fprintf(os.Stderr, "[detach] 已转入后台（无控制台，RDP 断开/注销/关窗不影响）PID %d；日志 %s\n",
		reexec.Process.Pid, logPath)
	return true
}

// newDetachedCmd 平台相关（windows.go / other.go）
var newDetachedCmd = func(exe string, logF *os.File) *exec.Cmd {
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Env = append(os.Environ(), envMark+"=1")
	cmd.Stdout = logF
	cmd.Stderr = logF
	return applyDetached(cmd)
}

// Tee 前台模式的日志镜像：进程带控制台运行（--foreground，窗口可见可最小化），
// 标准输出同时写屏幕与 log\<redirectFile>——屏幕实时滚、文件完整留档，与
// 后台模式的日志路径一致（挂机切前台排查问题时两个时期的日志同一个文件连续）。
// 打不开文件时退化为纯屏幕（提示一句，不致命）
func Tee(redirectFile string) {
	logPath := filepath.Join("log", redirectFile)
	if err := os.MkdirAll("log", 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "[foreground] 日志目录不可建（仅屏幕输出）: %v\n", err)
		return
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[foreground] 日志 %s 打不开（仅屏幕输出）: %v\n", logPath, err)
		return
	}
	// 前台模式标记（日志里可辨形态；区别于 detach 的 CLIPSTREAM_DETACHED=1）
	os.Setenv("CLIPSTREAM_FOREGROUND", "1")
	teeStream(&os.Stdout, f)
	teeStream(&os.Stderr, f)
	log.SetOutput(os.Stderr) // log 默认 logger 在 init 时绑定旧句柄，指向替换后的流
}

// teeStream 把 *std 重定向为"原句柄 + 文件"双写。os.Stdout 是 *os.File 类型
// 装不下 MultiWriter——标准解法：造管道，*std 指向管道写端（类型匹配，所有
// fmt.Print* 按写入时点读全局变量即流经管道），后台 goroutine 从读端泵到
// MultiWriter(原句柄, 文件)。管道建不起时保持原样（纯屏幕）
func teeStream(std **os.File, f *os.File) {
	orig := *std
	r, w, err := os.Pipe()
	if err != nil {
		return
	}
	*std = w
	go func() {
		_, _ = io.Copy(io.MultiWriter(orig, f), r)
	}()
}
