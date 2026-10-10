package web

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"chopperbot/internal/config"
	"chopperbot/internal/core"
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
	return logRequest(mux)
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
