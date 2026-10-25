package config

import (
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	DataDir string  `yaml:"data_dir"`
	Proxy   string  `yaml:"proxy"`
	Engines Engines `yaml:"engines"`
	LLM     LLM     `yaml:"llm"`
	Web     Web     `yaml:"web"`
	Publish Publish `yaml:"publish"`
	Notify  Notify  `yaml:"notify"`
	Chrome  Chrome  `yaml:"chrome"`
	Live      Live      `yaml:"live"`
	Retention Retention `yaml:"retention"`
	AutoReview AutoReview `yaml:"auto_review"`
}

type Engines struct {
	YtDlp  string `yaml:"ytdlp"` // 可执行文件路径或命令名
	FFmpeg string `yaml:"ffmpeg"`
	Probe  string `yaml:"ffprobe"`
	Biliup string `yaml:"biliup"` // B站投稿 CLI 引擎（github.com/biliup/biliup）
	ASR    ASR    `yaml:"asr"`
}

type ASR struct {
	// 转写引擎：目前支持 whisper（whisper.cpp 的 whisper-cli 二进制）
	Binary string `yaml:"binary"`
	Model  string `yaml:"model"`
}

type LLM struct {
	BaseURL  string `yaml:"base_url" json:"base_url"` // OpenAI 兼容端点，默认 https://api.openai.com/v1
	APIKey   string `yaml:"api_key" json:"api_key"`
	Model    string `yaml:"model" json:"model"`
	Thinking string `yaml:"thinking" json:"thinking"` // V4 系思考模式：disabled=关（批量打分更快更省）/ enabled=开 / 留空=不传参（端点默认）
	Proxy    string `yaml:"proxy" json:"proxy"` // 可选：LLM 专用代理
}

type Web struct {
	Listen string `yaml:"listen"`
}

type Publish struct {
	Headless     bool   `yaml:"headless"`      // 抖音浏览器发布是否无头（默认有头，风控更稳）
	Screenshots  bool   `yaml:"screenshots"`   // 发布前/后截图存档
	PlatformsDir string `yaml:"platforms_dir"` // 平台选择器配置目录
	// 发布窗口（如 "10:00-23:00"）：窗口外不派发（夜里发没人看，产出即刻洪峰
	// 也是机器签名）。空 = 全天可发。支持跨夜（"22:00-06:00"）
	ActiveHours string    `yaml:"active_hours"` // 发布窗口
	RateLimit   RateLimit `yaml:"rate_limit"`   // 发布节流（防风控：高频投稿限流/封号）
}

// RateLimit 发布节流三闸：发布间隔 / 每日上限 / 同时在途上限。
// 每道闸 0 = 不限制（用户按需开关：求快放开 daily_max、怕风控收紧 interval）。
// 节流命中时挡账号不挡发布（被拦账号让位给次优账号，都被拦则条目留池等下轮）。
// 手动发布同受节流（间隔/在途），计数含失败尝试但不含 dry-run 演练。
// （旧字段 batch_max 已随"定时计划→发布泵"重构废弃：泵模型下每个 tick
// 至多派 1 条，yaml 里残留的 batch_max 会被忽略）
type RateLimit struct {
	// 同账号两次派发最小间隔分钟（默认 10）：手动发布也计入；叠加 ±5% 抖动
	MinIntervalMin int `yaml:"min_interval_min" json:"min_interval_min"`
	// 每账号每日最大发布数（默认 9，0=不限）：全天总量闸——9 条 × 10min
	// ≈ 窗口开头 90 分钟发完当日配额
	DailyMax int `yaml:"daily_max" json:"daily_max"`
	// 同账号同时在途（running）发布任务上限（默认 9，0=不限）：10min 间隔下
	// 上传耗时 >10min 时会多条并发，这道闸是"不要同时推送超过 N 条"的保险丝
	MaxInflight int `yaml:"max_inflight" json:"max_inflight"`
	// 风控冷却基础时长（小时，默认 24，0=用默认）：平台限流（B站 137022"投稿
	// 过于频繁"）触发账号级熔断的冷却基准，第 N 次触发翻倍（24→48→96h，上限 7 天）；
	// 冷却到点自动恢复，也可在发布中心手动恢复
	RiskCooldownHours int `yaml:"risk_cooldown_hours" json:"risk_cooldown_hours"`
}

type Notify struct {
	Pushme Pushme `yaml:"pushme"`
}

