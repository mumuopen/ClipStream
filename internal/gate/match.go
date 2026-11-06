package gate

import (
	"image"
	"image/draw"
	"math"
	_ "image/jpeg"
	_ "image/png"
	"os"
)

// 本文件：NCC 归一化互相关模板匹配（关卡 1 水印检测，手写零依赖——选型结论
// 2026-10-25：不引入 gocv/feature 检测库，区域内滑窗 NCC 足用）。
// 匹配语义：帧的指定区域内滑动模板图，找 NCC 峰值——峰值 ≥ 阈值（默认 0.85）
// 判水印命中。区域（RectConf.X/Y/W/H）用于把搜索窗缩到已知水印位附近，
// 全帧搜索时把区域设为整个画面即可。

// LoadGray 读图并灰度化为 *image.Gray（jpg/png 均可；失败返回错误）
func LoadGray(path string) (*image.Gray, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}
	return toGray(img), nil
}

// MatchInRegion 在区域内找模板的最佳匹配。
// 返回 (峰值 0~1, 峰值位置)。模板大于区域时返回 0（调用方按未命中处理）。
// 性能：区域 260x80、模板 64x64 时约 3.3k 个偏移 × 4k 像素 ≈ 1350 万乘加/帧，
// 单帧毫秒级，采样帧数少（≤10），整体可忽略
func MatchInRegion(frame *image.Gray, tpl *image.Gray, region image.Rectangle) (float64, image.Point) {
	if frame == nil || tpl == nil {
		return 0, image.Point{}
	}
	// 区域与帧边界求交（配置坐标越界自动裁正）
	region = region.Intersect(frame.Bounds())
	if region.Empty() {
		return 0, image.Point{}
	}
	tw, th := tpl.Bounds().Dx(), tpl.Bounds().Dy()
	if tw <= 0 || th <= 0 || tw > region.Dx() || th > region.Dy() {
		return 0, image.Point{}
	}
	tplData := grayPixels(tpl)
	tSum, tSum2 := sumStats(tplData)
	tn := float64(len(tplData))
	tMean := tSum / tn
	tVar := tSum2/tn - tMean*tMean
	if tVar <= 0 {
		return 0, image.Point{} // 纯色模板无信息量
	}

	best := 0.0
	bestAt := image.Point{}
	fb := frame.Bounds()
	// 模板坐标系平移到 (0,0)：tpl(x) ≡ tplData[y*tw+x]
	for dy := region.Min.Y; dy+th <= region.Max.Y; dy++ {
		for dx := region.Min.X; dx+tw <= region.Max.X; dx++ {
			ncc := nccAt(frame, fb, dx, dy, tw, th, tplData, tMean, tVar, tn)
			if ncc > best {
				best = ncc
				bestAt = image.Point{X: dx, Y: dy}
			}
		}
	}
	// NCC ∈ [-1,1]；负相关（反色）不算命中，钳到 0
	if best < 0 {
		return 0, bestAt
	}
	return best, bestAt
}

// nccAt 单个偏移处的 zero-mean NCC
func nccAt(f *image.Gray, fb image.Rectangle, dx, dy, tw, th int, tpl []float64, tMean, tVar, tn float64) float64 {
	var fSum, fSum2, cross float64
	fMean := 0.0
	// 第一遍求窗口均值
	for y := 0; y < th; y++ {
		row := f.PixOffset(fb.Min.X+dx, fb.Min.Y+dy+y)
		for x := 0; x < tw; x++ {
			v := float64(f.Pix[row+x])
			fSum += v
			fSum2 += v * v
		}
	}
	fMean = fSum / tn
	fVar := fSum2/tn - fMean*fMean
	if fVar <= 0 {
		return 0 // 纯色窗口无信息量
	}
	// 第二遍互相关（用预减均值：cross = Σ(f-fMean)(t-tMean)）
	for y := 0; y < th; y++ {
		row := f.PixOffset(fb.Min.X+dx, fb.Min.Y+dy+y)
		for x := 0; x < tw; x++ {
			cross += (float64(f.Pix[row+x]) - fMean) * (tpl[y*tw+x] - tMean)
		}
	}
	return cross / (math.Sqrt(fVar*tVar) * tn)
}

func toGray(img image.Image) *image.Gray {
	b := img.Bounds()
	g := image.NewGray(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(g, g.Bounds(), img, b.Min, draw.Src)
	return g
}

func grayPixels(g *image.Gray) []float64 {
	w, h := g.Bounds().Dx(), g.Bounds().Dy()
	out := make([]float64, w*h)
	for y := 0; y < h; y++ {
		row := g.PixOffset(0, y)
		for x := 0; x < w; x++ {
			out[y*w+x] = float64(g.Pix[row+x])
		}
	}
	return out
}

func sumStats(s []float64) (sum, sum2 float64) {
	for _, v := range s {
		sum += v
		sum2 += v * v
	}
	return
}
