// ClipStream（暂用名）—— ChopperBot 融合平台单二进制入口
//
// 致敬来源：Geniusay/ChopperBot（Java 版智能直播切片机器人，2023 后停更）。
// 本平台用 Go 重写其思想：直播录制 + 搬运下载 + 智能切片 + 翻译配音 + 多平台发布。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"chopperbot/internal/config"
	"chopperbot/internal/core"
	"chopperbot/internal/detach"
	"chopperbot/internal/engine"
	"chopperbot/internal/engines"
	"chopperbot/internal/live"
	"chopperbot/internal/notify"
	"chopperbot/internal/pipeline"
	"chopperbot/internal/publish"
	"chopperbot/internal/store"
	"chopperbot/internal/web"
)

const version = "0.6.1"

func main() {
	log.SetFlags(log.LstdFlags)
	cfgPath := "config.yaml"

	if len(os.Args) < 2 {
		usage(cfgPath)
		return
	}
	sub := os.Args[1]
	args := os.Args[2:]

	switch sub {
	case "version":
		fmt.Println("clipstream", version)
	case "init":
		doInit(cfgPath)
	case "serve":
		// 默认无人值守自脱离（RDP 断开/注销/误关黑窗不再杀服务，0xC000013A
		// 实战教训）；--foreground 前台模式：带控制台窗口运行（可最小化），
		// 日志屏幕+文件双写——看得见实况，代价是关窗/注销会杀进程
		fg := false
		for _, a := range args {
			if a == "-foreground" || a == "--foreground" {
				fg = true
			}
		}
		if fg {
			detach.Tee("serve.log")
		} else if detach.Self("serve.log") {
			return
		}
		if err := doServe(cfgPath); err != nil {
			log.Fatalf("serve 退出: %v", err)
		}
	case "run":
		if err := doRun(cfgPath, args); err != nil {
			log.Fatalf("run 退出: %v", err)
		}
	case "status":
		doStatus(cfgPath)
	case "engines":
		if err := doEngines(cfgPath, args); err != nil {
			log.Fatalf("engines 退出: %v", err)
		}
	case "login":
		if err := doLogin(cfgPath, args); err != nil {
			log.Fatalf("login 退出: %v", err)
		}
	case "record-test":
		doRecordTest(args)
	default:
		usage(cfgPath)
	}
}

func usage(cfgPath string) {
	fmt.Printf(`clipstream %s —— ChopperBot 融合平台（Go 重写版）

用法：
  clipstream init                              初始化配置（%s）并检查引擎
  clipstream serve                             启动 Web 控制台（默认 127.0.0.1:8899）
  clipstream serve --foreground                前台模式：带控制台窗口运行，日志屏幕+文件双写
  clipstream-daemon / clipstream-daemon -foreground   直播录制守护进程（独立运行，勿从 serve 拉起）
  clipstream run -i <URL或本地文件>             创建任务并前台执行，实时打印日志
  clipstream run -i <URL> -kind publish ...    发布任务（一般从审核台发起）
  clipstream login --platform douyin|bilibili  首次登录平台（扫码，登录态长期复用）
  clipstream status                            查看任务列表
  clipstream engines                           引擎体检（缺什么一目了然）
  clipstream engines install                   自动下载缺失引擎（官方源；--model 换 whisper 模型档位）
  clipstream version                           版本

管线（M1 剪切）：download(yt-dlp) → transcribe(whisper.cpp) → highlight(LLM/等分回退) → cut(ffmpeg) → review(审核台)
管线（M2 发布）：prepare → publish(B站=biliup直传 / 抖音=rod浏览器) → finalize(推送通知)
`, version, cfgPath)
}

// ---- init ----

func doInit(cfgPath string) {
	if _, err := os.Stat(cfgPath); err != nil {
		cfg := config.Default()
		if err := cfg.Save(cfgPath); err != nil {
			log.Fatalf("写入默认配置失败: %v", err)
		}
		fmt.Printf("✔ 已生成默认配置 %s（请按需修改代理/引擎/LLM）\n", cfgPath)
	} else {
		fmt.Printf("• 配置已存在：%s（保持不动）\n", cfgPath)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("读取配置失败: %v", err)
	}

	// 数据目录
	for _, d := range []string{cfg.DataDir, filepath.Join(cfg.DataDir, "jobs")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			log.Fatalf("创建目录失败 %s: %v", d, err)
		}
	}
	fmt.Printf("✔ 数据目录就绪：%s\n", filepath.Join(cfg.DataDir, "jobs"))

	// 引擎检查
	engineCheck(cfg)

	// 发布与通知
	fmt.Println("── 发布配置 ──")
	fmt.Printf("  抖音浏览器：headless=%v，截图存档=%v\n", cfg.Publish.Headless, cfg.Publish.Screenshots)
	for _, p := range []string{"bilibili", "douyin"} {
		if _, err := os.Stat(filepath.Join(cfg.Publish.PlatformsDir, p+".yaml")); err != nil {
			fmt.Printf("  △ %s 平台配置缺失：%s.yaml\n", p, p)
		} else {
			fmt.Printf("  ✔ %s 平台配置就绪（%s）\n", p, filepath.Join(cfg.Publish.PlatformsDir, p+".yaml"))
		}
	}
	fmt.Println("── 通知 ──")
	if cfg.Notify.Pushme.Enable && cfg.Notify.Pushme.URL != "" && cfg.Notify.Pushme.PushKey != "" {
		fmt.Printf("  ✔ pushme 已启用：%s\n", cfg.Notify.Pushme.URL)
	} else {
		fmt.Println("  △ pushme 未启用（配置 notify.pushme 的 enable/url/push_key，审核/发布结果推手机）")
	}
	fmt.Println()
	fmt.Println("下一步：clipstream serve 打开控制台，或 clipstream run -i <视频地址> 直接跑")
}

