package publish

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// PlatformConfig 平台配置（configs/platforms/<name>.yaml）
type PlatformConfig struct {
	Engine string `yaml:"engine"` // biliup | douyin

	// —— B站（biliup）——
	TID         int      `yaml:"tid"`
	TagsDefault []string `yaml:"tags_default"`

	// —— 抖音（rod）——
	UploadURL           string            `yaml:"upload_url"`
	PublishURLContains  []string          `yaml:"publish_url_contains"`
	SuccessURLContains  string            `yaml:"success_url_contains"`
	LoginMarkers        []string          `yaml:"login_markers"`
	LoggedInURLContains string            `yaml:"logged_in_url_contains"`
	Selectors           map[string]string `yaml:"selectors"`
	TitleMaxLen         int               `yaml:"title_max_len"`
	UploadTimeoutSec    int               `yaml:"upload_timeout_sec"`
}

// LoadPlatformConfig 加载平台配置；configDir 通常为 configs/platforms
func LoadPlatformConfig(name, configDir string) (*PlatformConfig, error) {
	path := filepath.Join(configDir, name+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取平台配置失败 %s: %w", path, err)
	}
	var pc PlatformConfig
	if err := yaml.Unmarshal(data, &pc); err != nil {
		return nil, fmt.Errorf("解析平台配置失败 %s: %w", path, err)
	}
	// 兜底默认值
	if pc.TitleMaxLen <= 0 {
		pc.TitleMaxLen = 30
	}
	if pc.UploadTimeoutSec <= 0 {
		pc.UploadTimeoutSec = 600
	}
	return &pc, nil
}

// AccountDir 账号登录态目录：<dataDir>/accounts/<platform>-<account>
func AccountDir(dataDir, platform, account string) string {
	return filepath.Join(dataDir, "accounts", platform+"-"+account)
}
