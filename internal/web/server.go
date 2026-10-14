package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	pty "github.com/aymanbagabas/go-pty"

	"chopperbot/internal/config"
	"chopperbot/internal/core"
	"chopperbot/internal/live"
	"chopperbot/internal/pipeline"
	"chopperbot/internal/publish"
	"chopperbot/internal/store"
)

//go:embed console.html
var consoleHTML string

type Server struct {
	cfg     *config.Config
	cfgPath string // 配置文件路径（后台保存时写回）
	store   *store.Store
	runner  *pipeline.Runner
	bus     *core.Bus

	loginMu      sync.Mutex
	loginSessions map[string]*loginSession // 扫码登录会话（B站 biliup 子进程托管）
}

func NewServer(cfg *config.Config, cfgPath string, st *store.Store, r *pipeline.Runner, bus *core.Bus) *Server {
	return &Server{cfg: cfg, cfgPath: cfgPath, store: st, runner: r, bus: bus, loginSessions: map[string]*loginSession{}}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("POST /api/jobs", s.createJob)
	mux.HandleFunc("GET /api/jobs", s.listJobs)
	mux.HandleFunc("GET /api/jobs/{id}", s.getJob)
	mux.HandleFunc("DELETE /api/jobs/{id}", s.deleteJob)
	mux.HandleFunc("GET /api/jobs/{id}/events", s.events)
	mux.HandleFunc("GET /api/files/{job}/{file...}", s.serveFile)
	mux.HandleFunc("GET /api/stats", s.stats)
	// 直播关注（录制由独立 daemon 进程负责，serve 只读状态）
	mux.HandleFunc("GET /api/live/watches", s.listWatches)
	mux.HandleFunc("POST /api/live/watches", s.addWatch)
	mux.HandleFunc("DELETE /api/live/watches/{id}", s.deleteWatch)
	mux.HandleFunc("GET /api/live/daemon", s.liveDaemon)
	mux.HandleFunc("GET /api/live/barrage", s.liveBarrage)
	// 发布中心：账号 + 定时计划 + 待发池
	mux.HandleFunc("GET /api/accounts", s.listAccounts)
	mux.HandleFunc("POST /api/accounts", s.createAccount)
	mux.HandleFunc("DELETE /api/accounts/{id}", s.deleteAccount)
	mux.HandleFunc("GET /api/schedules", s.listSchedules)
	mux.HandleFunc("POST /api/schedules", s.createSchedule)
	mux.HandleFunc("PUT /api/schedules/{id}/enabled", s.enableSchedule)
	mux.HandleFunc("DELETE /api/schedules/{id}", s.deleteSchedule)
	mux.HandleFunc("GET /api/pool", s.listPool)
	mux.HandleFunc("POST /api/pool", s.addPool)
	mux.HandleFunc("DELETE /api/pool/{id}", s.deletePool)
	// 全部配置后台化：运营参数 + LLM/引擎/发布段免改文件
	mux.HandleFunc("GET /api/config", s.getConfig)
	mux.HandleFunc("PUT /api/config", s.updateConfig)
	// 扫码登录：B站 biliup 二维码流式到浏览器弹窗
	mux.HandleFunc("POST /api/login/start", s.loginStart)
	mux.HandleFunc("GET /api/login/status", s.loginStatus)
	mux.HandleFunc("GET /api/login/qrpng", s.loginQRPNG)
	return logRequest(mux)
}

