package publish

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"

	"chopperbot/internal/config"
)

// DouyinPublish 抖音发布：rod 驱动创作者中心（选择器全部来自 configs/platforms/douyin.yaml）
//
// 前置：guanclip login --platform douyin 生成登录态目录（user-data-dir 长期复用）
func DouyinPublish(ctx context.Context, cfg *config.Config, pc *PlatformConfig, req *Request, videoPath, coverPath, workDir string, logf LogFunc) (PlatformResult, error) {
	res := PlatformResult{Platform: "douyin"}
	sel := pc.Selectors
	ud := AccountDir(cfg.DataDir, "douyin", req.Account)

	logf("启动 Chrome（登录态目录 %s，headless=%v）", ud, cfg.Publish.Headless)
	browser, cleanup, err := Launch(ctx, BrowserOptions{
		UserDataDir: ud,
		ChromePath:  cfg.Chrome.Path,
		Headless:    cfg.Publish.Headless,
	})
	if err != nil {
		return res, err
	}
	defer cleanup()

	page, err := NewStealthPage(browser, 15*time.Minute)
	if err != nil {
		return res, fmt.Errorf("创建页面失败: %w", err)
	}

	logf("打开创作者中心：%s", pc.UploadURL)
	if err := page.Navigate(pc.UploadURL); err != nil {
		return res, fmt.Errorf("打开上传页失败: %w", err)
	}
	_ = page.WaitLoad()

	// 登录态检测：URL 未进入 creator-micro 或出现登录标记 → 需要扫码
	time.Sleep(3 * time.Second)
	if !strings.Contains(PageURL(page), pc.LoggedInURLContains) {
		res.Detail = fmt.Sprintf("未登录（当前 URL: %s）：请先执行 guanclip login --platform douyin --account %s", PageURL(page), req.Account)
		return res, fmt.Errorf("%s", res.Detail)
	}
	for _, m := range pc.LoginMarkers {
		if HasText(page, m) {
			res.Detail = fmt.Sprintf("登录态已失效（检测到“%s”）：请重新执行 guanclip login --platform douyin", m)
			return res, fmt.Errorf("%s", res.Detail)
		}
	}

	// 1) 文件上传 input（依次尝试主选择器与兜底）
	logf("定位上传 input 并注入视频：%s", videoPath)
	uploadEl, err := firstElement(page, sel["upload_input"], sel["upload_input_fallback"])
	if err != nil {
		res.Detail = "未找到上传 input，页面可能改版（更新 configs/platforms/douyin.yaml 的 upload_input）"
		return res, err
	}
	if err := uploadEl.SetFiles([]string{videoPath}); err != nil {
		return res, fmt.Errorf("注入视频文件失败: %w", err)
	}

	// 2) 等待进入发布页（两种版本 URL）
	logf("等待进入发布页…")
	if !URLContains(page, pc.PublishURLContains, 180*time.Second) {
		res.Detail = "上传后未跳转到发布页（当前 URL: " + PageURL(page) + "）"
		return res, fmt.Errorf("%s", res.Detail)
	}
	time.Sleep(2 * time.Second)

	// 3) 填标题
	title := req.Title
	if pc.TitleMaxLen > 0 && len([]rune(title)) > pc.TitleMaxLen {
		title = string([]rune(title)[:pc.TitleMaxLen])
		logf("标题超长，截断为 %d 字", pc.TitleMaxLen)
	}
	titleEl, err := page.Element(sel["title_input"])
	if err != nil {
		res.Detail = "未找到标题输入框（更新 douyin.yaml 的 title_input）"
		return res, err
	}
	if err := titleEl.Input(title); err != nil {
		return res, fmt.Errorf("填写标题失败: %w", err)
	}
	logf("标题已填：%s", title)

	// 4) 填描述 + 话题（正文 + " #tag"，Esc 收起话题下拉）
	descParts := []string{}
	if strings.TrimSpace(req.Desc) != "" {
		descParts = append(descParts, strings.TrimSpace(req.Desc))
	}
	for _, t := range req.Tags {
		descParts = append(descParts, "#"+t)
	}
	if len(descParts) > 0 {
		descEl, err := page.Element(sel["desc_editor"])
		if err == nil {
			if err := descEl.Click(proto.InputMouseButtonLeft, 1); err == nil {
				_ = descEl.Input(strings.Join(descParts, " "))
				_ = page.Keyboard.Press(input.Escape)
				logf("描述与 %d 个话题已填", len(req.Tags))
			}
		} else {
			logf("未找到描述编辑器，仅填标题继续（可更新 douyin.yaml 的 desc_editor）")
		}
	}

	// 5) 等视频传完（“重新上传”出现；失败则重注入一次）
	logf("等待视频上传完成…")
	if !WaitTextAppeared(page, sel["upload_done_text"], time.Duration(pc.UploadTimeoutSec)*time.Second) {
		res.Detail = "等待上传完成超时"
		return res, fmt.Errorf("%s", res.Detail)
	}
	logf("视频上传完成")

	// 6) 推荐封面兜底（抖音要求先设封面；自定义封面 M2 从简）
	if HasText(page, sel["cover_required_text"]) {
		logf("检测到“%s”，尝试应用推荐封面", sel["cover_required_text"])
		_ = applyRecommendCover(page, sel, logf)
	}

	// 7) 清除新手引导浮层 + 发布前截图存档
	_, _ = page.Eval(`() => { document.querySelectorAll('.shepherd-element,.shepherd-modal-overlay-container,[class*="mention-wrapper"]').forEach(e => e.remove()); }`)
	if cfg.Publish.Screenshots {
		if p := Screenshot(page, filepath.Join(workDir, "publish"), "douyin-before.jpg"); p != "" {
			res.Screenshot = filepath.ToSlash(relTo(workDir, p))
		}
	}

	// 8) dry-run：停在发布按钮前
	if req.DryRun {
		res.OK = true
		res.Detail = "dry-run：已到发布按钮前，未点击发布（见截图）"
		logf("dry-run 停止，未点击发布")
		return res, nil
	}

	// 9) 点发布（文本精确匹配，失败回退 contains）
	logf("点击发布按钮…")
	btn, err := page.ElementR("button", "^"+sel["publish_button_text"]+"$")
	if err != nil {
		btn, err = page.ElementX(fmt.Sprintf("//button[contains(text(), %s)]", xpathText(sel["publish_button_text"])))
		if err != nil {
			res.Detail = "未找到发布按钮（更新 douyin.yaml 的 publish_button_text）"
			return res, err
		}
	}
	if err := btn.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return res, fmt.Errorf("点击发布按钮失败: %w", err)
	}

	// 10) 等成功跳转
	if !URLContains(page, []string{pc.SuccessURLContains}, 120*time.Second) {
		// 可能弹了验证码/确认框：截图留证
		if cfg.Publish.Screenshots {
			_ = Screenshot(page, filepath.Join(workDir, "publish"), "douyin-fail.jpg")
		}
		res.Detail = "点击发布后未跳转到管理页（可能触发风控验证，查看 publish/douyin-fail.jpg）"
		return res, fmt.Errorf("%s", res.Detail)
	}

	if cfg.Publish.Screenshots {
		if p := Screenshot(page, filepath.Join(workDir, "publish"), "douyin-after.jpg"); p != "" {
			res.Screenshot = filepath.ToSlash(relTo(workDir, p))
		}
	}
	res.OK = true
	res.Detail = "发布成功"
	logf("抖音发布成功")
	return res, nil
}

