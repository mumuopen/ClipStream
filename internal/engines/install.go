// Package engines 引擎按需自动安装：检测缺失 → 从固定官方源下载 → 解压落位
// 到 engines\ → 返回新路径由调用方（cmd 层）写回 config.yaml → 冒烟复检。
// 核心理念：核心程序（双 exe ~40MB）与引擎（ffmpeg ~80MB、whisper 模型数百
// MB）分离——Release/部署包瘦身，首次使用时 `clipstream engines install`
// 一键补齐。
//
// 安全边界：
//   - 只从固定官方源下载（URL 白名单），不做任何源切换；落位目录显式指定
//   - 下载是用户显式触发的命令；serve/daemon 运行时绝不静默拉取（延续
//     “浏览器绝不自动下载”的既有原则——无人值守不悄悄装可执行文件）
//   - 下载尊重 config.proxy（国内环境必走代理）；失败给出官方手动下载地址
//   - 落位后由调用方冒烟复检，坏引擎当场报错不留“文件在、一跑就崩”
package engines

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"chopperbot/internal/config"
)

// 官方固定 URL（只信任这里；一律跟随各项目 latest）
const (
	// yt-dlp 单 exe
	ytdlpURL = "https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp.exe"
	// ffmpeg/ffprobe：gyan.dev essentials 构建（国内直连友好），zip 内 bin/{ffmpeg,ffprobe}.exe
	ffmpegURL = "https://www.gyan.dev/ffmpeg/builds/ffmpeg-release-essentials.zip"
	// whisper.cpp Windows 预编译包（含 whisper-cli.exe；资产名模糊匹配）
	whisperBinURL = "https://github.com/ggml-org/whisper.cpp/releases/latest/download/whisper-binaries-x64.zip"
	// whisper 模型：HuggingFace ggerganov 官方
	whisperModelBase = "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/"
	// biliup B站投稿引擎
	biliupURL = "https://github.com/biliup/biliup/releases/latest/download/biliup-x86_64-pc-windows-msvc.exe"
)

// ModelSizes 模型档位（下载前提示用）
var ModelSizes = map[string]string{
	"tiny":   "~75 MB",
	"base":   "~142 MB",
	"small":  "~466 MB",
	"medium": "~1.5 GB",
}

// Result 单个引擎安装结果
type Result struct {
	Name   string // yt-dlp / ffmpeg / biliup / whisper-cli / whisper-model
	Status string // installed / skipped / failed
	Path   string // 落位后的文件路径（供调用方写回 config）
	Detail string // 失败原因 / 手动下载提示
}

// InstallPlan 一次安装请求
type InstallPlan struct {
	WantModel string // whisper 模型档位，默认 small
	Names     []string // 要装的项（Missing 的项）；空=全部缺失项
	// TargetDir 引擎落位目录（默认 "<config同目录>/engines"，与部署包布局一致）
	TargetDir string
}

// Missing 返回缺失清单（体检补充：每个缺失项可被 engines install 补齐）
func Missing(cfg *config.Config, enginesDir string) []string {
	var out []string
	add := func(name string, bin string) {
		if !present(bin) {
			out = append(out, name)
		}
	}
	add("yt-dlp", cfg.Engines.YtDlp)
	add("ffmpeg", cfg.Engines.FFmpeg)
	add("ffprobe", cfg.Engines.Probe)
	add("biliup", cfg.Engines.Biliup)
	add("whisper-cli", cfg.Engines.ASR.Binary)
	add("whisper-model", cfg.Engines.ASR.Model)
	return out
}

