package web

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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

	"github.com/skip2/go-qrcode"

	"chopperbot/internal/biliapi"
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
	qrSessions   map[string]*qrSession // 扫码登录会话（B站 passport 原生 API）
}

func NewServer(cfg *config.Config, cfgPath string, st *store.Store, r *pipeline.Runner, bus *core.Bus) *Server {
	return &Server{cfg: cfg, cfgPath: cfgPath, store: st, runner: r, bus: bus, qrSessions: map[string]*qrSession{}}
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
	mux.HandleFunc("PUT /api/live/watches/{id}/bind", s.bindWatchAccount)
	mux.HandleFunc("GET /api/live/daemon", s.liveDaemon)
	mux.HandleFunc("GET /api/live/barrage", s.liveBarrage)
	// 发布中心：账号 + 待发池（发布节奏由 config 窗口+两闸接管）
	mux.HandleFunc("GET /api/accounts", s.listAccounts)
	mux.HandleFunc("POST /api/accounts", s.createAccount)
	mux.HandleFunc("DELETE /api/accounts/{id}", s.deleteAccount)
	mux.HandleFunc("POST /api/accounts/{id}/resume_publish", s.resumePublish)
	mux.HandleFunc("GET /api/pool", s.listPool)
	mux.HandleFunc("POST /api/pool", s.addPool)
	mux.HandleFunc("DELETE /api/pool/{id}", s.deletePool)
	// 维护：一键清理历史任务（预览与执行分开，UI 确认弹窗用预览数字）
	mux.HandleFunc("GET /api/maintenance/purge_preview", s.purgePreview)
	mux.HandleFunc("POST /api/maintenance/purge", s.purgeExecute)
	// 全部配置后台化：运营参数 + LLM/引擎/发布段免改文件
	mux.HandleFunc("GET /api/config", s.getConfig)
	mux.HandleFunc("PUT /api/config", s.updateConfig)
	// 扫码登录：Go 原生 B站 passport 二维码（生成 + 轮询）
	mux.HandleFunc("POST /api/login/qr/start", s.loginQRStart)
	mux.HandleFunc("GET /api/login/qr/poll", s.loginQRPoll)
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
		"publish": map[string]any{"headless": s.cfg.Publish.Headless, "screenshots": s.cfg.Publish.Screenshots,
			"active_hours": s.cfg.Publish.ActiveHours,
			"rate_limit": map[string]any{
				"min_interval_min":    s.cfg.Publish.RateLimit.MinIntervalMin,
				"daily_max":           s.cfg.Publish.RateLimit.DailyMax,
				"max_inflight":        s.cfg.Publish.RateLimit.MaxInflight,
				"risk_cooldown_hours": s.cfg.Publish.RateLimit.RiskCooldownHours,
			}},
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
			Headless     *bool  `json:"headless"`
			Screenshots  *bool  `json:"screenshots"`
			ActiveHours  *string `json:"active_hours"`
			RateLimit    *config.RateLimit `json:"rate_limit"`
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
		// 池背压双水位：high=0 关闭；启用时 low 必须 < high（滞回区间才有意义）
		if lv.PoolHigh < 0 || lv.PoolLow < 0 || (lv.PoolHigh > 0 && lv.PoolLow >= lv.PoolHigh) {
			httpError(w, 400, "池上限需 0（关闭）或正数，且解除水位须低于触发水位")
			return
		}
		// 预告态关键词：表单没有输入框，客户端不传时为 nil——保留原值，
		// 否则每保存一次都会把 daemon 的预告过滤整段清空（赛事房白天轮播的
		// 预告画面会被当真开播录整场）。空切片是非 nil，表示用户主动清空。
		if lv.PreviewKeywords == nil {
			lv.PreviewKeywords = s.cfg.Live.PreviewKeywords
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
		if rt.MaxUsagePercent < 0 || rt.MaxUsagePercent > 95 || rt.PublishedKeepDays < 0 || rt.PublishedKeepDays > 90 {
			httpError(w, 400, "占用上限 0-95%（0=关），成品保留 0-90 天（0=不清）")
			return
		}
		s.cfg.Retention = *rt
	}
	if req.Notify != nil && req.Notify.Pushme != nil {
		s.cfg.Notify.Pushme = *req.Notify.Pushme
	}
	if req.LLM != nil {
		l := req.LLM
		// LLM 是可选的（未配置时高光退化为等分切片，设置页对此有明示）。因此
		// 只拒绝"半配置"（填了地址没填模型或反之）——旧版把两个都为空也当错误，
		// 导致全新部署连"改池上限"这种无关操作都保存不了。
		// 2026-10-28 修。
		if (l.BaseURL == "") != (l.Model == "") {
			httpError(w, 400, "llm.base_url 与 llm.model 需同时填写（都留空=不使用 LLM，高光退化为等分切片）")
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
		if req.Publish.RateLimit != nil {
			rl := req.Publish.RateLimit
			// 三闸均为 0 = 不限制；负数或离谱值拒绝（防误填把自己锁死）
			if rl.MinIntervalMin < 0 || rl.DailyMax < 0 || rl.MaxInflight < 0 ||
				rl.MinIntervalMin > 1440 || rl.DailyMax > 200 || rl.MaxInflight > 50 {
				httpError(w, 400, "节流参数需在 0-1440（间隔分钟）/ 0-200（每日）/ 0-50（同时在途）内，0=不限制")
				return
			}
			if rl.RiskCooldownHours < 0 || rl.RiskCooldownHours > 168 {
				httpError(w, 400, "风控冷却时长需在 0-168 小时（0=用默认 24，连续触发翻倍封顶 7 天）")
				return
			}
			s.cfg.Publish.RateLimit = *rl
		}
		// 发布窗口：格式校验交 ParseActiveHours（支持跨夜如 22:00-06:00；空=全天）
		if req.Publish.ActiveHours != nil {
			if _, _, ok := config.ParseActiveHours(*req.Publish.ActiveHours); !ok {
				httpError(w, 400, "发布窗口格式应为 HH:MM-HH:MM（如 10:00-23:00，支持跨夜），留空=全天可发")
				return
			}
			s.cfg.Publish.ActiveHours = *req.Publish.ActiveHours
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

// accountCookieOK 登录态以磁盘为准：B站=cookies.json 在；抖音=user-data-dir
// 登录态目录存在（真实失效由发布时页面级检测并标 login_expired）
func (s *Server) accountCookieOK(platform, name string) bool {
	dir := publish.AccountDir(s.cfg.DataDir, platform, name)
	if platform == "douyin" {
		fi, err := os.Stat(dir)
		return err == nil && fi.IsDir() && !strings.Contains(filepath.Base(dir), "pending-")
	}
	_, err := os.Stat(filepath.Join(dir, "cookies.json"))
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

// resumePublish 手动恢复风控冷却：清冷却终点，保留触发计数（再触发冷却
// 时长继续翻倍——手动恢复也是风险操作，不重置惩罚梯度）
func (s *Server) resumePublish(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	acc, err := s.store.AccountByID(id)
	if err != nil {
		httpError(w, 404, err.Error())
		return
	}
	if acc.RiskPausedUntil <= time.Now().Unix() {
		httpError(w, 400, "该账号不在风控冷却中")
		return
	}
	if err := s.store.ClearAccountRiskPause(acc.Name); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true,
		"note": fmt.Sprintf("账号「%s」已提前恢复发布（触发计数保留，再触发冷却继续翻倍）", acc.Name)})
}

// ── 维护：一键清理历史（2026-10-25：风控事故复盘产物——服务器上积了 125 条失败
// 发布任务与 40 条已退池条目，手动一条条删不现实）──

// purgeStats 清理统计（预览与执行共用结构）
type purgeStats struct {
	PoolItems int      `json:"pool_items"` // 清掉的已完结池条目（dispatched+failed）
	Pending   int      `json:"pending"`    // 待发池现存条目（include_pending=true 时一并清空）
	Jobs      int      `json:"jobs"`       // 清掉的历史任务数
	Bytes     int64    `json:"bytes"`      // 释放磁盘字节数
	Errors    []string `json:"errors,omitempty"`
}

func (s *Server) purgePreview(w http.ResponseWriter, r *http.Request) {
	pool, err := s.store.CountFinishedPoolItems()
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	pending, err := s.store.CountPendingPoolItems()
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	jobs, err := s.store.PurgeableJobs()
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	var bytes int64
	for _, j := range jobs {
		if dir, ok := s.safeJobDir(j.Dir); ok {
			bytes += dirSize(dir)
		}
	}
	writeJSON(w, purgeStats{PoolItems: pool, Pending: pending, Jobs: len(jobs), Bytes: bytes})
}

// purgeExecute 执行清理：①删池条目 ②删无活跃引用的终态任务（记录+产物目录）。
// include_pending=true 时连待发池一起清空——风控冷却期池子发不出去、pending 占着
// 背压水位（≥pool_high 一直暂停录制）形成死锁，老积压重发价值低，用户拍板可清空重来。
// 待发素材的目录保护由 PurgeableJobs 的两道闸兜底（池引用 + 非终态任务引用），
// 清空池后仍有在途任务引用的源目录不会被删
func (s *Server) purgeExecute(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IncludePending bool `json:"include_pending"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req) // 空/坏 body=只清已完结（UI 未勾选时不发 body）
	}
	st := purgeStats{}
	var pool int64
	var err error
	if req.IncludePending {
		pool, err = s.store.DeleteAllPoolItems()
	} else {
		pool, err = s.store.DeleteFinishedPoolItems()
	}
	if err != nil {
		httpError(w, 500, "清理池条目失败: "+err.Error())
		return
	}
	st.PoolItems = int(pool)
	targets, err := s.store.PurgeableJobs()
	if err != nil {
		httpError(w, 500, "列出可清任务失败: "+err.Error())
		return
	}
	for _, j := range targets {
		dir, ok := s.safeJobDir(j.Dir)
		if !ok {
			st.Errors = append(st.Errors, fmt.Sprintf("跳过 %s：任务目录不在数据目录内（%q），疑似异常数据", j.ID, j.Dir))
			continue
		}
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			freed := dirSize(dir)
			if err := os.RemoveAll(dir); err != nil {
				st.Errors = append(st.Errors, fmt.Sprintf("%s 目录删除失败: %v", j.ID, err))
				continue
			}
			st.Bytes += freed
		}
		if err := s.store.DeleteJobRecord(j.ID); err != nil {
			st.Errors = append(st.Errors, fmt.Sprintf("%s 记录删除失败: %v", j.ID, err))
			continue
		}
		st.Jobs++
	}
	writeJSON(w, st)
}

// safeJobDir 任务目录安全校验：解析为绝对路径后必须在 <DataDir>/jobs 之下
// （jobs 表 dir 列理论上是 newJob 写入的规范路径，但删除操作必须有这层防线）
func (s *Server) safeJobDir(dir string) (string, bool) {
	if dir == "" {
		return "", false
	}
	root, err := filepath.Abs(filepath.Join(s.cfg.DataDir, "jobs"))
	if err != nil {
		return "", false
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}
	if abs == root || !strings.HasPrefix(abs, root+string(os.PathSeparator)) {
		return "", false
	}
	return abs, true
}

// dirSize 目录总字节数（不存在返回 0）
func dirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				total += fi.Size()
			}
		}
		return nil
	})
	return total
}

// ── 内容池 API（M5：审核通过入池，发布泵窗口内派发；定时计划已随
// “发布窗口+泵”重构退役，schedules 数据表与 API 一并移除）──

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
	// 来源直播间：从源任务 video 路径解析（直播↔账号绑定路由；非直播源为空）。
	// 前端不感知，服务端补齐——自动入池（autoreview）与手工入池口径一致
	if src, err := s.store.GetJob(it.SourceJob); err == nil && it.RoomID == "" {
		it.RoomID = pipeline.RoomIDFromPath(src.ParamsMap()["video"])
	}
	id, err := s.store.AddPoolItem(&it)
	if err != nil {
		if errors.Is(err, store.ErrAlreadyInPool) {
			httpError(w, 409, err.Error())
			return
		}
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

// ── 扫码登录（Go 原生：B站 passport TV 端二维码 API，协议与 biliup-rs 一致）──
// 旧方案（ConPTY 伪终端驱动 biliup login + 终端菜单自动选择）在真机上因输出分帧
// 识别失败而卡死（用户实测"扫不了码"），且二维码要走 ASCII/PNG 双通道极其脆弱。
// 现直接调 B站原生接口：生成（auth_code）+ 轮询（poll），二维码 Go 渲染 PNG 送
// 前端 <img>；落盘的 cookies.json 与 biliup LoginInfo 格式兼容（上传过渡期仍由
// biliup 消费）。零子进程、零伪终端。

// qrSession 一次扫码登录会话。
// bilibili：原生 HTTP 轮询（auth_code + poll）；douyin：rod headless 驱动
// 登录页（dy 非空时）。account 语义：空 = 新账号（登录成功后自动用平台
// 昵称建账号）；非空 = 指定落账名——弹窗让用户输入的账号名（扫码成功即
// 以此名建档），或既有账号重新登录（保持原名刷新登录态）。
type qrSession struct {
	id       string
	platform string
	account  string // 会话指定名（用户输入的新账号名/重新登录）；空则落账时自动起名
	authCode string // bilibili 轮询凭证
	dy       *publish.DouyinLoginSession // 抖音 rod 会话（bilibili 为 nil）
	startedAt time.Time
	mu        sync.Mutex
	state     string // booting / waiting / ok / expired / error
	message   string
	result    string // 落定的账号名（ok 时回填前端展示）
}

// sanitizeAccountName 平台昵称 → 合法账号名（目录名安全 + 长度限 32 rune）
func sanitizeAccountName(nick string) string {
	name := accountNameRe.ReplaceAllString(nick, "")
	if len([]rune(name)) > 32 {
		name = string([]rune(name)[:32])
	}
	return name
}

// loginQRStart 生成扫码登录二维码。account 可选：传=既有账号重新登录；不传
// =新账号（登录成功后自动用平台昵称建账号，用户零输入）。
// bilibili：原生 passport API（快，同步返回二维码）；douyin：rod headless 后台
// 起浏览器（~10-30s），先返回 booting 态，二维码由 poll 阶段送达。
func (s *Server) loginQRStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Platform string `json:"platform"`
		Account  string `json:"account"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Platform == "" {
		req.Platform = "bilibili"
	}
	if req.Platform != "bilibili" && req.Platform != "douyin" {
		httpError(w, 400, "不支持的平台（支持 bilibili / douyin）")
		return
	}
	if req.Account != "" && !accountNameRe.MatchString(req.Account) {
		httpError(w, 400, "账号名仅限中英文数字、下划线、连字符")
		return
	}

	if req.Platform == "douyin" {
		sess := &qrSession{
			id:        fmt.Sprintf("qr-%d-%s", time.Now().UnixNano(), strconv.Itoa(rand.Intn(1000))),
			platform:  req.Platform,
			account:   req.Account,
			startedAt: time.Now(),
			state:     "booting",
			message:   "正在启动浏览器…",
		}
		s.loginMu.Lock()
		s.qrSessions[sess.id] = sess
		s.loginMu.Unlock()
		go func() {
			pc, err := publish.LoadPlatformConfig("douyin", s.cfg.Publish.PlatformsDir)
			if err != nil {
				log.Printf("[douyin-login] 配置加载失败: %v", err)
				sess.mu.Lock()
				sess.state, sess.message = "error", "加载抖音配置失败: "+err.Error()
				sess.mu.Unlock()
				return
			}
			dy, err := publish.StartDouyinLogin(context.Background(), s.cfg, pc, req.Account)
			if err != nil {
				log.Printf("[douyin-login] 浏览器启动失败: %v", err)
				sess.mu.Lock()
				sess.state, sess.message = "error", err.Error()
				sess.mu.Unlock()
				return
			}
			sess.mu.Lock()
			sess.dy = dy
			if sess.state == "booting" {
				sess.state, sess.message = "waiting", "等待扫码确认"
			}
			sess.mu.Unlock()
		}()
		// 兜底自回收：用户关闭弹窗（前端停 poll）后无人触发超时，浏览器进程
		// 会泄漏——8 分钟后无条件收尾（幂等：终态/已 Close 的会话不受影响）
		time.AfterFunc(8*time.Minute, func() {
			sess.mu.Lock()
			defer sess.mu.Unlock()
			if sess.state == "waiting" || sess.state == "booting" {
				sess.state, sess.message = "expired", "二维码超时，请重新生成"
			}
			if sess.dy != nil {
				sess.dy.Close()
				sess.dy = nil
			}
		})
		writeJSON(w, map[string]any{"id": sess.id, "account": req.Account, "qr": ""})
		return
	}

	authCode, qrURL, err := biliapi.QRGenerate()
	if err != nil {
		httpError(w, 502, "B站二维码生成失败: "+err.Error())
		return
	}
	// Go 渲染 PNG（512px，Medium 容错足够扫码），data URL 直接嵌前端 <img>
	png, err := qrcode.Encode(qrURL, qrcode.Medium, 512)
	if err != nil {
		httpError(w, 500, "二维码渲染失败: "+err.Error())
		return
	}
	qrDataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)

	sess := &qrSession{
		id:        fmt.Sprintf("qr-%d-%s", time.Now().UnixNano(), strconv.Itoa(rand.Intn(1000))),
		platform:  req.Platform,
		account:   req.Account,
		authCode:  authCode,
		startedAt: time.Now(),
		state:     "waiting",
	}
	s.loginMu.Lock()
	s.qrSessions[sess.id] = sess
	// 清理：超 20 个或超 1 小时的旧会话
	if len(s.qrSessions) > 20 {
		for k, v := range s.qrSessions {
			if time.Since(v.startedAt) > time.Hour {
				delete(s.qrSessions, k)
			}
		}
	}
	s.loginMu.Unlock()

	writeJSON(w, map[string]any{"id": sess.id, "account": req.Account, "qr": qrDataURL})
}

// loginQRPoll 前端轮询扫码状态：state=waiting/ok/expired/error。
// ok 时服务端把登录信息写入账号目录 cookies.json（biliup LoginInfo 兼容格式），
// 新账号流程自动以 B站昵称建账号（用户零输入）；既有账号流程只回填登录态。
func (s *Server) loginQRPoll(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	s.loginMu.Lock()
	sess := s.qrSessions[id]
	s.loginMu.Unlock()
	if sess == nil {
		httpError(w, 404, "登录会话不存在（可能已过期，请重新发起）")
		return
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	// 会话超 8 分钟判过期（B站二维码 ~3 分钟；抖音浏览器慢启动 + 扫码窗口放宽）
	if (sess.state == "waiting" || sess.state == "booting") && time.Since(sess.startedAt) > 8*time.Minute {
		sess.state, sess.message = "expired", "二维码超时，请重新生成"
		if sess.dy != nil {
			sess.dy.Close() // 终态释放浏览器
			sess.dy = nil
		}
	}
	if sess.state != "waiting" && sess.state != "booting" { // 终态缓存，不再外呼
		writeJSON(w, map[string]any{"state": sess.state, "message": sess.message, "account": sess.result})
		return
	}

	// ── 抖音（rod 会话）──
	if sess.platform == "douyin" {
		if sess.dy == nil { // 浏览器仍在后台启动（启动失败已置 error，不会到这）
			writeJSON(w, map[string]any{"state": sess.state, "message": sess.message})
			return
		}
		if !sess.dy.Ready() {
			sess.state, sess.message = "error", "浏览器会话异常退出（Chrome 崩溃/被系统回收），请重试"
			sess.dy.Close()
			sess.dy = nil
			writeJSON(w, map[string]any{"state": sess.state, "message": sess.message})
			return
		}
		if !sess.dy.LoggedIn() {
			writeJSON(w, map[string]any{"state": "waiting", "message": "等待扫码确认", "qr": sess.dy.LoginQR()})
			return
		}
		nick := sess.dy.Nickname()
		name := sess.account
		if name == "" {
			// 新账号：关闭浏览器（释放目录锁）→ pending 目录 rename 为昵称目录
			sess.dy.Close()
			_, name, _ = sess.dy.Settle(nick, sanitizeAccountName)
		} else {
			sess.dy.Close()
		}
		sess.dy = nil
		if name == "" {
			name = "douyin_" + time.Now().Format("0102-1504")
		}
		s.upsertAccount("douyin", name)
		// 重新登录成功必须把 status 拉回 ok（2026-10-28 补）：upsertAccount 对已存在
		// 账号直接 return，不改状态，而 routeAccount 又过滤 status!=ok——抖音账号一旦
		// 被判 login_expired，重扫多少次也发不出去，UI 却显示"登录成功"。
		// （bilibili 分支早已有对称的一行，抖音侧遗漏。）
		_ = s.store.SetAccountStatus("douyin", name, "ok")
		sess.result = name
		sess.state, sess.message = "ok", "登录成功："+name
		writeJSON(w, map[string]any{"state": sess.state, "message": sess.message, "account": sess.result})
		return
	}

	// ── B站（原生 passport API）──
	st, err := biliapi.QRPoll(sess.authCode)
	if err != nil {
		sess.state, sess.message = "error", err.Error()
		writeJSON(w, map[string]any{"state": sess.state, "message": sess.message})
		return
	}
	switch {
	case st.OK:
		name, aerr := s.settleBiliLogin(sess, &st.Info)
		if aerr != nil {
			sess.state, sess.message = "error", aerr.Error()
			break
		}
		sess.result = name
		sess.state, sess.message = "ok", "登录成功："+name
	case st.Expired:
		sess.state, sess.message = "expired", st.Message
	default: // Waiting：保持 waiting，继续轮
		sess.message = st.Message
	}
	writeJSON(w, map[string]any{"state": sess.state, "message": sess.message, "account": sess.result})
}

// settleBiliLogin 落账：写 cookies.json（biliup LoginInfo 兼容），并按会话类型
// 建账号（新账号用 B站昵称自动命名，昵称不可用时 fallback mid）或回填既有账号。
// 返回最终账号名。
func (s *Server) settleBiliLogin(sess *qrSession, info *biliapi.TVLoginInfo) (string, error) {
	name := sess.account // 既有账号重新登录：保持原名
	if name == "" {
		// 新账号：B站昵称自动命名（清掉目录不安全字符；空则用 mid）
		nick := biliapi.Nickname(*info)
		nick = accountNameRe.ReplaceAllString(nick, "")
		if len([]rune(nick)) > 32 {
			nick = string([]rune(nick)[:32])
		}
		if nick == "" {
			nick = fmt.Sprintf("bili_%d", info.TokenInfo.MID)
		}
		name = nick
	}
	dir := publish.AccountDir(s.cfg.DataDir, "bilibili", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("账号目录创建失败: %w", err)
	}
	b, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "cookies.json"), b, 0o644); err != nil {
		return "", fmt.Errorf("cookie 写盘失败: %w", err)
	}
	// 账号入库（幂等：同名平台账号已存在则跳过）。三种路径统一走这：
	// 新账号（昵称自动命名）、用户指定名的新账号（弹窗输入，扫码即落此名）、
	// 既有账号重新登录（已存在则只刷登录态）。旧版只在 sess.account=="" 时
	// 建档——用户指定名的新账号会有 cookies 目录但无 DB 行，发布中心看不到
	s.upsertAccount("bilibili", name)
	_ = s.store.SetAccountStatus("bilibili", name, "ok")
	return name, nil
}

// upsertAccount 新账号入库存档（同名平台账号已存在则跳过，不重复建）
func (s *Server) upsertAccount(platform, name string) {
	if as, lerr := s.store.ListAccounts(); lerr == nil {
		for _, a := range as {
			if a.Platform == platform && a.Name == name {
				return
			}
		}
	}
	acc := &store.Account{Platform: platform, Name: name, Priority: 0}
	_, _ = s.store.CreateAccount(acc)
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
		URL         string `json:"url"`
		BindAccount string `json:"bind_account"` // 可选：本房间切片优先发到的账号
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, 400, "参数错误: "+err.Error())
		return
	}
	if req.BindAccount != "" && !s.accountExists(req.BindAccount) {
		httpError(w, 400, "绑定账号不存在："+req.BindAccount)
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
		BindAccount: req.BindAccount,
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

// accountExists 账号名存在性检查（直播绑定校验用；跨平台任一命中即真）
func (s *Server) accountExists(name string) bool {
	as, err := s.store.ListAccounts()
	if err != nil {
		return false
	}
	for _, a := range as {
		if a.Name == name {
			return true
		}
	}
	return false
}

// bindWatchAccount 改绑直播间的发布账号（直播↔账号绑定：矩阵号人设垂直，
// 本房间切片优先发到该账号；空串=解绑，回退化签路由）
func (s *Server) bindWatchAccount(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpError(w, 400, "无效 id")
		return
	}
	var req struct {
		Account string `json:"account"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, 400, "参数错误: "+err.Error())
		return
	}
	if req.Account != "" && !s.accountExists(req.Account) {
		httpError(w, 400, "绑定账号不存在："+req.Account)
		return
	}
	if err := s.store.SetWatchBind(id, req.Account); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "bind_account": req.Account})
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
	// 账号登录态：以 store 真账号列表为准（此前硬编码只查 bilibili-default——
	// 用户扫码建的账号如 bili_xxx 在设置页永远显示"待扫码"，与发布中心的
	// "正常"自相矛盾）。登录态语义同 /api/accounts 的 accountCookieOK：
	// B站=cookies.json 在；抖音=登录态目录在。accounts 平台级汇总保留向后
	// 兼容，account_list 供设置页逐账号渲染。
	accounts := map[string]bool{
		"bilibili": false,
		"douyin":   false,
	}
	accountList := []map[string]any{}
	if as, err := s.store.ListAccounts(); err == nil {
		for _, a := range as {
			ok := s.accountCookieOK(a.Platform, a.Name)
			accountList = append(accountList, map[string]any{
				"platform":  a.Platform,
				"name":      a.Name,
				"status":    a.Status,
				"logged_in": ok,
			})
			if _, known := accounts[a.Platform]; known && ok && a.Status != "login_expired" {
				accounts[a.Platform] = true
			}
		}
	}
	writeJSON(w, map[string]any{"engines": engines, "accounts": accounts, "account_list": accountList})
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
	// 发布任务：Input 必须是合法的发布请求 JSON；非演练走三闸（防手点连发
	// 复现批量风控——手动路径旧版零限制，是 2026-10-17 事故的手动复现面；
	// dry-run 不真发不占闸；强发出口=设置页把间隔/每日上限临时调 0）。
	// 同账号互斥（S6）：与调度/泵的派发并发时"查闸+建任务"必须原子——
	// 锁内只做判定与落库，HTTP 响应统一在锁外写
	if kind == "publish" {
		preq, err := publish.ParseRequest(req.Input)
		if err != nil {
			httpError(w, 400, err.Error())
			return
		}
		if !preq.DryRun && preq.Account != "" {
			var throttleMsg, buildErr string
			var job *core.Job
			s.store.WithAccountLock(preq.Account, func() {
				if why := throttleReason(s.store, s.cfg.Publish.RateLimit, preq.Account); why != "" {
					throttleMsg = why
					return
				}
				j, berr := s.buildJob(kind, strings.TrimSpace(req.Input))
				if berr != nil {
					buildErr = berr.Error()
					return
				}
				s.runner.Enqueue(j.ID)
				job = j
			})
			if throttleMsg != "" {
				httpError(w, 429, throttleMsg+"（防风控限流：手动发布同样受限；如需救急可在设置页临时调大间隔/上限参数）")
				return
			}
			if buildErr != "" {
				httpError(w, 500, buildErr)
				return
			}
			writeJSON(w, job)
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

	job, err := s.buildJob(kind, strings.TrimSpace(req.Input))
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	s.runner.Enqueue(job.ID)
	writeJSON(w, job)
}

// buildJob 建任务并落库（磁盘目录 + DB 行），不入队不发响应——供 createJob
// 主路径与账号锁临界区共用（锁内禁止写 HTTP 响应：http.ResponseWriter 并
// 闭包路径重复写会二次 SetHeader 崩溃）
func (s *Server) buildJob(kind, input string) (*core.Job, error) {
	job := &core.Job{
		ID:     newJobID(),
		Kind:   kind,
		Status: core.StatusQueued,
		Input:  input,
	}
	job.Dir = filepath.Join(s.cfg.DataDir, "jobs", job.ID)
	if err := os.MkdirAll(job.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建任务目录失败: %w", err)
	}
	if err := s.store.CreateJob(job); err != nil {
		return nil, fmt.Errorf("保存任务失败: %w", err)
	}
	return job, nil
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
	// 池联动：只在任务「未产生投稿结果」时把池条目回 pending——否则孤儿
	// dispatched 永远卡住（全库另一条回池路径是 finalize，任务没了就再也不会跑）。
	//
	// 判据必须是任务状态，不能只看 kind（2026-10-28 修正）：旧实现只要
	// kind==publish 就无条件回池，于是「删一条已经发布成功的任务」= 把同一视频
	// 再发一遍。更隐蔽的是成功发布后 TrimPoolItemPlatforms 把 platforms 写成空串，
	// 而发布泵把空串当 bilibili——重发只打 B 站，从抖音侧根本看不出来。
	// 已 done/failed 的任务一律不动池条目：finalize 早已善后完毕（成功=dispatched
	// 完结，失败=已回池或已退池），删任务不应重置这个结论。
	if job, err := s.store.GetJob(id); err == nil && job.Kind == "publish" {
		switch job.Status {
		case core.StatusQueued, core.StatusRunning, core.StatusCanceled:
			if req, perr := publish.ParseRequest(job.Input); perr == nil && req.PoolItemID > 0 {
				_ = s.store.SetPoolItemStatus(req.PoolItemID, "pending")
			}
		}
	}
	if err := s.store.DeleteJob(id); err != nil {
		httpError(w, 404, err.Error())
		return
	}
	writeJSON(w, map[string]string{"ok": "true"})
}

// throttleReason 手动发布四闸判定（与调度侧同语义；main.go 的 publishThrottled
// 在 cmd 包，这里复用同套 SQL 语义）。闸序与调度侧一致：风控冷却 → 在途 → 间隔
// → 每日上限（同为 AND 判定，顺序只影响多闸同时命中时透出的第一条原因）。
//
// ⓪ 风控冷却闸（2026-10-28 补）：0.6.1 的熔断只在泵侧 publishThrottled 生效，
// 手动路径漏了这道闸——注释写着"同语义"而实际少一闸，冷却期内从审核台点一次
// 发布就能继续撞 137022，并让 risk_pause_count +1 把冷却 24h 翻成 48h。用户最常
// 走的就是手动路径，漏这一闸等于熔断形同虚设。
func throttleReason(st *store.Store, rl config.RateLimit, accName string) string {
	now := time.Now()
	if until, count := st.AccountRiskPaused(accName); until > now.Unix() {
		left := time.Until(time.Unix(until, 0)).Round(time.Minute)
		return fmt.Sprintf("该账号风控冷却中（平台限流熔断，剩余 %s，第 %d 次触发；可在发布中心手动恢复）", left, count)
	}
	if rl.MaxInflight > 0 {
		if n, err := st.CountRunningPublish(accName); err == nil && n >= rl.MaxInflight {
			return fmt.Sprintf("该账号同时在途任务已达上限（%d/%d 条），等上传完成后再发", n, rl.MaxInflight)
		}
	}
	if rl.MinIntervalMin > 0 {
		if last, err := st.LastPublishAt(accName); err == nil && last > 0 {
			if wait := time.Duration(rl.MinIntervalMin)*time.Minute - now.Sub(time.Unix(last, 0)); wait > 0 {
				minutes := int(wait.Minutes()) + 1
				return fmt.Sprintf("距上次发布间隔不足（还需约 %d 分钟）", minutes)
			}
		}
	}
	if rl.DailyMax > 0 {
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
		if n, err := st.CountPublishSince(accName, today); err == nil && n >= rl.DailyMax {
			return fmt.Sprintf("该账号今日已达上限（%d/%d 条）", n, rl.DailyMax)
		}
	}
	return ""
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
