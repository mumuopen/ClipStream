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
}

type Engines struct {
	YtDlp  string `yaml:"ytdlp"` // 可执行文件路径或命令名
	FFmpeg string `yaml:"ffmpeg"`
	Probe  string `yaml:"ffprobe"`
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

func Default() *Config {
	return &Config{
		DataDir: "./data",
		Proxy:   "http://127.0.0.1:7897",
		Engines: Engines{YtDlp: "yt-dlp", FFmpeg: "ffmpeg", Probe: "ffprobe"},
		Web:     Web{Listen: "127.0.0.1:8899"},
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
