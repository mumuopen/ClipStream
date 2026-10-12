// clipstream-daemon: 直播录制守护进程（独立于 serve 进程树运行）
//
// 背景：Windows 上 serve 进程（rod/stealth 重依赖）拉起的子进程无法做 FLV 流式 IO
//（实测全部 0KB），独立进程则 100% 正常。因此录制职责拆到此独立进程：
// 自行轮询 watches 表 → 开播进程内拉流 → 状态/心跳落盘 → 下播分段入切片队列。
//
// 用法:
//   clipstream-daemon [-config config.yaml]     # 常驻运行
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
	"chopperbot/internal/live"
	"chopperbot/internal/notify"
	"chopperbot/internal/store"
)

func main() {
	log.SetFlags(log.LstdFlags)

	var cfgPath string
	flag.StringVar(&cfgPath, "config", "config.yaml", "配置文件路径")
	flag.Parse()

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
	// 残留锁处理：锁文件在但心跳已陈旧（>3 个轮询间隔）说明持有者已死，
	// 自动接管删除；心跳新鲜则视为有活实例，退出。
	lockPath := filepath.Join(cfg.DataDir, "live", "daemon.lock")
	if _, err := os.Stat(lockPath); err == nil {
		stale := true
		if b, err2 := os.ReadFile(filepath.Join(cfg.DataDir, "live", "daemon.heartbeat")); err2 == nil {
			if v, err3 := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err3 == nil {
				stale = time.Now().Unix()-v > int64(cfg.Live.CheckIntervalSec)*3+30
			}
		}
		if !stale {
			log.Fatalf("已有 daemon 实例在运行（心跳新鲜）；如需重启先停掉旧实例")
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