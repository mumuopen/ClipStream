package live

// B站 WBI 签名（2024-10 后 getDanmuInfo 等接口强制要求，缺 w_rid 返回 -352）
// 算法公开（bilibili-API-collect 等社区实现一致）：
//  1. nav 接口拿 wbi_img.img_url / sub_url，取文件名去扩展名为 img_key/sub_key
//  2. mixinKey = img_key+sub_key 按 mixinKeyEncTab 重排取前 32 位
//  3. 参数加 wts=unix 秒，按 key 字典序拼 query，md5(query+mixinKey) 得 w_rid

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// mixinKeyEncTab 官方混淆表（社区逆向公开，长期稳定）
const mixinKeyEncTab = "46,47,18,2,53,8,23,32,15,50,10,31,58,3,45,35,27,43,5,49,33,9,42,19,29,28,14,39,12,38,41,13,37,48,7,16,24,55,40,61,26,17,0,1,60,51,30,4,22,25,54,21,56,59,6,63,57,62,11,36,20,34,44,52"

var wbiTab [64]int

func init() {
	parts := strings.Split(mixinKeyEncTab, ",")
	for i, p := range parts {
		var v int
		fmt.Sscanf(p, "%d", &v)
		wbiTab[i] = v
	}
}

// wbiKeys 缓存 nav 拿到的 key（10 分钟过期，避免高频打 nav）
type wbiKeys struct {
	imgKey  string
	subKey  string
	fetched time.Time
}

var (
	wbiMu     sync.Mutex
	wbiCache  *wbiKeys
	wbiExpiry = 10 * time.Minute
)

// fetchWbiKeys 从 nav 接口获取 wbi key（带缓存）
func fetchWbiKeys(ctx context.Context) (string, string, error) {
	wbiMu.Lock()
	defer wbiMu.Unlock()
	if wbiCache != nil && time.Since(wbiCache.fetched) < wbiExpiry {
		return wbiCache.imgKey, wbiCache.subKey, nil
	}
	u := "https://api.bilibili.com/x/web-interface/nav"
	var out struct {
		Code int `json:"code"`
		Data struct {
			WbiImg struct {
				ImgURL string `json:"img_url"`
				SubURL string `json:"sub_url"`
			} `json:"wbi_img"`
		} `json:"data"`
	}
	if err := apiJSON(ctx, u, &out); err != nil {
		return "", "", err
	}
	imgKey := wbiKeyFromURL(out.Data.WbiImg.ImgURL)
	subKey := wbiKeyFromURL(out.Data.WbiImg.SubURL)
	if imgKey == "" || subKey == "" {
		return "", "", fmt.Errorf("nav 未返回 wbi key（img=%q sub=%q）", out.Data.WbiImg.ImgURL, out.Data.WbiImg.SubURL)
	}
	wbiCache = &wbiKeys{imgKey: imgKey, subKey: subKey, fetched: time.Now()}
	return imgKey, subKey, nil
}

// wbiKeyFromURL 从 /bfs/wbi/<key>.png 提取 key
func wbiKeyFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	base := u.Path[strings.LastIndex(u.Path, "/")+1:]
	base = strings.TrimSuffix(base, ".png")
	return base
}

// getMixinKey 混淆取 key：img+sub 按表重排，取前 32 位
func getMixinKey(imgKey, subKey string) string {
	raw := imgKey + subKey
	if len(raw) < 64 {
		return ""
	}
	b := make([]byte, 32)
	for i := 0; i < 32; i++ {
		b[i] = raw[wbiTab[i]]
	}
	return string(b)
}

// encWbi 给参数集签名，返回带 w_rid/wts 的完整参数
func encWbi(params map[string]string, imgKey, subKey string) map[string]string {
	mixin := getMixinKey(imgKey, subKey)
	params["wts"] = fmt.Sprintf("%d", time.Now().Unix())

	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(url.QueryEscape(params[k]))
	}
	query := sb.String()
	sum := md5.Sum([]byte(query + mixin))
	params["w_rid"] = hex.EncodeToString(sum[:])
	return params
}

// signedQuery 构造带 wbi 签名的查询串（参数自动签名）
func signedQuery(ctx context.Context, params map[string]string) (string, error) {
	imgKey, subKey, err := fetchWbiKeys(ctx)
	if err != nil {
		return "", err
	}
	signed := encWbi(params, imgKey, subKey)
	keys := make([]string, 0, len(signed))
	for k := range signed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(url.QueryEscape(signed[k]))
	}
	return sb.String(), nil
}