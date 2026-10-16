// Package biliapi B站原生 API 客户端。
// 扫码登录协议与 biliup-rs（MIT）保持一致：TV 端二维码登录走
//   生成: POST https://passport.bilibili.com/x/passport-tv-login/qrcode/auth_code
//   轮询: POST https://passport.bilibili.com/x/passport-tv-login/qrcode/poll
// 签名 = md5(form-urlencoded + appsec)；参数按字母序编码（serde_urlencoded 语义，
// 与 Go url.Values.Encode() 一致）。落盘的 cookies.json 与 biliup LoginInfo 结构
// 兼容（上传引擎过渡期仍由 biliup 子进程消费该文件）。
package biliapi

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// BiliTV 平台凭证（biliup-rs AppKeyStore::BiliTV）
const (
	biliTVAppKey = "4409e2ce8ffd12b8"
	biliTVAppSec = "59b43e04ad6965f34319062b478f83dd"
	biliPlatform = "BiliTV"
)

// 轮询状态码（B站 passport 语义）
const (
	pollOK      = 0     // 登录成功
	pollWaiting = 86039 // 二维码尚未确认（未扫/已扫未确认）
	pollExpired = 86038 // 二维码已失效
)

// TVLoginInfo biliup cookies.json 兼容结构（biliup-rs LoginInfo）
type TVLoginInfo struct {
	CookieInfo json.RawMessage `json:"cookie_info"`
	SSO        []string        `json:"sso"`
	TokenInfo  TVTokenInfo     `json:"token_info"`
	Platform   string          `json:"platform"`
}

// TVTokenInfo 账号 token（biliup renew/上传用）
type TVTokenInfo struct {
	AccessToken  string `json:"access_token"`
	ExpiresIn    uint32 `json:"expires_in"`
	MID          uint64 `json:"mid"`
	RefreshToken string `json:"refresh_token"`
}

type response struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type authCodeData struct {
	AuthCode string `json:"auth_code"`
	URL      string `json:"url"`
}

var httpClient = &http.Client{
	Timeout: 15 * time.Second,
}

func postForm(endpoint string, form url.Values, out any) error {
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", "https://www.bilibili.com/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/63.0.3239.108")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("B站接口返回 %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// sign MD5(参数串 + appsec)，与 biliup-rs Credential::sign 一致
func sign(form url.Values) string {
	sum := md5.Sum([]byte(form.Encode() + biliTVAppSec))
	return hex.EncodeToString(sum[:])
}

func tvBaseForm() url.Values {
	form := url.Values{}
	form.Set("appkey", biliTVAppKey)
	form.Set("local_id", "0")
	form.Set("ts", strconv.FormatInt(time.Now().Unix(), 10))
	return form
}

// QRGenerate 生成扫码登录二维码：返回 auth_code（轮询凭证）与二维码内容 URL
func QRGenerate() (authCode, qrURL string, err error) {
	form := tvBaseForm()
	form.Set("sign", sign(form))
	var res response
	if err := postForm("https://passport.bilibili.com/x/passport-tv-login/qrcode/auth_code", form, &res); err != nil {
		return "", "", err
	}
	if res.Code != 0 {
		return "", "", fmt.Errorf("生成二维码失败（code=%d %s）", res.Code, res.Message)
	}
	var d authCodeData
	if err := json.Unmarshal(res.Data, &d); err != nil || d.AuthCode == "" || d.URL == "" {
		return "", "", fmt.Errorf("二维码响应异常: %s", string(res.Data))
	}
	return d.AuthCode, d.URL, nil
}

// QRPollState 一次轮询的结果状态
type QRPollState struct {
	OK      bool        // 登录成功（Info 有效）
	Waiting bool        // 等待扫码/确认
	Expired bool        // 二维码已失效
	Info    TVLoginInfo // OK 时的完整登录信息
	Message string      // 透出文案
}

// QRPoll 轮询扫码状态（不重试，由调用方按节奏调用）
func QRPoll(authCode string) (*QRPollState, error) {
	form := tvBaseForm()
	form.Set("auth_code", authCode)
	form.Set("sign", sign(form))
	var res response
	if err := postForm("https://passport.bilibili.com/x/passport-tv-login/qrcode/poll", form, &res); err != nil {
		return nil, err
	}
	switch res.Code {
	case pollOK:
		info := TVLoginInfo{Platform: biliPlatform}
		if err := json.Unmarshal(res.Data, &info); err != nil || len(info.CookieInfo) == 0 {
			return nil, fmt.Errorf("登录响应解析失败: %s", string(res.Data))
		}
		return &QRPollState{OK: true, Message: "登录成功", Info: info}, nil
	case pollWaiting:
		return &QRPollState{Waiting: true, Message: "等待扫码确认"}, nil
	case pollExpired:
		return &QRPollState{Expired: true, Message: "二维码已失效，请重新生成"}, nil
	default:
		return nil, fmt.Errorf("轮询失败（code=%d %s）", res.Code, res.Message)
	}
}

// Nickname 取扫码账号昵称：TV 登录的 cookie_info 里含 web cookie（SESSDATA 等），
// 用它调 web 端导航接口拿昵称；失败时返回空串（调用方 fallback 用 mid）。
func Nickname(info TVLoginInfo) string {
	var ci struct {
		Cookies []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"cookies"`
	}
	if err := json.Unmarshal(info.CookieInfo, &ci); err != nil {
		return ""
	}
	var sess string
	for _, c := range ci.Cookies {
		if c.Name == "SESSDATA" {
			sess = c.Value
			break
		}
	}
	if sess == "" {
		return ""
	}
	req, err := http.NewRequest(http.MethodGet, "https://api.bilibili.com/x/web-interface/nav", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Cookie", "SESSDATA="+sess)
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/63.0.3239.108")
	resp, err := httpClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var nav struct {
		Code int `json:"code"`
		Data struct {
			MID      uint64 `json:"mid"`
			Nickname string `json:"nickname"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&nav); err != nil || nav.Code != 0 {
		return ""
	}
	return nav.Data.Nickname
}
