// Package gate 发布前体检门（第 1 期止血版）：候选切片导出后、发布前过四关。
// 关卡 1 干净度（水印模板匹配+广告 OCR）｜关卡 2 风格基线漂移（场级）｜
// 关卡 3 高光分阈值｜关卡 4 人工确认（审核台）。任一关不过即拦截（enforce），
// dry-run 只记决策日志不拦截；开关关闭时流水线行为与现状完全一致。
//
// 本文件：图像感知特征——手写 DCT pHash（零依赖：图像标准库 + 预计算 DCT 基），
// 分辨率与主色调直方图。选型结论（2026-10-25 拍板）：手写 DCT 优于引入
// goimagehash/gocv——零新依赖、阈值自控，止血版够用。
package gate

import (
	"image"
	"math"
	"math/bits"
)

// pHash 参数：32x32 灰度 → 32x32 DCT-II（分离行列两遍）→ 取左上 8x8 系数 →
// 中值二值化 → 64bit 汉明可比哈希
const (
	phashSize = 32 // 输入边长（先 box 池化到 32x32）
	phashCoef = 8  // 取 DCT 左上 8x8
)

// dctBasis 一维 DCT-II 基：dctBasis[u][x] = c(u)·cos((2x+1)uπ/2N)
var dctBasis = buildDCTBasis(phashSize)

func buildDCTBasis(n int) [][]float64 {
	b := make([][]float64, n)
	for u := 0; u < n; u++ {
		b[u] = make([]float64, n)
		c := 1.0
		if u == 0 {
			c = math.Sqrt2 / 2 // c(0)=√½，其余 1
		}
		for x := 0; x < n; x++ {
			b[u][x] = c * math.Cos((2*float64(x)+1)*float64(u)*math.Pi/(2*float64(n)))
		}
	}
	return b
}

// FrameFeatures 一帧的风格特征（关卡 2 基线与比对的最小特征集）：
// 分辨率 + pHash + RGB 各 16bin 归一化直方图 + 均值色。
// 注意：At().RGBA() 是 16bit 预乘值，>>8 归到 0~255——检测帧均为 jpg（无 alpha），
// 预乘不影响灰度/色调计算
type FrameFeatures struct {
	W         int         `json:"w"`
	H         int         `json:"h"`
	PHash     uint64      `json:"phash"`
	ColorHist [48]float32 `json:"color_hist"` // R16+G16+B16 归一化
	AvgColor  [3]float64 `json:"avg_color"`
}