// ── 后台配置 API：全配置读写（运营 + LLM + 引擎 + 发布 + Chrome）──
// 注意：改后立即生效的是 serve 内按 Cfg 指针读的段（auto_review）；
// daemon 侧参数（live/retention）与启动时建好的引擎客户端（LLM/engines）需重启对应进程。

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"auto_review": s.cfg.AutoReview,
		"live":        s.cfg.Live,
		"retention":   s.cfg.Retention,
		"notify":      map[string]any{"pushme": s.cfg.Notify.Pushme},
		"llm":         s.cfg.LLM,
		"engines":     map[string]any{
			"ytdlp":  s.cfg.Engines.YtDlp,
			"ffmpeg": s.cfg.Engines.FFmpeg,
			"ffprobe": s.cfg.Engines.Probe,
			"biliup": s.cfg.Engines.Biliup,
			"asr":    map[string]string{"binary": s.cfg.Engines.ASR.Binary, "model": s.cfg.Engines.ASR.Model},
		},
		"publish": map[string]any{"headless": s.cfg.Publish.Headless, "screenshots": s.cfg.Publish.Screenshots},
		"chrome":  map[string]string{"path": s.cfg.Chrome.Path},
	})
}

func (s *Server) updateConfig(w http.ResponseWriter, r *http.Request) {
	// 用指针字段区分“未传”与“传 false/空串”：未传的段保持不动
	var req struct {
		AutoReview *config.AutoReview `json:"auto_review"`
		Live       *config.Live       `json:"live"`
		Retention  *config.Retention  `json:"retention"`
		Notify     *struct {
			Pushme *config.Pushme `json:"pushme"`
		} `json:"notify"`
		LLM     *config.LLM     `json:"llm"`
		Engines *config.Engines `json:"engines"`
		Publish *struct {
			Headless    *bool `json:"headless"`
			Screenshots *bool `json:"screenshots"`
		} `json:"publish"`
		Chrome *struct {
			Path string `json:"path"`
		} `json:"chrome"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, 400, "参数错误: "+err.Error())
		return
	}
	if req.AutoReview != nil {
		ar := req.AutoReview
		if ar.MinScore < 0 || ar.MinScore > 100 {
			httpError(w, 400, "min_score 需在 0-100")
			return
		}
		if ar.HardMinDurSec < 0 || ar.HardMaxDurSec <= ar.HardMinDurSec {
			httpError(w, 400, "硬闸时长需 0 ≤ min < max")
			return
		}
		if ar.BannedWords == nil {
			ar.BannedWords = s.cfg.AutoReview.BannedWords // 表单未带时保留原值，不静默清空
		}
		s.cfg.AutoReview = *ar
	}
	if req.Live != nil {
		lv := req.Live
		if lv.CheckIntervalSec < 5 || lv.SegmentSec < 60 {
			httpError(w, 400, "检测间隔 ≥5s，分段时长 ≥60s")
			return
		}
		s.cfg.Live = *lv
	}
	if req.Retention != nil {
		rt := req.Retention
		if rt.SegmentsDays < 1 || rt.MinFreePercent < 1 || rt.MinFreePercent > 95 || rt.CheckMinutes < 5 {
			httpError(w, 400, "保留天数 ≥1，水位 1-95%，清理间隔 ≥5min")
			return
		}
		if rt.ConsumedSegDays < 0 {
			rt.ConsumedSegDays = s.cfg.Retention.ConsumedSegDays
		}
		s.cfg.Retention = *rt
	}
	if req.Notify != nil && req.Notify.Pushme != nil {
		s.cfg.Notify.Pushme = *req.Notify.Pushme
	}
	if req.LLM != nil {
		l := req.LLM
		if l.BaseURL == "" || l.Model == "" {
			httpError(w, 400, "llm.base_url 与 llm.model 不能为空")
			return
		}
		if l.APIKey == "" {
			l.APIKey = s.cfg.LLM.APIKey // 空值视为未改动（password 框不明文回传时）
		}
		if l.Thinking != "disabled" && l.Thinking != "enabled" {
			l.Thinking = ""
		}
		s.cfg.LLM = *l
	}
	if req.Engines != nil {
		en := *req.Engines // 逐字段合并：空串 = 未改动，保持原值（前端只提交非空字段）
		old := s.cfg.Engines
		if en.YtDlp == "" {
			en.YtDlp = old.YtDlp
		}
		if en.FFmpeg == "" {
			en.FFmpeg = old.FFmpeg
		}
		if en.Probe == "" {
			en.Probe = old.Probe
		}
		if en.Biliup == "" {
			en.Biliup = old.Biliup
		}
		if en.ASR.Binary == "" {
			en.ASR.Binary = old.ASR.Binary
		}
		if en.ASR.Model == "" {
			en.ASR.Model = old.ASR.Model
		}
		s.cfg.Engines = en
	}
	if req.Publish != nil {
		if req.Publish.Headless != nil {
			s.cfg.Publish.Headless = *req.Publish.Headless
		}
		if req.Publish.Screenshots != nil {
			s.cfg.Publish.Screenshots = *req.Publish.Screenshots
		}
	}
	if req.Chrome != nil {
		s.cfg.Chrome.Path = req.Chrome.Path
	}
	if err := s.cfg.Save(s.cfgPath); err != nil {
		httpError(w, 500, "写入 config.yaml 失败: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true,
		"note": "已保存并写回 config.yaml：审核/通知即时生效；直播检测/磁盘清理需重启 daemon；LLM 与引擎路径需重启 serve 生效"})
}

// ── M5 矩阵账号 / 发布调度 API（占位骨架）──

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	as, err := s.store.ListAccounts()
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	for _, a := range as {
		a.LoggedIn = s.accountCookieOK(a.Platform, a.Name)
	}
	if as == nil {
		as = []*store.Account{}
	}
	writeJSON(w, as)
}

// accountCookieOK 登录态以磁盘为准：cookie 文件在 = 已登录
func (s *Server) accountCookieOK(platform, name string) bool {
	cookie := filepath.Join(publish.AccountDir(s.cfg.DataDir, platform, name), "cookies.json")
	_, err := os.Stat(cookie)
	return err == nil
}

var accountNameRe = regexp.MustCompile(`^[A-Za-z0-9_\-\x{4e00}-\x{9fa5}]{1,32}$`)

func (s *Server) createAccount(w http.ResponseWriter, r *http.Request) {
	var a store.Account
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		httpError(w, 400, "参数错误: "+err.Error())
		return
	}
	if a.Platform == "" || a.Name == "" {
		httpError(w, 400, "platform 与 name 必填")
		return
	}
	// 账号名会拼进登录态目录（data/accounts/<platform>-<name>）：禁止路径字符，防穿越
	if !accountNameRe.MatchString(a.Name) {
		httpError(w, 400, "账号名仅限中英文数字、下划线、连字符，长度 ≤32")
		return
	}
	a.Status = "ok" // 展示层登录态看 LoggedIn（磁盘 cookie），status 仅在发布失败时回写 login_expired
	id, err := s.store.CreateAccount(&a)
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	a.ID = id
	a.LoggedIn = s.accountCookieOK(a.Platform, a.Name)
	writeJSON(w, a)
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.store.DeleteAccount(id); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]string{"ok": "true"})
}

func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request) {
	scs, err := s.store.ListSchedules()
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	if scs == nil {
		scs = []*store.Schedule{}
	}
	writeJSON(w, scs)
}

func (s *Server) createSchedule(w http.ResponseWriter, r *http.Request) {
	// enabled 用指针：前端没传时默认启用（否则 Go 零值 false 会让计划从未不触发）
	var req struct {
		Name      string `json:"name"`
		Time      string `json:"time"`
		Platforms string `json:"platforms"`
		Enabled   *bool  `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, 400, "参数错误: "+err.Error())
		return
	}
	if req.Name == "" || req.Time == "" {
		httpError(w, 400, "name 与 time(HH:MM) 必填")
		return
	}
	sc := &store.Schedule{Name: req.Name, Time: req.Time, Platforms: req.Platforms, Enabled: true}
	if req.Enabled != nil {
		sc.Enabled = *req.Enabled
	}
	id, err := s.store.CreateSchedule(sc)
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	sc.ID = id
	writeJSON(w, sc)
}

