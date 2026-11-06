package gate

import (
	"context"
	"image"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"chopperbot/internal/wexec"
)

// 本文件：tesseract CLI 适配器（关卡 1 广告 OCR）——选型结论（2026-10-25 拍板）：
// 走 whisper 同款外部二进制集成（wexec 隔离），不引入 gosseract（CGO 违反零 cgo
// 纪律）与 RapidOCR（部署重）。ocr_bin 空 = 未配置：广告 OCR 与台标 OCR 自动
// 跳过（决策日志记 skipped），模板匹配照常。

// OCR tesseract 外部 CLI 引擎
type OCR struct {
	Bin  string // tesseract 可执行文件路径
	Lang string // 语言包，如 chi_sim+eng
}

// Available 引擎可用：路径非空且能被 exec.LookPath 找到（配置即路径名均可）
func (o *OCR) Available() bool {
	if o == nil || o.Bin == "" {
		return false
	}
	_, err := exec.LookPath(o.Bin)
	return err == nil
}

// Text 识别整张图（crop 由调用方先做好——Go image 裁区域+2x 放大后落临时 png）
func (o *OCR) Text(ctx context.Context, imgPath string) (string, error) {
	lang := o.Lang
	if lang == "" {
		lang = "chi_sim+eng"
	}
	out, err := wexec.Command(ctx, o.Bin, imgPath, "stdout", "-l", lang, "--psm", "6").CombinedOutput()
	if err != nil {
		return "", err
	}
	return normalizeOCR(string(out)), nil
}

// DetectKeywordInRegion 裁出区域、2x 最近邻放大（tesseract 对小字需要放大）、OCR、
// 命中 keywords 任一子串即返回命中的词与全文。区域越界自动裁正。
// 返回 (命中词, 全文, error)；未命中 = ("", 全文, nil)
func (o *OCR) DetectKeywordInRegion(ctx context.Context, imgPath string, rect Rect, keywords []string) (string, string, error) {
	if len(keywords) == 0 {
		return "", "", nil
	}
	tmp, err := cropScale2x(imgPath, rect)
	if err != nil {
		return "", "", err
	}
	defer os.Remove(tmp)
	text, err := o.Text(ctx, tmp)
	if err != nil {
		return "", "", err
	}
	for _, kw := range keywords {
		if kw != "" && strings.Contains(text, kw) {
			return kw, text, nil
		}
	}
	return "", text, nil
}

// normalizeOCR tesseract 输出清洗：全角→半角统一比对、压空白
func normalizeOCR(s string) string {
	r := strings.NewReplacer(
		"　", " ", "，", ",", "。", ".", "：", ":", "；", ";",
		"\r", "", "\n", " ",
	)
	return strings.Join(strings.Fields(r.Replace(s)), " ")
}

// Rect 像素矩形（与 config.RectConf 对应，独立定义避免 config 反向依赖）
type Rect struct{ X, Y, W, H int }

// cropScale2x 裁区域并 2x 最近邻放大存临时 png（tesseract 对低分辨率小字识别差，
// 放大是常规预处理；最近邻保边缘锐度）。返回临时文件路径，调用方负责删
func cropScale2x(imgPath string, rect Rect) (string, error) {
	f, err := os.Open(imgPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return "", err
	}
	b := img.Bounds()
	if rect.X < 0 {
		rect.X = 0
	}
	if rect.Y < 0 {
		rect.Y = 0
	}
	if rect.X+rect.W > b.Dx() {
		rect.W = b.Dx() - rect.X
	}
	if rect.Y+rect.H > b.Dy() {
		rect.H = b.Dy() - rect.Y
	}
	if rect.W <= 0 || rect.H <= 0 {
		return "", errBadRegion
	}
	crop := image.NewRGBA(image.Rect(0, 0, rect.W, rect.H))
	for y := 0; y < rect.H; y++ {
		for x := 0; x < rect.W; x++ {
			crop.Set(x, y, img.At(b.Min.X+rect.X+x, b.Min.Y+rect.Y+y))
		}
	}
	// 2x 最近邻
	scaled := image.NewRGBA(image.Rect(0, 0, rect.W*2, rect.H*2))
	for y := 0; y < rect.H*2; y++ {
		for x := 0; x < rect.W*2; x++ {
			scaled.Set(x, y, crop.At(x/2, y/2))
		}
	}
	tmp := filepath.Join(os.TempDir(), "gate_ocr_"+strconv.FormatInt(time.Now().UnixNano(), 10)+".png")
	of, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	defer of.Close()
	if err := jpeg.Encode(of, scaled, &jpeg.Options{Quality: 92}); err != nil {
		return "", err
	}
	return tmp, nil
}

var errBadRegion = &regionError{}

type regionError struct{}

func (*regionError) Error() string { return "OCR 区域无效（越界或空）" }