// FeaturesOf 提取一帧特征：
//   - 灰度按 32x32 box 平均池化（抗噪、稳 DCT）
//   - 分离式二维 DCT：先行变换再列变换，2N·N² ≈ 65k 乘加，微秒级
//   - 色调直方图全像素统计（RGB 各 16bin 归一化）
func FeaturesOf(img image.Image) FrameFeatures {
	if img == nil {
		return FrameFeatures{}
	}
	b := img.Bounds()
	f := FrameFeatures{W: b.Dx(), H: b.Dy()}

	gray := make([]float64, phashSize*phashSize)
	var hist [48]float64
	var sum [3]float64
	totalPx := float64(b.Dx() * b.Dy())
	if totalPx == 0 {
		return f
	}
	for gy := 0; gy < phashSize; gy++ {
		for gx := 0; gx < phashSize; gx++ {
			x0 := b.Min.X + gx*b.Dx()/phashSize
			y0 := b.Min.Y + gy*b.Dy()/phashSize
			x1 := b.Min.X + (gx+1)*b.Dx()/phashSize
			y1 := b.Min.Y + (gy+1)*b.Dy()/phashSize
			if x1 <= x0 {
				x1 = x0 + 1
			}
			if y1 <= y0 {
				y1 = y0 + 1
			}
			var gs float64
			var n float64
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					r, g, bl, _ := img.At(x, y).RGBA()
					r8, g8, b8 := float64(r>>8), float64(g>>8), float64(bl>>8)
					gs += 0.299*r8 + 0.587*g8 + 0.114*b8
					sum[0] += r8
					sum[1] += g8
					sum[2] += b8
					hist[clampBin(r8)]++
					hist[16+clampBin(g8)]++
					hist[32+clampBin(b8)]++
					n++
				}
			}
			gray[gy*phashSize+gx] = gs / n
		}
	}
	for i := range f.ColorHist {
		f.ColorHist[i] = float32(hist[i] / totalPx)
	}
	f.AvgColor = [3]float64{sum[0] / totalPx, sum[1] / totalPx, sum[2] / totalPx}

	// 分离式 DCT：rows → cols
	rows := make([]float64, phashSize*phashSize)
	for y := 0; y < phashSize; y++ {
		for u := 0; u < phashSize; u++ {
			var acc float64
			for x := 0; x < phashSize; x++ {
				acc += gray[y*phashSize+x] * dctBasis[u][x]
			}
			rows[y*phashSize+u] = acc
		}
	}
	coef := make([]float64, phashSize*phashSize)
	for x := 0; x < phashSize; x++ {
		for v := 0; v < phashSize; v++ {
			var acc float64
			for y := 0; y < phashSize; y++ {
				acc += rows[y*phashSize+x] * dctBasis[v][y]
			}
			coef[v*phashSize+x] = acc
		}
	}

	// 左上 8x8 系数（含 DC 共 64 个）中值二值化 → 64bit
	coefs := make([]float64, 0, phashCoef*phashCoef)
	for v := 0; v < phashCoef; v++ {
		for u := 0; u < phashCoef; u++ {
			coefs = append(coefs, coef[v*phashSize+u])
		}
	}
	med := median(coefs)
	var h uint64
	for i, c := range coefs {
		if c > med {
			h |= 1 << (63 - i)
		}
	}
	f.PHash = h
	return f
}

// HashDistance pHash 汉明距离（0~64）
func HashDistance(a, b uint64) int {
	return bits.OnesCount64(a ^ b)
}

// ColorDistance 主色调直方图距离：1 - Σmin(a,b)（0=完全一致，1=完全无交集）。
// 比卡方对空 bin 更稳，阈值语义直观：0.35 ≈ 三分之一以上分布错位。
// 防御：任一方直方图和 ≈ 0（特征缺失/全黑帧）返回 0——无信息不判漂移，
// 否则基线损坏或纯色帧会让漂移检测永远误报（2026-10-25 测试实测教训）
func ColorDistance(a, b [48]float32) float64 {
	var sa, sb, inter float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		sa += x
		sb += y
		if x < y {
			inter += x
		} else {
			inter += y
		}
	}
	if sa < 1e-6 || sb < 1e-6 {
		return 0
	}
	if inter > 1 {
		inter = 1
	}
	return 1 - inter
}

// SizeRatio 分辨率相似比：min(w1/w2, w2/w1)·min(h1/h2, h2/h1)，1=完全一致
func SizeRatio(a, b FrameFeatures) float64 {
	if a.W == 0 || a.H == 0 || b.W == 0 || b.H == 0 {
		return 0
	}
	rw := float64(a.W) / float64(b.W)
	if rw > 1 {
		rw = 1 / rw
	}
	rh := float64(a.H) / float64(b.H)
	if rh > 1 {
		rh = 1 / rh
	}
	return rw * rh
}

func median(s []float64) float64 {
	if len(s) == 0 {
		return 0
	}
	t := append([]float64(nil), s...)
	for i := 1; i < len(t); i++ { // 64 元素插入排序足够
		for j := i; j > 0 && t[j] < t[j-1]; j-- {
			t[j], t[j-1] = t[j-1], t[j]
		}
	}
	return t[len(t)/2]
}

func clampBin(v float64) int {
	b := int(v / 16)
	if b < 0 {
		b = 0
	}
	if b > 15 {
		b = 15
	}
	return b
}