// enableSchedule 启/停定时发布
func (s *Server) enableSchedule(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var req struct{ Enabled bool `json:"enabled"` }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, 400, "参数错误: "+err.Error())
		return
	}
	if err := s.store.EnableSchedule(id, req.Enabled); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "enabled": req.Enabled})
}

func (s *Server) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.store.DeleteSchedule(id); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]string{"ok": "true"})
}

// ── 内容池 API（M5：审核通过入池，调度到点派发）──

func (s *Server) listPool(w http.ResponseWriter, r *http.Request) {
	all := r.URL.Query().Get("all") == "1"
	items, err := s.store.ListPoolItems(all)
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	if items == nil {
		items = []*store.PoolItem{}
	}
	writeJSON(w, items)
}

func (s *Server) addPool(w http.ResponseWriter, r *http.Request) {
	var it store.PoolItem
	if err := json.NewDecoder(r.Body).Decode(&it); err != nil {
		httpError(w, 400, "参数错误: "+err.Error())
		return
	}
	if it.SourceJob == "" || it.Clip == "" {
		httpError(w, 400, "source_job 与 clip 必填")
		return
	}
	id, err := s.store.AddPoolItem(&it)
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	it.ID = id
	writeJSON(w, it)
}

func (s *Server) deletePool(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.store.DeletePoolItem(id); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]string{"ok": "true"})
}

