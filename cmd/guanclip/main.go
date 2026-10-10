// GuanClip（暂用名）—— ChopperBot 融合平台单二进制入口
//
// 致敬来源：Geniusay/ChopperBot（Java 版智能直播切片机器人，2023 后停更）。
// 本平台用 Go 重写其思想：直播录制 + 搬运下载 + 智能切片 + 翻译配音 + 多平台发布。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"chopperbot/internal/config"
	"chopperbot/internal/core"
	"chopperbot/internal/engine"
	"chopperbot/internal/notify"
	"chopperbot/internal/pipeline"
	"chopperbot/internal/publish"
	"chopperbot/internal/store"
	"chopperbot/internal/web"
)

const version = "0.2.0 (M0+M1+M2)"

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
		fmt.Println("guanclip", version)
	case "init":
		doInit(cfgPath)
	case "serve":
		if err := doServe(cfgPath); err != nil {
			log.Fatalf("serve 退出: %v", err)
		}
	case "run":
		if err := doRun(cfgPath, args); err != nil {
			log.Fatalf("run 退出: %v", err)
		}
	case "status":
		doStatus(cfgPath)
	case "login":
		if err := doLogin(cfgPath, args); err != nil {
			log.Fatalf("login 退出: %v", err)
		}
	default:
		usage(cfgPath)
	}
}

func usage(cfgPath string) {
	fmt.Printf(`guanclip %s —— ChopperBot 融合平台（Go 重写版）

用法：
  guanclip init                              初始化配置（%s）并检查引擎
  guanclip serve                             启动 Web 控制台（默认 127.0.0.1:8899）
  guanclip run -i <URL或本地文件>             创建任务并前台执行，实时打印日志
  guanclip run -i <URL> -kind publish ...    发布任务（一般从审核台发起）
  guanclip login --platform douyin|bilibili  首次登录平台（扫码，登录态长期复用）
  guanclip status                            查看任务列表
  guanclip version                           版本

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
	fmt.Println("── 引擎检查 ──")
	checkBin("yt-dlp", cfg.Engines.YtDlp)
	checkBin("ffmpeg", cfg.Engines.FFmpeg)
	checkBin("ffprobe", cfg.Engines.Probe)
	if cfg.Engines.Biliup == "" {
		fmt.Println("  △ biliup: 未配置（B站发布不可用；下载 github.com/biliup/biliup release 后填入 engines.biliup）")
	} else {
		checkBin("biliup", cfg.Engines.Biliup)
	}
	if cfg.Engines.ASR.Binary == "" || cfg.Engines.ASR.Model == "" {
		fmt.Println("  △ ASR  : 未配置（管线将跳过转写，用等分切片回退）")
	} else {
		fmt.Printf("  ASR   : %s\n    模型: %s\n", cfg.Engines.ASR.Binary, cfg.Engines.ASR.Model)
	}
	if cfg.LLM.APIKey == "" {
		fmt.Println("  △ LLM  : 未配置 api_key（高光评分将回退为等分切片）")
	} else {
		fmt.Printf("  LLM   : %s  model=%s\n", base(cfg.LLM.BaseURL), cfg.LLM.Model)
	}

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
	fmt.Println("下一步：guanclip serve 打开控制台，或 guanclip run -i <视频地址> 直接跑")
}

func checkBin(name, bin string) {
	if _, err := exec.LookPath(bin); err != nil {
		fmt.Printf("  ✗ %-6s: 未找到 %q —— 请安装或在 config.yaml 配置绝对路径\n", name, bin)
	} else {
		fmt.Printf("  ✔ %-6s: %s\n", name, bin)
	}
}

func base(u string) string {
	if u == "" {
		return "https://api.openai.com/v1（默认）"
	}
	return u
}

// ---- 公共装配 ----

type app struct {
	cfg    *config.Config
	store  *store.Store
	bus    *core.Bus
	runner *pipeline.Runner
}

func setup(cfgPath string, reset bool) (*app, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "jobs"), 0o755); err != nil {
		return nil, err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "guanclip.db"))
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
	env := &pipeline.Env{
		Cfg:    cfg,
		Store:  st,
		YTDLP:  &engine.YTDLP{Bin: cfg.Engines.YtDlp},
		FFMPEG: &engine.FFMPEG{Bin: cfg.Engines.FFmpeg, ProbeBin: cfg.Engines.Probe},
		ASR:    &engine.WhisperTranscriber{Bin: cfg.Engines.ASR.Binary, Model: cfg.Engines.ASR.Model},
		LLM:    engine.NewLLMClient(cfg.LLM.BaseURL, cfg.LLM.APIKey, cfg.LLM.Model, cfg.LLM.Proxy),
		Bus:    bus,
		Notify: notify.NewPushme(cfg.Notify.Pushme),
	}
	return &app{cfg: cfg, store: st, bus: bus, runner: pipeline.NewRunner(env)}, nil
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

	srv := web.NewServer(app.cfg, app.store, app.runner, app.bus)
	addr := app.cfg.Web.Listen
	if addr == "" {
		addr = "127.0.0.1:8899"
	}
	httpSrv := &http.Server{Addr: addr, Handler: srv.Handler()}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		fmt.Println("\n正在停止…")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	}()

	fmt.Printf("GuanClip %s 控制台已启动：%s  （Ctrl+C 停止）\n", version, "http://"+addr)
	fmt.Println("数据目录：", app.cfg.DataDir)
	return httpSrv.ListenAndServe()
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
		return fmt.Errorf("缺少输入：guanclip run -i <URL或本地文件>")
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
				fmt.Println("已进入审核台：guanclip serve 打开控制台预览/下载切片")
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

	switch *platform {
	case "bilibili":
		return publish.BiliupLogin(cfg, *account)
	case "douyin":
		pc, err := publish.LoadPlatformConfig("douyin", cfg.Publish.PlatformsDir)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		return publish.DouyinLogin(ctx, cfg, pc, *account)
	case "":
		return fmt.Errorf("缺少 --platform（bilibili 或 douyin）")
	default:
		return fmt.Errorf("不支持的平台 %q（当前支持 bilibili / douyin）", *platform)
	}
}
