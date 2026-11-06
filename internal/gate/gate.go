package gate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"chopperbot/internal/config"
	"chopperbot/internal/wexec"
)

// 本文件：体检门主流程 Check——四关串行 + fail-closed + 决策日志。
// 挂载点：pipeline.stageCut 每片导出后调用（serve 侧）。
// 开关纪律：cfg.Enabled=false 时调用方根本不进 gate（stages.go 判定），
// 本包不做兜底开关——保证"关闭=流水线与现状完全一致"由唯一入口保证。

// BaselineReader 场级风格基线读取接口（store 实现；nil=关卡 2 全部 skipped）
type BaselineReader interface {
	// RoomStyleState 查房间最近一场的基线状态：drifted=风格漂移标记，ver=基线版本
	RoomStyleState(roomID string) (drifted bool, ver int, ok bool)
}

// Input 单片体检输入（由 stageCut 组装）
type Input struct {
	ClipID    string  // "<jobID>/clips/clip_03.mp4"（决策日志唯一键）
	ClipPath  string  // 切片文件路径
	SessionID string  // 会话目录名（非直播来源为空）
	RoomID    string  // 来源房间（房间级检测配置与基线查询键；空=只跑通用关）
	Start     float64 // 片段在源视频的时间区间
	End       float64
	Score     float64 // LLM 高光分（等分切片=0）
	FFmpegBin string
	FramesDir string // 采样帧目录（data/gate/frames/<sanitized clipID>）
}

// 匹配置信度阈值（NCC 峰值 ≥ 此值判水印命中；止血版单一常量，实测后再进配置）
const watermarkNCCThresh = 0.85

// Check 单片四关体检。永不返回 error（检测异常全部落进 Decision，
// fail-closed 由 Final/Pass 表达）——调用方只需看 Pass。
func Check(ctx context.Context, cfg *config.PublishGate, base BaselineReader, in Input) *Decision {
	start := time.Now()
	d := &Decision{
		ClipID:    in.ClipID,
		SessionID: in.SessionID,
		TRange:    [2]float64{in.Start, in.End},
		Gate4:     &Gate4{Human: "pending"},
		CfgVer:    cfgVer(cfg),
		At:        start.Unix(),
	}
	defer func() { d.CostMs = time.Since(start).Milliseconds() }()

	enforce := cfg.Mode == "enforce"
	if ctx == nil { // 调用方（测试/极简路径）不传 ctx 时兜底
		ctx = context.Background()
	}
	// 单片检测总上限：超时/工具崩溃 → fail-closed（宁可少发）
	if cfg.ClipTimeoutSec > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(cfg.ClipTimeoutSec)*time.Second)
		defer cancel()
	}

	// ── 关卡 1：干净度（房间未配=skipped；检测异常=fail） ──
	d.Gate1 = gate1Clean(ctx, cfg, in)

	// ── 关卡 2：风格基线漂移（读场级结论；无基线/无房=skipped） ──
	d.Gate2 = gate2Style(cfg, base, in.RoomID)
	d.BaselineVer = d.Gate2.BaselineVer

	// ── 关卡 3：高光分阈值（接现有 LLM Score；宁缺毋滥） ──
	d.Gate3 = gate3Score(cfg, in.Score)

	// ── 处置汇总 ──
	blocked, action := summarize(d, cfg)
	switch {
	case !blocked: // 四关全过（或仅 skip 项）→ 放行
		d.Final, d.Pass = "pass", true
	case !enforce: // dry-run：记"将会怎么处置"，不拦截
		d.Final, d.Pass = "would-"+action, true
	case action == "crop": // enforce + crop 处置：按安全区裁切重导出，裁切成功=放行
		if p, err := cropClip(ctx, in, cfg.Clean.SafeCrop); err == nil {
			d.Final, d.Pass, d.CroppedPath = "cropped", true, p
		} else {
			// 裁切失败按 fail-closed 处理（不fallback reject——配置的模板还在画面里）
			d.Final, d.Pass = "fail-closed", false
			d.Gate1.Note = appendNote(d.Gate1.Note, "安全区裁切失败: "+err.Error())
		}
	case action == "fail-closed": // 工具故障：enforce 下一律不发
		d.Final, d.Pass = "fail-closed", false
	default: // reject / manual
		d.Final, d.Pass = action, false
	}
	return d
}

