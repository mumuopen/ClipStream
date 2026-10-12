package config

import (
	"os"

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
	BaseURL string `yaml:"base_url"` // OpenAI 兼容端点，默认 https://api.openai.com/v1
	APIKey  string `yaml:"api_key"`
	Model   string `yaml:"model"`
	Proxy   string `yaml:"proxy"` // 可选：LLM 专用代理
}

type Web struct {
	Listen string `yaml:"listen"`
}

type Publish struct {
	Headless     bool   `yaml:"headless"`      // 抖音浏览器发布是否无头（默认有头，风控更稳）
	Screenshots  bool   `yaml:"screenshots"`   // 发布前/后截图存档
	PlatformsDir string `yaml:"platforms_dir"` // 平台选择器配置目录
}

type Notify struct {
	Pushme Pushme `yaml:"pushme"`
}

type Pushme struct {
	Enable  bool   `yaml:"enable"`   // 总开关
	URL     string `yaml:"url"`      // 自建 pushme 服务地址，如 http://server:3100/
	PushKey string `yaml:"push_key"` // 客户端生成的 push_key（手机 App 订阅的那个）
}

type Chrome struct {
	Path string `yaml:"path"` // 本机 Chrome 可执行路径，留空则 rod 自动探测
}

type Live struct {
	CheckIntervalSec int  `yaml:"check_interval"` // 开播检测轮询间隔，默认 60
	SegmentSec       int  `yaml:"segment_time"`   // 录制分段时长，默认 600
	AutoClip         bool `yaml:"auto_clip"`      // 下播后自动将分段送入切片管线
}

// Retention 磁盘清理策略（挂机刚需：数据流入-流出平衡）
type Retention struct {
	Enabled bool `yaml:"enabled"`  // 总开关（daemon 内定时清理）
	// 保留天数：超过的直播分段目录整个删除（挂机磁盘有限的最重要防线）
	SegmentsDays int `yaml:"segments_days"` // data/live/ 会话目录，默认 7 天
	// 已入池/已派发的源分段：切片已完成使命，更积极清理（默认 1 天）
	ConsumedSegDays int `yaml:"consumed_seg_days"`
	// 磁盘水位：剩余百分比低于阈值时预警 + 自动清理最旧会话
	MinFreePercent int `yaml:"min_free_percent"` // 默认 10
	// 清理检查间隔（分钟）
	CheckMinutes int `yaml:"check_minutes"` // 默认 60
}

// AutoReview 自动审核（挂机无人值守：切片完成不再等人工，分级闸门自动入池）
type AutoReview struct {
	Enabled bool `yaml:"enabled"` // 总开关（关=全部进人工审核台，现状行为）
	// 自动入池的信任门槛：LLM 评分 ≥ MinScore（等分回退切片无评分，不会自动过）
	MinScore int `yaml:"min_score"` // 默认 60
	// 硬闸——任一命中直接拒绝（不入池也不留审，标记 failed 留档）：
	HardMinDurSec int `yaml:"hard_min_dur_sec"` // 默认 15：过短的切片无传播价值
	HardMaxDurSec int `yaml:"hard_max_dur_sec"` // 默认 120：过长超出短视频定位
	// 质闸——不过不自动发，进人工审核台等早晨处理：
	//   转写为空（纯音乐/无法识别）→ 内容不明，人看
	//   敏感词命中 → 发布必翻车，人看
	BannedWords []string `yaml:"banned_words"` // 敏感词黑名单（标题+转写匹配）
}

func Default() *Config {
	return &Config{
		DataDir: "./data",
		Proxy:   "http://127.0.0.1:7897",
		Engines: Engines{YtDlp: "yt-dlp", FFmpeg: "ffmpeg", Probe: "ffprobe"},
		Web:     Web{Listen: "127.0.0.1:8899"},
		Publish: Publish{Headless: false, Screenshots: true, PlatformsDir: "configs/platforms"},
		Live:      Live{CheckIntervalSec: 60, SegmentSec: 600, AutoClip: true},
		Retention: Retention{Enabled: true, SegmentsDays: 7, ConsumedSegDays: 1, MinFreePercent: 10, CheckMinutes: 60},
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