type Pushme struct {
	Enable  bool   `yaml:"enable" json:"enable"`     // 总开关
	URL     string `yaml:"url" json:"url"`            // 自建 pushme 服务地址，如 http://server:3100/
	PushKey string `yaml:"push_key" json:"push_key"` // 客户端生成的 push_key（手机 App 订阅的那个）
}

type Chrome struct {
	Path string `yaml:"path"` // 浏览器路径（Chrome 或 Edge 均可），留空自动探测：Chrome → 系统 Edge；都找不到报错，绝不自动下载
}

type Live struct {
	CheckIntervalSec int  `yaml:"check_interval" json:"check_interval"` // 开播检测轮询间隔，默认 60
	SegmentSec       int  `yaml:"segment_time" json:"segment_time"`     // 录制分段时长，默认 600
	AutoClip         bool `yaml:"auto_clip" json:"auto_clip"`           // 下播后自动将分段送入切片管线
	// 素材停滞兜底（秒）：录制会话超过该时长无新分段即强制结束（下播/API 假在播防挂死）
	MaxStaleSec int `yaml:"max_stale_sec" json:"max_stale_sec"`
	// 预告态关键词：标题含任一关键词时不视为真开播（不录制）；录制中标题
	// 变为预告态也自动收工。赛事房白天轮播"预告"画面但 API 仍报在播——
	// 码率与真播无异（实测段均 764KB），只有标题能区分。默认 ["预告","凌晨见"]
	PreviewKeywords []string `yaml:"preview_keywords" json:"preview_keywords"`
	// 池背上（backpressure）：pending 待发池 ≥ PoolHigh 时暂停录制（新开播
	// 不再起会话；下播分段不送切，素材留在会话目录由磁盘治理兑底），降回
	// PoolLow 以下自动恢复。防“生成速度>发布速度”的慢性积压——赛事切片
	// 隔周发=白发，且 pending 引用的成品目录清不掉，磁盘与 LLM 成本白烧。
	// PoolHigh=0 关闭。默认 20/10（约 3 场直播的量，滞回防临界抖动）
	PoolHigh int `yaml:"pool_high" json:"pool_high"` // 触发水位（默认 20）
	PoolLow  int `yaml:"pool_low" json:"pool_low"`  // 解除水位（默认 high/2）
}

// Retention 磁盘清理策略（挂机刚需：数据流入-流出平衡）
type Retention struct {
	Enabled bool `yaml:"enabled" json:"enabled"` // 总开关（daemon 内定时清理）
	// 保留天数：超过的直播分段目录整个删除（挂机磁盘有限的最重要防线）
	SegmentsDays int `yaml:"segments_days" json:"segments_days"` // data/live/ 会话目录，默认 7 天
	// 已入池/已派发的源分段：切片已完成使命，更积极清理（默认 1 天）
	ConsumedSegDays int `yaml:"consumed_seg_days" json:"consumed_seg_days"`
	// 磁盘水位：剩余百分比低于阈值时预警 + 自动清理最旧会话
	MinFreePercent int `yaml:"min_free_percent" json:"min_free_percent"` // 默认 10
	// 清理检查间隔（分钟）
	CheckMinutes int `yaml:"check_minutes" json:"check_minutes"` // 默认 60
	// 目录占用上限（整盘百分比，默认 50）：ClipStream 数据目录占用超过
	// 盘容量的一半即从最旧录制会话删起（水位守的是“盘快满”，占用上限
	// 守的是“这块盘我只肯给你一半”，两者互补）。0 = 关闭占用上限治理
	MaxUsagePercent int `yaml:"max_usage_percent" json:"max_usage_percent"`
	// 已发布切片成品保留天数（data/jobs/，默认 3，0 = 不自动清）：
	// 成品与发布截图只进不出是磁盘长尾膨胀主因；到期且池条目已全部终态
	// （无 pending/dispatched 引用）才删磁盘目录，DB 任务行保留可查
	PublishedKeepDays int `yaml:"published_keep_days" json:"published_keep_days"`
}

// AutoReview 自动审核（挂机无人值守：切片完成不再等人工，分级闸门自动入池）
type AutoReview struct {
	Enabled bool `yaml:"enabled" json:"enabled"` // 总开关（关=全部进人工审核台，现状行为）
	// 自动入池的信任门槛：LLM 评分 ≥ MinScore（等分回退切片无评分，不会自动过）
	MinScore int `yaml:"min_score" json:"min_score"` // 默认 60
	// 硬闸——任一命中直接拒绝（不入池也不留审，标记 failed 留档）：
	HardMinDurSec int `yaml:"hard_min_dur_sec" json:"hard_min_dur_sec"` // 默认 15：过短的切片无传播价值
	HardMaxDurSec int `yaml:"hard_max_dur_sec" json:"hard_max_dur_sec"` // 默认 120：过长超出短视频定位
	// 质闸——不过不自动发，进人工审核台等早晨处理：
	//   转写为空（纯音乐/无法识别）→ 内容不明，人看
	//   敏感词命中 → 发布必翻车，人看
	BannedWords []string `yaml:"banned_words" json:"banned_words"` // 敏感词黑名单（标题+转写匹配）
}

