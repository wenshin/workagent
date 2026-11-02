package config

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// Config 保存服务运行所需的全部配置，全部来源于环境变量（支持 .env 文件）。
type Config struct {
	HTTPAddr   string
	AppBaseURL string // 本服务对外地址，用于拼接回调等
	WebBaseURL string // 前端站点地址，用于登录成功/失败后的跳转

	DatabaseURL string

	DingTalkClientID     string
	DingTalkClientSecret string
	DingTalkCorpID       string
	DingTalkRedirectURI  string

	SessionCookie string
	SessionTTL    time.Duration
	CookieSecure  bool

	StorageRoot string

	// LLM 走 OpenAI 兼容协议，DeepSeek / 通义兼容模式 / vLLM / Ollama 均可直接接入
	LLMBaseURL   string
	LLMAPIKey    string
	LLMModel     string
	LLMTimeout   time.Duration
	LLMMaxRounds int // 单轮对话内允许的最大工具调用轮次
}

// Load 读取 .env（存在时）并解析环境变量。
func Load() (*Config, error) {
	_ = loadDotEnv(".env")

	ttl, err := time.ParseDuration(getenv("SESSION_TTL", "168h"))
	if err != nil {
		return nil, fmt.Errorf("SESSION_TTL 不是合法的时长: %w", err)
	}

	llmTimeout, err := time.ParseDuration(getenv("LLM_TIMEOUT", "120s"))
	if err != nil {
		return nil, fmt.Errorf("LLM_TIMEOUT 不是合法的时长: %w", err)
	}

	maxRounds := 5
	if v := getenv("LLM_MAX_ROUNDS", ""); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &maxRounds); err != nil || maxRounds <= 0 {
			return nil, fmt.Errorf("LLM_MAX_ROUNDS 必须是正整数")
		}
	}

	c := &Config{
		HTTPAddr:             getenv("HTTP_ADDR", "127.0.0.1:8000"),
		AppBaseURL:           strings.TrimRight(getenv("APP_BASE_URL", "http://127.0.0.1:8000"), "/"),
		WebBaseURL:           strings.TrimRight(getenv("WEB_BASE_URL", "http://127.0.0.1:5173"), "/"),
		DatabaseURL:          getenv("DATABASE_URL", ""),
		DingTalkClientID:     getenv("DINGTALK_CLIENT_ID", ""),
		DingTalkClientSecret: getenv("DINGTALK_CLIENT_SECRET", ""),
		DingTalkCorpID:       getenv("DINGTALK_CORP_ID", ""),
		DingTalkRedirectURI:  getenv("DINGTALK_REDIRECT_URI", "http://127.0.0.1:8000/auth/dingtalk/callback"),
		SessionCookie:        getenv("SESSION_COOKIE", "aa_session"),
		SessionTTL:           ttl,
		CookieSecure:         getenv("COOKIE_SECURE", "false") == "true",
		StorageRoot:          getenv("STORAGE_ROOT", "./data/storage"),

		LLMBaseURL:   strings.TrimRight(getenv("LLM_BASE_URL", "https://api.deepseek.com/v1"), "/"),
		LLMAPIKey:    getenv("LLM_API_KEY", ""),
		LLMModel:     getenv("LLM_MODEL", "deepseek-chat"),
		LLMTimeout:   llmTimeout,
		LLMMaxRounds: maxRounds,
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

// LLMReady 判断模型服务是否可用。
//
// 云端服务必须有 API Key；本地推理（Ollama / vLLM）通常不校验密钥，按主机名放行。
func (c *Config) LLMReady() bool {
	if c.LLMBaseURL == "" || c.LLMModel == "" {
		return false
	}
	if c.LLMAPIKey != "" {
		return true
	}

	u, err := url.Parse(c.LLMBaseURL)
	if err != nil {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "0.0.0.0", "::1", "host.docker.internal":
		return true
	}
	return false
}

// MissingLLM 返回缺失的模型配置项名称，用于启动告警。
func (c *Config) MissingLLM() []string {
	missing := make([]string, 0, 2)
	if c.LLMBaseURL == "" {
		missing = append(missing, "LLM_BASE_URL")
	}
	if c.LLMModel == "" {
		missing = append(missing, "LLM_MODEL")
	}
	if c.LLMAPIKey == "" && c.LLMReadyBaseOnly() {
		missing = append(missing, "LLM_API_KEY")
	}
	return missing
}

func (c *Config) LLMReadyBaseOnly() bool {
	u, err := url.Parse(c.LLMBaseURL)
	if err != nil {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "0.0.0.0", "::1", "host.docker.internal":
		return false
	}
	return true
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
