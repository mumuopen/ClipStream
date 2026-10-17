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
	"golang.org/x/sys/windows/registry"
)

// BrowserOptions 浏览器启动参数
type BrowserOptions struct {
	UserDataDir string // 登录态目录（长期复用）
	ChromePath  string // 显式指定的浏览器（Chrome 或 Edge 均可），留空自动探测
	Headless    bool
}

// detectBrowser 浏览器二进制定位：显式配置 → 本机 Chrome → Edge——找不到就
// 报错，绝不静默下载（rod 默认行为：Bin 为空时自动下载 ~600MB Chromium 到
// Roaming 目录，挂机完全无法接受；Edge 是 Chromium 内核（CDP 协议一致），
// Windows 自带、服务器标配，优先级紧随 Chrome）。
func detectBrowser(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("chrome.path 指定的浏览器不存在: %s", explicit)
		}
		return explicit, nil
	}
	// 注册表 App Paths（安装版 Chrome / Edge 都会登记）
	for _, name := range []string{"chrome.exe", "msedge.exe"} {
		for _, root := range []registry.Key{registry.LOCAL_MACHINE, registry.CURRENT_USER} {
			k, err := registry.OpenKey(root, `SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\`+name, registry.QUERY_VALUE)
			if err != nil {
				continue
			}
			v, _, err := k.GetStringValue("")
			k.Close()
			if err == nil && v != "" {
				if _, serr := os.Stat(v); serr == nil {
					return v, nil
				}
			}
		}
	}
	// 常见固定路径兜底（绿色版/精简系统）
	for _, p := range []string{
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
	} {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("未找到 Chromium 内核浏览器（Chrome / Edge 均未检出）：Windows 自带 Edge 即可工作，若被卸载请在 config.yaml 的 chrome.path 显式指定浏览器路径（系统不会自动下载浏览器）")
}

// Launch 启动带登录态的浏览器（rod + stealth 反检测；Chrome 或 Edge 均可）
func Launch(ctx context.Context, opts BrowserOptions) (b *rod.Browser, cleanup func(), err error) {
	bin, err := detectBrowser(opts.ChromePath)
	if err != nil {
		return nil, nil, err
	}
	l := launcher.New().
		Bin(bin).
		UserDataDir(opts.UserDataDir).
		Headless(opts.Headless).
		Set("disable-blink-features", "AutomationControlled").
		Leakless(false) // leakless.exe 常被 Windows Defender 误杀导致启动失败；关闭后由 rod 进程句柄兜底回收
	wsURL, err := l.Context(ctx).Launch()
	if err != nil {
		return nil, nil, fmt.Errorf("启动浏览器失败（%s，可在 config.yaml 的 chrome.path 换一个）: %w", filepath.Base(bin), err)
	}
	b = rod.New().Context(ctx).ControlURL(wsURL)
	if err := b.Connect(); err != nil {
		return nil, nil, fmt.Errorf("连接浏览器失败: %w", err)
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
