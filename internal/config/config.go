package config

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// 运行形态。两档互斥，由部署显式声明，不按「探测到什么」推断，也不分开发/生产——
// 推断会让 server 的沙箱形态取决于镜像里有没有装 bwrap，或者取决于这次是不是本机调试。
const (
	// RoleServer 是服务端（含本机调试与 CI）：沙箱只能走远端执行体。
	RoleServer = "server"
	// RoleClient 是客户端本地 runtime：沙箱只能是本进程的 bwrap。
	RoleClient = "client"
)

// Config 保存服务运行所需的全部配置，全部来源于环境变量（支持 .env 文件）。
type Config struct {
	HTTPAddr   string
	AppBaseURL string // 本服务对外地址，用于拼接回调等
	WebBaseURL string // 前端站点地址，用于登录成功/失败后的跳转

	// PluginHostBase 是「插件在沙箱里回调宿主」用的地址（wa.config().host_base）。
	//
	// 与 AppBaseURL 分开是因为两者不是一回事：对外地址要浏览器能解析，沙箱里
	// 只认执行体所在的网络（集群里是 Service / 宿主网关，127.0.0.1 指向 Pod 自己）。
	// 留空时回落到 AppBaseURL。
	PluginHostBase string
	// PluginTokenSecret 是签发插件身份令牌的密钥（渠道插件回调 /inbox 用）。
	//
	// 插件没有、也不该有人的登录态，身份靠平台签发的令牌认。留空时回落到
	// SandboxTokenSecret——那个值在任何部署里都必须配，且不进前端、不进清单。
	PluginTokenSecret string

	DatabaseURL string

	// RedisURL 是登录态与会话流缓存的地址。留空时退回进程内存：
	// 单实例能跑，但重启即掉登录、丢当前轮正文（§5.1.1、§15.12.2）。
	RedisURL string
	// RunStreamTTL 是单个 run 的流缓存保留时长（秒级检查点 + 事件流）。
	RunStreamTTL time.Duration

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

	// UploadMaxFileMB 是会话上传单个文件的大小上限（MB），环境变量 UPLOAD_MAX_FILE_MB。
	// 0 表示沿用默认 100MB（负数启动报错）；单次上传的文件数上限仍是 10 个。
	UploadMaxFileMB int

	// SpaceLogRetentionDays 是空间运行日志（files/.logs）压缩后的保留天数，
	// 环境变量 SPACE_LOG_RETENTION_DAYS；0 表示永久保留。
	SpaceLogRetentionDays int
	// SpaceLogMaxFileMB 是空间单日日志文件的上限（MB），环境变量 SPACE_LOG_MAX_MB。
	// 超过后当天不再追加：日志是排障材料，不该长成配额与备份的主要来源。
	SpaceLogMaxFileMB int

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

	// AgentDispatch 控制是否把 agent_dispatch（派生子 Agent）暴露给模型。
	// 关掉不影响主循环，只是模型看不到这个工具。
	AgentDispatch bool
	// AgentMaxDepth 是派发深度上限：1 表示只能主 → 子，2 表示允许再派一层。
	AgentMaxDepth int
	// AgentStallRounds 是「连续多少轮没有新信息」判定停滞并强制收尾。
	AgentStallRounds int

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

	// Role 是运行形态，决定沙箱能且只能用哪一种（docs/本地空间与双端架构.md §5.7）：
	//   server —— 服务端（含本机调试与 CI）：只能建容器化执行体（SANDBOX_BACKEND=k8s）。
	//   client —— 客户端本地 runtime：只能在本进程起 bwrap。
	// 两档互斥且必须显式声明：沙箱形态不能取决于「这台机器上恰好装了什么」，
	// 也不能取决于「这次是不是开发环境」，否则同一份配置换台机器就从隔离变成裸跑。
	Role string

	// SandboxBackend 选择执行面形态，取值由 Role 决定：
	//   server —— 只能是 k8s（控制面自己建容器：每空间一个空间容器、每租户一个插件容器）
	//   client —— 只能是 local（默认），且必须是 bwrap
	SandboxBackend string
	// SandboxRunnerToken 是控制面调执行体的鉴权令牌；为空直接启动失败。
	//
	// 没有「连一个现成执行体」的 remote 档：那会让所有空间共享同一个 /usr
	// （一个空间装包改变另一个空间的环境），且开发与生产的隔离形态不一致。
	SandboxRunnerToken string

	// k8s 执行面（SANDBOX_BACKEND=k8s）：控制面自己建按空间 / 租户的 Pod。
	//
	// 命名空间里需要先有 SANDBOX_RUNNER_TOKEN 对应的 Secret
	// （默认 sandbox-runner-token / token），Pod 从它注入同一个令牌。
	SandboxNamespace    string
	SandboxImage        string
	SandboxKubeconfig   string
	SandboxTokenSecret  string
	SandboxTokenKey     string
	SandboxStorageClass string
	// SandboxStoragePVC / SandboxStoragePVCSize 是控制面与执行体共享的存储卷。
	SandboxStoragePVC     string
	SandboxStoragePVCSize string
	SandboxCachePVCSize   string
	SandboxShellMemory    string
	SandboxPluginMemory   string
	// SandboxShellIdle 是空间容器空闲回收时长。
	SandboxShellIdle time.Duration
	// SandboxReadyTimeout 是等容器就绪的上限。
	SandboxReadyTimeout time.Duration
	// SandboxCacheShared 为 true 时租户缓存用 RWX PVC 跨 Pod 共享。
	SandboxCacheShared bool
	// SandboxInitImage 是准备缓存目录的初始容器镜像。
	SandboxInitImage string

	// 插件常驻进程池（只装声明了 runtime.resident 的插件，docs/插件安全基线.md §2.5）。
	// 常驻进程不按空闲回收，只在池满时淘汰最久未用的空闲进程；0 用默认 32。
	PluginProcMax int
	// 常驻进程连续崩溃多少次后停止自动重启（熔断）；<0 用默认 5，0 表示不重启。
	PluginProcMaxRestarts int

	// 沙箱额外只读路径（逗号分隔，绝对路径）。
	// 白名单挂载之后，装在非标准位置的运行时（npm 装的 deno 是 node 包装脚本、
	// 自建 node / python 发行版）需要把安装前缀挂进去，否则会报「找不到模块」。
	SandboxReadExtra []string

	// 安装源镜像。空值使用内置默认（国内镜像），海外部署或走内网 Nexus 时覆盖。
	//
	// 只影响注入给命令的环境变量（PIP_INDEX_URL / NPM_CONFIG_REGISTRY 等），
	// 不影响任何出网边界——边界由 NetworkPolicy 与沙箱挂载决定，
	// 所以改这里不会让「多挂一个目录」这种事发生。
	InstallMirrorNPM  string
	InstallMirrorPyPI string
	InstallMirrorNode string
	InstallMirrorGo   string

	// 上下文压缩与记忆（§5.7.2 / §5.7.4）
	LLMSummaryModel string // 摘要用小模型，为空则复用主模型
	LLMEmbedBaseURL string
	LLMEmbedAPIKey  string
	LLMEmbedModel   string

	// 预算控制（§5.7.2 关键约束）
	LLMMaxToolCalls int
	LLMMaxParallel  int
	// LLMMaxWallClock 是一轮回答的总时长上限（<=0 表示不限）。
	//
	// 它只是最后一道保险，防「确实在动但永远跑不完」；判定「卡住」的是 RunIdleTimeout，
	// 判定「该收了」的是进度守卫与 token 预算。长任务不该因为这个值被砍。
	LLMMaxWallClock time.Duration
	// RunIdleTimeout 是连续多久没有任何事件（模型增量、工具返回）就判定卡住。
	// 跑得久不是问题，没有动静地跑很久才是。
	RunIdleTimeout time.Duration
	// LLMMaxTotalTokens 是一次 run 的累计 token 上限（所有轮次相加），
	// 与 LLMMaxTokens（单次响应）不是一回事：后者限一次输出，前者限累计成本。
	// 0 = 不限（默认）：长任务按无进展、轮次、工具次数与墙钟收口。
	LLMMaxTotalTokens int
	// LLMContextTokens 是模型上下文窗口（token）：输入超过它的 60% 就折叠较早的
	// 工具往返（见 builtin/compact.go）。0 表示不做轮内压缩。
	LLMContextTokens int

	// 跨轮上下文预算（§5.7.2 三段式）：
	// CtxWindow 是保留原文的最近消息条数，CtxMaxRunes 是总字数上限，
	// CtxSummaryRunes 是摘要目标长度，CtxTopK 是向量检索补充的片段数。
	CtxWindow       int
	CtxMaxRunes     int
	CtxSummaryRunes int
	CtxTopK         int
	// 滚动摘要按增量折叠（旧摘要 + 新增段），下面是它的节奏与预算：
	// CtxSummaryEvery 是攒够多少条新增才折一次（折叠落后时原文照带，不丢消息），
	// CtxSummaryInputTokens 是单次折叠的输入上限（超了分段连续折），
	// CtxSummaryTimeout 是后台折叠的整体超时，CtxRetrieveTimeout 是请求路径上
	// 向量检索的等待上限。
	CtxSummaryEvery       int
	CtxSummaryInputTokens int
	CtxSummaryTimeout     time.Duration
	CtxRetrieveTimeout    time.Duration

	// 凭证加密（§5.5.2 的 MVP 形态）：KEK 只从环境变量读，不进仓库。
	// 为空时凭证存储不可用，搜索等需要密钥的能力自动降级为「未配置」。
	SecretKEK string

	// 插件发布扫描（docs/插件安全基线.md）：
	//   ScanRulesDir 为空则只用内置规则；有值时按 <dir>/active.json 热加载。
	//   ScanOsvMode 取 off / local / remote；私有化默认 local，指向本地 OSV 镜像。
	ScanRulesDir   string
	ScanOsvMode    string
	ScanOsvDir     string
	ScanAllowRules []string
}

