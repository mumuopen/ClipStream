// clipstream-record: 独立 FLV 录制器进程（从 serve 拉起，避免主进程 runtime 干扰）
// 用法: clipstream-record <FLV_URL> <输出目录> <分段秒数>
package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

func main() {
	if len(os.Args) < 4 {
		fmt.Println("用法: clipstream-record <FLV_URL> <输出目录> <分段秒数>")
		os.Exit(1)
	}
	playURL := os.Args[1]
	segDir := os.Args[2]
	segmentSec := 600
	fmt.Sscanf(os.Args[3], "%d", &segmentSec)

	if err := os.MkdirAll(segDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "创建目录失败: %v\n", err)
		os.Exit(1)
	}

	// Go HTTP 客户端自动跟 302 且保留 headers（与 ffmpeg 不同）
	// CDN 常见 302 → smtcdns.net，Go 能正确处理
	client := &http.Client{
		Timeout: 3600 * time.Second,
		Transport: &http.Transport{
			Proxy:             nil,
			ForceAttemptHTTP2: false,
			TLSNextProto:      make(map[string]func(string, *tls.Conn) http.RoundTripper),
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// 重定向时保留 Referer
			if req.Header.Get("Referer") == "" {
				req.Header.Set("Referer", "https://live.bilibili.com/")
			}
			if req.Header.Get("User-Agent") == "" {
				req.Header.Set("User-Agent", ua)
			}
			return nil // 允许跟随
		},
	}

	req, err := http.NewRequest("GET", playURL, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "构造请求失败: %v\n", err)
		os.Exit(1)
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Referer", "https://live.bilibili.com/")

	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接失败: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "CDN 返回 %d\n", resp.StatusCode)
		os.Exit(2) // 2=需要换 CDN 节点重试
	}

	fmt.Printf("CDN 已连接（%s），开始拉流\n", resp.Request.URL.Host)

	segIdx := 0
	segFile, err := os.Create(filepath.Join(segDir, fmt.Sprintf("seg_%03d.flv", segIdx)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "创建段文件失败: %v\n", err)
		os.Exit(1)
	}
	segStart := time.Now()
	defer segFile.Close()

	buf := make([]byte, 64*1024)
	var total int64
	for {
		if time.Since(segStart) >= time.Duration(segmentSec)*time.Second {
			_ = segFile.Close()
			segIdx++
			segFile, err = os.Create(filepath.Join(segDir, fmt.Sprintf("seg_%03d.flv", segIdx)))
			if err != nil {
				fmt.Fprintf(os.Stderr, "创建段文件失败: %v\n", err)
				os.Exit(1)
			}
			segStart = time.Now()
			fmt.Printf("段 %d 开始（累计 %.1fMB）\n", segIdx+1, float64(total)/(1<<20))
		}

		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := segFile.Write(buf[:n]); writeErr != nil {
				fmt.Fprintf(os.Stderr, "写段失败: %v\n", writeErr)
				os.Exit(1)
			}
			total += int64(n)
		}
		if readErr != nil {
			if readErr == io.EOF {
				fmt.Printf("流结束（EOF，共 %.1fMB，%d 段）\n", float64(total)/(1<<20), segIdx+1)
				os.Exit(0)
			}
			fmt.Fprintf(os.Stderr, "流中断（%.1fMB）: %v\n", float64(total)/(1<<20), readErr)
			os.Exit(3) // 3=流断，需重连
		}
	}
}