// doEngines 引擎按需安装：
//   clipstream engines                   体检（同 init 的引擎段）+ 缺失清单
//   clipstream engines install           下载全部缺失项到 engines\ 并写回配置
//   clipstream engines install yt-dlp ffmpeg whisper-cli   指定项
//   clipstream engines install --model tiny                换模型档位（默认 small）
// 固定官方源 + 代理尊重（config.proxy）；失败给手动地址，不静默。
func doEngines(cfgPath string, args []string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("读取配置失败: %v", err)
	}
	// 引擎目录与 config.yaml 同根（部署包布局 <root>\engines\）
	dir := "engines"
	if d := filepath.Dir(cfgPath); d != "." && d != "" {
		dir = filepath.Join(d, "engines")
	}

	install := false
	dryRun := false
	model := ""
	var names []string
	for _, a := range args {
		switch {
		case a == "install":
			install = true
		case a == "--dry-run":
			dryRun = true
		case strings.HasPrefix(a, "--model="):
			model = strings.TrimPrefix(a, "--model=")
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("未知参数 %q（用法：engines / engines install [yt-dlp ffmpeg biliup whisper-cli whisper-model] [--model tiny|base|small|medium]）", a)
		default:
			names = append(names, a)
		}
	}

	if !install {
		// 体检模式：引擎检查 + 缺失清单与安装建议
		engineCheck(cfg)
		missing := engines.Missing(cfg, dir)
		if len(missing) > 0 {
			fmt.Printf("\n缺失 %d 项：%s\n", len(missing), strings.Join(missing, ", "))
			fmt.Println("自动补齐：clipstream engines install（从官方源下载，尊重 config.proxy；--dry-run 可先看计划）")
			if containsStr(missing, "whisper-model") {
				fmt.Println("模型档位：--model tiny|base|small|medium（默认 small，约 466MB）")
			}
		} else {
			fmt.Println("\n引擎全部就位 ✔")
		}
		return nil
	}

	// 安装模式
	if dryRun {
		missing := engines.Missing(cfg, dir)
		if len(names) > 0 {
			missing = names
		}
		fmt.Println("── 安装计划（--dry-run，未下载）──")
		for _, n := range missing {
			size := ""
			if n == "whisper-model" {
				size = "（" + engines.ModelSizes[modelOr(model)] + "）"
			}
			fmt.Printf("  %s%s\n", n, size)
		}
		return nil
	}

	fmt.Printf("── 引擎安装（落位 %s，官方源）──\n", dir)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	results, err := engines.Install(ctx, cfg, engines.InstallPlan{WantModel: model, Names: names, TargetDir: dir})
	if err != nil {
		return err
	}
	// 写回配置（相对路径可移植：<config同目录>/engines/...）
	for _, r := range results {
		if r.Status != "installed" {
			continue
		}
		rel, _ := filepath.Rel(filepath.Dir(cfgPath), r.Path)
		switch r.Name {
		case "yt-dlp":
			cfg.Engines.YtDlp = rel
		case "ffmpeg":
			cfg.Engines.FFmpeg = rel
			cfg.Engines.Probe = filepath.Join(filepath.Dir(rel), "ffprobe.exe")
		case "biliup":
			cfg.Engines.Biliup = rel
		case "whisper-cli":
			cfg.Engines.ASR.Binary = rel
		case "whisper-model":
			cfg.Engines.ASR.Model = rel
		}
	}
	if err := cfg.Save(cfgPath); err != nil {
		return fmt.Errorf("配置写回失败: %v", err)
	}
	fmt.Println("── 复检 ──")
	engineCheck(cfg)
	// 汇总失败项
	var failed []string
	for _, r := range results {
		if r.Status == "failed" {
			failed = append(failed, r.Name+": "+r.Detail)
		}
	}
	if len(failed) > 0 {
		fmt.Printf("\n%d 项未完成：\n%s\n", len(failed), strings.Join(failed, "\n"))
	} else {
		fmt.Println("\n全部就位 ✔ 可启动 clipstream serve")
	}
	return nil
}

// modelOr 空档位回退 small（与 engines.Install 默认一致，供提示用）
func modelOr(m string) string {
	if m == "" {
		return "small"
	}
	return m
}

// engineCheck 引擎体检：存在性 + 冒烟执行。init 与 serve 启动都会跑——
// 挂机部署最怕"文件在、一跑就崩"（pip 包装器内嵌本机路径 / 缺 DLL），
// 坏引擎必须当场报警，而不是等任务失败才发现。
func engineCheck(cfg *config.Config) {
	fmt.Println("── 引擎检查 ──")
	checkBin("yt-dlp", cfg.Engines.YtDlp, "--version")
	checkBin("ffmpeg", cfg.Engines.FFmpeg, "-version")
	checkBin("ffprobe", cfg.Engines.Probe, "-version")
	if cfg.Engines.Biliup == "" {
		fmt.Println("  △ biliup: 未配置（B站发布不可用；下载 github.com/biliup/biliup release 后填入 engines.biliup）")
	} else {
		checkBin("biliup", cfg.Engines.Biliup, "--version")
	}
	if cfg.Engines.ASR.Binary == "" || cfg.Engines.ASR.Model == "" {
		fmt.Println("  △ ASR  : 未配置（管线将跳过转写，用等分切片回退）")
	} else {
		checkASR(cfg.Engines.ASR.Binary, cfg.Engines.ASR.Model)
	}
	if cfg.LLM.APIKey == "" {
		fmt.Println("  △ LLM  : 未配置 api_key（高光评分将回退为等分切片）")
	} else {
		fmt.Printf("  LLM   : %s  model=%s\n", base(cfg.LLM.BaseURL), cfg.LLM.Model)
	}
}

