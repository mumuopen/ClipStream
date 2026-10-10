package publish

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"github.com/go-rod/stealth"
)

// BrowserOptions 浏览器启动参数
type BrowserOptions struct {
	UserDataDir string // 登录态目录（长期复用）
	ChromePath  string // 本机 Chrome，留空自动探测
	Headless    bool
}

// Launch 启动带登录态的 Chrome（rod + stealth 反检测）
func Launch(ctx context.Context, opts BrowserOptions) (b *rod.Browser, cleanup func(), err error) {
	l := launcher.New().
		UserDataDir(opts.UserDataDir).
		Headless(opts.Headless).
		Set("disable-blink-features", "AutomationControlled").
		Leakless(false) // leakless.exe 常被 Windows Defender 误杀导致启动失败；关闭后由 rod 进程句柄兜底回收
	if opts.ChromePath != "" {
		l = l.Bin(opts.ChromePath)
	}
	wsURL, err := l.Context(ctx).Launch()
	if err != nil {
		return nil, nil, fmt.Errorf("启动 Chrome 失败（可在 config.yaml 的 chrome.path 指定本机浏览器）: %w", err)
	}
	b = rod.New().Context(ctx).ControlURL(wsURL)
	if err := b.Connect(); err != nil {
		return nil, nil, fmt.Errorf("连接 Chrome 失败: %w", err)
	}
	cleanup = func() { _ = b.Close() } // rod Close 会一并回收 launcher 拉起的进程
	return b, cleanup, nil
}

// NewStealthPage 创建带反检测脚本的页面，并设默认超时
func NewStealthPage(b *rod.Browser, timeout time.Duration) (*rod.Page, error) {
	page, err := stealth.Page(b)
	if err != nil {
		return nil, err
	}
	return page.Timeout(timeout), nil
}

// ---- 页面操作小工具 ----

// PageURL 取当前 URL
func PageURL(page *rod.Page) string {
	info, err := page.Info()
	if err != nil {
		return ""
	}
	return info.URL
}

// URLContains 轮询等待 URL 命中任一片段（命中返回 true，超时返回 false）
func URLContains(page *rod.Page, parts []string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		u := PageURL(page)
		for _, p := range parts {
			if p != "" && strings.Contains(u, p) {
				return true
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// xpathText 文本转 XPath 字面量（平台检测文本均为中文短句，不含引号）
func xpathText(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "") + "'"
}

// HasText XPath 全文检索：页面是否出现指定文本
func HasText(page *rod.Page, text string) bool {
	if text == "" {
		return false
	}
	_, err := page.ElementX(fmt.Sprintf("//*[contains(text(), %s)]", xpathText(text)))
	return err == nil
}

// WaitTextAppeared 轮询等待文本出现
func WaitTextAppeared(page *rod.Page, text string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if HasText(page, text) {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// Screenshot 截图存档，返回保存路径（失败不致命，返回空）
func Screenshot(page *rod.Page, dir, name string) string {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	path := filepath.Join(dir, name)
	quality := 70
	b, err := page.Screenshot(false, &proto.PageCaptureScreenshot{
		Format:  proto.PageCaptureScreenshotFormatJpeg,
		Quality: &quality,
	})
	if err != nil {
		return ""
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return ""
	}
	return path
}