// DouyinLogin 首次登录（CLI 用）：开有头浏览器等扫码，登录后保存 user-data-dir
func DouyinLogin(ctx context.Context, cfg *config.Config, pc *PlatformConfig, account string) error {
	ud := AccountDir(cfg.DataDir, "douyin", account)
	fmt.Printf("启动 Chrome（登录态目录：%s）\n请在弹出的浏览器中扫码登录抖音创作者中心…\n", ud)
	browser, cleanup, err := Launch(ctx, BrowserOptions{UserDataDir: ud, ChromePath: cfg.Chrome.Path, Headless: false})
	if err != nil {
		return err
	}
	defer cleanup()

	page, err := NewStealthPage(browser, 10*time.Minute)
	if err != nil {
		return err
	}
	if err := page.Navigate(pc.UploadURL); err != nil {
		return err
	}
	_ = page.WaitLoad()

	if !URLContains(page, []string{pc.LoggedInURLContains}, 8*time.Minute) {
		return fmt.Errorf("8 分钟内未完成扫码登录，请重试 guanclip login --platform douyin")
	}
	fmt.Println("✔ 登录成功，登录态已保存，后续发布自动复用")
	return nil
}

// ---- 内部工具 ----

// firstElement 依次尝试多个选择器（逗号分隔视为多个）
func firstElement(page *rod.Page, selectors ...string) (*rod.Element, error) {
	var lastErr error = fmt.Errorf("未找到元素：所有候选选择器均未命中")
	for _, group := range selectors {
		if strings.TrimSpace(group) == "" {
			continue
		}
		for _, s := range strings.Split(group, ",") {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			e, err := page.Element(s)
			if err == nil {
				return e, nil
			}
			lastErr = err
		}
	}
	return nil, lastErr
}

// applyRecommendCover 应用第一个推荐封面（抖音要求先设封面）
func applyRecommendCover(page *rod.Page, sel map[string]string, logf LogFunc) bool {
	covers, err := page.Elements(sel["recommend_cover"])
	if err != nil || len(covers) == 0 {
		logf("未找到推荐封面，跳过（抖音可能要求手动设封面）")
		return false
	}
	if err := covers[0].Click(proto.InputMouseButtonLeft, 1); err != nil {
		return false
	}
	time.Sleep(1 * time.Second)
	if HasText(page, sel["cover_confirm_text"]) {
		if confirm, err := page.ElementR("button", "^"+sel["cover_confirm_button"]+"$"); err == nil {
			_ = confirm.Click(proto.InputMouseButtonLeft, 1)
			logf("已应用推荐封面")
			time.Sleep(1 * time.Second)
			return true
		}
	}
	return false
}

func relTo(base, full string) string {
	r, err := filepath.Rel(base, full)
	if err != nil {
		return full
	}
	return r
}
