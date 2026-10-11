package engine

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
)

// YTDLP yt-dlp 下载引擎（子进程）
type YTDLP struct {
	Bin string
}

// domesticHosts 国内站点：直连不走代理（代理挂了也不影响国内源）
var domesticHosts = []string{
	"bilibili.com", "b23.tv", "biliapi.net", "biliapi.com", "hdslb.com", "biligame.com",
	"douyin.com", "iesdouyin.com", "douyu.com", "huya.com", "ixigua.com", "weibo.com", "xiaohongshu.com",
}

func isDomestic(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, d := range domesticHosts {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// Download 下载 URL 到 outDir，返回本地媒体文件路径
// proxy 非空且非国内站点时才走代理；国内平台一律直连
func (y *YTDLP) Download(ctx context.Context, url, outDir, proxy string) (string, error) {
	args := []string{
		"-f", "bv*+ba/b",
		"--merge-output-format", "mp4",
		"--no-playlist",
		"--no-part",
		"-o", filepath.Join(outDir, "video.%(ext)s"),
	}
	if proxy != "" && !isDomestic(url) {
		args = append(args, "--proxy", proxy)
	}
	args = append(args, url)

	cmd := exec.CommandContext(ctx, y.Bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("yt-dlp 下载失败: %v\n%s", err, tail(out, 2000))
	}
	matches, _ := filepath.Glob(filepath.Join(outDir, "video.*"))
	for _, m := range matches {
		switch strings.ToLower(filepath.Ext(m)) {
		case ".mp4", ".mkv", ".webm", ".flv", ".mov", ".m4a":
			return m, nil
		}
	}
	if len(matches) > 0 {
		return matches[0], nil
	}
	return "", errors.New("yt-dlp 执行完成但未找到输出文件")
}
