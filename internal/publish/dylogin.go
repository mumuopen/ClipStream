package publish

// Web 扫码登录会话：rod headless 驱动抖音创作者中心登录页。
// 与 B站（biliapi 原生 HTTP）不同，抖音无公开登录/上传 API（Web 接口有
// X-Bogus 签名加密 + 强风控），浏览器自动化即业界标准做法，复用发布链路的
// stealth 启动与 user-data-dir 登录态（发布时同一目录免重登）。
import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-rod/rod"

	"chopperbot/internal/config"
)

// DouyinLoginSession 一次抖音扫码登录会话（浏览器常驻到登录完成/超时）
type DouyinLoginSession struct {
	page   *rod.Page
	cleanup func()
	cancel context.CancelFunc
	pc     *PlatformConfig
	// udDraft 会话期间的 user-data-dir；登录成功后 rename 到正式账号目录
	udDraft string
	// fixedDir 非空 = 既有账号重登（目录名不变，不 rename）
	fixedDir string
}

// StartDouyinLogin 启动浏览器并打开创作者中心（后台慢启动，二维码稍后可取）。
// account 为空 = 新账号（draft 目录，登录成功后再按昵称落名）；非空 = 既有账号重登。
// 整体 60 秒硬超时：无头 Edge 冷启动 + 页面加载正常 ~20-30s，卡死（实例锁/杀软拦截）
// 时立即报错，绝不无限挂起。
func StartDouyinLogin(parent context.Context, cfg *config.Config, pc *PlatformConfig, account string) (*DouyinLoginSession, error) {
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	ud := AccountDir(cfg.DataDir, "douyin", account)
	fixed := ""
	if account == "" {
		ud = AccountDir(cfg.DataDir, "douyin", fmt.Sprintf("pending-%d", time.Now().UnixNano()))
	} else {
		fixed = ud
	}
	s := &DouyinLoginSession{pc: pc, udDraft: ud, fixedDir: fixed, cancel: cancel}

	browser, cleanup, err := Launch(ctx, BrowserOptions{
		UserDataDir: ud,
		ChromePath:  cfg.Chrome.Path,
		Headless:    true, // 服务器挂机形态（登录页实测无 headless 风控）
	})
	if err != nil {
		cancel()
		return nil, err
	}
	s.cleanup = cleanup
	page, err := NewStealthPage(browser, 10*time.Minute)
	if err != nil {
		cleanup()
		cancel()
		return nil, err
	}
	s.page = page
	if err := page.Navigate(pc.UploadURL); err != nil {
		s.Close()
		return nil, err
	}
	_ = page.WaitLoad()
	return s, nil
}

// Ready 浏览器是否仍在会话中（超时/崩溃后为 false）
func (s *DouyinLoginSession) Ready() bool {
	if s == nil || s.page == nil {
		return false
	}
	select {
	case <-s.page.GetContext().Done():
		return false
	default:
		return true
	}
}

// LoginQR 取当前二维码 data URL（抖音二维码 img 的 src 即 base64 data URL）。
// 返回空串 = 二维码尚未渲染/已刷新无（前端保持上一张）。
func (s *DouyinLoginSession) LoginQR() string {
	if !s.Ready() {
		return ""
	}
	sel := s.pc.Selectors["login_qr_img"]
	if sel == "" {
		sel = "#animate_qrcode_container img"
	}
	el, err := s.page.Element(sel)
	if err != nil {
		return ""
	}
	src, err := el.Attribute("src")
	if err != nil || src == nil {
		return ""
	}
	v := *src
	if strings.HasPrefix(v, "data:image") {
		return v
	}
	if strings.HasPrefix(v, "http") { // 非内嵌图（少见）：交给前端直链加载
		return v
	}
	return ""
}

// LoggedIn 登录成功判定：登录弹层的「扫码登录」文本消失（旧版等 URL 片段的
// 判定在 upload 页恒真——未登录 URL 也停在 creator-micro，等于从不等待）。
func (s *DouyinLoginSession) LoggedIn() bool {
	if !s.Ready() {
		return false
	}
	if HasText(s.page, "扫码登录") || HasText(s.page, "手机号登录") {
		return false
	}
	return strings.Contains(PageURL(s.page), s.pc.LoggedInURLContains)
}

// Nickname 登录成功后从创作者中心取昵称；miss 返回空（调用方自动命名）。
func (s *DouyinLoginSession) Nickname() string {
	if !s.Ready() {
		return ""
	}
	for _, group := range strings.Split(s.pc.Selectors["nickname_selectors"], ",") {
		for _, sel := range strings.Split(group, ",") {
			sel = strings.TrimSpace(sel)
			if sel == "" {
				continue
			}
			if el, err := s.page.Element(sel); err == nil {
				if txt, terr := el.Text(); terr == nil {
					txt = strings.TrimSpace(txt)
					if txt != "" && len([]rune(txt)) <= 32 {
						return txt
					}
				}
			}
		}
	}
	return ""
}

// Close 关闭浏览器并释放资源；draft 落名由调用方在 Close 前用 Settle 完成。
func (s *DouyinLoginSession) Close() {
	if s == nil {
		return
	}
	if s.cleanup != nil {
		s.cleanup() // rod Close 一并回收 Chrome 进程
		s.cleanup = nil
	}
	s.cancel()
}

// Settle 登录成功后的落名：返回最终账号目录。新账号把 pending 目录 rename 为
// 正式名（昵称清洗；miss 则 douyin_<MMdd-HHmm>）；既有账号原目录复用。
// rename 必须在浏览器关闭后进行（Windows 文件锁），调用顺序：Settle → Close。
func (s *DouyinLoginSession) Settle(nick string, sanitize func(string) string) (string, string, error) {
	if s.fixedDir != "" {
		return s.fixedDir, filepath.Base(s.fixedDir), nil
	}
	name := sanitize(nick)
	if name == "" {
		name = "douyin_" + time.Now().Format("0102-1504")
	}
	target := filepath.Join(filepath.Dir(s.udDraft), name)
	if target == s.udDraft {
		return target, name, nil
	}
	if _, err := os.Stat(target); err == nil {
		// 目标目录已存在（同名账号重新扫码）：合并策略——draft 直接废弃，
		// 用旧目录（登录态以最新扫码为准时旧目录作废，保持简单）
		_ = os.RemoveAll(s.udDraft)
		return target, name, nil
	}
	if err := os.Rename(s.udDraft, target); err != nil {
		// rename 失败（浏览器句柄未完全释放等）：退回 draft 名，不动数据
		return s.udDraft, filepath.Base(s.udDraft), nil
	}
	return target, name, nil
}
