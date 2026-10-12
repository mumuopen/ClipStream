package live

// 进程内 FLV 拉流核心（daemon 用）。
// 血统：从 cmd/guanclip-record 提炼，去掉子进程依赖——
// Windows 上 serve 进程拉起的子进程无法做 FLV 流式 IO（实测），
// 独立进程内直接拉流则 100% 正常，因此录制守护进程在自身进程内完成拉流。

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// StreamFLV 拉取 FLV 流并分段写入 segDir，阻塞直到 ctx 取消或流结束。
//   - onSeg 在每段开始写前回调（携带分段序号与累计字节数），用于上报 SegCount
//   - 返回累计分段数；err==nil 表示流正常 EOF 结束
//
// 注意：调用方需自行保证 playURL 已是最新可用地址；本函数不做 302 预解析
//（Go http.Client 自动跟重定向并保留 Referer/UA，预解析会额外消耗 CDN 会话）。
func StreamFLV(ctx context.Context, playURL, segDir string, segSec int, onSeg func(segIdx int, totalBytes int64)) (segCount int, total int64, err error) {
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		return 0, 0, err
	}

	client := &http.Client{
		Timeout: 3600 * time.Second,
		Transport: &http.Transport{
			Proxy:             nil, // B站 CDN 国内直连不走代理
			ForceAttemptHTTP2: false,
			TLSNextProto:      make(map[string]func(string, *tls.Conn) http.RoundTripper),
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.Header.Get("Referer") == "" {
				req.Header.Set("Referer", "https://live.bilibili.com/")
			}
			if req.Header.Get("User-Agent") == "" {
				req.Header.Set("User-Agent", bilibiliUA)
			}
			return nil
		},
	}

	req, err := http.NewRequestWithContext(ctx, "GET", playURL, nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("User-Agent", bilibiliUA)
	req.Header.Set("Referer", "https://live.bilibili.com/")

	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, 0, ctx.Err()
		}
		return 0, 0, fmt.Errorf("CDN 连接失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("CDN 返回 %d（需要换节点重试）", resp.StatusCode)
	}

	segIdx := 0
	segFile, err := os.Create(filepath.Join(segDir, fmt.Sprintf("seg_%03d.flv", segIdx)))
	if err != nil {
		return 0, 0, fmt.Errorf("创建段文件失败: %w", err)
	}
	segStart := time.Now()
	closeSeg := func() {
		_ = segFile.Close()
	}
	// 首段就绪即回调（让 SegCount 从 1 开始显示，而非等第一个分段完成）
	if onSeg != nil {
		onSeg(segIdx, 0)
	}

	buf := make([]byte, 64*1024)
	for {
		// 分段切换
		if time.Since(segStart) >= time.Duration(segSec)*time.Second {
			closeSeg()
			segIdx++
			segFile, err = os.Create(filepath.Join(segDir, fmt.Sprintf("seg_%03d.flv", segIdx)))
			if err != nil {
				return segIdx, total, fmt.Errorf("创建段文件失败: %w", err)
			}
			segStart = time.Now()
			if onSeg != nil {
				onSeg(segIdx, total)
			}
		}

		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := segFile.Write(buf[:n]); werr != nil {
				closeSeg()
				return segIdx + 1, total, fmt.Errorf("写段失败: %w", werr)
			}
			total += int64(n)
		}
		if readErr != nil {
			closeSeg()
			if ctx.Err() != nil {
				return segIdx + 1, total, ctx.Err()
			}
			if readErr == io.EOF {
				return segIdx + 1, total, nil
			}
			return segIdx + 1, total, fmt.Errorf("流中断: %w", readErr)
		}
	}
}