// checkBin 引擎检查：LookPath 查存在；带 smokeArgs 时真实执行一次版本命令，
// 退出码非 0 = 引擎残废（内嵌路径失效的包装器 / 缺运行库），比"文件存在"可信得多。
func checkBin(name, bin string, smokeArgs ...string) {
	if _, err := exec.LookPath(bin); err != nil {
		fmt.Printf("  ✗ %-6s: 未找到 %q —— 可运行 clipstream engines install 自动补齐，或在 config.yaml 配置绝对路径\n", name, bin)
		return
	}
	if len(smokeArgs) == 0 {
		fmt.Printf("  ✔ %-6s: %s\n", name, bin)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, smokeArgs...)
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		fmt.Printf("  ✗ %-6s: %s 存在但无法执行（%v）—— 引擎可能损坏或缺少依赖\n    输出：%s\n", name, bin, err, tailLines(out.String(), 3))
		return
	}
	fmt.Printf("  ✔ %-6s: %s（%s）\n", name, bin, firstLine(out.String()))
}

// checkASR whisper-cli 没有 --version 参数，改用"裸跑出 usage"判定能启动：
// 退出码 >= 0 = 进程正常起来过（2 = usage 提示）；负数（如 0xC0000135）=
// 加载器失败（缺 DLL / VC 运行库），正是打包分发最容易踩的坑。
func checkASR(binary, model string) {
	if _, err := exec.LookPath(binary); err != nil {
		fmt.Printf("  ✗ ASR  : 未找到 %q\n", binary)
		return
	}
	if _, err := os.Stat(model); err != nil {
		fmt.Printf("  ✗ ASR  : 模型缺失 %q（%v）\n", model, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, binary)
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() >= 0 {
		fmt.Printf("  ✔ ASR  : %s\n    模型: %s\n", binary, model)
		return
	}
	if err == nil {
		fmt.Printf("  ✔ ASR  : %s\n    模型: %s\n", binary, model)
		return
	}
	fmt.Printf("  ✗ ASR  : %s 无法启动（缺依赖/损坏）：%v\n    输出：%s\n", binary, err, tailLines(out.String(), 3))
}

// firstLine 取输出首行，超长按 rune 裁剪（版本号一行足够）
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > 60 {
		return strings.TrimSpace(string(r[:60]))
	}
	return s
}

// tailLines 取输出末尾 n 行（引擎报错的关键信息通常在最后）
func tailLines(s string, n int) string {
	s = strings.TrimRight(s, "\r\n")
	if s == "" {
		return "（无输出）"
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, " | "))
}

func base(u string) string {
	if u == "" {
		return "https://api.openai.com/v1（默认）"
	}
	return u
}

// ---- 公共装配 ----

type app struct {
	cfg      *config.Config
	store    *store.Store
	bus      *core.Bus
	runner   *pipeline.Runner
	notifier *notify.Pushme
}

func setup(cfgPath string, reset bool) (*app, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "jobs"), 0o755); err != nil {
		return nil, err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "clipstream.db"))
	if err != nil {
		return nil, err
	}
	// 仅长驻进程（serve）负责复位中断任务；run/status 只读，避免误伤并行运行中的任务
	if reset {
		if err := st.ResetInterrupted(); err != nil {
			return nil, err
		}
	}
	bus := core.NewBus()
	ff := &engine.FFMPEG{Bin: cfg.Engines.FFmpeg, ProbeBin: cfg.Engines.Probe}
	notifier := notify.NewPushme(cfg.Notify.Pushme)
	env := &pipeline.Env{
		Cfg:    cfg,
		Store:  st,
		YTDLP:  &engine.YTDLP{Bin: cfg.Engines.YtDlp},
		FFMPEG: ff,
		ASR:    &engine.WhisperTranscriber{Bin: cfg.Engines.ASR.Binary, Model: cfg.Engines.ASR.Model, FF: ff},
		LLM:    engine.NewLLMClient(cfg.LLM.BaseURL, cfg.LLM.APIKey, cfg.LLM.Model, cfg.LLM.Thinking, cfg.LLM.Proxy),
		Bus:    bus,
		Notify: notifier,
	}
	return &app{cfg: cfg, store: st, bus: bus, runner: pipeline.NewRunner(env), notifier: notifier}, nil
}

func (a *app) newJob(input, kind string) (*core.Job, error) {
	if kind == "" {
		kind = "clips"
	}
	job := &core.Job{
		ID:     fmt.Sprintf("j%d-%04d", time.Now().Unix(), time.Now().UnixNano()%10000),
		Kind:   kind,
		Status: core.StatusQueued,
		Input:  input,
	}
	job.Dir = filepath.Join(a.cfg.DataDir, "jobs", job.ID)
	if err := os.MkdirAll(job.Dir, 0o755); err != nil {
		return nil, err
	}
	if err := a.store.CreateJob(job); err != nil {
		return nil, err
	}
	return job, nil
}

// ---- serve ----

