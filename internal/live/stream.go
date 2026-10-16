package live

// 进程内 FLV 拉流核心（daemon 用）。
// 血统：从 cmd/clipstream-record 提炼，去掉子进程依赖——
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
//   - startSeq 起始段序号（断流重连时续写，避免覆盖已有段文件）
//   - onSeg 在每段开始写前回调（携带分段序号与累计字节数），用于上报 SegCount
//   - 返回累计分段数；err==nil 表示流正常 EOF 结束
//
// 注意：调用方需自行保证 playURL 已是最新可用地址；本函数不做 302 预解析
//（Go http.Client 自动跟重定向并保留 Referer/UA，预解析会额外消耗 CDN 会话）。
func StreamFLV(ctx context.Context, playURL, segDir string, segSec int, startSeq int, onSeg func(segIdx int, totalBytes int64)) (segCount int, total int64, err error) {
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

	segIdx := startSeq
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

	// FLV tag 感知写入：保证每段是完整独立的 FLV（头 + prevTagSize0 + 完整 tag 集合）。
	// 背景（2026-10 实测踩坑）：
	//   ① B站长连接 600s 分段——FLV 头只在流首出现一次，旧实现直接切文件导致后续段
	//     全是无头裸流（ffmpeg "Invalid data found"，whisper 抽音频即失败）；
	//   ② 分段边界在任意字节位——段首可能是半个 tag（虎牙段 probe 返回 0 的元凶）；
	//   ③ EOF/ctx 取消时最后写一半的 tag 留在段尾（合并/转写端的脏数据）。
	// 改为流式解析 tag（type 1B + size 3B + ts 4B + sid 3B + data sizeB + prevSize 4B），
	// 只写完整 tag，分段切换发生在 tag 边界，新段开头补写流头 + prevTagSize0。
	var flvHeader []byte // 流头 9B（首段以后每段开头补写）
	var pending []byte   // 跨 chunk 缓冲：凑齐一个完整 tag 才落盘
	buf := make([]byte, 64*1024)
	rotSeg := func() error {
		closeSeg()
		segIdx++
		f, err := os.Create(filepath.Join(segDir, fmt.Sprintf("seg_%03d.flv", segIdx)))
		if err != nil {
			return err
		}
		segFile = f
		segStart = time.Now()
		if onSeg != nil {
			onSeg(segIdx, total)
		}
		if flvHeader != nil {
			if _, err := segFile.Write(flvHeader); err != nil {
				return err
			}
			if _, err := segFile.Write([]byte{0, 0, 0, 0}); err != nil { // prevTagSize0
				return err
			}
		}
		return nil
	}
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if flvHeader == nil && len(pending) < 13 {
				// 攒到 13 字节再判定流头（header 9B + prevTagSize0 4B）
				pending = append(pending, buf[:n]...)
				if len(pending) >= 13 {
					if string(pending[:3]) == "FLV" {
						flvHeader = make([]byte, 9)
						copy(flvHeader, pending[:9])
						if _, werr := segFile.Write(pending[:13]); werr != nil { // 头 + prevTagSize0 归首段
							closeSeg()
							return segIdx + 1, total, fmt.Errorf("写段失败: %w", werr)
						}
						total += 13
						pending = pending[13:]
					}
					// 非 "FLV" 开头：无头流（未见过的 CDN 形态），退化为透传（同旧行为）
				}
			} else {
				pending = append(pending, buf[:n]...)
			}
			if flvHeader == nil && len(pending) >= 13 {
				// 无头流透传：pending 是已攒的原始字节，直接写
				if _, werr := segFile.Write(pending); werr != nil {
					closeSeg()
					return segIdx + 1, total, fmt.Errorf("写段失败: %w", werr)
				}
				total += int64(len(pending))
				pending = pending[:0]
			}
			// tag 模式：组装完整 tag 再写，分段切换只在 tag 边界
			for flvHeader != nil && len(pending) >= 15 {
				size := int(uint32(pending[1])<<16 | uint32(pending[2])<<8 | uint32(pending[3]))
				tagLen := 11 + size + 4
				if size > 32<<20 {
					closeSeg()
					return segIdx + 1, total, fmt.Errorf("FLV tag 异常 size=%d（流损坏）", size)
				}
				if len(pending) < tagLen {
					break // 数据不足，等下一 chunk
				}
				if segSec > 0 && time.Since(segStart) >= time.Duration(segSec)*time.Second {
					if err := rotSeg(); err != nil {
						return segIdx, total, fmt.Errorf("创建段文件失败: %w", err)
					}
				}
				if _, werr := segFile.Write(pending[:tagLen]); werr != nil {
					closeSeg()
					return segIdx + 1, total, fmt.Errorf("写段失败: %w", werr)
				}
				total += int64(tagLen)
				pending = pending[tagLen:]
				if len(pending) == 0 && cap(pending) > 256*1024 {
					pending = make([]byte, 0, 64*1024) // 闲置时释放大缓冲
				}
			}
		}
		if readErr != nil {
			closeSeg()
			if ctx.Err() != nil {
				return segIdx + 1, total, ctx.Err()
			}
			if readErr == io.EOF {
				// 尾部未凑齐的 tag 随 pending 丢弃（不再产生截断尾脏数据）
				return segIdx + 1, total, nil
			}
			return segIdx + 1, total, fmt.Errorf("流中断: %w", readErr)
		}
	}
}