// ── 扫码登录（UI 化：B站 biliup 终端二维码流到浏览器弹窗）──

// loginSession 一次扫码登录会话：ConPTY 伪终端跑 biliup login（它强制要求 TTY），
// 终端输出（含二维码）持续供前端轮询渲染
type loginSession struct {
	id        string
	platform  string
	account   string
	cmd       *pty.Cmd
	tty       pty.Pty
	qrPNG     string    // biliup 生成的 qrcode.png 路径（账号目录内）
	mu        sync.Mutex
	output    string    // 剥离 ANSI 后的累计输出（尾部截断）
	done      bool
	exitErr   string
	cookieOK  bool
	startedAt time.Time
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]|\x1b\][^\x07]*\x07`)
var blankRuns = regexp.MustCompile(`\n{4,}`) // ConPTY 屏幕滚动会堆积大量空行，压缩后前端才能看到二维码

// loginStart 启动 B站扫码登录（当前仅 bilibili：抖音登录需要有头浏览器，不适合无人值守服务器）
func (s *Server) loginStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Platform string `json:"platform"`
		Account  string `json:"account"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, 400, "参数错误: "+err.Error())
		return
	}
	if req.Platform != "bilibili" {
		httpError(w, 400, "当前仅支持 bilibili 扫码登录（抖音需在装有浏览器的电脑上操作）")
		return
	}
	if req.Account == "" {
		req.Account = "default"
	}
	if !accountNameRe.MatchString(req.Account) {
		httpError(w, 400, "账号名仅限中英文数字、下划线、连字符")
		return
	}
	biliupBin := s.cfg.Engines.Biliup
	if biliupBin == "" {
		httpError(w, 400, "B站引擎未配置：请先在设置页填 biliup 路径（或用部署包内置 ./engines/biliup.exe）")
		return
	}
	// 引擎路径可能是相对路径（如 ./engines/biliup.exe）：子进程工作目录将切到账号目录，必须先绝对化
	bin, err := filepath.Abs(biliupBin)
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	// 同一账号并发登录：顶掉旧会话
	s.loginMu.Lock()
	for _, sess := range s.loginSessions {
		if sess.platform == req.Platform && sess.account == req.Account && !sess.done {
			if sess.cmd != nil && sess.cmd.Process != nil {
				_ = sess.cmd.Process.Kill()
			}
			if sess.tty != nil {
				_ = sess.tty.Close()
			}
		}
	}
	s.loginMu.Unlock()

	dir := publish.AccountDir(s.cfg.DataDir, "bilibili", req.Account)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		httpError(w, 500, "创建账号目录失败: "+err.Error())
		return
	}
	// cookie 路径绝对化：工作目录将切到账号目录（biliup 的 qrcode.png 落在那里，多会话互不覆盖）
	cookieAbs, err := filepath.Abs(filepath.Join(dir, "cookies.json"))
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}

	// biliup login 强制交互式终端（非 TTY 直接报 not a terminal）：
	// 用 ConPTY 伪终端承载，二维码/提示输出流式回传浏览器
	tty, err := pty.New()
	if err != nil {
		httpError(w, 500, "无法创建登录终端: "+err.Error())
		return
	}
	_ = tty.Resize(110, 40)
	cmd := tty.Command(bin, "-u", cookieAbs, "login")
	cmd.Dir = dir

	sess := &loginSession{
		id:        fmt.Sprintf("login-%d-%s", time.Now().UnixNano(), strconv.Itoa(rand.Intn(1000))),
		platform:  req.Platform,
		account:   req.Account,
		cmd:       cmd,
		tty:       tty,
		qrPNG:     filepath.Join(dir, "qrcode.png"),
		startedAt: time.Now(),
	}
	if err := cmd.Start(); err != nil {
		_ = tty.Close()
		httpError(w, 500, "启动 biliup 登录失败: "+err.Error())
		return
	}
	go func() {
		menuDone := false // 菜单自动选择只触发一次
		appendOut := func(b []byte) {
			clean := ansiRe.ReplaceAllString(string(b), "")
			clean = blankRuns.ReplaceAllString(clean, "\n\n\n")
			sess.mu.Lock()
			sess.output += clean
			if len(sess.output) > 64<<10 { // 保留尾部 64KB（二维码输出足够）
				sess.output = sess.output[len(sess.output)-64<<10:]
			}
			sess.mu.Unlock()
		}
		buf := make([]byte, 8192)
		for {
			n, rerr := tty.Read(buf)
			if n > 0 {
				appendOut(buf[:n])
			}
			// biliup 首屏是登录方式菜单（箭头选择）：自动选中「扫码登录」
			if !menuDone {
				sess.mu.Lock()
				menuSeen := strings.Contains(sess.output, "选择一种登录方式")
				sess.mu.Unlock()
				if menuSeen {
					time.Sleep(700 * time.Millisecond) // 等菜单渲染稳定
					menuDone = true
					sess.autoSelectQRLogin(tty)
				}
			}
			if rerr != nil {
				break
			}
		}
		werr := cmd.Wait()
		_ = tty.Close()
		sess.mu.Lock()
		sess.done = true
		if werr != nil {
			sess.exitErr = werr.Error()
		}
		sess.mu.Unlock()
		// 登录成功：cookie 落盘 → 恢复账号可用状态（若有账号记录）
		if s.accountCookieOK(req.Platform, req.Account) {
			sess.mu.Lock()
			sess.cookieOK = true
			sess.mu.Unlock()
			_ = s.store.SetAccountStatus(req.Platform, req.Account, "ok")
		}
	}()

	s.loginMu.Lock()
	s.loginSessions[sess.id] = sess
	// 简单清理：保留最近 20 个会话
	if len(s.loginSessions) > 20 {
		for k, v := range s.loginSessions {
			if time.Since(v.startedAt) > time.Hour {
				delete(s.loginSessions, k)
			}
		}
	}
	s.loginMu.Unlock()

	writeJSON(w, map[string]any{"id": sess.id, "account": req.Account})
}