func doServe(cfgPath string) error {
	app, err := setup(cfgPath, true)
	if err != nil {
		return err
	}
	defer app.store.Close()

	// 直播录制由独立守护进程 clipstream-daemon 负责（Windows 进程树网络问题的解法）。
	// serve 只消费下播分段队列：把 data/live/queue/*.seg 送入切片管线
	queueDir := filepath.Join(app.cfg.DataDir, "live", "queue")
	if err := os.MkdirAll(queueDir, 0o755); err != nil {
		return err
	}

	// 启动恢复：上次进程退出（崩溃/重启/服务器断电）时未完成的任务重新入列。
	// 不恢复的话这些任务会永远停在 queued/running 成孤儿——挂机系统不可接受。
	// clips 等本地管线重跑幂等（产物覆盖写）；publish 例外：投稿有外部副作用，
	// 盲目重跑同一视频会重复发表——置 failed 终止自动重试，池条目回 pending 由泵
	// 冷却后重发（受间隔闸限速，不会立即双投），通知用户确认。
	if jobs, err := app.store.ListJobs(500); err == nil {
		recovered, publishDropped := 0, 0
		for _, j := range jobs {
			if j.Status != core.StatusQueued && j.Status != core.StatusRunning {
				continue
			}
			if j.Kind == "publish" {
				j.Status = core.StatusFailed
				j.Error = "服务中断：发布不自动重试（防重复投稿），对应待发池条目已回池，由发布泵冷却后重发"
				_ = app.store.UpdateJob(j)
				if req, err := publish.ParseRequest(j.Input); err == nil && req.PoolItemID > 0 {
					_ = app.store.SetPoolItemStatus(req.PoolItemID, "pending")
				}
				publishDropped++
				continue
			}
			j.Status = core.StatusQueued
			_ = app.store.UpdateJob(j)
			app.runner.Enqueue(j.ID)
			recovered++
		}
		if recovered > 0 {
			fmt.Printf("[recover] 恢复 %d 个未完成任务（queued/running）\n", recovered)
		}
		if publishDropped > 0 {
			fmt.Printf("[recover] %d 个中断的发布任务置 failed（防重复投稿），池条目已回池\n", publishDropped)
			app.notifier.Notify("ClipStream 服务重启", fmt.Sprintf("%d 个发布任务因服务中断终止自动重试，对应切片已退回待发池（冷却后自动重发）", publishDropped))
		}
	}

	go scanClipQueue(queueDir, func(segPath, roomTitle string) {
		job, err := app.newJob(segPath, "clips")
		if err != nil {
			fmt.Printf("[live] 分段送入切片管线失败 %s: %v\n", segPath, err)
			return
		}
		app.runner.Enqueue(job.ID)
		fmt.Printf("[live] 分段已送入切片管线：%s → 任务 %s（《%s》）\n", segPath, job.ID, roomTitle)
	})

	// 发布调度：30s tick，发布泵持续消化待发池（发布窗口内）。旧版“每日 HH:MM
	// 定时计划”已退役：入池即排队，窗口+间隔闸+每日上限控制节奏，不再需要
	// 人工设定“几点开始发”
	go scheduleTicker(app)

	// M5 每日摘要：早上 09:00 pushme 昨天发生了什么（挂机视角）
	go dailyDigestTicker(app)

	srv := web.NewServer(app.cfg, cfgPath, app.store, app.runner, app.bus)
	addr := app.cfg.Web.Listen
	if addr == "" {
		addr = "127.0.0.1:8899"
	}
	httpSrv := &http.Server{Addr: addr, Handler: srv.Handler()}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("端口 %s 监听失败: %w", addr, err)
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		fmt.Println("\n正在停止…")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	}()

	fmt.Printf("ClipStream %s 控制台已启动：%s  （Ctrl+C 停止）\n", version, "http://"+addr)
	fmt.Println("数据目录：", app.cfg.DataDir)
	fmt.Println("直播录制：由 clipstream-daemon 独立守护（若未启动，直播不会自动录制）")

	// 挂机流程不跑 init，serve 启动时也要做引擎体检：坏引擎当场报警，别等任务失败
	engineCheck(app.cfg)
	return httpSrv.Serve(listener)
}

// scheduleTicker 发布调度：30s tick，单一职责——发布窗口内持续泵发待发池。
// 窗口外（active_hours，如 10:00-23:00）挂起不动池：夜里发没人看且“下播
// 即刻批量入池→立刻开始发”的时间指纹明显。check 包 recover：任一 panic
// 不能杀死调度 goroutine（否则挂机形态下静默停发，数日无人察觉）
func scheduleTicker(app *app) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	check := func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("[schedule] panic 已拦截（继续调度）: %v\n%s\n", r, debug.Stack())
			}
		}()
		if !config.InActiveHours(app.cfg.Publish.ActiveHours, time.Now()) {
			return // 窗口外：池子挂起（入池/审核不受影响，只是不派发）
		}
		pumpTick(app)
	}
	check()
	for range t.C {
		check()
	}
}

