package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"
)

// Config 保存服务运行所需的全部配置，全部来源于环境变量（支持 .env 文件）。
type Config struct {
	HTTPAddr  string
	AppBaseURL string

	DatabaseURL string

	DingTalkClientID     string
	DingTalkClientSecret string
	DingTalkCorpID       string
	DingTalkRedirectURI  string

	SessionCookie string
	SessionTTL    time.Duration
	CookieSecure  bool

	StorageRoot string
}

// Load 读取 .env（存在时）并解析环境变量。
func Load() (*Config, error) {
	_ = loadDotEnv(".env")

	ttl, err := time.ParseDuration(getenv("SESSION_TTL", "168h"))
	if err != nil {
		return nil, fmt.Errorf("SESSION_TTL 不是合法的时长: %w", err)
	}

	c := &Config{
		HTTPAddr:             getenv("HTTP_ADDR", "127.0.0.1:8000"),
		AppBaseURL:           strings.TrimRight(getenv("APP_BASE_URL", "http://127.0.0.1:8000"), "/"),
		DatabaseURL:          getenv("DATABASE_URL", ""),
		DingTalkClientID:     getenv("DINGTALK_CLIENT_ID", ""),
		DingTalkClientSecret: getenv("DINGTALK_CLIENT_SECRET", ""),
		DingTalkCorpID:       getenv("DINGTALK_CORP_ID", ""),
		DingTalkRedirectURI:  getenv("DINGTALK_REDIRECT_URI", "http://127.0.0.1:8000/auth/dingtalk/callback"),
		SessionCookie:        getenv("SESSION_COOKIE", "aa_session"),
		SessionTTL:           ttl,
		CookieSecure:         getenv("COOKIE_SECURE", "false") == "true",
		StorageRoot:          getenv("STORAGE_ROOT", "./data/storage"),
	}

	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL 未配置")
	}
	return c, nil
}

// DingTalkReady 判断钉钉凭证是否已配置齐全。
func (c *Config) DingTalkReady() bool {
	return c.DingTalkClientID != "" && c.DingTalkClientSecret != ""
}

// MissingDingTalk 返回缺失的钉钉配置项名称，用于启动告警与页面提示。
func (c *Config) MissingDingTalk() []string {
	var missing []string
	if c.DingTalkClientID == "" {
		missing = append(missing, "DINGTALK_CLIENT_ID")
	}
	if c.DingTalkClientSecret == "" {
		missing = append(missing, "DINGTALK_CLIENT_SECRET")
	}
	return missing
}

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// loadDotEnv 解析简单的 KEY=VALUE 文件，已存在的环境变量优先。
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		_ = os.Setenv(key, value)
	}
	return scanner.Err()
}