// autoSelectQRLogin 在 biliup 登录方式菜单中自动选中「扫码登录」：
// 菜单形如（❯ 指向当前项）：
//   ? 选择一种登录方式 ›
//     账号密码
//   ❯ 短信登录
//     扫码登录          ← 目标
//     浏览器登录
// 认准「扫码登录」的行号，计算与 ❯ 行的差值发方向键，最后回车确认。
// 注意不能匹配 ❯ 行自身且必须要求菜单上下文（避免误判后续输出里的普通文字）。
func (sess *loginSession) autoSelectQRLogin(tty pty.Pty) {
	sess.mu.Lock()
	out := sess.output
	sess.mu.Unlock()
	lines := strings.Split(out, "\n")
	menu := -1 // 菜单标题行（"选择一种登录方式"）
	cur, target := -1, -1
	for i, ln := range lines {
		if strings.Contains(ln, "选择一种登录方式") && menu < 0 {
			menu = i
		}
		if menu >= 0 && i > menu {
			if strings.Contains(ln, "❯") && cur < 0 {
				cur = i
			}
			// 目标行必须仍在菜单区间内（菜单下最多 6 项，防越界吃到后续输出）
			if i <= menu+8 && strings.Contains(ln, "扫码登录") && target < 0 {
				target = i
			}
		}
	}
	if target < 0 || cur < 0 || target == cur {
		// 结构识别失败或已选中：直接回车（菜单默认选中短信，识别失败时宁可重开）
		if target != cur {
			_, _ = tty.Write([]byte("\r"))
		}
		return
	}
	keys := ""
	if target > cur {
		keys = strings.Repeat("\x1b[B", target-cur) // 方向下键
	} else {
		keys = strings.Repeat("\x1b[A", cur-target) // 方向上键
	}
	_, _ = tty.Write([]byte(keys))
	time.Sleep(250 * time.Millisecond)
	_, _ = tty.Write([]byte("\r")) // 确认
}