// present 引擎已就位：绝对/相对路径存在 或 PATH 命令可寻
func present(bin string) bool {
	if bin == "" {
		return false
	}
	if strings.ContainsAny(bin, `/\`) {
		_, err := os.Stat(bin)
		return err == nil
	}
	_, err := lookPath(bin)
	return err == nil
}

// lookPath 可替换的 PATH 查找（单测注入）
var lookPath = func(bin string) (string, error) {
	return lookPathReal(bin)
}

// lookPathReal 真实实现：exec.LookPath（PATH 可寻命令）
func lookPathReal(bin string) (string, error) {
	p, err := exec.LookPath(bin)
	if err != nil {
		return "", err
	}
	return p, nil
}

// Install 执行安装。返回逐项结果；单项目失败不中断整体（调用方汇总报告）。
// 不负责写 config（需要 cfgPath 与编辑策略，由 cmd 层根据 Result.Path 更新）。
func Install(ctx context.Context, cfg *config.Config, plan InstallPlan) ([]Result, error) {
	if plan.WantModel == "" {
		plan.WantModel = "small"
	}
	if _, ok := ModelSizes[plan.WantModel]; !ok {
		return nil, fmt.Errorf("未知模型档位 %q（支持 tiny/base/small/medium）", plan.WantModel)
	}
	dir := plan.TargetDir
	if dir == "" {
		dir = "engines"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建引擎目录失败 %s: %w", dir, err)
	}

	var results []Result
	run := func(name string, fn func() (string, error)) {
		p, err := fn()
		if err != nil {
			results = append(results, Result{Name: name, Status: "failed", Detail: err.Error()})
			return
		}
		results = append(results, Result{Name: name, Status: "installed", Path: p})
	}

	names := plan.Names
	if len(names) == 0 {
		names = []string{"yt-dlp", "ffmpeg", "biliup", "whisper-cli", "whisper-model"}
	}
	for _, n := range names {
		switch n {
		case "yt-dlp":
			run("yt-dlp", func() (string, error) { return installBinary(ctx, cfg, dir, "yt-dlp", ytdlpURL) })
		case "ffmpeg", "ffprobe":
			run("ffmpeg", func() (string, error) { return installFFmpeg(ctx, cfg, dir) })
		case "biliup":
			run("biliup", func() (string, error) { return installBinary(ctx, cfg, dir, "biliup", biliupURL) })
		case "whisper-cli":
			run("whisper-cli", func() (string, error) { return installWhisperCli(ctx, cfg, dir) })
		case "whisper-model":
			run("whisper-model", func() (string, error) { return installWhisperModel(ctx, cfg, dir, plan.WantModel) })
		default:
			results = append(results, Result{Name: n, Status: "failed", Detail: "未知引擎（支持 yt-dlp/ffmpeg/biliup/whisper-cli/whisper-model）"})
		}
	}
	return results, nil
}

// 单文件直落（yt-dlp/biliup）
func installBinary(ctx context.Context, cfg *config.Config, dir, name, srcURL string) (string, error) {
	dst := filepath.Join(dir, name+".exe")
	if present(dst) {
		return dst, fmt.Errorf("已存在 %s（跳过）", dst)
	}
	fmt.Printf("  ↓ 下载 %s：%s\n", name, srcURL)
	if err := download(ctx, cfg, srcURL, dst); err != nil {
		return "", fmt.Errorf("下载失败：%w；手动地址 %s → 放入 %s", err, srcURL, dst)
	}
	fmt.Printf("  ✔ %s 就绪：%s\n", name, dst)
	return dst, nil
}

// ffmpeg+ffprobe：gyan zip 取 bin/{ffmpeg,ffprobe}.exe
// 2026-10-28 修：旧版只要 ffmpeg.exe 在就整体跳过，只缺 ffprobe.exe 的部署
// 永远无法自动修复（而 ffprobe 缺失会让高光时长线/分段探测链路降级）。
// 现在逐个判定：缺哪个补哪个，已有的不动（可能正在被占用）。
func installFFmpeg(ctx context.Context, cfg *config.Config, dir string) (string, error) {
	bin := filepath.Join(dir, "ffmpeg.exe")
	probe := filepath.Join(dir, "ffprobe.exe")
	needFfmpeg, needProbe := !present(bin), !present(probe)
	if !needFfmpeg && !needProbe {
		return bin, fmt.Errorf("已存在 ffmpeg/ffprobe（跳过）")
	}
	fmt.Printf("  ↓ 下载 ffmpeg：%s\n", ffmpegURL)
	zipPath := filepath.Join(dir, ".download-ffmpeg.zip")
	if err := download(ctx, cfg, ffmpegURL, zipPath); err != nil {
		return "", fmt.Errorf("下载失败：%w；手动地址 %s → 解压取 bin\\ffmpeg.exe、bin\\ffprobe.exe 放入 %s", err, ffmpegURL, dir)
	}
	defer os.Remove(zipPath)
	got := map[string]string{}
	if err := extractZip(zipPath, "bin", func(name string) bool {
		b := filepath.Base(name)
		return b == "ffmpeg.exe" || b == "ffprobe.exe"
	}, got); err != nil {
		return "", fmt.Errorf("解析 ffmpeg 包失败：%w", err)
	}
	// 解出来的临时文件要么重命名就位，要么删掉（不能留 .x-*.exe 残渣）
	place := func(key, dst string, need bool) error {
		tmp, ok := got[key]
		if !ok {
			return nil
		}
		if !need {
			_ = os.Remove(tmp)
			return nil
		}
		return os.Rename(tmp, dst)
	}
	if err := place("ffmpeg.exe", bin, needFfmpeg); err != nil {
		return "", err
	}
	if err := place("ffprobe.exe", probe, needProbe); err != nil {
		return "", err
	}
	fmt.Printf("  ✔ ffmpeg/ffprobe 就绪：%s\n", dir)
	return bin, nil
}

// whisper-cli 预编译包（资产名模糊匹配 whisper-cli.exe）
func installWhisperCli(ctx context.Context, cfg *config.Config, dir string) (string, error) {
	dst := filepath.Join(dir, "whisper-cli.exe")
	if present(dst) {
		return dst, fmt.Errorf("已存在 whisper-cli（跳过）")
	}
	fmt.Printf("  ↓ 下载 whisper-cli：%s\n", whisperBinURL)
	zipPath := filepath.Join(dir, ".download-whisper.zip")
	if err := download(ctx, cfg, whisperBinURL, zipPath); err != nil {
		return "", fmt.Errorf("下载失败：%w；手动地址 %s → 解压取 whisper-cli.exe 放入 %s", err, whisperBinURL, dst)
	}
	defer os.Remove(zipPath)
	got := map[string]string{}
	if err := extractZip(zipPath, "", func(name string) bool {
		return filepath.Base(name) == "whisper-cli.exe"
	}, got); err != nil {
		return "", fmt.Errorf("解析 whisper 包失败：%w", err)
	}
	f, ok := got["whisper-cli.exe"]
	if !ok {
		return "", fmt.Errorf("包里未找到 whisper-cli.exe（资产可能改名，手动去 %s 核对）", whisperBinURL)
	}
	if err := os.Rename(f, dst); err != nil {
		return "", err
	}
	fmt.Printf("  ✔ whisper-cli 就绪：%s\n", dst)
	return dst, nil
}

// whisper 模型（指定档位）
func installWhisperModel(ctx context.Context, cfg *config.Config, dir, size string) (string, error) {
	dst := filepath.Join(dir, "ggml-"+size+".bin")
	if present(dst) {
		return dst, fmt.Errorf("已存在 %s（跳过）", dst)
	}
	u := whisperModelBase + "ggml-" + size + ".bin"
	fmt.Printf("  ↓ 下载 whisper 模型（%s，%s）：%s\n", size, ModelSizes[size], u)
	if err := download(ctx, cfg, u, dst); err != nil {
		return "", fmt.Errorf("下载失败：%w；国内不畅可换 hf-mirror：https://hf-mirror.com/ggerganov/whisper.cpp/resolve/main/ggml-%s.bin → 放入 %s", err, size, dst)
	}
	fmt.Printf("  ✔ whisper 模型（%s）就绪：%s\n", size, dst)
	return dst, nil
}

// hasPathSegment 路径任一段等于目标目录（gyan zip 带版本前缀目录：
// ffmpeg-7.1-essentials/bin/ffmpeg.exe → bin 段命中）
func hasPathSegment(name, seg string) bool {
	for _, p := range strings.Split(name, "/") {
		if p == seg {
			return true
		}
	}
	return false
}

// overrideURL 测试注入点（单测指向本地 httptest 源；生产恒等）
var overrideURL = func(u string) string { return u }

// download 下载（尊重代理；校验非空）
func download(ctx context.Context, cfg *config.Config, srcURL, dst string) error {
	srcURL = overrideURL(srcURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srcURL, nil)
	if err != nil {
		return err
	}
	tr := &http.Transport{}
	if cfg != nil && cfg.Proxy != "" {
		if pu, perr := url.Parse(cfg.Proxy); perr == nil {
			tr.Proxy = http.ProxyURL(pu)
		}
	}
	client := &http.Client{Transport: tr}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// 先写 .part 再改名（2026-10-28）：旧版直接 os.Create(dst)，下载中途被杀会
	// 留下半截 exe，而 present() 只看“文件在且非空”——下一轮直接报“已存在（跳过）”，
	// 坏引擎永久钉死。再加上文件头校验，防止 CDN 返回 HTML 错误页。
	tmp := dst + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	n, cerr := io.Copy(out, resp.Body)
	closeErr := out.Close()
	if cerr != nil {
		os.Remove(tmp)
		return cerr
	}
	if closeErr != nil {
		os.Remove(tmp)
		return closeErr
	}
	if n == 0 {
		os.Remove(tmp)
		return fmt.Errorf("下载内容为空")
	}
	if resp.ContentLength > 0 && n != resp.ContentLength {
		os.Remove(tmp)
		return fmt.Errorf("下载不完整：%d/%d 字节", n, resp.ContentLength)
	}
	if err := verifyHeader(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// verifyHeader 校下载产物文件头：exe 必须 MZ、zip 必须 PK。
// 对付“CDN/代理返回 200 + HTML 错误页”，以及被截断成前几字节的空壳。
func verifyHeader(tmp, dst string) error {
	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	defer f.Close()
	var h [2]byte
	if _, err := io.ReadFull(f, h[:]); err != nil {
		return fmt.Errorf("文件过小（不足 2 字节）")
	}
	switch strings.ToLower(filepath.Ext(dst)) {
	case ".exe":
		if h[0] != 'M' || h[1] != 'Z' {
			return fmt.Errorf("不是有效的 Windows 可执行文件（缺少 MZ 头，可能下到了 HTML 错误页）")
		}
	case ".zip":
		if !(h[0] == 'P' && h[1] == 'K') {
			return fmt.Errorf("不是有效的 zip（缺少 PK 头）")
		}
	}
	return nil
}

// extractZip 解压 zip 中满足 pick 的条目到临时文件；out[basename]→临时路径
// baseDir：只扫该目录前缀（如 "bin"，正反斜杠统一）；空=全部
func extractZip(zipPath, baseDir string, pick func(name string) bool, out map[string]string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	dir := filepath.Dir(zipPath)
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := strings.ReplaceAll(f.Name, "\\", "/")
		if baseDir != "" && !hasPathSegment(name, baseDir) {
			continue
		}
		if !pick(name) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		tmp := filepath.Join(dir, ".x-"+filepath.Base(name))
		dst, err := os.Create(tmp)
		if err != nil {
			rc.Close()
			return err
		}
		_, cerr := io.Copy(dst, rc)
		rc.Close()
		dst.Close()
		if cerr != nil {
			os.Remove(tmp)
			return cerr
		}
		out[filepath.Base(name)] = tmp
	}
	return nil
}