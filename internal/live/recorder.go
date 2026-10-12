package live

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"
)

// Recorder 单场直播录制器（纯 Go HTTP 拉流，对齐原版 ChopperBot FlvHandle）
// 生命周期：Start() → HTTP GET FLV 流 → 分时段写文件 → 流断时自查开播状态 →
//
//	还在播就重连，下播则回调 onEnd
type Recorder struct {
	RoomID     string
	SegDir     string
	FFmpegBin  string // 保留字段：后续 flv→mp4 转码用
	SegmentSec int

	cancel   context.CancelFunc
	done     chan struct{}
	segCount atomic.Int32
	started  time.Time

	// OnEnd 录制结束回调（无论下播还是手动停止）；segs 为本场全部分段文件
	OnEnd func(rec *Recorder, segs []string, reason string)
	// OnLog 进度日志
	OnLog func(format string, args ...any)
}

// Start 启动录制（非阻塞）
func (r *Recorder) Start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	r.cancel = cancel
	r.done = make(chan struct{})
	r.started = time.Now()
	go func() {
		defer close(r.done)
		_ = r.runLoop(ctx)
	}()
}

// Stop 手动停止
func (r *Recorder) Stop() []string {
	if r.cancel != nil {
		r.cancel()
	}
	<-r.done
	return r.listSegs()
}

// Done 录制流程是否已结束
func (r *Recorder) Done() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// SegCount 已产生分段数
func (r *Recorder) SegCount() int32 { return r.segCount.Load() }

// Elapsed 已录制时长
func (r *Recorder) Elapsed() time.Duration { return time.Since(r.started) }

func (r *Recorder) log(format string, args ...any) {
	if r.OnLog != nil {
		r.OnLog(format, args...)
	}
}

// runLoop 断流重连主循环：退出条件 = 手动取消 或 下播
func (r *Recorder) runLoop(ctx context.Context) error {
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			r.finish("手动停止")
			return nil
		}
		playURL, err := GetPlayURL(ctx, r.RoomID)
		if err != nil {
			if ctx.Err() != nil {
				r.finish("手动停止")
				return nil
			}
			// 拿不到流地址：3 次内重试，超 3 次查开播状态
			if attempt >= 3 {
				info, err2 := GetRoomInfo(ctx, r.RoomID)
				if err2 != nil || info.LiveStatus == 0 {
					r.finish("下播（无法获取流地址）")
					return nil
				}
				attempt = 0
			}
			r.log("流地址获取失败，%ds 后重试：%v", 15, err)
			if !sleepCtx(ctx, 15*time.Second) {
				r.finish("手动停止")
				return nil
			}
			continue
		}

		r.log("开始拉流（第 %d 段起）", r.SegCount()+1)
		err = r.recordOnce(ctx, playURL)
		if ctx.Err() != nil {
			r.finish("手动停止")
			return nil
		}
		// 流断后判直播间状态：API 失败 ≠ 下播，连续 3 次才收工（防误杀）
		down := false
		for probe := 0; probe < 3; probe++ {
			info, err2 := GetRoomInfo(ctx, r.RoomID)
			if err2 == nil {
				down = info.LiveStatus == 0
				break
			}
			if !sleepCtx(ctx, 5*time.Second) {
				r.finish("手动停止")
				return nil
			}
		}
		// 3 次全失败：保守判定为仍在播继续重连（宁可多试，不误杀）
		if down {
			r.finish(fmt.Sprintf("下播（共 %d 段）", r.SegCount()))
			return nil
		}
		r.log("主播仍在播，5s 后重连（累计 %d 段）", r.SegCount())
		if !sleepCtx(ctx, 5*time.Second) {
			r.finish("手动停止")
			return nil
		}
	}
}

// recordOnce 拉起独立录制器进程做 FLV 拉流（serve 主进程的 Go runtime 有 HTTP 流读取
// 阻塞问题——独立进程无此问题，独立进程测试已验证 454KB/秒级拉流）
// 退出码：0=流正常结束  2=CDN 拒绝/302（换节点重试）  3=流断（重连）  其他=异常
func (r *Recorder) recordOnce(ctx context.Context, playURL string) error {
	// 从当前可执行文件同目录找录制器
	exe, _ := os.Executable()
	exeDir := filepath.Dir(exe)
	recordBin := filepath.Join(exeDir, "guanclip-record.exe")
	if _, err := os.Stat(recordBin); err != nil {
		return fmt.Errorf("找不到录制器 %s: %w", recordBin, err)
	}

	// Windows 排除法最终方案：serve 进程（rod/leakless/sqlite）可能在 Windows 层面
	// 创建了 Job Object 限制了子进程网络。CREATE_BREAKAWAY_FROM_JOB 让子进程脱离 Job
	const (
		winBREAKAWAY  = 0x01000000 // CREATE_BREAKAWAY_FROM_JOB
		winDETACHED   = 0x00000008 // DETACHED_PROCESS
		winNewProcGrp = 0x00000200 // CREATE_NEW_PROCESS_GROUP
	)
	cmd := exec.Command(recordBin, playURL, r.SegDir, fmt.Sprintf("%d", r.SegmentSec))
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: winBREAKAWAY | winDETACHED | winNewProcGrp,
	}
	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		r.log("录制器输出：%s", string(out))
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			switch exitErr.ExitCode() {
			case 0:
				return nil // 正常结束
			case 2:
				return fmt.Errorf("CDN 拒绝（需换节点）")
			case 3:
				return fmt.Errorf("流断（需重连）")
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("录制器异常: %w", err)
	}
	return nil
}

func (r *Recorder) listSegs() []string {
	matches, _ := filepath.Glob(filepath.Join(r.SegDir, "seg_*.flv"))
	var out []string
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && fi.Size() > 1<<20 { // 忽略 <1MB 的残段
			out = append(out, m)
		}
	}
	return out
}

func (r *Recorder) finish(reason string) {
	segs := r.listSegs()
	r.log("录制结束：%s（%d 段，%s）", reason, len(segs), r.Elapsed().Round(time.Second))
	if r.OnEnd != nil {
		r.OnEnd(r, segs, reason)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
