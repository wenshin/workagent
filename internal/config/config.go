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

	// 身份接入（§5.1.1 IdentityProvider）：dingtalk | wecom | feishu
	IdentityProvider string

	DingTalkClientID     string
	DingTalkClientSecret string
	DingTalkCorpID       string
	DingTalkCorpName     string
	DingTalkRedirectURI  string

	SessionCookie string
	SessionTTL    time.Duration
	CookieSecure  bool

	StorageRoot string

	// 系统管理员工号（逗号分隔）：只有这些用户能看到并编辑「系统空间」。
	// MVP 用配置文件指定，后续接入组织架构后改为角色表。
	AdminJobs []string

	// LLM 走 Provider 适配层（§5.7.1）：openai 兼容协议覆盖 vLLM / 国产模型，anthropic 走原生协议。
	LLMProvider  string // openai | anthropic
	LLMBaseURL   string
	LLMAPIKey    string
	LLMModel     string
	LLMTimeout   time.Duration
	LLMMaxTokens int
	LLMMaxRounds int // 单轮对话内允许的最大工具调用轮次

	// 失败降级（§5.7.3）：主模型 5xx / 超时后切备模型，协议族不同则不降级
	LLMFallbackProvider string
	LLMFallbackBaseURL  string
	LLMFallbackAPIKey   string
	LLMFallbackModel    string

	// AgentImpl 选择 Agent 编排实现（§5.7.7）：builtin（内置 ReAct）/ direct（无工具直答）。
	// 未注册的名字在启动时告警并回退 builtin，不影响服务可用。
	AgentImpl string

	// PluginsDefaultInstall 是部署者定义的默认安装集：空间首次初始化时装这些内置插件。
	// 空表示不预装。改动只影响之后新建的空间。
	PluginsDefaultInstall []string

	// SandboxRequire 要求命令执行必须有真实隔离后端（bwrap / sandbox-exec）。
	// 生产环境打开：探测不到就启动失败，而不是悄悄退化成裸跑。
	// 开发机保持默认（false），降级到 local 时日志里会有告警。
	SandboxRequire bool

	// 上下文压缩与记忆（§5.7.2 / §5.7.4）
	LLMSummaryModel string // 摘要用小模型，为空则复用主模型
	LLMEmbedBaseURL string
	LLMEmbedAPIKey  string
	LLMEmbedModel   string

	// 预算控制（§5.7.2 关键约束）
	LLMMaxToolCalls int
	LLMMaxParallel  int
	LLMMaxWallClock time.Duration

	// 凭证加密（§5.5.2 的 MVP 形态）：KEK 只从环境变量读，不进仓库。
	// 为空时凭证存储不可用，搜索等需要密钥的能力自动降级为「未配置」。
	SecretKEK string

	// 搜索（§5.6.1 search.query）：endpoint 与白名单是平台属性，skill / 模型都无法指定。
	SearchProvider string
	SearchEndpoint string
	// SearchAllow 是出网白名单（逗号分隔的域名）。空表示不允许任何外部搜索。
	SearchAllow []string
	// SearchAPIKey 只用于首次播种到凭证存储，之后以凭证存储为准。
	SearchAPIKey string
}

