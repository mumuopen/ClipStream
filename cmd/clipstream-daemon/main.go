// clipstream-daemon: 直播录制守护进程（独立于 serve 进程树运行）
//
// 背景：Windows 上 serve 进程（rod/stealth 重依赖）拉起的子进程无法做 FLV 流式 IO
//（实测全部 0KB），独立进程则 100% 正常。因此录制职责拆到此独立进程：
// 自行轮询 watches 表 → 开播进程内拉流 → 状态/心跳落盘 → 下播分段入切片队列。
//
// 用法:
//   clipstream-daemon [-config config.yaml]            # 常驻运行（默认无窗口后台挂机模式）
//   clipstream-daemon -foreground [-config config.yaml] # 前台模式：带控制台窗口，日志屏幕+文件双写
//
// 与 serve 的关系：两者共享 SQLite（WAL 多进程安全）；serve 只读 watches 展示，
// 并消费 data/live/queue/ 队列文件把直播分段送入切片管线。daemon 需与 serve 一并
// 常驻（建议开机自启/计划任务，勿从 serve 拉起以避开进程树问题）。
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"chopperbot/internal/config"
	"chopperbot/internal/detach"
	"chopperbot/internal/live"
	"chopperbot/internal/notify"
	"chopperbot/internal/store"
)

func main() {
	log.SetFlags(log.LstdFlags)

	var cfgPath string
	var foreground bool
	flag.StringVar(&cfgPath, "config", "config.yaml", "配置文件路径")
	flag.BoolVar(&foreground, "foreground", false, "前台运行：带控制台窗口（可最小化），日志屏幕+文件双写；默认无窗口后台挂机模式")
	flag.Parse()

	// 默认无人值守自脱离（RDP 断开/注销/误关黑窗不再杀录制守护，0xC000013A
	// 实战教训）；-foreground 切前台（看得到实况，代价是关窗/注销杀进程）
	if foreground {
		detach.Tee("daemon.log")
	} else if detach.Self("daemon.log") {
		return
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("读取配置失败: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "live"), 0o755); err != nil {
		log.Fatalf("创建 live 目录失败: %v", err)
	}

	st, err := store.Open(filepath.Join(cfg.DataDir, "clipstream.db"))
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer st.Close()

	// 单实例锁：防止误起多个 daemon 重复录制。
	// 残留锁处理：锁文件在但持有者已死则接管。旧版只看心跳新鲜度——持有者被
	// 强杀（断电/任务管理器/0xC000013A 类事件）后心跳仍新鲜，重启被"已有实例"
	// 拒绝几分钟（本地实测踩坑），需手动删心跳文件。现锁里写了 PID，复核两步：
	// ①心跳陈旧 → 接管；②心跳新鲜但锁内 PID 已不是活着的 clipstream-daemon →
	// 同样接管（PID 存活详见 winlock.go / unixlock.go）。
	lockPath := filepath.Join(cfg.DataDir, "live", "daemon.lock")
	if _, err := os.Stat(lockPath); err == nil {
		stale := true
		if b, err2 := os.ReadFile(filepath.Join(cfg.DataDir, "live", "daemon.heartbeat")); err2 == nil {
			if v, err3 := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err3 == nil {
				stale = time.Now().Unix()-v > int64(cfg.Live.CheckIntervalSec)*3+30
			}
		}
		if !stale {
			if raw, rerr := os.ReadFile(lockPath); rerr == nil {
				if oldPID, perr := strconv.Atoi(strings.TrimSpace(string(raw))); perr == nil && oldPID > 0 && oldPID != os.Getpid() {
					if daemonAlive(oldPID) {
						log.Fatalf("已有 daemon 实例在运行（PID %d，心跳新鲜）；如需重启先停掉旧实例", oldPID)
					}
					fmt.Printf("接管残留锁：锁内 PID %d 进程已不存在（上次被强杀）\n", oldPID)
					stale = true
				}
			}
			if !stale {
				log.Fatalf("已有 daemon 实例在运行（心跳新鲜）；如需重启先停掉旧实例")
			}
		}
		_ = os.Remove(lockPath) // 残留锁接管
		fmt.Println("接管残留锁：上次 daemon 已退出")
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatalf("已有 daemon 实例在运行（锁文件 %s 存在；若为残留可手动删除后重启）", lockPath)
	}
	_, _ = fmt.Fprintf(lock, "%d\n", os.Getpid())
	_ = lock.Close()
	defer os.Remove(lockPath) // 正常退出时释放

	notifier := notify.NewPushme(cfg.Notify.Pushme)
	d := live.NewDaemon(cfg, st, notifier)
	d.Start()
	defer d.Stop()

	fmt.Printf("ClipStream 录制守护进程已启动（每 %ds 轮询，分段 %ds）\n",
		cfg.Live.CheckIntervalSec, cfg.Live.SegmentSec)
	fmt.Println("心跳文件：", d.HeartbeatFile())
	fmt.Println("切片队列：", d.QueueDir())
	fmt.Println("Ctrl+C 停止")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Println("\n正在停止录制守护进程…")
}