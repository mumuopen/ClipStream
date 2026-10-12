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
	"strconv"
	"strings"
	"time"

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
	cfg    *config.Config
	store  *store.Store
	runner *pipeline.Runner
	bus    *core.Bus
}

func NewServer(cfg *config.Config, st *store.Store, r *pipeline.Runner, bus *core.Bus) *Server {
	return &Server{cfg: cfg, store: st, runner: r, bus: bus}
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
	// M4 直播关注（录制由独立 daemon 进程负责，serve 只读状态）
	mux.HandleFunc("GET /api/live/watches", s.listWatches)
	mux.HandleFunc("POST /api/live/watches", s.addWatch)
	mux.HandleFunc("DELETE /api/live/watches/{id}", s.deleteWatch)
	mux.HandleFunc("GET /api/live/daemon", s.liveDaemon)
	mux.HandleFunc("GET /api/live/barrage", s.liveBarrage)
	// M5 矩阵：多账号 + 发布调度（占位骨架）
	mux.HandleFunc("GET /api/accounts", s.listAccounts)
	mux.HandleFunc("POST /api/accounts", s.createAccount)
	mux.HandleFunc("DELETE /api/accounts/{id}", s.deleteAccount)
	mux.HandleFunc("GET /api/schedules", s.listSchedules)
	mux.HandleFunc("POST /api/schedules", s.createSchedule)
	mux.HandleFunc("DELETE /api/schedules/{id}", s.deleteSchedule)
	return logRequest(mux)
}

// ── M5 矩阵账号 / 发布调度 API（占位骨架）──

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	as, err := s.store.ListAccounts()
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, as)
}

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
	id, err := s.store.CreateAccount(&a)
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	a.ID = id
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
	writeJSON(w, scs)
}

func (s *Server) createSchedule(w http.ResponseWriter, r *http.Request) {
	var sc store.Schedule
	if err := json.NewDecoder(r.Body).Decode(&sc); err != nil {
		httpError(w, 400, "参数错误: "+err.Error())
		return
	}
	if sc.Name == "" || sc.Time == "" {
		httpError(w, 400, "name 与 time(HH:MM) 必填")
		return
	}
	id, err := s.store.CreateSchedule(&sc)
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	sc.ID = id
	writeJSON(w, sc)
}

func (s *Server) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.store.DeleteSchedule(id); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]string{"ok": "true"})
}

// ── 直播关注 API ──

func (s *Server) listWatches(w http.ResponseWriter, r *http.Request) {
	ws, err := s.store.ListWatches()
	if err != nil {
		httpError(w, 500, err.Error())
		return
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
		roomID, err = live.ExtractHuyaRoomID(rawURL)
		if err != nil {
			return "", "", "", err
		}
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		h, err := live.GetHuyaRoomInfo(cctx, roomID)
		if err != nil {
			return "", "", "", fmt.Errorf("房间号识别成功但信息获取失败: %v", err)
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
