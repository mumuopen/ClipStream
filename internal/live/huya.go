package live

// 虎牙直播适配器（M4.1 多平台扩展，对齐 SimpleLive 的 huya 实现血统）
// 契约（2026-10 实测）：
//   - 房间信息：GET https://m.huya.com/<roomId>（Android UA），
//     正则抓 window.HNF_GLOBAL_INIT JSON；eLiveStatus==2 表示直播中；
//     tLiveInfo.tLiveStreamInfo.vStreamInfo.value[] 为流线路（sFlvUrl/sStreamName/sFlvAntiCode）
//   - 播放地址：sFlvUrl + "/" + sStreamName + ".flv?" + 重建的 anticode
//     （页面自带 antiCode 不能直接用，需按 fm/wsTime 重算 wsSecret，否则 CDN 403）
//   - 弹幕：wss://cdnws.api.huya.com + TARS 编码（另文件实现）

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const huyaMobileUA = "Mozilla/5.0 (Linux; Android 11; Pixel 5) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/90.0.4430.91 Mobile Safari/537.36"

// HuyaRoomInfo 虎牙直播间信息
type HuyaRoomInfo struct {
	RoomID     string // lProfileRoom
	Title      string // sIntroduction 优先，sRoomName 兜底
	Nick       string
	LiveStatus int // 2=直播中
	Online     int64
	Yyid       int64 // 弹幕用 ayyuid
	TopSid     int64 // 弹幕用 pid
	SubSid     int64

	StreamName string
	FlvURL     string // 如 http://al.flv.huya.com/src（首选线路）
	AntiCode   string // 页面 sFlvAntiCode（需重建后用于拉流）
	PresenterUID int64

	// 全部流线路（同 streamName，不同 CDN）
	LineURLs []string
}

// ExtractHuyaRoomID 从虎牙房间 URL/ID 提取房间号（https://www.huya.com/<id> 或纯数字）
func ExtractHuyaRoomID(raw string) (string, error) {
	if m := regexp.MustCompile(`huya\.com/(\d+)`).FindStringSubmatch(raw); len(m) == 2 {
		return m[1], nil
	}
	if _, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil {
		return strings.TrimSpace(raw), nil
	}
	return "", fmt.Errorf("无法从 %q 识别虎牙房间号（形如 https://www.huya.com/441195）", raw)
}

