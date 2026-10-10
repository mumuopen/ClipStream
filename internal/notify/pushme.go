package notify

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"chopperbot/internal/config"
)

// Pushme 自建 pushme 通知（对齐 yafoo/pushme-server 协议：GET /?push_key=..&title=..&content=..）
// 未启用或未配置时所有方法安静返回，管线零感知
type Pushme struct {
	cfg  config.Pushme
	http *http.Client
}

func NewPushme(cfg config.Pushme) *Pushme {
	return &Pushme{cfg: cfg, http: &http.Client{Timeout: 10 * time.Second}}
}

// Enabled 是否已启用且配置完整
func (n *Pushme) Enabled() bool {
	return n != nil && n.cfg.Enable && n.cfg.URL != "" && n.cfg.PushKey != ""
}

// Send 推送通知（失败仅返回错误，由调用方决定是否记日志）
func (n *Pushme) Send(title, content string) error {
	if !n.Enabled() {
		return nil
	}
	u := strings.TrimSuffix(n.cfg.URL, "/") + "/"
	q := url.Values{}
	q.Set("push_key", n.cfg.PushKey)
	q.Set("title", title)
	q.Set("content", content)
	full := u + "?" + q.Encode()
	resp, err := n.http.Get(full)
	if err != nil {
		return fmt.Errorf("pushme 推送失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pushme 返回 %d", resp.StatusCode)
	}
	return nil
}

// Notify 管线回调入口：发送并吞掉错误（通知失败不应影响主流程）
func (n *Pushme) Notify(title, content string) {
	if err := n.Send(title, content); err != nil {
		// 通知失败只打不到 stderr，不进任务日志（任务日志由管线层写）
		fmt.Printf("[notify] %v\n", err)
	}
}