// summarize 判定是否拦截与处置动作：
// 干净度 dirty → 配置的 action（reject|crop|manual）；干净度 fail → fail；
// 漂移 → manual（整场转人工）；低分 → reject
func summarize(d *Decision, cfg *config.PublishGate) (blocked bool, action string) {
	if d.Gate1 != nil {
		switch d.Gate1.Status {
		case "dirty":
			a := cfg.Clean.Action
			if a != "crop" && a != "manual" {
				a = "reject"
			}
			return true, a
		case "fail":
			return true, "fail-closed"
		}
	}
	if d.Gate2 != nil && d.Gate2.Drifted {
		return true, "manual" // 整场风格漂移：所有候选默认拦截转人工
	}
	if d.Gate3 != nil && d.Gate3.Status == "blocked" {
		return true, "reject"
	}
	return false, ""
}

// gate1Clean 关卡 1：固定间隔(+场景)采样 → 水印模板匹配 + 广告区域 OCR
func gate1Clean(ctx context.Context, cfg *config.PublishGate, in Input) *Gate1 {
	g1 := &Gate1{Status: "skipped"}
	room := cfg.Rooms[in.RoomID]
	if room == nil || (len(room.Watermarks) == 0 && len(room.AdRegions) == 0) {
		g1.Note = "房间未配置水印/广告区域，关卡跳过"
		return g1
	}
	frames, err := ExtractSamples(ctx, in.FFmpegBin, in.ClipPath, in.FramesDir,
		cfg.SampleIntervalSec, cfg.SceneDetect)
	if err != nil || len(frames) == 0 {
		g1.Status, g1.Note = "fail", "抽帧失败: "+err.Error()
		return g1
	}
	g1.Frames = len(frames)
	g1.Status = "clean"

	ocr := &OCR{Bin: cfg.Clean.OCRBin, Lang: cfg.Clean.OCRLang}
	ocrReady := ocr.Available()
	// 模板缓存：同一模板跨帧复用（LoadGray 一次）
	tplCache := map[string]*grayPair{}

	for _, f := range frames {
		// ① 水印模板匹配（区域配置+模板图都在才算；模板缺失项跳过记 note）
		for _, wm := range room.Watermarks {
			if wm.Tpl == "" {
				g1.Note = appendNote(g1.Note, "水印项 "+wm.Name+" 未配模板图，模板匹配跳过")
				continue
			}
			gp, ok := tplCache[wm.Tpl]
			if !ok {
				tpl, err := LoadGray(wm.Tpl)
				if err != nil {
					g1.Note = appendNote(g1.Note, "模板图加载失败 "+wm.Tpl+": "+err.Error())
					continue
				}
				gp = &grayPair{tpl: tpl}
				tplCache[wm.Tpl] = gp
			}
			if gp.frame == nil || gp.framePath != f.Path {
				fr, err := LoadGray(f.Path)
				if err != nil {
					continue // 单帧解码失败：跳过该帧（其余帧仍有结论）
				}
				gp.frame, gp.framePath = fr, f.Path
			}
			peak, _ := MatchInRegion(gp.frame, gp.tpl, regionOf(wm))
			if peak >= watermarkNCCThresh {
				g1.Status = "dirty"
				g1.Hits = append(g1.Hits, Gate1Hit{
					T: f.T, Rect: [4]int{wm.X, wm.Y, wm.W, wm.H},
					Kind: "watermark", Name: wm.Name, Conf: round2(peak),
				})
			}
		}
		// ② 固定位置广告文字 OCR（引擎未配=整项 skipped，只记一次）
		if len(room.AdRegions) > 0 && !ocrReady {
			g1.Note = appendNote(g1.Note, "OCR 引擎未配置（clean.ocr_bin），广告文字检测跳过")
			continue
		}
		for _, ad := range room.AdRegions {
			if len(ad.Keywords) == 0 {
				continue
			}
			kw, text, err := ocr.DetectKeywordInRegion(ctx, f.Path,
				Rect{X: ad.X, Y: ad.Y, W: ad.W, H: ad.H}, ad.Keywords)
			if err != nil {
				// OCR 引擎崩溃：关卡置 fail（fail-closed）——广告可能在画面里而系统失明
				g1.Status, g1.Note = "fail", "广告 OCR 执行失败: "+err.Error()
				return g1
			}
			if kw != "" {
				g1.Status = "dirty"
				g1.Hits = append(g1.Hits, Gate1Hit{
					T: f.T, Rect: [4]int{ad.X, ad.Y, ad.W, ad.H},
					Kind: "ad_ocr", Name: ad.Name, Conf: 1.0,
					Text: clipStr(text, 60),
				})
			}
		}
	}
	return g1
}