// pumpTick 发布泵：待发池有条目就按间隔与速发（窗口内每 30s tick 尝试 1 条，
// 间隔/每日闸天然限速——如 60min 间隔即每小时一条，6 条/日约 6 小时清空
// 当日池）。配额用尽或池空即静默空转。失败回池的条目同样走泵重试（失败尝试
// 计入间隔计时，自然冷却，不会拿头撞风控）。
// 账号路由：直播绑定优先（watches.bind_account，矩阵号人设垂直）→
// 标签路由回退（绑定账号失效/无绑定时）
func pumpTick(app *app) {
	items, err := app.store.ListPoolItems(false)
	if err != nil || len(items) == 0 {
		return
	}
	// 遍历池（FIFO 尽力而为）：头部条目无匹配账号时不能冻结整池并零感知（队头
	// 阻塞）。全池无可派（账号全被节流/无匹配/配额尽）则本轮空转；无匹配账号
	// 属于需要人工介入的状态（不再是节流自愈场景），首次发现时告警一次
	//（进程级去重，防每 30s 刷屏）。
	noMatchNotified := false
	for _, it := range items {
		platforms := it.Platforms
		if platforms == "" {
			platforms = "bilibili"
		}
		bind := app.store.WatchBindAccount(it.RoomID)
		acc := dispatchOne(app, it, platforms, map[string]string{}, "pump", false)
		if acc != nil {
			if bind != "" && acc.Name != bind {
				app.notifier.Notify("ClipStream 绑定账号回退",
					fmt.Sprintf("《%s》绑定账号「%s」当前不可用，已回退标签路由至「%s」", it.Title, bind, acc.Name))
			}
			fmt.Printf("[pump] 续发：《%s》→ 账号「%s」\n", it.Title, acc.Name)
			app.notifier.Notify("ClipStream 发布泵续发",
				fmt.Sprintf("《%s》→ 账号「%s」（按间隔自动匀速续发，配额尽自动停）", it.Title, acc.Name))
			return // 每 tick 至多 1 条：间隔限速，不多发
		}
		// 区分失败原因：dispatchOne 内部节流让位会被 blocked 吞掉，无匹配
		// 账号（含全部过期）才是真卡死信号——通过探测一次路由确认
		if acc2 := probeRoute(app, platforms, it.Tags, bind); acc2 == nil {
			noMatchNotified = true
		} else {
			// 有可用账号但被节流：正常等待，本轮到此为止。
			// 风控冷却中被闸时补充一次可见性告警（去重）：触发那测 finalize 已
			// 发过通知，这里是 serve 重启后/长冷却期的状态提醒
			if until, count := app.store.AccountRiskPaused(acc2.Name); until > time.Now().Unix() {
				maybeNotifyRiskPause(app, acc2.Name, until, count)
			}
			return
		}
	}
	if noMatchNotified && maybeNotifyNoMatch(app) {
		fmt.Println("[pump] 待发池有条目但无可用账号（无匹配或全部过期）——需要人工检查账号/标签")
	}
}

// pumpNoMatchState 无匹配告警去重：同一天只提醒一次（进程级）
var pumpNoMatchState = struct {
	mu   sync.Mutex
	last string // yyyymmdd
}{}

// riskPauseNotified 风控冷却告警去重（name→until）：同一冷却期只提醒一次；
// 手动恢复后再次触发 until 变化 → 重新告警
var riskPauseNotified sync.Map

func maybeNotifyRiskPause(app *app, name string, until int64, count int) {
	if v, ok := riskPauseNotified.Load(name); ok && v.(int64) == until {
		return
	}
	riskPauseNotified.Store(name, until)
	left := time.Until(time.Unix(until, 0)).Round(time.Minute)
	fmt.Printf("[pump] 账号「%s」风控冷却中：平台限流熔断，约 %s 后自动恢复（第 %d 次触发）\n", name, left, count)
	app.notifier.Notify("ClipStream 发布已暂停（平台限流）",
		fmt.Sprintf("账号「%s」触发平台投稿频率限制，已自动冷却，约 %s 后恢复，期间发布泵静默等待不再投稿。可到发布中心手动提前恢复（不重置惩罚梯度）。", name, left))
}

func maybeNotifyNoMatch(app *app) bool {
	pumpNoMatchState.mu.Lock()
	defer pumpNoMatchState.mu.Unlock()
	today := time.Now().Format("20060102")
	if pumpNoMatchState.last == today {
		return false
	}
	pumpNoMatchState.last = today
	app.notifier.Notify("ClipStream 待发池阻塞",
		"待发池有条目，但当前无可用发布账号（无匹配平台/标签，或账号登录已过期）——发布已暂停，请到发布中心检查账号状态")
	return true
}

// probeRoute 只探测不派发：该条目现在有没有能通过路由的账号（状态 ok+
// 平台+绑定/标签匹配），供泵区分“被节流（自愈）”与“无匹配（需人工）”
func probeRoute(app *app, platforms, tags, bind string) *store.Account {
	all, _ := app.store.ListAccounts()
	return routeAccountWithBind(all, commaSet(platforms), tags, bind)
}

