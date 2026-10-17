package live

// 碎段合并（FLV tag 级拼接）测试。
// 背景：虎牙 CDN 短连接流带非标 script/text 轨（ffmpeg FLV demuxer 解析即错位），
// 且段尾截断是常态，故 mergeAndQueue 改为 Go 原生 tag 拼接（剥 script 轨 + 断链止损）。
// - TestConcatFLV：合成 FLV 覆盖剥离/止损/拼接纯逻辑（CI 可跑）
// - TestMergeAndQueueRealData：真实虎牙碎段目录的端到端验证（默认 Skip，
//   环境变量 CLIPSTREAM_MERGE_E2E=1 开启；同时用于线下重放送切）

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"chopperbot/internal/config"
)

// makeFLV 生成合成 FLV：header + [script?] + n 个音视频交替 tag + [可选截断尾]
func makeFLV(t *testing.T, path string, n int, withScript, truncatedTail bool) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("创建 %s: %v", path, err)
	}
	defer f.Close()
	f.Write([]byte{'F', 'L', 'V', 0x01, 0x05, 0, 0, 0, 9}) // header
	binary.Write(f, binary.BigEndian, uint32(0))            // prevTagSize0
	writeTag := func(typ byte, size int, ts uint32) {
		hdr := []byte{typ, byte(size >> 16), byte(size >> 8), byte(size),
			byte(ts >> 16), byte(ts >> 8), byte(ts), byte(ts >> 24), 0, 0, 0}
		f.Write(hdr)
		data := make([]byte, size)
		for i := range data {
			data[i] = typ // 填充可识别内容
		}
		f.Write(data)
		binary.Write(f, binary.BigEndian, uint32(11+size)) // prevTagSize
	}
	ts := uint32(1000)
	if withScript {
		writeTag(18, 32, ts) // script 轨（应被剥离）
	}
	for i := 0; i < n; i++ {
		writeTag(byte(8+i%2), 64, ts) // audio/video 交替
		ts += 40
	}
	if truncatedTail {
		// 声明 200B data 但只写 100B（模拟 CDN 断流截断）
		f.Write([]byte{9, 0x00, 0x00, 0xC8, 0, 0, 0, 0, 0, 0, 0})
		f.Write(make([]byte, 100))
	}
}

func TestConcatFLV(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "seg_000.flv")
	b := filepath.Join(dir, "seg_001.flv")
	makeFLV(t, a, 4, true, true)  // 带 script + 截断尾
	makeFLV(t, b, 3, false, true) // 无 script + 截断尾

	out := filepath.Join(dir, "merged.flv")
	n, meta, err := concatFLV([]string{a, b}, out, nil)
	if err != nil {
		t.Fatalf("concatFLV: %v", err)
	}
	// 4 + 3 个音视频 tag，各文件截断尾均应被丢弃；script tag 应被剥掉
	if n != 7 {
		t.Fatalf("期望拼接 7 个 tag，实际 %d", n)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("读产物: %v", err)
	}
	if len(data) < 13 || string(data[:3]) != "FLV" {
		t.Fatalf("产物 FLV header 损坏")
	}
	// 时间戳归一化验证：两段（tag ts 均从 1000 起，段间本有 ts 倒退"空洞"），
	// 产物 ts 必须从 0 起、单调不减且续接（段1 末=120ms，段2 首 tag 应=120ms）。
	// 无归一化时段2 ts 回到 1000 造成倒退——LLM 高光区间错位 + Cut 缺斤短两的根源
	var tsList []uint32
	for pos := 13; pos+11 <= len(data); {
		typ := data[pos]
		if typ != 8 && typ != 9 {
			t.Fatalf("产物中混入了非音视频 tag（type=%d）@offset %d", typ, pos)
		}
		size := int(uint32(data[pos+1])<<16 | uint32(data[pos+2])<<8 | uint32(data[pos+3]))
		ts := uint32(data[pos+4])<<16 | uint32(data[pos+5])<<8 | uint32(data[pos+6]) |
			uint32(data[pos+7])<<24
		tsList = append(tsList, ts)
		pos += 11 + size + 4
	}
	if len(tsList) != 7 {
		t.Fatalf("ts 校验的 tag 数 %d ≠ 7", len(tsList))
	}
	if tsList[0] != 0 {
		t.Fatalf("产物首 tag ts=%d，归一化后应为 0", tsList[0])
	}
	for i := 1; i < len(tsList); i++ {
		if tsList[i] < tsList[i-1] {
			t.Fatalf("ts 倒退 @%d: %d < %d（段间空洞未归一化）", i, tsList[i], tsList[i-1])
		}
	}
	if tsList[4] != 120 { // 段1 4 tag（0..120ms），段2 首 tag 续接 120
		t.Fatalf("段2 首 tag ts=%d，应续接在段1 末尾 120", tsList[4])
	}
	if len(meta.Segs) != 2 || meta.Segs[1].Offset < 0.11 || meta.Segs[1].Offset > 0.13 {
		t.Fatalf("meta 段偏移异常: %+v", meta.Segs)
	}
}

