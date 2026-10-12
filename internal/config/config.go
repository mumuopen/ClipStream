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
	Live    Live    `yaml:"live"`
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

func Default() *Config {
	return &Config{
		DataDir: "./data",
		Proxy:   "http://127.0.0.1:7897",
		Engines: Engines{YtDlp: "yt-dlp", FFmpeg: "ffmpeg", Probe: "ffprobe"},
		Web:     Web{Listen: "127.0.0.1:8899"},
		Publish: Publish{Headless: false, Screenshots: true, PlatformsDir: "configs/platforms"},
		Live:    Live{CheckIntervalSec: 60, SegmentSec: 600, AutoClip: true},
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