// dispatchOne 派发单条池条目（发布泵调用）：
// 账号路由（直播绑定优先→标签路由回退）+ 节流让位重试——挡账号不挡发布
//（被拦账号拉黑让位次优，全被拦则返回 nil 条目留池）。blocked 跨轮共享
//（已让位账号不重复探测）。
// source 用于日志前缀与降噪：pump 每 30s 一轮会被间隔闸拦住，不打印
// 让位明细，泵静默空转不打日志。
func dispatchOne(app *app, it *store.PoolItem, platforms string, blocked map[string]string, source string, verbose bool) *store.Account {
	bind := app.store.WatchBindAccount(it.RoomID) // 直播绑定账号（无绑定/非直播源为空）
	var acc *store.Account
	for {
		allAccounts, _ := app.store.ListAccounts()
		var candidates []*store.Account
		for _, a := range allAccounts {
			if _, hit := blocked[a.Name]; !hit {
				candidates = append(candidates, a)
			}
		}
		acc = routeAccountWithBind(candidates, commaSet(platforms), it.Tags, bind)
		if acc == nil {
			return nil
		}
		why := publishThrottled(app.store, app.cfg.Publish.RateLimit, acc.Name)
		if why == "" {
			break // 通过两闸，放行
		}
		if _, exists := blocked[acc.Name]; !exists && verbose {
			fmt.Printf("[%s] 账号「%s」%s——让位\n", source, acc.Name, why)
		}
		blocked[acc.Name] = why
	}

	req := map[string]any{
		"source_job": it.SourceJob,
		"clip":       it.Clip,
		"cover":      it.Cover,
		"title":      it.Title,
		"desc":       it.Desc,
		"tags":       commaSlice(it.Tags),
		"platforms":  platSlice(platforms), // Request.platforms 是 []string，不能传 map
		"account":    acc.Name,
		"dry_run":    false,
		"pool_item_id": it.ID, // 失败回池依据：发布任务失败时把池条目置回 pending 供泵重试
	}
	reqJSON, _ := json.Marshal(req)
	// 同账号互斥临界区（S6）：手动发布（web createJob）与泵/调度并发时，
	// "闸判定 → 建任务 → 置 dispatched"必须原子——否则同秒双查双放行，
	// 同账号两条任务都在间隔窗内出去。进锁后二次确认闸：等待锁期间
	// 手动路径可能已占了配额
	job, ok := func() (*core.Job, bool) {
		var job *core.Job
		app.store.WithAccountLock(acc.Name, func() {
			if why := publishThrottled(app.store, app.cfg.Publish.RateLimit, acc.Name); why != "" {
				return // 临界区窗口内配额被并发占走：本轮放弃，条目留池
			}
			// 先置 dispatched 再建任务：newJob+Enqueue 是异步消费，极快失败路径
			// （prepare 素材缺失→回池 pending）可能跑赢 dispatched 覆盖
			_ = app.store.SetPoolItemStatus(it.ID, "dispatched")
			j, err := app.newJob(string(reqJSON), "publish")
			if err != nil {
				_ = app.store.SetPoolItemStatus(it.ID, "pending")
				fmt.Printf("[%s] 创建发布任务失败 %s: %v\n", source, it.Clip, err)
				return
			}
			app.runner.Enqueue(j.ID)
			job = j
		})
		return job, job != nil
	}()
	if !ok {
		return nil
	}
	fmt.Printf("[%s] 已派发：《%s》→ 账号「%s」(优先级 %d) 任务 %s\n",
		source, it.Title, acc.Name, acc.Priority, job.ID)
	return acc
}

// publishThrottled 发布节流四闸判定（全部通过返回空串；被拦返回原因）。
// ⓪风控冷却闸（平台限流熔断：B站 137022 投稿过于频繁后账号级冷却，冷却中
// 不派不发——越撞窗口越久，详见 MarkAccountRiskPaused）
// ①间隔闸按 jobs 表派发时刻（含手动发布与失败尝试但不含 dry-run 演练——见 store 计数过滤）。
// 间隔闸叠加 ±5% 随机抖动：精确的整分钟间隔（每天同一时刻起跳、严格执行固定
// 节拍器）本身是机器行为签名；随机化放行时刻（通过即立即派发，不会"放行又
// 收回"），对可用性无实际影响（daily_max 兑底总量）。
// ②每日上限闸 ③同时在途闸（running 任务数，"不要同时推送超过 N 条"的保险丝：
// 10min 间隔下上传耗时超过间隔时会多条并发，超量即挡）
func publishThrottled(st *store.Store, rl config.RateLimit, accName string) string {
	now := time.Now()
	if until, count := st.AccountRiskPaused(accName); until > now.Unix() {
		left := time.Until(time.Unix(until, 0)).Round(time.Minute)
		return fmt.Sprintf("账号风控冷却中（平台限流熔断，剩余 %s，第 %d 次触发；可在发布中心手动恢复）", left, count)
	}
	if rl.MaxInflight > 0 {
		if n, err := st.CountRunningPublish(accName); err == nil && n >= rl.MaxInflight {
			return fmt.Sprintf("同时在途已达上限（%d/%d 条，等上传完成释放）", n, rl.MaxInflight)
		}
	}
	if rl.MinIntervalMin > 0 {
		if last, err := st.LastPublishAt(accName); err == nil && last > 0 {
			base := time.Duration(rl.MinIntervalMin) * time.Minute
			// threshold ∈ [95%, 105%] * base：随机提前或延后最多 5%（3 分钟档位 ±3s，
			// 60 分钟档位 ±3min），通过即立即派发，不会"放行又收回"
			jitter := rand.Int63n(int64(float64(base) * 0.10))
			threshold := base - base/20 + time.Duration(jitter)
			if wait := threshold - now.Sub(time.Unix(last, 0)); wait > 0 {
				return fmt.Sprintf("距上次发布间隔不足（还需约 %s）", (wait / time.Minute * time.Minute).String())
			}
		}
	}
	if rl.DailyMax > 0 {
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
		if n, err := st.CountPublishSince(accName, today); err == nil && n >= rl.DailyMax {
			return fmt.Sprintf("已达每日上限（今日 %d/%d 条）", n, rl.DailyMax)
			}
	}
	return ""
}

// routeAccountWithBind 账号路由：直播绑定优先（bind 非空且匹配平台/状态 ok
// 时直接命中——矩阵号人设垂直，绑定即意图明确），未命中回退标签路由
//（platform 匹配 + 标签命中任一 + 状态 ok，优先级降序）。绑定账号被节流让位
//（blocked）/停号/平台不匹配时自然回退次优，不会卡死条目
func routeAccountWithBind(accounts []*store.Account, platforms map[string]bool, itemTags, bind string) *store.Account {
	if bind != "" {
		for _, a := range accounts {
			if a.Status == "ok" && a.Platform != "" && platforms[a.Platform] && a.Name == bind {
				return a
			}
		}
	}
	return routeAccount(accounts, platforms, itemTags)
}