// GetHuyaRoomInfo 抓取虎牙房间信息（HNF_GLOBAL_INIT JSON）
func GetHuyaRoomInfo(ctx context.Context, roomID string) (*HuyaRoomInfo, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://m.huya.com/"+url.PathEscape(roomID), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", huyaMobileUA)
	req.Header.Set("Referer", "https://m.huya.com/")
	c := &http.Client{Timeout: 15 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("虎牙页面请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("虎牙页面返回 %d", resp.StatusCode)
	}
	body := make([]byte, 0, 1<<20)
	buf := make([]byte, 64<<10)
	for {
		n, err := resp.Body.Read(buf)
		body = append(body, buf[:n]...)
		if err != nil {
			break
		}
		if len(body) > 4<<20 {
			break
		}
	}
	return parseHuyaRoomPage(string(body))
}

var huyaInitRe = regexp.MustCompile(`window\.HNF_GLOBAL_INIT\s*=\s*(\{[\s\S]*?\})\s*</script>`)

func parseHuyaRoomPage(html string) (*HuyaRoomInfo, error) {
	m := huyaInitRe.FindStringSubmatch(html)
	if len(m) < 2 {
		// 房间不存在时 HNF_GLOBAL_INIT = {"exceptionType":0}
		if strings.Contains(html, "找不到此页面") || strings.Contains(html, "exceptionType") {
			return nil, fmt.Errorf("虎牙房间不存在或页面未初始化")
		}
		return nil, fmt.Errorf("虎牙页面未包含房间初始化数据")
	}
	jsonText := m[1]
	// 去掉 JS 函数片段（JSON 不合法）
	jsonText = regexp.MustCompile(`function\s*\([^)]*\)\s*\{[\s\S]*?\}`).ReplaceAllString(jsonText, `""`)
	var root struct {
		RoomInfo *struct {
			ELiveStatus int `json:"eLiveStatus"`
			TLiveInfo   *struct {
				SIntroduction string `json:"sIntroduction"`
				SRoomName     string `json:"sRoomName"`
				LProfileRoom  int64  `json:"lProfileRoom"`
				LTotalCount   int64  `json:"lTotalCount"`
				LYyid         int64  `json:"lYyid"`
				TProfileInfo  *struct {
					LProfileRoom int64  `json:"lProfileRoom"`
					LYyid        int64  `json:"lYyid"`
					SNick        string `json:"sNick"`
				} `json:"tProfileInfo"`
				TLiveStreamInfo *struct {
					VStreamInfo *struct {
						Value []*struct {
							SFlvUrl       string `json:"sFlvUrl"`
							SStreamName   string `json:"sStreamName"`
							SFlvAntiCode  string `json:"sFlvAntiCode"`
							SCdnType      string `json:"sCdnType"`
						} `json:"value"`
					} `json:"vStreamInfo"`
				} `json:"tLiveStreamInfo"`
			} `json:"tLiveInfo"`
		} `json:"roomInfo"`
	}
	if err := json.Unmarshal([]byte(jsonText), &root); err != nil {
		return nil, fmt.Errorf("虎牙房间 JSON 解析失败: %w", err)
	}
	if root.RoomInfo == nil {
		return nil, fmt.Errorf("虎牙房间信息缺失")
	}
	info := &HuyaRoomInfo{LiveStatus: root.RoomInfo.ELiveStatus}
	li := root.RoomInfo.TLiveInfo
	if li != nil {
		info.RoomID = fmt.Sprintf("%d", li.LProfileRoom)
		info.Title = li.SIntroduction
		if info.Title == "" {
			info.Title = li.SRoomName
		}
		info.Online = li.LTotalCount
		info.Yyid = li.LYyid
		if li.TProfileInfo != nil && li.TProfileInfo.LProfileRoom > 0 {
			info.RoomID = fmt.Sprintf("%d", li.TProfileInfo.LProfileRoom)
			if info.Yyid == 0 {
				info.Yyid = li.TProfileInfo.LYyid
			}
			info.Nick = li.TProfileInfo.SNick
		}
		if li.TLiveStreamInfo != nil && li.TLiveStreamInfo.VStreamInfo != nil {
			for _, v := range li.TLiveStreamInfo.VStreamInfo.Value {
				if v == nil || v.SFlvUrl == "" || v.SStreamName == "" {
					continue
				}
				if info.StreamName == "" {
					info.StreamName = v.SStreamName
					info.AntiCode = v.SFlvAntiCode
				}
				info.LineURLs = append(info.LineURLs, v.SFlvUrl)
				if info.FlvURL == "" {
					info.FlvURL = v.SFlvUrl
				}
			}
		}
	}
	if info.RoomID == "" {
		return nil, fmt.Errorf("虎牙房间号缺失")
	}
	info.TopSid, _ = strconv.ParseInt(huyaField(html, `lChannelId":(\d+)`), 10, 64)
	info.SubSid, _ = strconv.ParseInt(huyaField(html, `lSubChannelId":(\d+)`), 10, 64)
	info.PresenterUID = info.TopSid
	if info.PresenterUID <= 0 {
		info.PresenterUID = info.SubSid
	}
	return info, nil
}

func huyaField(html, pattern string) string {
	m := regexp.MustCompile(pattern).FindStringSubmatch(html)
	if len(m) == 2 {
		return m[1]
	}
	return ""
}

// HuyaPlayURLs 构建全部线路的 FLV 播放地址（重建 anticode）
func HuyaPlayURLs(info *HuyaRoomInfo) ([]string, error) {
	if info.StreamName == "" || len(info.LineURLs) == 0 {
		return nil, fmt.Errorf("房间 %s 无可用流线路（可能未开播）", info.RoomID)
	}
	anti := buildHuyaAntiCode(info.StreamName, info.PresenterUID, info.AntiCode)
	var urls []string
	for _, line := range info.LineURLs {
		urls = append(urls, fmt.Sprintf("%s/%s.flv?%s&codec=264", line, info.StreamName, anti))
	}
	return urls, nil
}

// buildHuyaAntiCode 重建虎牙 antiCode（对齐 SimpleLive buildAntiCode）
// 页面自带 antiCode 的 wsSecret 会过期（CDN 403），需用 fm/wsTime 重算；
// 注意：fm 等参数须保留页面原始编码（避免二次 urlencode 导致 CDN 识别失败）
func buildHuyaAntiCode(stream string, presenterUID int64, antiCode string) string {
	if presenterUID <= 0 || antiCode == "" {
		return antiCode
	}
	rawParam := func(key string) string {
		m := regexp.MustCompile(key + `=([^&]+)`).FindStringSubmatch(antiCode)
		if len(m) == 2 {
			return m[1]
		}
		return ""
	}
	fmRaw := rawParam("fm") // 页面原始值（单层 urlencoded 的 base64，勿二次编码）
	if fmRaw == "" {
		return antiCode
	}
	params, _ := url.ParseQuery(antiCode) // 解码后的值用于签名计算
	ctype := params.Get("ctype")
	if ctype == "" {
		ctype = "huya_pc_exe"
	}
	platformID, _ := strconv.ParseInt(params.Get("t"), 10, 64)
	isWap := platformID == 103
	clacStart := time.Now().UnixMilli()
	seqID := presenterUID + clacStart
	secretHash := md5Hex(fmt.Sprintf("%d|%s|%d", seqID, ctype, platformID))
	convertUID := rotl64(presenterUID)
	calcUID := presenterUID
	if !isWap {
		calcUID = convertUID
	}
	fmDecoded, err := base64.StdEncoding.DecodeString(params.Get("fm"))
	if err != nil {
		return antiCode
	}
	secretPrefix := strings.Split(string(fmDecoded), "_")[0]
	wsTime := params.Get("wsTime")
	wsSecret := md5Hex(fmt.Sprintf("%s_%d_%s_%s_%s", secretPrefix, calcUID, stream, secretHash, wsTime))

	parts := []string{
		"wsSecret=" + wsSecret,
		"wsTime=" + wsTime,
		"seqid=" + fmt.Sprintf("%d", seqID),
		"ctype=" + ctype,
		"ver=1",
	}
	if fs := rawParam("fs"); fs != "" {
		parts = append(parts, "fs="+fs)
	}
	parts = append(parts, "fm="+fmRaw) // 原样输出
	if t := params.Get("t"); t != "" {
		parts = append(parts, "t="+t)
	}
	if isWap {
		parts = append(parts, "uid="+fmt.Sprintf("%d", presenterUID), "uuid="+huyaUUID())
	} else {
		parts = append(parts, "u="+fmt.Sprintf("%d", convertUID))
	}
	return strings.Join(parts, "&")
}

// rotl64 低 32 位循环左移 8 位（虎牙 anticode 算法）
func rotl64(v int64) int64 {
	low := v & 0xFFFFFFFF
	rotated := ((low << 8) | (low >> 24)) & 0xFFFFFFFF
	return (v &^ 0xFFFFFFFF) | rotated
}

func huyaUUID() string {
	now := time.Now().UnixMilli()
	r := rand.Int63n(4294967295)
	return fmt.Sprintf("%d", (now%10000000000*1000+r)%4294967295)
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}