func Default() *Config {
	return &Config{
		DataDir: "./data",
		Proxy:   "http://127.0.0.1:7897",
		Engines: Engines{YtDlp: "yt-dlp", FFmpeg: "ffmpeg", Probe: "ffprobe"},
		Web:     Web{Listen: "127.0.0.1:8899"},
		Publish: Publish{Headless: false, Screenshots: true, PlatformsDir: "configs/platforms", ActiveHours: "10:00-23:00",
			RateLimit: RateLimit{MinIntervalMin: 10, DailyMax: 9, MaxInflight: 9, RiskCooldownHours: 24}},
		Live:      Live{CheckIntervalSec: 60, SegmentSec: 600, AutoClip: true, MaxStaleSec: 300, PreviewKeywords: []string{"预告", "凌晨见"}, PoolHigh: 20, PoolLow: 10},
		Retention: Retention{Enabled: true, SegmentsDays: 7, ConsumedSegDays: 1, MinFreePercent: 10, CheckMinutes: 60, MaxUsagePercent: 50, PublishedKeepDays: 3},
		AutoReview: AutoReview{Enabled: false, MinScore: 60, HardMinDurSec: 15, HardMaxDurSec: 120, BannedWords: []string{}},
	}
}

func Load(path string) (*Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, err
	}
	if err := yaml.Unmarshal(b, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) Save(path string) error {
	b, _ := yaml.Marshal(c)
	return os.WriteFile(path, b, 0o644)
}

// ParseActiveHours 解析发布窗口串 "HH:MM-HH:MM"（如 "10:00-23:00"，支持跨夜
// "22:00-06:00"）。返回当日窗口起止时刻；空串 = 全天（ok=true，start/end 零值）。
// 非法格式 ok=false（server 表单校验与调度侧防御共用）
func ParseActiveHours(s string) (start, end time.Time, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, time.Time{}, true
	}
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return time.Time{}, time.Time{}, false
	}
	// 容忍 H:M 短格式（"9:5" → "09:05"）：time layout "15:04" 要求零填充
	hm := func(v string) (time.Time, bool) {
		ab := strings.SplitN(strings.TrimSpace(v), ":", 2)
		if len(ab) != 2 {
			return time.Time{}, false
		}
		parsed, err := time.ParseInLocation("15:04", pad2(ab[0])+":"+pad2(ab[1]), time.Local)
		if err != nil {
			return time.Time{}, false
		}
		return parsed, true
	}
	p, ok1 := hm(parts[0])
	q, ok2 := hm(parts[1])
	if !ok1 || !ok2 || q.Sub(p) == 0 {
		return time.Time{}, time.Time{}, false
	}
	now := time.Now()
	start = time.Date(now.Year(), now.Month(), now.Day(), p.Hour(), p.Minute(), 0, 0, time.Local)
	end = time.Date(now.Year(), now.Month(), now.Day(), q.Hour(), q.Minute(), 0, 0, time.Local)
	return start, end, true
}

// pad2 数字串零填充到两位（"9" → "09"；超长原样返回交给 Parse 报错）
func pad2(s string) string {
	if len(s) == 1 && s[0] >= '0' && s[0] <= '9' {
		return "0" + s
	}
	return s
}

// InActiveHours 判断 now 是否在发布窗口内。窗口为空 = 全天；支持跨夜
// 窗口（start > end 时窗口跨午夜，如 "22:00-06:00"）
func InActiveHours(s string, now time.Time) bool {
	start, end, ok := ParseActiveHours(s)
	if !ok || start.IsZero() {
		return true // 空/非法窗口：不因配置错误把发布全部锁死，全天可发（防御默认）
	}
	nowHm := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), now.Minute(), 0, 0, time.Local)
	if !start.After(end) {
		return !nowHm.Before(start) && nowHm.Before(end)
	}
	// 跨夜窗口：22:00-06:00 = [22:00, 24:00) ∪ [00:00, 06:00)
	return !nowHm.Before(start) || nowHm.Before(end)
}