// Load 读取 .env（存在时）并解析环境变量。
func Load() (*Config, error) {
	_ = loadDotEnv(".env")

	// 形态先定，后面的默认值都跟着它走：同一份 .env 换台机器就从隔离变成裸跑，
	// 是比「没配」更难查的故障。
	role := strings.ToLower(strings.TrimSpace(getenv("WA_ROLE", RoleServer)))
	switch role {
	case RoleServer, RoleClient:
	default:
		return nil, fmt.Errorf("WA_ROLE 只能是 server / client，当前 %q（dev 已删除：本机调试同样连远端执行体）", role)
	}
	// 服务端没有本机执行面，只能建容器；客户端默认本机起进程。
	backendDefault := "k8s"
	if role != RoleServer {
		backendDefault = "local"
	}

	ttl, err := time.ParseDuration(getenv("SESSION_TTL", "168h"))
	if err != nil {
		return nil, fmt.Errorf("SESSION_TTL 不是合法的时长: %w", err)
	}

	// 流缓存的保留时长要覆盖「审批等人 + 用户离开再回来」：
	// 设得太短，切页回来时 Redis 里已经没有这一轮，续流退化成什么都不显示。
	runStreamTTL, err := time.ParseDuration(getenv("RUN_STREAM_TTL", "30m"))
	if err != nil {
		return nil, fmt.Errorf("RUN_STREAM_TTL 不是合法的时长: %w", err)
	}
	if runStreamTTL <= 0 {
		return nil, fmt.Errorf("RUN_STREAM_TTL 必须大于 0")
	}

	// 流式对话是「一个请求一条长连接」：这个值是整请求超时，必须 ≥ LLM_MAX_WALL_CLOCK，
	// 否则普通长回答会先被上游超时打断（默认取主流 SDK 的 600s）
	llmTimeout, err := time.ParseDuration(getenv("LLM_TIMEOUT", "10m"))
	if err != nil {
		return nil, fmt.Errorf("LLM_TIMEOUT 不是合法的时长: %w", err)
	}

	// 轮次是「一次模型调用」。它只是防死循环的最后一道，不是长任务的判据：
	// 真正决定收尾的是进度守卫（连续无新信息）、工具次数与累计 token。
	// 长任务常常要几十轮，按 8 轮砍会把「正在推进」的任务判死。
	maxRounds := 40
	if v := getenv("LLM_MAX_ROUNDS", ""); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &maxRounds); err != nil || maxRounds <= 0 {
			return nil, fmt.Errorf("LLM_MAX_ROUNDS 必须是正整数")
		}
	}

	maxToolCalls := 150
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

	// 累计 token 预算：默认 0 = 不限。
	//
	// 不设默认上限是刻意的：长任务与大上下文的成本随「每一轮重发完整上下文」
	// 自然增长，用一个写死的数字收口，最先砍掉的是「正在推进」的任务。
	// 真正该兜底的是无进展（进度守卫）、轮次、工具次数与墙钟；要卡成本时
	// 由部署者显式给一个正整数。
	maxTotalTokens := 0
	if v := getenv("LLM_MAX_TOTAL_TOKENS", ""); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &maxTotalTokens); err != nil || maxTotalTokens < 0 {
			return nil, fmt.Errorf("LLM_MAX_TOTAL_TOKENS 必须是非负整数（0 表示不限）")
		}
	}

	// 上下文窗口：轮内压缩的判据。一轮长任务里工具回执线性增长，
	// 输入超过窗口 60% 就把较早的往返折成进展记录（不调模型、零成本）。
	// 0 表示不做轮内压缩，只靠跨轮的 ctxmgr。
	//
	// 默认按 deepseek-chat 的 128k 给；换成窗口更小的模型要跟着改，
	// 否则会先撞上模型的 context-length 错误（那时折到只剩一轮再重试，能兜住但体验差）。
	contextTokens := 128000
	if v := getenv("LLM_CONTEXT_TOKENS", ""); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &contextTokens); err != nil || contextTokens < 0 {
			return nil, fmt.Errorf("LLM_CONTEXT_TOKENS 必须是非负整数（0 表示不做轮内压缩）")
		}
	}

	// 跨轮上下文预算（§5.7.2 三段式：摘要 + 检索 + 滑动窗口）
	ctxWindow := 16
	ctxMaxRunes := 24000
	ctxSummaryRunes := 600
	ctxTopK := 4
	ctxSummaryEvery := 4
	ctxSummaryInputTokens := 6000
	for _, item := range []struct {
		name string
		dst  *int
		def  int
	}{
		{"CTX_WINDOW", &ctxWindow, 16},
		{"CTX_MAX_RUNES", &ctxMaxRunes, 24000},
		{"CTX_SUMMARY_RUNES", &ctxSummaryRunes, 600},
		{"CTX_TOP_K", &ctxTopK, 4},
		{"CTX_SUMMARY_EVERY", &ctxSummaryEvery, 4},
		{"CTX_SUMMARY_INPUT_TOKENS", &ctxSummaryInputTokens, 6000},
	} {
		v := getenv(item.name, "")
		if v == "" {
			continue
		}
		if _, err := fmt.Sscanf(v, "%d", item.dst); err != nil || *item.dst <= 0 {
			return nil, fmt.Errorf("%s 必须是正整数", item.name)
		}
	}

	// 摘要在后台折叠，超时可以给得比请求宽松；检索在请求路径上，必须短。
	// 两者一个都不该成为「首 token 很慢」的原因，所以都显式可配。
	ctxSummaryTimeout := 90 * time.Second
	if v := getenv("CTX_SUMMARY_TIMEOUT", ""); v != "" {
		ctxSummaryTimeout, err = time.ParseDuration(v)
		if err != nil || ctxSummaryTimeout <= 0 {
			return nil, fmt.Errorf("CTX_SUMMARY_TIMEOUT 不是合法的时长")
		}
	}
	ctxRetrieveTimeout := 5 * time.Second
	if v := getenv("CTX_RETRIEVE_TIMEOUT", ""); v != "" {
		ctxRetrieveTimeout, err = time.ParseDuration(v)
		if err != nil || ctxRetrieveTimeout <= 0 {
			return nil, fmt.Errorf("CTX_RETRIEVE_TIMEOUT 不是合法的时长")
		}
	}

	// 停滞判定窗口：连续两轮没有任何新信息就收尾。
	// 一轮往往是「重试」，两轮才是「换汤不换药」。
	stallRounds := 2
	if v := getenv("AGENT_STALL_ROUNDS", ""); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &stallRounds); err != nil || stallRounds <= 0 {
			return nil, fmt.Errorf("AGENT_STALL_ROUNDS 必须是正整数")
		}
	}

	// 派发深度：1 = 主 → 子；2 = 允许再派一层。默认 2，够「主 + 专家 + 子任务」。
	maxDepth := 2
	if v := getenv("AGENT_MAX_DEPTH", ""); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &maxDepth); err != nil || maxDepth < 1 {
			return nil, fmt.Errorf("AGENT_MAX_DEPTH 必须是正整数")
		}
	}

	maxTokens := 4096
	if v := getenv("LLM_MAX_TOKENS", ""); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &maxTokens); err != nil || maxTokens <= 0 {
			return nil, fmt.Errorf("LLM_MAX_TOKENS 必须是正整数")
		}
	}

	// 总时长上限：长任务默认给 2 小时，0 = 不限。真正的收尾靠进度守卫与 token 预算，
	// 它只在「一直在动但跑不完」时兜底。
	wallClock, err := time.ParseDuration(getenv("LLM_MAX_WALL_CLOCK", "2h"))
	if err != nil {
		return nil, fmt.Errorf("LLM_MAX_WALL_CLOCK 不是合法的时长: %w", err)
	}
	// 无进展判定：长任务可以跑很久，但不能静默地跑很久
	idleTimeout, err := time.ParseDuration(getenv("RUN_IDLE_TIMEOUT", "10m"))
	if err != nil {
		return nil, fmt.Errorf("RUN_IDLE_TIMEOUT 不是合法的时长: %w", err)
	}
	if idleTimeout <= 0 {
		idleTimeout = 10 * time.Minute
	}
	if wallClock > 0 && wallClock < idleTimeout {
		return nil, fmt.Errorf("LLM_MAX_WALL_CLOCK 不能小于 RUN_IDLE_TIMEOUT（%s < %s）：总上限比无进展阈值还短会误杀正常的长回答", wallClock, idleTimeout)
	}

	// 上传单文件上限：按 MB 配，乘 2^20 落到字节；0 表示用 space 包的默认值（100MB）。
	uploadMaxFileMB := 100
	if v := getenv("UPLOAD_MAX_FILE_MB", ""); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &uploadMaxFileMB); err != nil || uploadMaxFileMB < 0 {
			return nil, fmt.Errorf("UPLOAD_MAX_FILE_MB 必须是非负整数")
		}
	}

	c := &Config{
		HTTPAddr:             getenv("HTTP_ADDR", "127.0.0.1:8321"),
		AppBaseURL:           strings.TrimRight(getenv("APP_BASE_URL", "http://127.0.0.1:8321"), "/"),
		PluginHostBase:       strings.TrimRight(getenv("PLUGIN_HOST_BASE", ""), "/"),
		PluginTokenSecret:    getenv("PLUGIN_TOKEN_SECRET", ""),
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
		RedisURL:             getenv("REDIS_URL", ""),
		RunStreamTTL:         runStreamTTL,
		CookieSecure:         getenv("COOKIE_SECURE", "false") == "true",
		StorageRoot:          getenv("STORAGE_ROOT", "./data/storage"),
		UploadMaxFileMB:      uploadMaxFileMB,
		AdminJobs:            splitList(getenv("ADMIN_JOB_NUMBERS", "")),

		// 空间运行日志：默认压缩后留 30 天、单日 2MB。0 天表示永久保留，
		// 上限为 0 时由 spacelog 用默认值兜底，避免配置写 0 变成「不写日志」。
		SpaceLogRetentionDays: getenvInt("SPACE_LOG_RETENTION_DAYS", 30),
		SpaceLogMaxFileMB:     getenvInt("SPACE_LOG_MAX_MB", 2),

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

		AgentImpl:        getenv("AGENT_IMPL", "builtin"),
		AgentDispatch:    getenv("AGENT_DISPATCH", "true") == "true",
		AgentMaxDepth:    maxDepth,
		AgentStallRounds: stallRounds,

		PluginsDefaultInstall: splitList(getenv("PLUGIN_DEFAULT_INSTALL", "")),

		Role: role,

		SandboxBackend:     getenv("SANDBOX_BACKEND", backendDefault),
		SandboxRunnerToken: getenv("SANDBOX_RUNNER_TOKEN", ""),

		SandboxNamespace:      getenv("SANDBOX_NAMESPACE", "workagent"),
		SandboxImage:          getenv("SANDBOX_IMAGE", "workagent/sandbox-runner:dev"),
		SandboxKubeconfig:     getenv("SANDBOX_KUBECONFIG", ""),
		SandboxTokenSecret:    getenv("SANDBOX_TOKEN_SECRET", "sandbox-runner-token"),
		SandboxTokenKey:       getenv("SANDBOX_TOKEN_KEY", "token"),
		SandboxStorageClass:   getenv("SANDBOX_STORAGE_CLASS", ""),
		SandboxStoragePVC:     getenv("SANDBOX_STORAGE_PVC", "workagent-storage"),
		SandboxStoragePVCSize: getenv("SANDBOX_STORAGE_PVC_SIZE", "100Gi"),
		SandboxCachePVCSize:   getenv("SANDBOX_CACHE_PVC_SIZE", "20Gi"),
		SandboxShellMemory:    getenv("SANDBOX_SHELL_MEMORY", "2Gi"),
		SandboxPluginMemory:   getenv("SANDBOX_PLUGIN_MEMORY", "4Gi"),
		SandboxShellIdle:      getenvDuration("SANDBOX_SHELL_IDLE", 15*time.Minute),
		SandboxReadyTimeout:   getenvDuration("SANDBOX_READY_TIMEOUT", 90*time.Second),
		SandboxCacheShared:    getenvBool("SANDBOX_CACHE_SHARED", false),
		SandboxInitImage:      getenv("SANDBOX_INIT_IMAGE", "busybox:1.36"),

		PluginProcMax:         getenvInt("PLUGIN_PROC_MAX", 32),
		PluginProcMaxRestarts: getenvInt("PLUGIN_PROC_MAX_RESTARTS", 5),
		SandboxReadExtra:      splitList(getenv("SANDBOX_READ_EXTRA", "")),

		InstallMirrorNPM:  getenv("INSTALL_MIRROR_NPM", ""),
		InstallMirrorPyPI: getenv("INSTALL_MIRROR_PYPI", ""),
		InstallMirrorNode: getenv("INSTALL_MIRROR_NODE", ""),
		InstallMirrorGo:   getenv("INSTALL_MIRROR_GO", ""),

		LLMSummaryModel: getenv("LLM_SUMMARY_MODEL", ""),
		LLMEmbedBaseURL: strings.TrimRight(getenv("LLM_EMBED_BASE_URL", ""), "/"),
		LLMEmbedAPIKey:  getenv("LLM_EMBED_API_KEY", ""),
		LLMEmbedModel:   getenv("LLM_EMBED_MODEL", ""),

		LLMMaxToolCalls:   maxToolCalls,
		LLMMaxParallel:    maxParallel,
		LLMMaxWallClock:   wallClock,
		RunIdleTimeout:    idleTimeout,
		LLMMaxTotalTokens: maxTotalTokens,
		LLMContextTokens:  contextTokens,
		CtxWindow:             ctxWindow,
		CtxMaxRunes:           ctxMaxRunes,
		CtxSummaryRunes:       ctxSummaryRunes,
		CtxTopK:               ctxTopK,
		CtxSummaryEvery:       ctxSummaryEvery,
		CtxSummaryInputTokens: ctxSummaryInputTokens,
		CtxSummaryTimeout:     ctxSummaryTimeout,
		CtxRetrieveTimeout:    ctxRetrieveTimeout,

		SecretKEK: getenv("SECRET_KEK", ""),

		ScanRulesDir:   getenv("SCAN_RULES_DIR", ""),
		ScanOsvMode:    getenv("SCAN_OSV_MODE", "local"),
		ScanOsvDir:     getenv("SCAN_OSV_DIR", ""),
		ScanAllowRules: splitList(getenv("SCAN_ALLOW_RULES", "")),
	}

	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL 未配置")
	}
	// 回落只在这里做一次，读的地方不必各判一遍：
	// 插件回调地址默认就是本服务地址；没有专门的插件密钥时用执行体令牌派生。
	if c.PluginHostBase == "" {
		c.PluginHostBase = c.AppBaseURL
	}
	if c.PluginTokenSecret == "" {
		c.PluginTokenSecret = c.SandboxTokenSecret
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

func getenvInt(key string, def int) int {
	if v := strings.TrimSpace(getenv(key, "")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getenvBool(key string, def bool) bool {
	v := strings.TrimSpace(strings.ToLower(getenv(key, "")))
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

// getenvDuration 接受 Go duration（15m / 90s）或纯数字（按秒）。
// 写错时回默认值而不是让服务起不来：容器生命周期参数不该成为启动门禁。
func getenvDuration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(getenv(key, ""))
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
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