// routeAccount 账号路由：平台匹配 + 标签命中（任一）+ 状态 ok，按优先级降序取第一个
func routeAccount(accounts []*store.Account, platforms map[string]bool, itemTags string) *store.Account {
	itemTagSet := commaSet(itemTags)
	var best *store.Account
	for _, a := range accounts {
		if a.Status != "ok" || !platforms[a.Platform] {
			continue
		}
		// 标签匹配：条目标签与账号人设标签有交集（账号无标签视为通配）
		accTags := commaSet(a.Labels)
		if len(accTags) > 0 && !hasIntersection(accTags, itemTagSet) {
			continue
		}
		if best == nil || a.Priority > best.Priority {
			best = a
		}
	}
	return best
}

// containsStr 切片包含判断
func containsStr(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func commaSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out[p] = true
		}
	}
	return out
}

func commaSlice(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func platSlice(s string) []string {
	return commaSlice(s)
}

func hasIntersection(a, b map[string]bool) bool {
	for k := range a {
		if b[k] {
			return true
		}
	}
	return false
}

func or(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// dailyDigestTicker 每日 09:00 pushme 摘要：昨天自动发了什么/拦了什么/池里还剩什么。
// 挂机视角的第二双眼睛——早上看手机一屏了解昨晚全自动流水线的结果。
func dailyDigestTicker(app *app) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	lastDay := ""
	for range t.C {
		now := time.Now()
		day := now.Format("2006-01-02")
		if lastDay == "" {
			lastDay = day // 启动当天不发，次日开始
		}
		if now.Format("15:04") != "09:00" || day == lastDay {
			continue
		}
		lastDay = day
		// 统计昨天：切片任务数 / 池状态
		jobs, err := app.store.ListJobs(500)
		if err != nil {
			continue
		}
		y0 := now.AddDate(0, 0, -1)
		yStart := time.Date(y0.Year(), y0.Month(), y0.Day(), 0, 0, 0, 0, time.Local).Unix()
		yEnd := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local).Unix()
		var clipsJob, reviewPending, failed int
		for _, j := range jobs {
			if j.CreatedAt < yStart || j.CreatedAt >= yEnd || j.Kind != "clips" {
				continue
			}
			clipsJob++
			switch string(j.Status) {
			case "review":
				reviewPending++
			case "failed":
				failed++
			}
		}
		pool, _ := app.store.ListPoolItems(false)
		dispatched := 0
		if all, err := app.store.ListPoolItems(true); err == nil {
			for _, it := range all {
				if it.Status == "dispatched" && it.CreatedAt >= yStart {
					dispatched++
				}
			}
		}
		auto := ""
		if app.cfg.AutoReview.Enabled {
			auto = "（自动审核开）"
		}
		app.notifier.Notify("ClipStream 昨日战报",
			fmt.Sprintf("昨天：切片任务 %d，调度派出 %d%s\n待人工审核 %d｜失败 %d｜内容池待发 %d\n控制台看详情",
				clipsJob, dispatched, auto, reviewPending, failed, len(pool)))
	}
}

// scanClipQueue 轮询 data/live/queue/ 下的 .seg 队列文件，逐个回调后删除
// （防重复消费：文件名唯一；消费失败保留等下一轮）
func scanClipQueue(queueDir string, onClip func(segPath, roomTitle string)) {
	for {
		entries, err := os.ReadDir(queueDir)
		if err != nil {
			time.Sleep(10 * time.Second)
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".seg") {
				continue
			}
		full := filepath.Join(queueDir, e.Name())
		b, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		content := strings.TrimSpace(string(b))
		// 剥 UTF-8 BOM：队列文件若被带 BOM 的编辑器/脚本写过，BOM 会拼进路径导致 download 找不到文件
		content = strings.TrimPrefix(content, "\ufeff")
		parts := strings.SplitN(content, "\t", 2)
		if len(parts) < 2 || parts[0] == "" {
			_ = os.Remove(full) // 格式坏的文件直接弃
			continue
		}
			onClip(parts[0], parts[1])
			_ = os.Remove(full) // 消费后删除（onClip 内 Enqueue 失败会打日志，下轮不再重试）
		}
		time.Sleep(10 * time.Second)
	}
}

// ---- run（前台执行，实时打印日志）----

func doRun(cfgPath string, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	input := fs.String("i", "", "视频 URL 或本地文件路径")
	kind := fs.String("kind", "clips", "管线类型")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *input == "" && fs.NArg() > 0 {
		*input = fs.Arg(0)
	}
	if *input == "" {
		return fmt.Errorf("缺少输入：clipstream run -i <URL或本地文件>")
	}

	app, err := setup(cfgPath, false)
	if err != nil {
		return err
	}
	defer app.store.Close()

	job, err := app.newJob(*input, *kind)
	if err != nil {
		return err
	}
	fmt.Printf("任务 %s 已入队：%s\n", job.ID, job.Input)
	fmt.Println("────────────────────────────────────────")

	app.runner.Enqueue(job.ID)

	// 前台轮询 store：打印新增日志，等终态
	printed := 0
	lastStatus := ""
	for {
		j, err := app.store.GetJob(job.ID)
		if err != nil {
			return err
		}
		if len(j.Log) > printed {
			fmt.Print(j.Log[printed:])
			printed = len(j.Log)
		}
		if string(j.Status) != lastStatus {
			if lastStatus != "" {
				fmt.Printf("状态：%s → %s\n", statusCN(lastStatus), statusCN(string(j.Status)))
			}
			lastStatus = string(j.Status)
		}
		if j.Status.Terminal() || j.Status == core.StatusReview {
			fmt.Println("────────────────────────────────────────")
			fmt.Printf("任务结束：%s（进度 %d%%）\n", statusCN(string(j.Status)), j.Progress)
			if j.Status == core.StatusReview {
				printClips(j)
				fmt.Println("已进入审核台：clipstream serve 打开控制台预览/下载切片")
			}
			if j.Error != "" {
				fmt.Println("错误：", j.Error)
			}
			return nil
		}
		time.Sleep(400 * time.Millisecond)
	}
}

