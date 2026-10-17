package publish

// 抖音登录页 DOM 侦察（一次性工具，默认 Skip）。
// 用途：确定扫码页二维码元素选择器与登录成功标志，喂给 douyin.yaml。
// 开启：CLIPSTREAM_DYLOGIN_PROBE=1 go test ./internal/publish/ -run TestDouyinLoginProbe -v -timeout 300s
import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"chopperbot/internal/config"
)

func TestDouyinLoginProbe(t *testing.T) {
	if os.Getenv("CLIPSTREAM_DYLOGIN_PROBE") != "1" {
		t.Skip("设置 CLIPSTREAM_DYLOGIN_PROBE=1 开启抖音登录页侦察")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cfg := config.Default()
	cfg.DataDir = os.TempDir() + "/clipstream-dylogin-probe"
	pc, err := LoadPlatformConfig("douyin", "../../configs/platforms")
	if err != nil {
		t.Fatalf("加载 douyin.yaml 失败: %v", err)
	}

	browser, cleanup, err := Launch(ctx, BrowserOptions{
		UserDataDir: cfg.DataDir + "/dy-probe-ud",
		ChromePath:  cfg.Chrome.Path,
		Headless:    true, // 服务器挂机形态：无头
	})
	if err != nil {
		t.Fatalf("启动 Chrome 失败: %v", err)
	}
	defer cleanup()

	page, err := NewStealthPage(browser, 2*time.Minute)
	if err != nil {
		t.Fatalf("创建页面失败: %v", err)
	}
	if err := page.Navigate(pc.UploadURL); err != nil {
		t.Fatalf("打开上传页失败: %v", err)
	}
	_ = page.WaitLoad()
	time.Sleep(5 * time.Second) // 等登录弹层渲染

	u := PageURL(page)
	t.Logf("当前 URL: %s", u)
	t.Logf("登录标记「扫码登录」: %v", HasText(page, "扫码登录"))

	// dump 整页 HTML 相关片段：找二维码候选元素
	html, err := page.HTML()
	if err != nil {
		t.Fatalf("取 HTML 失败: %v", err)
	}
	// 抽取带 qrcode / qr 字样的标签行
	for _, kw := range []string{"qrcode", "qr-code", "QrCode", "canvas", "douyin-qrcode", "web-login"} {
		idx := 0
		found := 0
		for found < 3 {
			i := strings.Index(html[idx:], kw)
			if i < 0 {
				break
			}
			start := idx + i
			// 向前找标签开头
			ls := strings.LastIndex(html[:start], "<")
			if ls < 0 {
				ls = start
			}
			le := start + 200
			if le > len(html) {
				le = len(html)
			}
			frag := html[ls:le]
			frag = strings.ReplaceAll(frag, "\n", " ")
			t.Logf("[%s] …%s…", kw, frag[:min(160, len(frag))])
			idx = start + len(kw)
			found++
		}
	}

	// 截整页存档（人工核对二维码长啥样）
	p := Screenshot(page, cfg.DataDir, "dy-login-probe.jpg")
	t.Logf("整页截图: %s", p)

	// 候选二维码选择器逐个测命中（rod v0.116 用 Shape 拿坐标，此处只需命中与否）
	for _, sel := range []string{
		"img[src*='qrcode']",
		"[class*='qrcode']",
		"[class*='QRCode']",
		"[id*='qrcode']",
		"canvas",
		"[class*='web-login'] img",
		"[class*='login'] img",
	} {
		if el, err := page.Element(sel); err == nil && el != nil {
			t.Logf("命中选择器 %s", sel)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