// loginStatus 轮询登录进度：running=扫码中，ok=true 表示 cookie 已落盘
func (s *Server) loginStatus(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	s.loginMu.Lock()
	sess := s.loginSessions[id]
	s.loginMu.Unlock()
	if sess == nil {
		httpError(w, 404, "登录会话不存在（可能已过期，请重新发起）")
		return
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	// qrcode.png 存在且是本次会话写入的 → 前端优先展示 PNG（比终端 ASCII 码更可靠可扫）
	qrPNG := false
	if fi, err := os.Stat(sess.qrPNG); err == nil && fi.ModTime().After(sess.startedAt) {
		qrPNG = true
	}
	writeJSON(w, map[string]any{
		"id":      sess.id,
		"account": sess.account,
		"running": !sess.done,
		"done":    sess.done,
		"ok":      sess.cookieOK,
		"error":   sess.exitErr,
		"output":  sess.output,
		"qr_png":  qrPNG,
	})
}

// loginQRPNG 返回 biliup 生成的二维码图片（前端 <img> 直接展示）
func (s *Server) loginQRPNG(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	s.loginMu.Lock()
	sess := s.loginSessions[id]
	s.loginMu.Unlock()
	if sess == nil {
		httpError(w, 404, "登录会话不存在")
		return
	}
	http.ServeFile(w, r, sess.qrPNG)
}

// ── 直播关注 API ──

func (s *Server) listWatches(w http.ResponseWriter, r *http.Request) {
	ws, err := s.store.ListWatches()
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	if ws == nil {
		ws = []*store.Watch{}
	}
	writeJSON(w, ws)
}

func (s *Server) addWatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, 400, "参数错误: "+err.Error())
		return
	}
	platform, roomID, title, err := detectWatchRoom(r.Context(), req.URL)
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	wt := &store.Watch{
		Platform: platform,
		URL:      req.URL,
		RoomID:   roomID,
		Title:    title,
	}
	id, err := s.store.AddWatch(wt)
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	wt.ID = id
	// 录制检测由独立 daemon 轮询负责（默认周期内即可感知，无需踢一脚）
	writeJSON(w, wt)
}

