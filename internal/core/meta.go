package core

// MergedSegMeta 合并产物侧车元数据（live daemon 在 mergeAndQueue 时写，
// pipeline 的 barrageHotspots 读——弹幕墙钟时间 → 合并产物内容轴偏移的映射依据）。
// 背景：虎牙碎段流的内容轴有重连空洞（墙钟 ~5.3s/段但内容仅 ~2.2s），
// 弹幕桶是墙钟时间戳，直接相减会错位近一倍，需按段级映射换算。
type MergedSegMeta struct {
	Start int64       `json:"start"` // 组窗口墙钟起点（unix 秒，≈首段内容开始）
	End   int64       `json:"end"`   // 组窗口墙钟终点（unix 秒，≈末段完成时刻）
	Segs  []MergedSeg `json:"segs"`  // 组内各段（按拼接顺序）
}

// MergedSeg 单个源段在合并产物中的位置信息
type MergedSeg struct {
	Name   string  `json:"name"`   // 段文件名（仅展示/调试用）
	Wall   int64   `json:"wall"`   // 段完成时刻（mtime，unix 秒）——段墙钟区间终点
	Offset float64 `json:"offset"` // 段内容在合并产物中的起点（内容轴，秒）
	Dur    float64 `json:"dur"`    // 段真实内容时长（秒，tag 时间戳差）
}