func printClips(j *core.Job) {
	var r struct {
		Clips []struct {
			File  string  `json:"file"`
			Title string  `json:"title"`
			Start float64 `json:"start"`
			End   float64 `json:"end"`
		} `json:"clips"`
	}
	if err := parseJSON(j.Result, &r); err != nil || len(r.Clips) == 0 {
		return
	}
	fmt.Printf("切片成品 %d 个（目录 %s\\clips）：\n", len(r.Clips), j.Dir)
	for i, c := range r.Clips {
		fmt.Printf("  %d. [%d:%02d-%d:%02d] %s  → %s\n", i+1,
			int(c.Start/60), int(c.Start)%60, int(c.End/60), int(c.End)%60, c.Title, c.File)
	}
}

func parseJSON(s string, v any) error {
	if s == "" {
		return fmt.Errorf("空结果")
	}
	return json.Unmarshal([]byte(s), v)
}

func statusCN(s string) string {
	m := map[string]string{
		"queued": "排队", "running": "执行中", "review": "待审核",
		"done": "完成", "failed": "失败", "canceled": "已取消",
	}
	if v, ok := m[s]; ok {
		return v
	}
	return s
}

// ---- status ----

func doStatus(cfgPath string) {
	app, err := setup(cfgPath, false)
	if err != nil {
		log.Fatalf("初始化失败: %v", err)
	}
	defer app.store.Close()
	jobs, err := app.store.ListJobs(30)
	if err != nil {
		log.Fatalf("读取任务失败: %v", err)
	}
	if len(jobs) == 0 {
		fmt.Println("暂无任务")
		return
	}
	fmt.Printf("%-18s %-8s %-12s %-8s %s\n", "ID", "状态", "阶段", "进度", "输入")
	for _, j := range jobs {
		fmt.Printf("%-18s %-8s %-12s %-8d %.60s\n", j.ID, statusCN(string(j.Status)), j.Stage, j.Progress, j.Input)
	}
}

// ---- login ----

// doLogin 平台首次登录：B站走 biliup 交互式扫码；抖音开有头浏览器等扫码，登录态长期复用
func doLogin(cfgPath string, args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	platform := fs.String("platform", "", "bilibili 或 douyin")
	account := fs.String("account", "default", "账号别名（支持多账号）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "accounts"), 0o755); err != nil {
		return err
	}

	// 登录成功后恢复账号可用状态（L4）：login_expired 的账号 CLI 重登后
	// 路由仍规避它——重登的语义就是修复登录态，需同步置回 ok。控制台扫码
	// 路径已有该逻辑（settleBiliLogin），这里补齐 CLI 侧
	restoreStatus := func(platform, name string) {
		if st, err := store.Open(filepath.Join(cfg.DataDir, "clipstream.db")); err == nil {
			_ = st.SetAccountStatus(platform, name, "ok")
			st.Close()
		}
	}

	switch *platform {
	case "bilibili":
		if err := publish.BiliupLogin(cfg, *account); err != nil {
			return err
		}
		restoreStatus("bilibili", *account)
		return nil
	case "douyin":
		pc, err := publish.LoadPlatformConfig("douyin", cfg.Publish.PlatformsDir)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := publish.DouyinLogin(ctx, cfg, pc, *account); err != nil {
			return err
		}
		restoreStatus("douyin", *account)
		return nil
	case "":
		return fmt.Errorf("缺少 --platform（bilibili 或 douyin）")
	default:
		return fmt.Errorf("不支持的平台 %q（当前支持 bilibili / douyin）", *platform)
	}
}

// doRecordTest 直录制测试（绕过 guard/pipeline/bus，隔离排查网络问题）
// 用法: clipstream record-test <room_id> <输出目录>
func doRecordTest(args []string) {
	if len(args) < 2 {
		fmt.Println("用法: clipstream record-test <room_id> <输出目录>")
		os.Exit(1)
	}
	roomID := args[0]
	outDir := args[1]

	// 获取 FLV 地址
	ctx := context.Background()
	fmt.Printf("获取房间 %s 的 FLV 地址…\n", roomID)
	playURL, err := live.GetPlayURL(ctx, roomID)
	if err != nil {
		fmt.Printf("获取地址失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("FLV 地址: %.80s…\n", playURL)

	// 启动录制器
	exe, _ := os.Executable()
	recordBin := filepath.Join(filepath.Dir(exe), "clipstream-record.exe")
	if _, err := os.Stat(recordBin); err != nil {
		fmt.Printf("找不到录制器: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("启动录制器 %s → %s\n", recordBin, outDir)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Printf("创建目录失败: %v\n", err)
		os.Exit(1)
	}

	cmd := exec.Command(recordBin, playURL, outDir, "600")
	out, err := cmd.CombinedOutput()
	fmt.Printf("录制器输出: %s\n", string(out))
	if err != nil {
		fmt.Printf("录制器错误: %v\n", err)
	}

	matches, _ := filepath.Glob(filepath.Join(outDir, "seg_*.flv"))
	for _, m := range matches {
		fi, _ := os.Stat(m)
		fmt.Printf("段文件: %s (%d bytes = %.1fMB)\n", filepath.Base(m), fi.Size(), float64(fi.Size())/(1<<20))
	}
}