// detectWatchRoom 识别直播平台并取回房间信息（bilibili / huya）
func detectWatchRoom(ctx context.Context, rawURL string) (platform, roomID, title string, err error) {
	switch {
	case strings.Contains(rawURL, "huya.com"):
		// 数字房间号或短别名都行（www.huya.com/660000 会被虎牙重定向成 /lpl）：
		// 直接拿路径段去抓页面，页面 JSON 回传权威数字房间号
		roomPath := live.HuyaRoomPath(rawURL)
		if roomPath == "" {
			return "", "", "", fmt.Errorf("无法从 %q 识别虎牙房间（形如 https://www.huya.com/660000 或 /lpl）", rawURL)
		}
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		h, err := live.GetHuyaRoomInfo(cctx, roomPath)
		if err != nil {
			return "", "", "", fmt.Errorf("虎牙房间解析失败: %v", err)
		}
		return "huya", h.RoomID, h.Title, nil
	default: // live.bilibili.com
		roomID, err = live.ExtractRoomID(rawURL)
		if err != nil {
			return "", "", "", err
		}
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		info, err := live.GetRoomInfo(cctx, roomID)
		if err != nil {
			return "", "", "", fmt.Errorf("房间号识别成功但信息获取失败: %v", err)
		}
		return "bilibili", info.RoomID, info.Title, nil
	}
}

func (s *Server) deleteWatch(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpError(w, 400, "无效 id")
		return
	}
	// 删关注即停止录制；若录制中由 daemon 兜底（会话下轮 tick 检测到关注已删即停）
	if err := s.store.DeleteWatch(id); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]string{"ok": "true"})
}

// liveDaemon 录制守护进程存活探测（心跳文件距今未超过轮询间隔 3 倍视为运行中）
func (s *Server) liveDaemon(w http.ResponseWriter, r *http.Request) {
	beatPath := filepath.Join(s.cfg.DataDir, "live", "daemon.heartbeat")
	running := false
	lastBeat := int64(0)
	if b, err := os.ReadFile(beatPath); err == nil {
		if v, err2 := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err2 == nil {
			lastBeat = v
			threshold := int64(s.cfg.Live.CheckIntervalSec)*3 + 30
			running = time.Now().Unix()-lastBeat < threshold
		}
	}
	writeJSON(w, map[string]any{"running": running, "last_beat": lastBeat})
}

// liveBarrage 弹幕密度曲线数据（M4.2）：?room_id=6&minutes=60
func (s *Server) liveBarrage(w http.ResponseWriter, r *http.Request) {
	roomID := r.URL.Query().Get("room_id")
	if roomID == "" {
		httpError(w, 400, "缺少 room_id")
		return
	}
	minutes := 120
	if v := r.URL.Query().Get("minutes"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1440 {
			minutes = n
		}
	}
	since := time.Now().Unix() - int64(minutes)*60
	buckets, err := s.store.ListBarrage(roomID, since)
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	if buckets == nil {
		buckets = []store.BarrageBucket{}
	}
	writeJSON(w, map[string]any{"room_id": roomID, "minutes": minutes, "buckets": buckets})
}

// stats 引擎/账号配置状态（控制台状态灯与设置页数据源；只报就绪性，不做运行时探测）
func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg
	engines := map[string]bool{
		"yt-dlp":   cfg.Engines.YtDlp != "",
		"ffmpeg":   cfg.Engines.FFmpeg != "",
		"biliup":   cfg.Engines.Biliup != "",
		"whisper":  cfg.Engines.ASR.Binary != "" && cfg.Engines.ASR.Model != "",
		"deepseek": cfg.LLM.APIKey != "",
		"pushme":   cfg.Notify.Pushme.Enable && cfg.Notify.Pushme.URL != "" && cfg.Notify.Pushme.PushKey != "",
	}
	// 账号登录态：B站=biliup cookie 文件存在；抖音=仅能报"配置过浏览器目录"，真实扫码态发布时验证
	accounts := map[string]bool{
		"bilibili": false,
		"douyin":   false,
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "accounts", "bilibili-default", "cookies.json")); err == nil {
		accounts["bilibili"] = true
	}
	writeJSON(w, map[string]any{"engines": engines, "accounts": accounts})
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	// 控制台内嵌 HTML 每次取新：禁止缓存（页面 JS 迭代频繁，浏览器缓存旧版会造成误判）
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(consoleHTML))
}