// Load 读取 .env（存在时）并解析环境变量。
func Load() (*Config, error) {
	_ = loadDotEnv(".env")

	ttl, err := time.ParseDuration(getenv("SESSION_TTL", "168h"))
	if err != nil {
		return nil, fmt.Errorf("SESSION_TTL 不是合法的时长: %w", err)
	}

	// 流式对话是「一个请求一条长连接」：这个值是整请求超时，必须 ≥ LLM_MAX_WALL_CLOCK，
	// 否则普通长回答会先被上游超时打断（默认取主流 SDK 的 600s）
	llmTimeout, err := time.ParseDuration(getenv("LLM_TIMEOUT", "10m"))
	if err != nil {
		return nil, fmt.Errorf("LLM_TIMEOUT 不是合法的时长: %w", err)
	}

	maxRounds := 5
	if v := getenv("LLM_MAX_ROUNDS", ""); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &maxRounds); err != nil || maxRounds <= 0 {
			return nil, fmt.Errorf("LLM_MAX_ROUNDS 必须是正整数")
		}
	}

	maxToolCalls := 20
	if v := getenv("LLM_MAX_TOOL_CALLS", ""); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &maxToolCalls); err != nil || maxToolCalls <= 0 {
			return nil, fmt.Errorf("LLM_MAX_TOOL_CALLS 必须是正整数")
		}
	}

	maxParallel := 4
	if v := getenv("LLM_MAX_PARALLEL", ""); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &maxParallel); err != nil || maxParallel <= 0 {
			return nil, fmt.Errorf("LLM_MAX_PARALLEL 必须是正整数")
		}
	}

	maxTokens := 4096
	if v := getenv("LLM_MAX_TOKENS", ""); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &maxTokens); err != nil || maxTokens <= 0 {
			return nil, fmt.Errorf("LLM_MAX_TOKENS 必须是正整数")
		}
	}

	wallClock, err := time.ParseDuration(getenv("LLM_MAX_WALL_CLOCK", "10m"))
	if err != nil {
		return nil, fmt.Errorf("LLM_MAX_WALL_CLOCK 不是合法的时长: %w", err)
	}

	c := &Config{
		HTTPAddr:             getenv("HTTP_ADDR", "127.0.0.1:8321"),
		AppBaseURL:           strings.TrimRight(getenv("APP_BASE_URL", "http://127.0.0.1:8321"), "/"),
		WebBaseURL:           strings.TrimRight(getenv("WEB_BASE_URL", "http://127.0.0.1:5173"), "/"),
		DatabaseURL:          getenv("DATABASE_URL", ""),
		IdentityProvider:     getenv("IDENTITY_PROVIDER", "dingtalk"),
		DingTalkClientID:     getenv("DINGTALK_CLIENT_ID", ""),
		DingTalkClientSecret: getenv("DINGTALK_CLIENT_SECRET", ""),
		DingTalkCorpID:       getenv("DINGTALK_CORP_ID", ""),
		DingTalkCorpName:     getenv("DINGTALK_CORP_NAME", ""),
		DingTalkRedirectURI:  getenv("DINGTALK_REDIRECT_URI", "http://127.0.0.1:8321/auth/dingtalk/callback"),
		SessionCookie:        getenv("SESSION_COOKIE", "aa_session"),
		SessionTTL:           ttl,
		CookieSecure:         getenv("COOKIE_SECURE", "false") == "true",
		StorageRoot:          getenv("STORAGE_ROOT", "./data/storage"),
		AdminJobs:            splitList(getenv("ADMIN_JOB_NUMBERS", "")),

		LLMProvider:  getenv("LLM_PROVIDER", "openai"),
		LLMBaseURL:   strings.TrimRight(getenv("LLM_BASE_URL", "https://api.deepseek.com/v1"), "/"),
		LLMAPIKey:    getenv("LLM_API_KEY", ""),
		LLMModel:     getenv("LLM_MODEL", "deepseek-chat"),
		LLMTimeout:   llmTimeout,
		LLMMaxTokens: maxTokens,
		LLMMaxRounds: maxRounds,

		LLMFallbackProvider: getenv("LLM_FALLBACK_PROVIDER", ""),
		LLMFallbackBaseURL:  strings.TrimRight(getenv("LLM_FALLBACK_BASE_URL", ""), "/"),
		LLMFallbackAPIKey:   getenv("LLM_FALLBACK_API_KEY", ""),
		LLMFallbackModel:    getenv("LLM_FALLBACK_MODEL", ""),

		AgentImpl: getenv("AGENT_IMPL", "builtin"),

		PluginsDefaultInstall: splitList(getenv("PLUGIN_DEFAULT_INSTALL", "")),

		SandboxRequire: getenv("SANDBOX_REQUIRE", "false") == "true",

		LLMSummaryModel: getenv("LLM_SUMMARY_MODEL", ""),
		LLMEmbedBaseURL: strings.TrimRight(getenv("LLM_EMBED_BASE_URL", ""), "/"),
		LLMEmbedAPIKey:  getenv("LLM_EMBED_API_KEY", ""),
		LLMEmbedModel:   getenv("LLM_EMBED_MODEL", ""),

		LLMMaxToolCalls: maxToolCalls,
		LLMMaxParallel:  maxParallel,
		LLMMaxWallClock: wallClock,

		SecretKEK: getenv("SECRET_KEK", ""),

		SearchProvider: getenv("SEARCH_PROVIDER", "bocha"),
		SearchEndpoint: getenv("SEARCH_ENDPOINT", "https://api.bochaai.com/v1/web-search"),
		SearchAllow:    splitList(getenv("SEARCH_ALLOW", "api.bochaai.com")),
		SearchAPIKey:   getenv("SEARCH_API_KEY", ""),
	}

	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL 未配置")
	}
	return c, nil
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

// IsAdminJob 判断工号是否在系统管理员名单中；空工号永远不是管理员。
func (c *Config) IsAdminJob(jobNumber string) bool {
	job := strings.TrimSpace(jobNumber)
	if job == "" {
		return false
	}
	for _, item := range c.AdminJobs {
		if item == job {
			return true
		}
	}
	return false
}

// splitList 把逗号分隔的配置切成去空白的列表，保持配置顺序。
func splitList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
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