func TestConcatFLVBrokenChain(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "seg_000.flv")
	makeFLV(t, a, 2, false, false)
	// 手工破坏：把第一个 tag 尾的 prevTagSize 改错（链断裂，其后字节应全部止损丢弃）
	data, _ := os.ReadFile(a)
	data[13+11+64] = 0xFF // prevTagSize 高位（tag data size=64，尾偏移 = 13+11+64）
	os.WriteFile(a, data, 0o644)
	out := filepath.Join(dir, "merged.flv")
	n, _, err := concatFLV([]string{a}, out, nil)
	if err != nil {
		t.Fatalf("concatFLV: %v", err)
	}
	if n != 0 {
		t.Fatalf("链断裂应止损（期望 0 tag），实际 %d", n)
	}
}

// TestMergeAndQueueRealData 用真实虎牙碎段目录做端到端验证（同时承担线下重放送切）。
// 默认 Skip：依赖本机 temp_server 录制数据。CLIPSTREAM_MERGE_E2E=1 开启。
// 路径语义与生产 daemon 完全一致：Chdir 到 temp_server，DataDir=./data，
// 队列任务写入相对路径（serve 消费时同样以 temp_server 为基准）。
func TestMergeAndQueueRealData(t *testing.T) {
	if os.Getenv("CLIPSTREAM_MERGE_E2E") != "1" {
		t.Skip("跳过：设置 CLIPSTREAM_MERGE_E2E=1 用真实录制数据验证碎段合并")
	}
	const tempServer = "../../temp_server"
	cases := []struct {
		room, title string
		minSegs     int // 碎段流要求 >=20；B站裸段重放仅要求 >=5
	}{
		{"660000_20261015_164249", "WE vs BRO 德玛西亚杯淘汰赛", 20},
		{"998_20261015_164249", "新主播，双5090顶尖画质！", 20},
		{"7734200_20261015_164248", "【直播】WE vs BRO", 5},
		{"660000_20261016_101612", "KC vs MVK S16入围赛", 20}, // 今日停滞会话手动重放
	}
	oldWD, _ := os.Getwd()
	if err := os.Chdir(tempServer); err != nil {
		t.Skipf("本机无 temp_server 目录: %v", err)
	}
	defer os.Chdir(oldWD)

	for _, c := range cases {
		c := c
		t.Run(c.room, func(t *testing.T) {
			dir := filepath.Join("./data/live", c.room)
			if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
				t.Skipf("无录制目录 %s", dir)
			}
			cfg := &config.Config{DataDir: "./data"}
			if os.Getenv("CLIPSTREAM_MERGE_DRY_QUEUE") == "1" {
				// 干跑模式：queue 任务写临时目录（重放已有队列任务引用的文件时
				// 避免重复入队），merged 产物路径不受影响（segDir 是显式参数）
				cfg.DataDir = filepath.Join(os.TempDir(), "clipstream-merge-test-queue")
				_ = os.MkdirAll(filepath.Join(cfg.DataDir, "live", "queue"), 0o755)
			}
			cfg.Live.SegmentSec = 600
			d := &Daemon{cfg: cfg}
			segs := listSegFiles(dir)
			if len(segs) < c.minSegs {
				t.Fatalf("测试数据异常：仅 %d 段（期望 >=%d）", len(segs), c.minSegs)
			}
			merged, err := d.mergeAndQueue(segs, c.title, dir)
			if err != nil {
				t.Fatalf("mergeAndQueue: %v", err)
			}
			if merged <= 0 {
				t.Fatalf("未产出任何合并段")
			}
			// 抽验第一组合并产物：存在、FLV 头合法、tag 数量与输入量级一致
			outs, _ := filepath.Glob(filepath.Join(dir, "merged_*.flv"))
			if len(outs) != merged {
				t.Fatalf("合并产物数 %d 与返回值 %d 不一致", len(outs), merged)
			}
			first := outs[0]
			fi, err := os.Stat(first)
			if err != nil || fi.Size() < 64<<10 {
				t.Fatalf("合并产物 %s 异常（大小 %v）", first, fi.Size())
			}
			head := make([]byte, 3)
			fh, _ := os.Open(first)
			fh.Read(head)
			fh.Close()
			if string(head) != "FLV" {
				t.Fatalf("合并产物 %s 非 FLV", first)
			}
			t.Logf("房间 %s：%d 段 → %d 个合并段（首组 %s，%dKB）",
				c.room, len(segs), merged, filepath.Base(first), fi.Size()/1024)
		})
	}
}
