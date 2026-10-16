package web

import (
	"context"
	_ "embed"
	"encoding/base64"
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

// ── 扫码登录（Go 原生：B站 passport TV 端二维码 API，协议与 biliup-rs 一致）──
// 旧方案（ConPTY 伪终端驱动 biliup login + 终端菜单自动选择）在真机上因输出分帧
// 识别失败而卡死（用户实测"扫不了码"），且二维码要走 ASCII/PNG 双通道极其脆弱。
// 现直接调 B站原生接口：生成（auth_code）+ 轮询（poll），二维码 Go 渲染 PNG 送
// 前端 <img>；落盘的 cookies.json 与 biliup LoginInfo 格式兼容（上传过渡期仍由
// biliup 消费）。零子进程、零伪终端。

// qrSession 一次扫码登录会话（原生 HTTP 轮询）。
// preFillAccount 非空 = 既有账号重新登录（cookie 只回填不建账号）；
// 空 = 新账号流程（登录成功后自动用 B站昵称建账号，用户零输入）。
type qrSession struct {
	id       string
	platform string
	account  string // 会话指定名（重新登录）；空则落账时自动起名
	authCode string
	startedAt time.Time
	mu        sync.Mutex
	state     string // waiting / ok / expired / error
	message   string
	result    string // 落定的账号名（ok 时回填前端展示）
}

// loginQRStart 生成扫码登录二维码（当前仅 bilibili：抖音登录需要有头浏览器，
// 不适合无人值守服务器）。account 可选：传=既有账号重新登录；不传=新账号
// （登录成功后自动用 B站昵称建账号，用户零输入）。
func (s *Server) loginQRStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Platform string `json:"platform"`
		Account  string `json:"account"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Platform == "" {
		req.Platform = "bilibili"
	}
	if req.Platform != "bilibili" {
		httpError(w, 400, "当前仅支持 bilibili 扫码登录（抖音需在装有浏览器的电脑上操作）")
		return
	}
	if req.Account != "" && !accountNameRe.MatchString(req.Account) {
		httpError(w, 400, "账号名仅限中英文数字、下划线、连字符")
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
	// 会话超 5 分钟判过期（B站二维码本身 ~3 分钟有效期）
	if sess.state == "waiting" && time.Since(sess.startedAt) > 5*time.Minute {
		sess.state, sess.message = "expired", "二维码超时，请重新生成"
	}
	if sess.state != "waiting" { // 终态缓存（ok/expired/error 不再打 B站）
		writeJSON(w, map[string]any{"state": sess.state, "message": sess.message, "account": sess.result})
		return
	}
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
	// 新账号入库存档（同名已存在则只刷登录态，不重复建）
	if sess.account == "" {
		if as, lerr := s.store.ListAccounts(); lerr == nil {
			dup := false
			for _, a := range as {
				if a.Platform == "bilibili" && a.Name == name {
					dup = true
					break
				}
			}
			if !dup {
				acc := &store.Account{Platform: "bilibili", Name: name, Priority: 0}
				if _, cerr := s.store.CreateAccount(acc); cerr != nil {
					return name, nil // 账号建档失败不回滚登录态（cookie 已在，功能可用）
				}
			}
		}
	}
	_ = s.store.SetAccountStatus("bilibili", name, "ok")
	return name, nil
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
