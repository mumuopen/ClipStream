package engines

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"chopperbot/internal/config"
)

// TestInstallEndToEnd 用本地假源跑完整安装链：yt-dlp 直落 + ffmpeg zip
// 解压挑选 → 落位（不碰外网；overrideURL 指向 httptest）
func TestInstallEndToEnd(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "ffmpeg-src.zip")
	mkZip(t, zipPath)
	zipBytes, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/yt-dlp.exe":
			// MZ 头：download 现在会校验 exe 文件头（防 CDN 返回 HTML 错误页）
			_, _ = w.Write([]byte("MZ" + "YTDLP-EXE-CONTENT"))
		case "/ffmpeg.zip":
			_, _ = w.Write(zipBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	orig := overrideURL
	overrideURL = func(u string) string {
		switch u {
		case ytdlpURL:
			return srv.URL + "/yt-dlp.exe"
		case ffmpegURL:
			return srv.URL + "/ffmpeg.zip"
		}
		return u
	}
	defer func() { overrideURL = orig }()

	cfg := &config.Config{}
	out, err := Install(context.Background(), cfg, InstallPlan{Names: []string{"yt-dlp", "ffmpeg"}, TargetDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	// 两项都 installed
	if len(out) != 2 {
		t.Fatalf("应 2 项结果，got %v", out)
	}
	byName := map[string]Result{}
	for _, r := range out {
		byName[r.Name] = r
	}
	if byName["yt-dlp"].Status != "installed" {
		t.Fatalf("yt-dlp 应安装成功：%+v", byName["yt-dlp"])
	}
	if byName["ffmpeg"].Status != "installed" {
		t.Fatalf("ffmpeg 应安装成功：%+v", byName["ffmpeg"])
	}
	// 落位文件真实存在且内容正确
	b, err := os.ReadFile(filepath.Join(dir, "yt-dlp.exe"))
	if err != nil || string(b) != "MZYTDLP-EXE-CONTENT" {
		t.Fatalf("yt-dlp.exe 内容异常：err=%v content=%q", err, string(b))
	}
	ff, err := os.ReadFile(filepath.Join(dir, "ffmpeg.exe"))
	if err != nil || string(ff) != "FFMPEG-BIN" {
		t.Fatalf("ffmpeg.exe 内容异常：err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ffprobe.exe")); err != nil {
		t.Fatal("ffprobe.exe 应一并落位")
	}
	// 已存在 → 重装报 skipped 语义（installBinary 返回已存在错误）
	// Install 重跑同一项：应报 failed（已存在跳过提示）——验证幂等不覆盖
	out2, _ := Install(context.Background(), cfg, InstallPlan{Names: []string{"yt-dlp"}, TargetDir: dir})
	if out2[0].Status == "installed" {
		t.Fatal("已存在的引擎不应再次覆盖安装")
	}
}