package publish

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"chopperbot/internal/config"
)

// BiliupPublish B站发布：biliup CLI 引擎（子进程直传，免浏览器）
//
// 登录：biliup -u <cookies.json> login（交互式，终端出二维码）
// 上传：biliup -u <cookies.json> upload <video> --title --desc --tid [--tag a,b] [--cover img]
func BiliupPublish(ctx context.Context, cfg *config.Config, pc *PlatformConfig, req *Request, videoPath, coverPath string, logf LogFunc) (PlatformResult, error) {
	res := PlatformResult{Platform: "bilibili"}
	bin := cfg.Engines.Biliup
	if bin == "" {
		return res, fmt.Errorf("B站引擎未配置：config.yaml 的 engines.biliup 需指向 biliup 可执行文件（github.com/biliup/biliup releases 下载）")
	}
	cookie := filepath.Join(AccountDir(cfg.DataDir, "bilibili", req.Account), "cookies.json")
	if _, err := os.Stat(cookie); err != nil {
		return res, fmt.Errorf("B站登录态缺失（%s）：请先执行 guanclip login --platform bilibili --account %s", cookie, req.Account)
	}

	args := []string{"-u", cookie, "upload", videoPath,
		"--title", req.Title,
		"--desc", req.Desc,
		"--tid", strconv.Itoa(pc.TID),
	}
	tags := req.Tags
	if len(tags) == 0 {
		tags = pc.TagsDefault
	}
	if len(tags) > 0 {
		args = append(args, "--tag", strings.Join(tags, ","))
	}
	if coverPath != "" {
		args = append(args, "--cover", coverPath)
	}

	logf("biliup 上传：%s", strings.Join(args, " "))
	out, err := runBiliup(ctx, bin, args)
	if err != nil {
		res.Detail = fmt.Sprintf("biliup 失败: %v\n%s", err, tail(out, 800))
		return res, err
	}
	res.OK = true
	res.Detail = "biliup 投稿完成"
	logf("B站投稿完成")
	return res, nil
}

// BiliupLogin 交互式登录（CLI 用：终端二维码，扫码后 cookie 落盘）
func BiliupLogin(cfg *config.Config, account string) error {
	bin := cfg.Engines.Biliup
	if bin == "" {
		return fmt.Errorf("B站引擎未配置：config.yaml 的 engines.biliup 需指向 biliup 可执行文件")
	}
	dir := AccountDir(cfg.DataDir, "bilibili", account)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	cookie := filepath.Join(dir, "cookies.json")
	fmt.Printf("即将打开 biliup 登录（终端显示二维码，用 B站 App 扫码）：\n  %s login\nCookie 将保存到：%s\n\n", bin, cookie)
	cmd := exec.Command(bin, "-u", cookie, "login")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runBiliup(ctx context.Context, bin string, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), err
	}
	return string(out), nil
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "..." + s[len(s)-n:]
	}
	return s
}