// gate2Style 关卡 2：读场级漂移结论（建基线/比对在 daemon 侧 stylegate）
func gate2Style(cfg *config.PublishGate, base BaselineReader, roomID string) *Gate2 {
	g2 := &Gate2{Status: "skipped"}
	if base == nil || roomID == "" {
		g2.Note = "无基线来源（非直播片段或基线服务未接）"
		return g2
	}
	drifted, ver, ok := base.RoomStyleState(roomID)
	if !ok {
		g2.Note = "本场无风格基线（老素材或基线未建）"
		return g2
	}
	g2.BaselineVer = ver
	if drifted {
		g2.Status, g2.Drifted = "drift", true
		g2.Note = "本场风格漂移：所有候选默认拦截转人工，人工确认后可更新基线"
		return g2
	}
	g2.Status = "pass"
	return g2
}

// gate3Score 关卡 3：高光分阈值（接现有 LLM Score；等分切片按 reject_unscored）
func gate3Score(cfg *config.PublishGate, score float64) *Gate3 {
	g3 := &Gate3{Score: score, Threshold: cfg.MinScore}
	switch {
	case score > 0:
		if int(score) >= cfg.MinScore {
			g3.Status = "pass"
		} else {
			g3.Status = "blocked"
		}
	case cfg.RejectUnscored:
		g3.Status = "blocked"
		g3.Note = "等分切片无 LLM 评分且 reject_unscored=true，按宁缺毋滥拦截"
	default:
		g3.Status = "unscored"
		g3.Note = "等分切片无 LLM 评分（reject_unscored=false，仅标注不拦）"
	}
	return g3
}

// cropClip crop 处置：按 safe_crop 安全区重导出（ffmpeg crop 滤镜），
// 返回新文件路径（原文件保留供审计，不覆盖）
func cropClip(ctx context.Context, in Input, safeCrop string) (string, error) {
	parts := strings.Split(safeCrop, ",")
	if len(parts) != 4 {
		return "", fmt.Errorf("safe_crop 配置格式应为 x,y,w,h：得到 %q", safeCrop)
	}
	var v [4]int
	for i, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 0 {
			return "", fmt.Errorf("safe_crop 坐标非法: %q", safeCrop)
		}
		v[i] = n
	}
	out := strings.TrimSuffix(in.ClipPath, filepath.Ext(in.ClipPath)) + ".crop.mp4"
	args := []string{"-y", "-i", in.ClipPath, "-vf",
		fmt.Sprintf("crop=%d:%d:%d:%d", v[2], v[3], v[0], v[1]), "-c:a", "copy",
		filepath.ToSlash(out)}
	if out, err := wexec.Command(ctx, in.FFmpegBin, args...).CombinedOutput(); err != nil {
		return "", fmt.Errorf("ffmpeg crop 失败: %v：%.120s", err, out)
	}
	return out, nil
}

// ── 小工具 ──

type grayPair struct {
	tpl, frame *image.Gray
	framePath string
}

func cfgVer(cfg *config.PublishGate) string {
	b, _ := json.Marshal(cfg)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:4])
}

func regionOf(r config.RectConf) image.Rectangle {
	return image.Rect(r.X, r.Y, r.X+r.W, r.Y+r.H)
}

func appendNote(old, add string) string {
	if old == "" {
		return add
	}
	if strings.Contains(old, add) {
		return old // 同一原因（如 OCR 未配提示）只记一次
	}
	return old + "；" + add
}

func round2(v float64) float64 { return float64(int(v*100+0.5)) / 100 }

func clipStr(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
