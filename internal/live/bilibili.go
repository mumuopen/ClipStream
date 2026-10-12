// Package live M4 直播链：开播监控 + FLV 录制 + 断流重连 + 下播自动接切片管线
// 血统：对齐原版 ChopperBot 的 hot/live 模块（LiverFollower + FlvHandle）
package live

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

const bilibiliUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

var roomIDRe = regexp.MustCompile(`live\.bilibili\.com/(?:h5/)?(\d+)`)

// ExtractRoomID 从直播间 URL 提取房间号（支持 live.bilibili.com/6、/h5/6、带查询参数）
func ExtractRoomID(rawURL string) (string, error) {
	if m := roomIDRe.FindStringSubmatch(rawURL); len(m) == 2 {
		return m[1], nil
	}
	return "", fmt.Errorf("无法从 %q 识别 B站房间号（形如 https://live.bilibili.com/6）", rawURL)
}

// RoomInfo 直播间状态
type RoomInfo struct {
	RoomID     string
	Title      string
	LiveStatus int // 1=直播中 0=未开播 2=轮播中
	Cover      string
}

// GetRoomInfo 开播检测（API：room/v1/Room/get_info，国内直连）
func GetRoomInfo(ctx context.Context, roomID string) (*RoomInfo, error) {
	u := "https://api.live.bilibili.com/room/v1/Room/get_info?room_id=" + url.QueryEscape(roomID)
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			RoomID     int    `json:"room_id"`
			Title      string `json:"title"`
			LiveStatus int    `json:"live_status"`
			UserCover  string `json:"user_cover"`
		} `json:"data"`
	}
	if err := apiJSON(ctx, u, &out); err != nil {
		return nil, err
	}
	if out.Code != 0 {
		return nil, fmt.Errorf("B站 get_info code=%d: %s", out.Code, out.Message)
	}
	return &RoomInfo{
		RoomID:     fmt.Sprintf("%d", out.Data.RoomID),
		Title:      out.Data.Title,
		LiveStatus: out.Data.LiveStatus,
		Cover:      out.Data.UserCover,
	}, nil
}

// GetPlayURL 获取 FLV 播放地址（API：xlive/web-room/v2/index/getRoomPlayInfo）
// 完整地址 = url_info.host + codec.base_url + url_info.extra（2026-10 实测契约）
func GetPlayURL(ctx context.Context, roomID string) (string, error) {
	u := "https://api.live.bilibili.com/xlive/web-room/v2/index/getRoomPlayInfo?room_id=" +
		url.QueryEscape(roomID) + "&protocol=0,1&format=0,1,2&codec=0,1&qn=0&platform=web&ptype=16"
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			PlayurlInfo struct {
				Playurl struct {
					Stream []struct {
						ProtocolName string `json:"protocol_name"`
						Format       []struct {
							FormatName string `json:"format_name"`
							Codec      []struct {
								CodecName string `json:"codec_name"`
								BaseURL   string `json:"base_url"`
								URLInfo   []struct {
									Host  string `json:"host"`
									Extra string `json:"extra"`
								} `json:"url_info"`
							} `json:"codec"`
						} `json:"format"`
					} `json:"stream"`
				} `json:"playurl"`
			} `json:"playurl_info"`
		} `json:"data"`
	}
	if err := apiJSON(ctx, u, &out); err != nil {
		return "", err
	}
	if out.Code != 0 {
		return "", fmt.Errorf("B站 getRoomPlayInfo code=%d: %s", out.Code, out.Message)
	}
	// 选 http_stream(FLV) + avc 编码（兼容性最好）
	// 注意：不做预解析 302——Go 的 http.Client 自动跟重定向且保留 headers；
	// 预解析会额外消耗 CDN 会话导致录制连接被节流（实测教训）
	for _, st := range out.Data.PlayurlInfo.Playurl.Stream {
		if st.ProtocolName != "http_stream" {
			continue
		}
		for _, f := range st.Format {
			if f.FormatName != "flv" {
				continue
			}
			for _, c := range f.Codec {
				if c.CodecName != "avc" || len(c.URLInfo) == 0 {
					continue
				}
				ui := c.URLInfo[0]
				full := ui.Host + c.BaseURL + ui.Extra
				if full != "" {
					return full, nil
				}
			}
		}
	}
	return "", fmt.Errorf("房间 %s 未返回可用的 FLV 流（可能需登录/风控）", roomID)
}

// resolveRedirect 预跟踪 HTTP 重定向，返回最终直连 URL
// B站 CDN 节点常见 302 → 腾讯 CDN(smtcdns.net)；ffmpeg 跟重定向不带 Referer 导致 403
func resolveRedirect(ctx context.Context, rawURL string) (string, error) {
	c := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy: nil, // B站 CDN 国内直连不走代理
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 5 {
				return fmt.Errorf("重定向超过 5 次")
			}
			// 重定向时保留 Referer（CDN 校验）
			if req.Header.Get("Referer") == "" {
				req.Header.Set("Referer", "https://live.bilibili.com/")
			}
			if req.Header.Get("User-Agent") == "" {
				req.Header.Set("User-Agent", bilibiliUA)
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", bilibiliUA)
	req.Header.Set("Referer", "https://live.bilibili.com/")
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("CDN 返回 %d", resp.StatusCode)
	}
	// 只取最终 URL，关连接不下载数据
	return resp.Request.URL.String(), nil
}

// Referer 返回请求流地址时应带的 Referer
func Referer(roomID string) string {
	return "https://live.bilibili.com/" + roomID
}

func apiJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", bilibiliUA)
	req.Header.Set("Referer", "https://live.bilibili.com")
	c := &http.Client{Timeout: 15 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("B站 API 请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("B站 API 响应解析失败: %w（原文: %.200s）", err, string(raw))
	}
	return nil
}