type createReq struct {
	Input string `json:"input"`
	Kind  string `json:"kind"`
}

func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, 400, "参数错误: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Input) == "" {
		httpError(w, 400, "input 不能为空")
		return
	}
	kind := req.Kind
	if kind == "" {
		kind = "clips"
	}
	// 发布任务：Input 必须是合法的发布请求 JSON
	if kind == "publish" {
		if _, err := publish.ParseRequest(req.Input); err != nil {
			httpError(w, 400, err.Error())
			return
		}
	}
	// 字幕任务：Input 必须是合法的字幕请求 JSON
	if kind == "subtitle" {
		if _, err := publish.ParseSubtitleRequest(req.Input); err != nil {
			httpError(w, 400, err.Error())
			return
		}
	}
	// 配音任务：Input 必须是合法的配音请求 JSON
	if kind == "dub" {
		if _, err := publish.ParseDubRequest(req.Input); err != nil {
			httpError(w, 400, err.Error())
			return
		}
	}

	job := &core.Job{
		ID:     newJobID(),
		Kind:   kind,
		Status: core.StatusQueued,
		Input:  strings.TrimSpace(req.Input),
	}
	job.Dir = filepath.Join(s.cfg.DataDir, "jobs", job.ID)
	if err := os.MkdirAll(job.Dir, 0o755); err != nil {
		httpError(w, 500, "创建任务目录失败: "+err.Error())
		return
	}
	if err := s.store.CreateJob(job); err != nil {
		httpError(w, 500, "保存任务失败: "+err.Error())
		return
	}
	s.runner.Enqueue(job.ID)
	writeJSON(w, job)
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.store.ListJobs(100)
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	if jobs == nil {
		jobs = []*core.Job{} // 空列表返回 [] 而非 null（前端 jobs.filter 依赖数组）
	}
	writeJSON(w, jobs)
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.store.GetJob(r.PathValue("id"))
	if err != nil {
		httpError(w, 404, err.Error())
		return
	}
	writeJSON(w, job)
}

func (s *Server) deleteJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteJob(id); err != nil {
		httpError(w, 404, err.Error())
		return
	}
	writeJSON(w, map[string]string{"ok": "true"})
}

// events SSE 实时日志/状态
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpError(w, 500, "SSE 不支持")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := s.bus.Subscribe(id)
	defer s.bus.Unsubscribe(id, ch)

	if job, err := s.store.GetJob(id); err == nil {
		b, _ := json.Marshal(map[string]any{"type": "job", "job": job})
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case data := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// serveFile 提供任务产物（视频/封面）预览，防目录穿越
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job")
	rel := filepath.FromSlash(r.PathValue("file"))
	if rel == "" {
		httpError(w, 400, "缺少文件名")
		return
	}
	base := filepath.Join(s.cfg.DataDir, "jobs", jobID)
	full := filepath.Join(base, rel)
	if !strings.HasPrefix(full, base+string(filepath.Separator)) {
		httpError(w, 403, "路径越界")
		return
	}
	if _, err := os.Stat(full); err != nil {
		httpError(w, 404, "文件不存在")
		return
	}
	http.ServeFile(w, r, full)
}

// ---- utils ----

func newJobID() string {
	return fmt.Sprintf("j%d-%04d", time.Now().Unix(), rand.Intn(10000))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	http.Error(w, msg, code)
}

func logRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}
