package live

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
	"time"

	"chopperbot/internal/config"
	"chopperbot/internal/gate"
)

// 场级风格基线与漂移（体检门关卡 2 · daemon 侧）：
//   首段落地 → 建本场基线（分辨率/pHash/色调；开场 5 分钟语义在分段录制下的
//   落地形态：首段完整落盘即建——600s 分段约束下的最干净挂点，已知限制写进
//   改动说明）；后续每段落地 → 抽帧比对（≈每 5-10 分钟一次），任一项超阈值
//   → 整场标记"风格漂移" + 推送一次；切片侧（体检门关卡 2）读到漂移标记即
//   整场候选拦截转人工；人工确认后 rebaseline（serve API）重建特征清标记。
//   第一版只要"能报警"：允许误报，尽量不漏报；阈值全可配（publish_gate.style）。

// styleCheckAsync 分段落地后的场级风格检查（异步执行，绝不阻塞拉流——
// onSeg 在段切换点被同步调用，检测耗时若挂在里面会丢直播数据）
func (d *Daemon) styleCheckAsync(s *liveSession, segIdx int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("[stylegate] 房间 %s 风格检查 panic 已拦截: %v\n", s.roomID, r)
		}
	}()
	if !d.cfg.PublishGate.Enabled {
		return
	}
	// 已漂移的场不再重复检测（标记已表达整场拦截，等人工 rebaseline）
	if _, _, _, drifted, ok := d.store.StyleBaseline(s.roomID); ok && drifted {
		return
	}
	segs := listSegFiles(s.segDir)
	if len(segs) == 0 {
		return
	}
	if segIdx >= 0 && segIdx < len(segs) {
		d.styleCheck(context.Background(), s, segs[segIdx], segIdx)
	}
}

// sessionKey 场标识 = 会话目录名（一场一目录）
func sessionKey(s *liveSession) string {
	if s == nil || s.segDir == "" {
		return ""
	}
	return filepath.Base(strings.TrimRight(s.segDir, `\/`))
}

// styleCheck 单段风格处理：无本场基线 → 建；有 → 比对（超阈值标漂移+推送）
func (d *Daemon) styleCheck(ctx context.Context, s *liveSession, segPath string, segIdx int) {
	g := &d.cfg.PublishGate
	ffmpeg := d.cfg.Engines.FFmpeg
	if ffmpeg == "" {
		return
	}
	sid, ftJSON, _, drifted, ok := d.store.StyleBaseline(s.roomID)
	cur := sessionKey(s)

	// 检测全程限时（基线 60s / 比对 45s；超时放弃本轮——分段每 600s 一次，下轮再来）
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	if !ok || sid != cur {
		// 首段建基线：抽 5 帧取中间帧特征（单代表帧——止血版最稳形态）
		frames, err := gate.ExtractCenterFrames(ctx, ffmpeg, segPath,
			filepath.Join(s.segDir, "stylegate"), 5)
		if err != nil || len(frames) == 0 {
			fmt.Printf("[stylegate] 房间 %s 基线抽帧失败（下段重试）: %v\n", s.roomID, err)
			return
		}
		mid := frames[len(frames)/2]
		f, err := decodeFrameFeature(mid.Path)
		if err != nil {
			return
		}
		b, _ := json.Marshal(f)
		if _, err := d.store.UpsertStyleBaseline(s.roomID, cur, string(b)); err != nil {
			fmt.Printf("[stylegate] 基线落库失败: %v\n", err)
			return
		}
		fmt.Printf("[stylegate] 房间 %s 本场基线已建（段 %d，会话 %s）\n", s.roomID, segIdx+1, cur)
		return
	}
	if drifted {
		return // 漂移后等人工（上方预判已拦，双保险）
	}

	// 比对：新段抽 3 帧，任一帧任一项超阈值即漂移（允许误报，尽量不漏报）
	var base gate.FrameFeatures
	if err := json.Unmarshal([]byte(ftJSON), &base); err != nil {
		return // 基线损坏：不误报（下段首条件会重建？sid==cur 不会——留给 rebaseline）
	}
	frames, err := gate.ExtractCenterFrames(ctx, ffmpeg, segPath,
		filepath.Join(s.segDir, "stylegate"), 3)
	if err != nil || len(frames) == 0 {
		return
	}
	for _, fr := range frames {
		f, err := decodeFrameFeature(fr.Path)
		if err != nil {
			continue
		}
		if drifted, why := styleVerdict(base, f, g.Style); drifted {
			if err := d.store.MarkStyleDrift(s.roomID, cur, why); err == nil {
				fmt.Printf("[stylegate] 房间 %s 风格漂移（段 %d）：%s —— 该场切片将全部拦截转人工\n", s.roomID, segIdx+1, why)
				d.notify.Send("ClipStream 直播风格漂移",
					fmt.Sprintf("《%s》（房间 %s）画面风格变化：%s\n本场所有切片将拦截转人工；确认风格变化是有意的，可在控制台更新基线放行。", s.title, s.roomID, why))
			}
			return
		}
	}
	fmt.Printf("[stylegate] 房间 %s 段 %d 风格正常\n", s.roomID, segIdx+1)
}

// styleVerdict 单帧与基线的偏差判定：pHash 距离 / 色调距离 / 分辨率缩比，
// 任一超阈值即判漂移（返回人读原因）
func styleVerdict(base, cur gate.FrameFeatures, th config.StyleGate) (bool, string) {
	if hd := gate.HashDistance(base.PHash, cur.PHash); hd > th.HashThresh {
		return true, fmt.Sprintf("画面结构变化（pHash 距离 %d > %d）", hd, th.HashThresh)
	}
	if cd := gate.ColorDistance(base.ColorHist, cur.ColorHist); cd > th.ColorThresh {
		return true, fmt.Sprintf("主色调变化（距离 %.2f > %.2f）", cd, th.ColorThresh)
	}
	if sr := gate.SizeRatio(base, cur); sr < th.SizeThresh {
		return true, fmt.Sprintf("分辨率变化（相似比 %.2f < %.2f）", sr, th.SizeThresh)
	}
	return false, ""
}

// decodeFrameFeature 解码一帧图片为特征（gate.FeaturesOf 的文件版）
func decodeFrameFeature(path string) (gate.FrameFeatures, error) {
	f, err := os.Open(path)
	if err != nil {
		return gate.FrameFeatures{}, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return gate.FrameFeatures{}, err
	}
	return gate.FeaturesOf(img), nil
}
