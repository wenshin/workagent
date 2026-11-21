// Package plugin 实现插件体系（docs/插件体系设计.md）。
//
// 四类插件共用一份清单（plugin.json）、一套能力声明、一条生命周期
// （开发 → 发布 → 安装 → 运行 → 卸载），差异只在执行器：
//
//	skill      无执行器，正文按需注入
//	tool       Deno 进程（默认一次性，runtime.resident 才复用）+ 宿主 ABI
//	mcp        外部服务，平台按白名单代理
//	component  前端产物，数据请求经宿主转成工具调用
//
// 三条硬规则：元数据只在 plugin.json（单一定义点）；风险由 capabilities
// 推导，作者不能自称；代码插件只能挂在 tool.Invoker 之下。
package plugin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/workagent/auto-agent/internal/mcp"
	"github.com/workagent/auto-agent/internal/policy"
)

// Protocol 是清单协议版本。不兼容变更才升版本。
const Protocol = "wa-plugin/1"

// ManifestFile 是插件目录下的唯一清单元数据文件。
const ManifestFile = "plugin.json"

// AgentEntry 是智能体提示词正文的默认文件名。
const AgentEntry = "AGENT.md"

// Kind 是插件类型。
type Kind string

const (
	KindSkill     Kind = "skill"
	KindTool      Kind = "tool"
	KindMCP       Kind = "mcp"
	KindComponent Kind = "component"
	// KindAgent 是智能体：只描述行为（提示词 + 继承 + 依赖），没有执行器。
	KindAgent Kind = "agent"
	// KindSource 是搜索源：不是模型可调用的工具，而是 web_search 背后的实现。
	KindSource Kind = "source"
)

// Kinds 是全部合法类型。
var Kinds = []Kind{KindSkill, KindTool, KindMCP, KindComponent, KindAgent, KindSource}

// Valid 判断类型是否合法。
func (k Kind) Valid() bool {
	switch k {
	case KindSkill, KindTool, KindMCP, KindComponent, KindAgent, KindSource:
		return true
	}
	return false
}

// 能力名固定枚举：插件只能引用，不能自造，否则策略规则表会被绕过。
const (
	CapSkillUse    = "skill.use"
	CapSearchQuery = "search.query"
	CapHTTPRequest = "http.request"
	CapFSRead      = "fs.read"
	CapFSWrite     = "fs.write"
	CapSQLQuery    = "sql.query"
	CapShellExec   = "shell.exec"
	CapUIRender    = "ui.render"
	CapPluginInvok = "plugin.invoke"
	CapAgentUse    = "agent.use"
)

var capabilities = map[string]bool{
	CapSkillUse: true, CapSearchQuery: true, CapHTTPRequest: true,
	CapFSRead: true, CapFSWrite: true, CapSQLQuery: true,
	CapShellExec: true, CapUIRender: true, CapPluginInvok: true,
	CapAgentUse: true,
}

// 宿主 RPC method（插件可申请的系统调用）。
//
// 文件不在其中：沙箱已经按 fs 段挂载，脚本用 Deno.readTextFile 直接读写。
// 在挂载之外再留一条「宿主代读写」的通道，等于让脚本绕过挂载去看文件。
const (
	// MethodHTTP 是清单里的出网声明，不是宿主 RPC：请求由插件在沙箱里发，
	// 宿主只按它建代理白名单。host.call("http.request") 已经不存在。
	MethodHTTP   = "http.request"
	MethodSecret = "secret.use"
	// MethodWSConnect 声明「允许建立加密隧道」：wss:// 靠它走 CONNECT，
	// 宿主只能看到目标域名，看不到隧道里的内容。不声明就没有隧道。
	MethodWSConnect = "ws.connect"
	// MethodSecretGet 是唯一的凭证通道：插件按清单里声明过的名字取明文。
	MethodSecretGet = "secret.get"
	// MethodLog 写一行宿主日志（插件进程的 stdout 有上限，长日志会被截）。
	MethodLog = "log"
	// MethodConfigGet 取安装方为这个插件填的配置（清单 config 段 + 默认值）。
	//
	// 它不需要在 capabilities 里声明：读的是「安装方给自己的插件填的值」，
	// 不涉及跨主体授权，也不是一个能触碰外部系统的动作。凭证另有 secret.use。
	MethodConfigGet = "config.get"
	// MethodShell 对应命令执行：内置工具用它表达风险，脚本插件暂不开放。
	MethodShell = "shell.exec"
	// MethodSearch 对应联网检索：只计读成本，不允许写。
	MethodSearch = "search.query"
)

// Limits 是单次调用的硬上限，平台只允许收紧不允许放宽。
type Limits struct {
	TimeoutMS      int `json:"timeout_ms,omitempty"`
	MaxRPC         int `json:"max_rpc,omitempty"`
	MaxResultBytes int `json:"max_result_bytes,omitempty"`
}

// Capability 是一条能力声明，决定出网白名单、文件范围与风险等级。
type Capability struct {
	Method string   `json:"method"`
	Hosts  []string `json:"hosts,omitempty"`
	Verbs  []string `json:"verbs,omitempty"`
	// Name 只对 secret.use 有意义：声明插件能取到哪一个凭证。
	// 一个凭证一条能力，没声明的名字 secret.get 一律拒绝——
	// 否则「有 secret.use」就等于「能读整个 plugin scope」。
	Name string `json:"name,omitempty"`
}

// FSSpec 是插件声明的文件访问范围，路径相对「空间工作区」（files/）。
//
// 默认边界：整个空间工作区可读、不可写，空间之外一律不可见。
// Read 声明后就只读声明的子目录；Write 必须显式声明才有写权限。
// 两者都只能是空间工作区下的**子目录**（`.` 与空串等价于整个空间，直接拒绝）。
// 声明只会在默认边界内收紧，不会扩权——所以这个字段不需要审批。
type FSSpec struct {
	// Read 是允许读取的子目录；空表示整个空间工作区。
	Read []string `json:"read,omitempty"`
	// Write 是允许写入的子目录；空表示没有写权限。
	Write []string `json:"write,omitempty"`
	// ReadOnly 为 true 时整个插件只读：即使声明了 Write 也不给写权限。
	ReadOnly bool `json:"read_only,omitempty"`
}

// RuntimeSpec 是 tool 的执行参数。
//
// 只有 Entry 有效：超时与 RPC 上限在 Limits（单次调用），进程寿命由常驻池管，
// 都不属于这里。
type RuntimeSpec struct {
	Entry string `json:"entry,omitempty"`
	// Resident 为 true 时跨调用复用同一个进程：模块级变量、连接、缓存都会保留，
	// 代价是进程常占内存并占用常驻池的一个名额（默认池上限 32）。
	// 默认 false：每次调用起一个一次性进程，跑完即退。
	Resident bool `json:"resident,omitempty"`
	// TimeoutMS / MaxRPC 已废弃，写了会被 Validate 拒绝。
	TimeoutMS int `json:"timeout_ms,omitempty"`
	MaxRPC    int `json:"max_rpc,omitempty"`
}

// Transport 是 mcp 的连接方式。
type Transport struct {
	Type string `json:"type"`
	URL  string `json:"url"`
	// AllowHTTP 允许明文 http 连接。
	//
	// 默认要求 https：MCP 是宿主自己发 HTTP 出去（internal/mcp 用的是裸 http.Client，
	// 不经 search.Guard 的白名单与私网拦截），URL 校验就是唯一的约束点。但企业内网
	// 的 MCP Server 常常只有 http 一档，强制 https 等于把内网接不进来，所以留一个
	// 显式口子：写出来才放行，权限文案也会标成「明文 http」，让安装的人看见代价。
	//
	// 它是「允许」而不是「必须」：清单可以写 https 地址同时声明 allow_http，
	// 安装方才能在安装级配置里把它覆盖成内网 http。
	AllowHTTP bool `json:"allow_http,omitempty"`
}

// Auth 是 mcp 的鉴权方式。
type Auth struct {
	In     string `json:"in,omitempty"`
	Name   string `json:"name,omitempty"`
	Secret string `json:"secret,omitempty"`
	// Mode 决定凭证来源：static（默认，引用 auth.secret 指向的预置凭证）
	// 或 jwt（平台按用户身份现签，无需用户录入凭证）。
	Mode string `json:"mode,omitempty"`
	// App 是应用名：mode=jwt 时平台以它为粒度生成独立的签名密钥，
	// 同一个应用在多个空间装同一个插件时共用一把密钥。留空回落到 slug。
	App string `json:"app,omitempty"`
	// TokenTTLSeconds 是用户令牌寿命（§15.11 决定 1）：默认 300，上限 3600。
	// exp 仍然保留，作为对端可依赖的上限；对端还能用 iat 收紧自己的 max-age。
	TokenTTLSeconds int `json:"token_ttl_seconds,omitempty"`
}

// UserTokenTTL 返回用户令牌寿命：默认 5 分钟，最多 1 小时。
func (a *Auth) UserTokenTTL() time.Duration {
	if a == nil || a.TokenTTLSeconds <= 0 {
		return mcp.UserTokenTTL
	}
	ttl := time.Duration(a.TokenTTLSeconds) * time.Second
	if ttl > time.Hour {
		return time.Hour
	}
	return ttl
}

// MCP 鉴权模式。
const (
	// AuthModeStatic 用 auth.secret 引用一条预置凭证（用户在会话里录入）。
	AuthModeStatic = "static"
	// AuthModeJWT 由平台签发含用户身份的 JWT，MCP Server 用应用密钥验签。
	AuthModeJWT = "jwt"
)

// appNameRe 与凭证名规则一致：应用名会直接作为凭证存储里的键。
var appNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// JWT 判断该 MCP 是否由平台签发用户令牌。
func (a *Auth) JWT() bool {
	return a != nil && strings.EqualFold(strings.TrimSpace(a.Mode), AuthModeJWT)
}

// AppName 返回应用名：未显式声明 auth.app 时回落到插件 slug。
func (a *Auth) AppName(slug string) string {
	if a == nil {
		return slug
	}
	if app := strings.TrimSpace(a.App); app != "" {
		return app
	}
	return slug
}

// HeaderName 返回携带令牌的 header 名。
func (a *Auth) HeaderName() string {
	if a == nil {
		return ""
	}
	if name := strings.TrimSpace(a.Name); name != "" {
		return name
	}
	if a.JWT() {
		return "Authorization"
	}
	return ""
}

// normalizeAuth 归一化并校验 MCP 鉴权声明。
//
// jwt 模式禁止同时写 secret：两种来源并存时无法判断该信哪个，
// 而且会让「平台签发」被静默降级成「用户录入」。
func (m *Manifest) normalizeAuth() error {
	a := m.Auth
	if a == nil {
		return nil
	}
	mode := strings.ToLower(strings.TrimSpace(a.Mode))
	switch mode {
	case "", AuthModeStatic:
		a.Mode = AuthModeStatic
	case AuthModeJWT:
		a.Mode = AuthModeJWT
		if strings.TrimSpace(a.Secret) != "" {
			return fmt.Errorf("auth.mode=jwt 不能同时声明 auth.secret：前者由平台签发，后者是用户录入的预置凭证")
		}
		app := strings.TrimSpace(a.App)
		if app == "" {
			app = m.Slug
		}
		if !appNameRe.MatchString(app) {
			return fmt.Errorf("auth.app 必须是 1-64 位字母数字与 _-，收到 %q", app)
		}
		a.App = app
		if strings.TrimSpace(a.Name) == "" {
			a.Name = "Authorization"
		}
	default:
		return fmt.Errorf("auth.mode 只能是 static / jwt，收到 %q", a.Mode)
	}
	return nil
}

// AllowedTools 是 mcp 的工具白名单。空清单视为非法：等价于开放全部。
type AllowedTools struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny,omitempty"`
	// Writes 声明这些远端工具会改动外部系统状态（建单、改状态、删数据）。
	//
	// 远端工具对平台是黑盒：除了 input_schema 之外没有语义，也看不到它到底发了
	// 什么请求，所以「是不是写」不可能像脚本插件那样从出网 verbs 推导，只能由
	// 作者声明。没声明按只读处理——远端鉴权与只读账号是第一道闸，平台不替它
	// 猜；声明之后每次调用都要人工确认，与脚本插件的「出网非只读」同一个口径。
	Writes bool `json:"writes,omitempty"`
}

// UISpec 是 component 的产物声明：入口是一份 DSL，不是可执行脚本。
type UISpec struct {
	Entry string `json:"entry"`
}

// SkillSpec 是 skill 的正文声明。
type SkillSpec struct {
	Entry  string   `json:"entry,omitempty"`
	Assets []string `json:"assets,omitempty"`
}

// DataSource 是组件的数据来源：指向同空间已安装的插件工具，由宿主代调。
type DataSource struct {
	ID     string `json:"id"`
	Plugin string `json:"plugin"`
	Tool   string `json:"tool,omitempty"`
	// Mode 为 action 时必须是用户手势触发。
	Mode   string         `json:"mode,omitempty"`
	Params map[string]any `json:"params,omitempty"`
}

// DerivedFrom 记录副本的来源基线，用于发现上游新版本。
// 它只描述「从哪来」，不重复定义 name / description。
type DerivedFrom struct {
	PluginID string `json:"plugin_id"`
	Version  string `json:"version"`
	SHA256   string `json:"sha256"`
}

// Manifest 是插件清单，也是元数据的唯一来源。
type Manifest struct {
	Protocol     string          `json:"protocol"`
	Kind         Kind            `json:"kind"`
	Slug         string          `json:"slug"`
	Version      string          `json:"version,omitempty"`
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	Capability   string          `json:"capability,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema,omitempty"`
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
	Capabilities []Capability    `json:"capabilities,omitempty"`
	Limits       Limits          `json:"limits,omitempty"`
	Extensible   *bool           `json:"extensible,omitempty"`
	DerivedFrom  *DerivedFrom    `json:"derived_from,omitempty"`
	Runtime      RuntimeSpec     `json:"runtime,omitempty"`
	Transport    *Transport      `json:"transport,omitempty"`
	Auth         *Auth           `json:"auth,omitempty"`
	Tools        *AllowedTools   `json:"tools,omitempty"`
	UI           *UISpec         `json:"ui,omitempty"`
	Skill        *SkillSpec      `json:"skill,omitempty"`
	Agent        *AgentSpec      `json:"agent,omitempty"`
	Source       *SourceSpec     `json:"source,omitempty"`
	DataSources  []DataSource    `json:"data_sources,omitempty"`
	// FS 是文件访问范围（只能收紧平台默认边界）。
	FS *FSSpec `json:"fs,omitempty"`
	// Config 是「安装方可填」的配置项声明：发布方定义 schema，安装方在空间里填值，
	// 平台负责渲染表单、校验与存储，运行时经 wa.config() 交给脚本。
	//
	// 它与 capabilities / limits 的区别：那两段约束的是「插件能做什么」，
	// 发布后不可改；config 是「这东西装在哪、怎么连、按什么策略跑」，属于安装。
	Config []ConfigItem `json:"config,omitempty"`
	// Builtin 表示「平台已在 Go 侧实现」的工具：没有脚本运行时，
	// 只借插件体系做全局安装与市场展示，执行仍走平台能力注册表。
	Builtin bool `json:"builtin,omitempty"`

	// DeriveRisk / DeriveEffects 只给平台内置件用，json:"-" 保证写不进清单。
	// 内置工具没有 capabilities 可推导，风险直接来自 Go 侧的那份声明。
	DeriveRisk    int    `json:"-"`
	DeriveEffects string `json:"-"`
}

// 安装级配置项的类型集合。
//
// 刻意保持小：每一种类型都要有「前端怎么渲染 + 后端怎么校验 + 运行时怎么读」
// 三处的确定语义，加一种就要把三处都补齐，而不是先塞进清单再让某处碰运气。
const (
	ConfigTypeText      = "text"       // 单行文本
	ConfigTypeTextarea  = "textarea"   // 多行文本
	ConfigTypeNumber    = "number"     // 十进制整数
	ConfigTypeBoolean   = "boolean"    // true / false
	ConfigTypeSelect    = "select"     // 枚举，值必须在 options 里
	ConfigTypeURL       = "url"        // http(s) 地址
	ConfigTypeList      = "list"       // 逗号/换行分隔的多值
	ConfigTypeSecretRef = "secret_ref" // 凭证引用名（只存名字，密钥在凭证存储里）
)

// configTypes 是允许在清单里声明的类型。
var configTypes = map[string]bool{
	ConfigTypeText: true, ConfigTypeTextarea: true, ConfigTypeNumber: true,
	ConfigTypeBoolean: true, ConfigTypeSelect: true, ConfigTypeURL: true,
	ConfigTypeList: true, ConfigTypeSecretRef: true,
}

// ConfigItem 是清单里的一条安装级配置声明。
//
// 为什么放在清单而不是产物里：安装方要看到「这个插件有哪些可调项、各自什么含义」，
// 而这些信息必须在发布时就是不可变的快照，不能由脚本在运行时临时改变表单。
type ConfigItem struct {
	// Key 是存储键，也是 wa.config() 里的字段名：发布后不可改（改了等于丢配置）。
	Key   string `json:"key"`
	Label string `json:"label"`
	Type  string `json:"type"`
	// Required 为 true 时安装方必须填非空值，保存时才校验（不能有默认值兜底）。
	Required bool `json:"required,omitempty"`
	// Default 是安装方没填过时的生效值，也必须通过本字段的类型校验。
	Default     string         `json:"default,omitempty"`
	Placeholder string         `json:"placeholder,omitempty"`
	Help        string         `json:"help,omitempty"`
	Options     []ConfigOption `json:"options,omitempty"`
	// Min / Max 只对 number 生效，写法是十进制整数的字符串（清单里统一用字符串，
	// 避免 JSON 数字在 float64 与 int 之间来回转）。
	Min string `json:"min,omitempty"`
	Max string `json:"max,omitempty"`
	// MaxLength 只对 text / textarea 生效，0 表示用平台默认（256 / 4096）。
	MaxLength int `json:"max_length,omitempty"`
}

// 搜索源的实现协议：声明式 JSON 与两段式脚本。
//
// 两者产出同一个东西——一段能从「检索词」走到「归一化结果」的适配器，
// 生命周期、权限、审计完全一致，差别只在「描述数据形状」还是「写变换逻辑」。
const (
	// SourceProtocolSpec 是声明式搜索源：search.json 描述请求与响应路径。
	SourceProtocolSpec = "spec"
	// SourceProtocolScript 是脚本搜索源：main.ts 输出 plan / map 两段。
	SourceProtocolScript = "script"
	// SourceSpecEntry 与 SourceScriptEntry 是对应协议的默认产物文件名。
	SourceSpecEntry   = "search.json"
	SourceScriptEntry = "main.ts"
)

// 搜索源的语义范围：让模型用「内网 / 公网」这种能力语义选源，而不是厂商名。
const (
	SourceScopeInternal = "internal"
	SourceScopeExternal = "external"
)

// SourceSpec 是搜索源插件的定义。
//
// endpoint 写在清单里而不是产物里：出网终点决定 Guard 的域名白名单，
// 必须在发布前就能静态看到，不能由脚本或 JSON 在运行时随意指定。
type SourceSpec struct {
	// Protocol 只能取 spec / script
	Protocol string `json:"protocol"`
	// Scope 是能力语义（internal / external），默认 external
	Scope string `json:"scope,omitempty"`
	// Entry 是产物文件，spec 默认 search.json，script 默认 main.ts
	Entry string `json:"entry,omitempty"`
	// Endpoint 是出网终点，必填；协议由 allow_private 决定
	Endpoint string `json:"endpoint"`
	// Secret 是凭证名（search scope），只写名字，密钥由平台注入
	Secret string `json:"secret,omitempty"`
	// AllowPrivate 放开「非 https + 私网地址」，用于内网搜索源；会推导成 L3
	AllowPrivate bool `json:"allow_private,omitempty"`
	// Priority 决定同 scope 下的优先顺序，数字小的优先
	Priority int `json:"priority,omitempty"`
	// TimeoutSeconds 是单次搜索的墙钟，默认 8 秒
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// AgentSpec 是智能体插件的定义。
//
// 智能体只描述行为：正文是提示词，执行能力来自 requires 引用的插件
// （技能/工具/MCP）。它不携带 endpoint、密钥或命令。
//
// Skills / Tools 是「自带」的内容：都在智能体插件目录内，随智能体一起发布与
// 安装，只对绑定了这个智能体的会话可见，不写 plugin_install，也不进插件市场。
// 和 requires 的差别就在这里——requires 借的是空间里的公共插件，会出现在空间
// 插件面板里；自带件属于智能体自己，别的智能体看不到。
type AgentSpec struct {
	// Entry 是提示词正文文件，默认 AGENT.md
	Entry string `json:"entry,omitempty"`
	// Extends 已废弃：父链拼接会取消，运行时不读这个字段。
	//
	// 保留它只为兼容存量清单（解析不报错），发布/试运行会提示它不再生效。
	// 要复用别的工作方式就复制正文，要复用能力写 Requires。
	Extends string `json:"extends,omitempty"`
	// Requires 是随智能体一并安装的插件 slug（技能/工具/MCP）
	Requires []string `json:"requires,omitempty"`
	// Skills 是自带技能页：相对插件目录的 .md 文件，或只取一层 *.md 的目录。
	// 解析出的名称与路径会进该智能体会话的检索池与系统提示词，正文仍按需 plugin_read。
	Skills []string `json:"skills,omitempty"`
	// Tools 是自带工具插件目录：相对插件目录，每个目录必须有 plugin.json 且 kind=tool。
	// 加载成 <智能体>__<工具>，只能在该智能体的会话里调用。
	Tools []string `json:"tools,omitempty"`
	// Icon 是头像文件（插件目录内的相对路径）。头像只用于列表与选择器展示，
	// 不参与执行；空表示没设置，前端用智能体兜底样式与普通会话区分。
	Icon string `json:"icon,omitempty"`
}

// 内置件的数量上限：清单直接进上下文，路径再多也只是噪音；体积上限沿用发布快照。
const (
	maxAgentSkills      = 12
	maxAgentTools       = 8
	maxAgentSkillFiles  = 50
	maxAgentSkillBytes  = 64 << 10
	maxAgentSkillDepth  = 3
	maxAgentSkillDescrR = 120
)

var (
	slugRe    = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)
	versionRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	// secretNameRe 是凭证名：只允许名字，不允许把密钥本身写进清单。
	secretNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.\-]{0,63}$`)
	// agentIconExts 是允许的头像格式：都是浏览器能直接显示的位图。
	// 上传接口会统一转成 256×256 的 PNG，这里只是兜底——手写清单写错扩展名会静默 404。
	agentIconExts = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".webp": true}
)

// validateAgentAssets 校验自带技能与工具的路径声明。
//
// 只看语法：文件在不在、工具清单合不合法由 validateAgentInternals 在发布与
// plugin_try 时对整份快照校验——清单本身不许碰文件系统，否则草稿也没法校验。
func validateAgentAssets(a *AgentSpec) error {
	if a == nil {
		return nil
	}
	if len(a.Skills) > maxAgentSkills {
		return fmt.Errorf("agent.skills 最多 %d 条", maxAgentSkills)
	}
	seen := map[string]bool{}
	for i, p := range a.Skills {
		if err := validateAgentAssetPath(p); err != nil {
			return fmt.Errorf("agent.skills[%d]: %w", i, err)
		}
		if seen[p] {
			return fmt.Errorf("agent.skills 里的 %q 重复", p)
		}
		seen[p] = true
	}
	if len(a.Tools) > maxAgentTools {
		return fmt.Errorf("agent.tools 最多 %d 个", maxAgentTools)
	}
	seen = map[string]bool{}
	for i, p := range a.Tools {
		if err := validateAgentAssetPath(p); err != nil {
			return fmt.Errorf("agent.tools[%d]: %w", i, err)
		}
		if seen[p] {
			return fmt.Errorf("agent.tools 里的 %q 重复", p)
		}
		seen[p] = true
	}
	return nil
}

// validateAgentAssetPath 校验内置件路径：必须是插件目录内的规整相对路径。
//
// 这些路径会被拼成存储 key，也会拼成 plugin_read 的路径，所以绝对路径、反斜杠、
// 隐藏段、越界一律拒绝；层级压住是为了让「智能体的附带内容」保持可读。
func validateAgentAssetPath(p string) error {
	if strings.TrimSpace(p) == "" {
		return fmt.Errorf("路径不能为空")
	}
	if strings.TrimSpace(p) != p {
		return fmt.Errorf("路径首尾不能有空白（%q）", p)
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return fmt.Errorf("必须是插件目录内的相对路径（%q）", p)
	}
	clean := path.Clean(p)
	if clean != p || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("路径必须是规整的相对路径（%q）", p)
	}
	segs := strings.Split(clean, "/")
	if len(segs) > maxAgentSkillDepth {
		return fmt.Errorf("路径最多 %d 层（%q）", maxAgentSkillDepth, p)
	}
	for _, seg := range segs {
		if seg == "" || seg == "." || seg == ".." || strings.HasPrefix(seg, ".") {
			return fmt.Errorf("路径不允许隐藏段或空段（%q）", p)
		}
	}
	if clean == ManifestFile {
		return fmt.Errorf("%q 不能是清单本身", p)
	}
	return nil
}

// validateAgentIcon 校验头像路径：必须是插件目录内的图片文件，不能跳出目录、不能是清单本身。
func validateAgentIcon(p string) error {
	if strings.HasPrefix(p, "/") || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") {
		return fmt.Errorf("agent.icon 必须是插件目录内的相对路径")
	}
	if p == ManifestFile {
		return fmt.Errorf("agent.icon 不能是 %s", ManifestFile)
	}
	if !agentIconExts[strings.ToLower(path.Ext(p))] {
		return fmt.Errorf("agent.icon 只支持 png / jpg / jpeg / webp，收到 %q", p)
	}
	return nil
}

// 上限：schema 直接进模型上下文，炸上下文比漏洞更快出事。
const (
	maxSchemaBytes = 4 << 10
	maxNameRunes   = 64
	maxDescRunes   = 500
	maxSlugLen     = 32
)

// Parse 解析并校验清单。publishing 为 true 时要求已写入合法版本号。
func Parse(raw []byte, publishing bool) (Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("清单解析失败（不允许未知字段）：%w", err)
	}
	if err := m.Validate(publishing); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// WritesRemote 判断这个插件的工具调用会不会改动外部系统状态。
//
// 两条判据，因为两类插件的出网方式不同：
//   - 脚本工具：请求由沙箱里的插件自己发，平台能看到动词，不是 GET/HEAD 就算写；
//   - MCP：请求由远端 Server 发，平台只能看到调用了哪个工具，所以只能看
//     tools.writes 声明。联网关时平台无法区分「写 PG 的库」和「删远端资源」，
//     所以宁可让作者显式说一句，也不替远端猜。
//
// 命中这条判据的调用都要人工确认；与 fs 写的默认（不审批）是分开的。
func (m Manifest) WritesRemote() bool {
	if m.Kind == KindMCP {
		return m.Tools != nil && m.Tools.Writes
	}
	_, verbs, _ := m.HostAllow()
	for v := range verbs {
		switch v {
		case "GET", "HEAD":
		default:
			return true
		}
	}
	return false
}

// HostAllow 把能力清单翻译成三张表：
//   - allow：脚本能调的宿主 RPC（只有 log 与 secret.get）；
//   - verbs：出网代理放行的 HTTP 动词；
//   - secrets：secret.get 能取到的凭证名，只含清单里逐个声明过的。
//
// 注意 http.request 在这里不是 RPC，而是「出网声明」：它只决定代理白名单与动词集合，
// 请求由插件自己在沙箱里发。verbs 为空时只放行 GET/HEAD，与 Derive 判 L2 的规则一致。
func (m Manifest) HostAllow() (allow, verbs, secrets map[string]bool) {
	allow = map[string]bool{MethodLog: true}
	verbs = map[string]bool{}
	secrets = map[string]bool{}
	for _, c := range m.Capabilities {
		switch c.Method {
		case MethodHTTP:
			for _, v := range c.Verbs {
				verbs[strings.ToUpper(v)] = true
			}
		case MethodSecret:
			if c.Name != "" {
				secrets[c.Name] = true
			}
		}
	}
	if len(verbs) == 0 && m.HasHTTP() {
		verbs["GET"] = true
		verbs["HEAD"] = true
	}
	if len(secrets) > 0 {
		allow[MethodSecretGet] = true
	}
	return allow, verbs, secrets
}

// HasHTTP 表示清单里有没有出网声明（有 hosts 才会建代理）。
func (m Manifest) HasHTTP() bool {
	for _, c := range m.Capabilities {
		if c.Method == MethodHTTP && len(c.Hosts) > 0 {
			return true
		}
	}
	return false
}

// NetHosts 返回清单声明的出网域名（小写去重）。
//
// 它是代理白名单与宿主守卫白名单的唯一来源：两处各算一份，就会出现
// 「宿主代发能过、沙箱代理连不上」这种作者完全无法自查的差异。
func (m Manifest) NetHosts() []string { return m.hostsOf(MethodHTTP) }

// WSNetHosts 返回 ws.connect 声明的域名：它们只用于 CONNECT 隧道，
// 与 http.request 的 absolute-form 白名单是两张表，不能混。
func (m Manifest) WSNetHosts() []string { return m.hostsOf(MethodWSConnect) }

func (m Manifest) hostsOf(method string) []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range m.Capabilities {
		if c.Method != method {
			continue
		}
		for _, h := range c.Hosts {
			h = strings.ToLower(strings.TrimSpace(h))
			if h == "" || seen[h] {
				continue
			}
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// FSWritable 表示插件是否被授予写权限：只有声明 fs.write 且没有 read_only。
func (m Manifest) FSWritable() bool {
	return m.FS != nil && !m.FS.ReadOnly && len(m.FS.Write) > 0
}

// Permissions 把清单翻译成「安装时展示给用户的权限条目」。
//
// 这里是权限文案的唯一定义点：市场、空间面板、安装确认都读它，前端只负责渲染。
// 让前端按 capabilities 自己拼句子，同一个插件在两个页面就会写成两种说法，
// 而且平台改了权限模型之后前端一定会漏改。
func (m Manifest) Permissions() []string {
	var out []string
	switch m.Kind {
	case KindSkill:
		out = append(out, "把技能正文注入模型上下文（仅在这个技能被检索或绑定时）")
	case KindAgent:
		out = append(out, "作为会话智能体的行为设定，本身不带额外权限")
		if m.Agent != nil && len(m.Agent.Requires) > 0 {
			out = append(out, "随包安装依赖："+strings.Join(m.Agent.Requires, "、"))
		}
	case KindComponent:
		out = append(out, "在会话里渲染界面（声明式 DSL，不含可执行代码）")
		for _, ds := range m.DataSources {
			verb := "读取"
			if ds.Mode == "action" {
				verb = "写操作"
			}
			id := ds.Plugin
			if ds.Tool != "" {
				id += "." + ds.Tool
			}
			out = append(out, verb+"插件数据："+id)
		}
	case KindTool:
		if m.Builtin {
			// 内置工具在 Go 侧实现，文件与网络由平台能力决定，不经过插件沙箱。
			// 按脚本插件那套文案套过来只会写着「不授予写权限」，与实际相反。
			out = append(out, "平台内置工具：由平台实现，不经过插件沙箱")
			out = append(out, m.capabilityLines()...)
			break
		}
		out = append(out, m.fsPermissionLines()...)
		out = append(out, m.capabilityLines()...)
	case KindMCP:
		if m.Transport != nil {
			line := "连接外部服务：" + hostOf(m.Transport.URL)
			// 明文连内网是允许的，但必须在安装确认里看得见：这条线只有作者能决定，
			// 安装的人没法从 host 反推出「走的是不是 https」。
			if strings.HasPrefix(m.Transport.URL, "http://") {
				line += "（明文 http，不加密）"
			}
			out = append(out, line)
		}
		if m.Tools != nil {
			line := "调用远端工具：" + strings.Join(m.Tools.Allow, "、")
			if len(m.Tools.Deny) > 0 {
				line += "（排除 " + strings.Join(m.Tools.Deny, "、") + "）"
			}
			out = append(out, line)
		}
		if m.Auth.JWT() {
			out = append(out, "以调用者身份签发 JWT：应用 "+m.Auth.AppName(m.Slug))
		} else if m.Auth != nil && m.Auth.Secret != "" {
			out = append(out, "使用凭证："+m.Auth.Secret+"（明文不进插件）")
		}
	case KindSource:
		if m.Source != nil {
			out = append(out, "访问网络："+hostOf(m.Source.Endpoint))
			if m.Source.AllowPrivate {
				out = append(out, "可访问内网地址")
			}
			if m.Source.Protocol == SourceProtocolScript {
				out = append(out, "执行搜索脚本（沙箱内，禁网）")
			}
			if m.Source.Secret != "" {
				out = append(out, "使用凭证："+m.Source.Secret+"（明文不进插件）")
			}
		}
	}
	return out
}

// fsPermissionLines 把文件范围说成人话：默认只读这件事必须写出来，
// 否则用户会以为「没写说明就是随便读写」。
func (m Manifest) fsPermissionLines() []string {
	read := "读取空间文件：全部（其他空间不可见）"
	if m.FS != nil && len(m.FS.Read) > 0 {
		read = "读取空间文件：" + strings.Join(m.FS.Read, "、")
	}
	out := []string{read}
	if m.FSWritable() {
		out = append(out, "写入空间文件："+strings.Join(m.FS.Write, "、"))
	} else {
		out = append(out, "不授予文件写权限")
	}
	return out
}

func (m Manifest) capabilityLines() []string {
	var out []string
	seen := map[string]bool{}
	add := func(line string) {
		if !seen[line] {
			seen[line] = true
			out = append(out, line)
		}
	}
	for _, c := range m.Capabilities {
		switch c.Method {
		case MethodHTTP:
			verbs := make([]string, 0, len(c.Verbs))
			for _, v := range c.Verbs {
				verbs = append(verbs, strings.ToUpper(v))
			}
			if len(verbs) == 0 {
				verbs = []string{"GET", "HEAD"}
			}
			add("访问网络：" + strings.Join(c.Hosts, "、") + "（" + strings.Join(verbs, "、") + "）")
		case MethodShell:
			add("执行命令（沙箱内，禁网）")
		case MethodSearch:
			add("调用平台搜索")
		case MethodWSConnect:
			add("建立加密隧道：" + strings.Join(c.Hosts, "、") + "（宿主只能看到域名，看不到隧道里的内容）")
		case MethodSecret:
			if c.Name != "" {
				add("读取平台凭证 " + c.Name + "（明文会进入插件进程，插件代码可以记录它）")
			}
		}
	}
	return out
}

// hostOf 只留主机名：安装确认里展示完整 URL 会把路径与查询串（可能带 token）带出来。
func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}

// validateMCPURL 校验 MCP 服务地址，清单与安装级配置共用这一份规则。
//
// 默认要求 https；http 只在两种情况下放行：目标是本机（调试用），或作者显式
// 声明了 transport.allow_http（企业内网常见）。allowHTTP 由调用方决定：清单校验
// 直接取 transport.allow_http，安装级配置取的是清单声明——发布方说「这个插件
// 允许明文」，部署方才能在配置里填内网 http 地址。
func validateMCPURL(raw string, allowHTTP bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("transport.url 不是合法 URL")
	}
	if u.User != nil {
		return fmt.Errorf("transport.url 不能带用户名密码，请改用 auth.secret 引用凭证")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if allowHTTP || isLocalHost(u.Hostname()) {
			return nil
		}
		// 报错里直接给写法：内网 http 是常态，别让人去翻文档猜怎么放开
		return fmt.Errorf("transport.url 只允许 https；内网明文服务请显式声明 transport.allow_http: true")
	default:
		return fmt.Errorf("transport.url 只支持 http / https，收到 %q", u.Scheme)
	}
}

// isLocalHost 判断是否本机：本机调试用 http 一直放行，不必为此声明 allow_http。
func isLocalHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// AllowsTool 判断远端工具是否在白名单内：allow 支持尾部 `*` 前缀匹配，deny 优先。
func (m Manifest) AllowsTool(name string) bool {
	if m.Tools == nil || len(m.Tools.Allow) == 0 {
		return false
	}
	hit := false
	for _, p := range m.Tools.Allow {
		if matchToolPattern(p, name) {
			hit = true
			break
		}
	}
	if !hit {
		return false
	}
	for _, p := range m.Tools.Deny {
		if matchToolPattern(p, name) {
			return false
		}
	}
	return true
}

func matchToolPattern(pattern, name string) bool {
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(name, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == name
}

// IsExtensible 表示插件是否允许在空间里生成可编辑的派生副本。
// 缺省为 true：只有平台预置件会显式写 false（不可改，只能整体升级）。
func (m Manifest) IsExtensible() bool {
	return m.Extensible == nil || *m.Extensible
}

// HasAgentSkills 表示智能体声明了自带技能页。
//
// 技能页要按 plugins/<slug>/... 的路径被 plugin_read 读到，所以这类智能体
// 即便不可扩展也要在空间里落一份副本。
func (m Manifest) HasAgentSkills() bool {
	return m.Kind == KindAgent && m.Agent != nil && len(m.Agent.Skills) > 0
}

// Validate 校验清单的硬约束。
func (m *Manifest) Validate(publishing bool) error {
	if m.Protocol != Protocol {
		return fmt.Errorf("protocol 必须是 %s，收到 %q", Protocol, m.Protocol)
	}
	if !m.Kind.Valid() {
		return fmt.Errorf("kind 只能是 skill / tool / mcp / component / agent / source，收到 %q", m.Kind)
	}
	if !slugRe.MatchString(m.Slug) {
		return fmt.Errorf("slug 必须是 2-%d 位小写字母开头的 [a-z0-9_]：收到 %q", maxSlugLen, m.Slug)
	}
	if n := utf8.RuneCountInString(strings.TrimSpace(m.Name)); n == 0 || n > maxNameRunes {
		return fmt.Errorf("name 必填且不超过 %d 个字符", maxNameRunes)
	}
	if n := utf8.RuneCountInString(strings.TrimSpace(m.Description)); n == 0 || n > maxDescRunes {
		return fmt.Errorf("description 必填且不超过 %d 个字符（模型靠它判断何时使用）", maxDescRunes)
	}
	// 版本只在发布时写入：草稿带版本会诱导手改版本号，绕过不可变约束。
	switch {
	case publishing && !versionRe.MatchString(m.Version):
		return fmt.Errorf("发布时必须提供 x.y.z 形式的小版本号，收到 %q", m.Version)
	case !publishing && m.Version != "":
		return fmt.Errorf("草稿不允许写 version（由发布流程生成）")
	}

	if m.Capability == "" {
		m.Capability = defaultCapability(m.Kind)
	}
	if !capabilities[m.Capability] {
		return fmt.Errorf("capability %q 不在允许列表内（插件不能自造能力名）", m.Capability)
	}
	if m.Builtin && m.Kind != KindTool {
		return fmt.Errorf("builtin 只用于平台内置工具（kind=tool）")
	}
	// tools 段描述的是「远端 Server 暴露哪些工具」，只有 MCP 有这层间接性。
	// 别的 kind 写了会被静默忽略，作者却以为声明生效了（尤其是 tools.writes）。
	if m.Tools != nil && m.Kind != KindMCP {
		return fmt.Errorf("tools 段只对 mcp 插件有效，%s 插件请用 capabilities 声明", m.Kind)
	}
	if err := checkSchema(m.InputSchema, "input_schema"); err != nil {
		return err
	}
	if err := checkSchema(m.OutputSchema, "output_schema"); err != nil {
		return err
	}

	for _, c := range m.Capabilities {
		if err := validateCapability(c); err != nil {
			return err
		}
	}
	if err := validateFS(m.FS); err != nil {
		return err
	}
	if err := validateConfigItems(*m); err != nil {
		return err
	}

	// resident 只对脚本工具有意义：技能/组件/智能体没有执行器，
	// 搜索源脚本走 search 包，不经过常驻池。
	if m.Runtime.Resident && m.Kind != KindTool {
		return fmt.Errorf("runtime.resident 只对 tool 插件有效")
	}

	switch m.Kind {
	case KindSkill:
		if m.Skill == nil {
			m.Skill = &SkillSpec{Entry: "SKILL.md"}
		}
		if m.Skill.Entry == "" {
			m.Skill.Entry = "SKILL.md"
		}
		if strings.Contains(m.Skill.Entry, "..") {
			return fmt.Errorf("skill.entry 不能包含 ..")
		}
	case KindTool:
		if m.Builtin {
			// 平台内置工具由 Go 实现，没有脚本入口与 schema 文件要求；
			// 仍然禁止声明 runtime.entry，避免出现「看起来有脚本」的假象。
			if m.Runtime.Entry != "" {
				return fmt.Errorf("builtin 工具不允许声明 runtime.entry")
			}
			if m.Runtime.Resident {
				return fmt.Errorf("builtin 工具不允许声明 runtime.resident：它由 Go 实现，没有脚本进程")
			}
			break
		}
		if m.Runtime.Entry == "" {
			return fmt.Errorf("tool 插件必须声明 runtime.entry")
		}
		if strings.Contains(m.Runtime.Entry, "..") {
			return fmt.Errorf("runtime.entry 不能包含 ..")
		}
		// 执行器只有 deno run：写成 .py / .sh 时清单能过、进程起不来，
		// 错误会推迟到试运行并显示成「未知扩展名」，作者看不出这是规则限制。
		// 与 source.entry 的 script 协议同口径。
		if !strings.HasSuffix(m.Runtime.Entry, ".ts") && !strings.HasSuffix(m.Runtime.Entry, ".js") {
			return fmt.Errorf("runtime.entry 必须是 .ts / .js：工具插件只能用 Deno 写，平台没有 Python / Node 运行时")
		}
		// 进程寿命由 runtime.resident 决定，单次调用的上限只有 limits 一个入口。
		// runtime.timeout_ms 从未生效过，留着会让作者以为「进程只活 8 秒」。
		if m.Runtime.TimeoutMS != 0 || m.Runtime.MaxRPC != 0 {
			return fmt.Errorf("runtime 只接受 entry 与 resident：超时与 RPC 上限统一写在 limits.timeout_ms / limits.max_rpc")
		}
		if len(m.InputSchema) == 0 {
			return fmt.Errorf("tool 插件必须声明 input_schema")
		}
	case KindAgent:
		if m.Agent == nil {
			m.Agent = &AgentSpec{}
		}
		if m.Agent.Entry == "" {
			m.Agent.Entry = AgentEntry
		}
		if strings.Contains(m.Agent.Entry, "..") || strings.HasPrefix(m.Agent.Entry, "/") {
			return fmt.Errorf("agent.entry 必须是插件目录内的相对路径")
		}
		if m.Agent.Entry == ManifestFile {
			return fmt.Errorf("agent.entry 不能是 %s：提示词与清单必须分开", ManifestFile)
		}
		if m.Agent.Icon != "" {
			if err := validateAgentIcon(m.Agent.Icon); err != nil {
				return err
			}
		}
		if m.Agent.Extends != "" {
			if !slugRe.MatchString(m.Agent.Extends) {
				return fmt.Errorf("agent.extends 必须是父智能体的 slug，收到 %q", m.Agent.Extends)
			}
			if m.Agent.Extends == m.Slug {
				return fmt.Errorf("agent.extends 不能指向自己")
			}
		}
		seenDep := map[string]bool{}
		for i, dep := range m.Agent.Requires {
			if !slugRe.MatchString(dep) {
				return fmt.Errorf("agent.requires[%d] 必须是插件 slug，收到 %q", i, dep)
			}
			if dep == m.Slug {
				return fmt.Errorf("agent.requires 不能引用自己")
			}
			if seenDep[dep] {
				return fmt.Errorf("agent.requires 里的 %q 重复", dep)
			}
			seenDep[dep] = true
		}
		if len(m.Agent.Requires) > 16 {
			return fmt.Errorf("agent.requires 最多 16 个")
		}
		if err := validateAgentAssets(m.Agent); err != nil {
			return err
		}
		// 智能体没有执行器，声明能力只会误导权限判断
		if len(m.Capabilities) > 0 {
			return fmt.Errorf("agent 插件不允许声明 capabilities（能力来自 requires 引用的插件）")
		}
	case KindSource:
		if m.Source == nil {
			return fmt.Errorf("source 插件必须声明 source 段")
		}
		s := m.Source
		s.Protocol = strings.ToLower(strings.TrimSpace(s.Protocol))
		switch s.Protocol {
		case SourceProtocolSpec:
			if s.Entry == "" {
				s.Entry = SourceSpecEntry
			}
			if !strings.HasSuffix(s.Entry, ".json") {
				return fmt.Errorf("source.entry 在 spec 协议下必须是 .json（描述数据形状，不是代码）")
			}
		case SourceProtocolScript:
			if s.Entry == "" {
				s.Entry = SourceScriptEntry
			}
			if !strings.HasSuffix(s.Entry, ".ts") && !strings.HasSuffix(s.Entry, ".js") {
				return fmt.Errorf("source.entry 在 script 协议下必须是 .ts / .js")
			}
		default:
			return fmt.Errorf("source.protocol 只能是 spec / script，收到 %q", s.Protocol)
		}
		if strings.Contains(s.Entry, "..") || strings.HasPrefix(s.Entry, "/") {
			return fmt.Errorf("source.entry 必须是插件目录内的相对路径")
		}
		if s.Entry == ManifestFile {
			return fmt.Errorf("source.entry 不能是 %s：产物与清单必须分开", ManifestFile)
		}

		s.Scope = strings.ToLower(strings.TrimSpace(s.Scope))
		if s.Scope == "" {
			s.Scope = SourceScopeExternal
		}
		if s.Scope != SourceScopeInternal && s.Scope != SourceScopeExternal {
			return fmt.Errorf("source.scope 只能是 internal / external，收到 %q", s.Scope)
		}

		// 出网终点必须能在发布前静态看到：Guard 的白名单由它推导，
		// 不能让脚本或 JSON 在运行期临时决定打到哪。
		ep := strings.TrimSpace(s.Endpoint)
		if ep == "" {
			return fmt.Errorf("source.endpoint 必填")
		}
		u, err := url.Parse(ep)
		if err != nil || u.Host == "" {
			return fmt.Errorf("source.endpoint 不是合法 URL: %q", ep)
		}
		if s.AllowPrivate {
			if u.Scheme != "https" && u.Scheme != "http" {
				return fmt.Errorf("source.endpoint 只允许 http / https，收到 %q", u.Scheme)
			}
		} else if u.Scheme != "https" {
			return fmt.Errorf("source.endpoint 只允许 https；内网源需要显式 allow_private")
		}

		if s.Secret != "" && !secretNameRe.MatchString(s.Secret) {
			return fmt.Errorf("source.secret 只能是凭证名（字母数字与 _.-），收到 %q", s.Secret)
		}
		if s.Priority < 0 || s.Priority > 1000 {
			return fmt.Errorf("source.priority 必须在 0-1000 之间")
		}
		if s.TimeoutSeconds == 0 {
			s.TimeoutSeconds = 8
		}
		if s.TimeoutSeconds < 1 || s.TimeoutSeconds > 25 {
			return fmt.Errorf("source.timeout_seconds 必须在 1-25 之间")
		}
		// 源不执行任意网络调用：终点由 endpoint 决定，不需要也不允许 capabilities
		if len(m.Capabilities) > 0 {
			return fmt.Errorf("source 插件不允许声明 capabilities（出网范围由 source.endpoint 决定）")
		}
	case KindMCP:
		if m.Transport == nil || m.Transport.URL == "" {
			return fmt.Errorf("mcp 插件必须声明 transport.url")
		}
		if err := validateMCPURL(m.Transport.URL, m.Transport.AllowHTTP); err != nil {
			return err
		}
		if m.Tools == nil || len(m.Tools.Allow) == 0 {
			return fmt.Errorf("tools.allow 必填且非空：默认不信任 MCP Server 的全部工具")
		}
		if err := m.normalizeAuth(); err != nil {
			return err
		}
	case KindComponent:
		if m.UI == nil {
			m.UI = &UISpec{}
		}
		if m.UI.Entry == "" {
			m.UI.Entry = DSLFile
		}
		if strings.Contains(m.UI.Entry, "..") || strings.HasPrefix(m.UI.Entry, "/") {
			return fmt.Errorf("ui.entry 必须是插件目录内的相对路径")
		}
		if m.UI.Entry == ManifestFile {
			return fmt.Errorf("ui.entry 不能是 %s：DSL 与清单必须分开", ManifestFile)
		}
		if !strings.HasSuffix(m.UI.Entry, ".json") {
			return fmt.Errorf("ui.entry 必须是 .json 的 DSL 文件（组件不是可执行脚本）")
		}
		if len(m.DataSources) > 16 {
			return fmt.Errorf("data_sources 最多 16 个")
		}
		seen := map[string]bool{}
		for i, ds := range m.DataSources {
			if ds.ID == "" || ds.Plugin == "" {
				return fmt.Errorf("data_sources[%d] 需要 id 与 plugin", i)
			}
			if !slugRe.MatchString(ds.ID) {
				return fmt.Errorf("data_sources[%d].id 必须是 2-%d 位小写字母开头的 [a-z0-9_]", i, maxSlugLen)
			}
			if seen[ds.ID] {
				return fmt.Errorf("data_sources 里的 id %q 重复", ds.ID)
			}
			seen[ds.ID] = true
			if ds.Mode == "" {
				m.DataSources[i].Mode = "read"
			} else if ds.Mode != "read" && ds.Mode != "action" {
				return fmt.Errorf("data_sources[%s].mode 只能是 read / action", ds.ID)
			}
			if err := checkParamsSize(ds.Params); err != nil {
				return fmt.Errorf("data_sources[%s].params: %w", ds.ID, err)
			}
		}
	}

	if m.Limits.TimeoutMS <= 0 {
		m.Limits.TimeoutMS = 8000
	}
	if m.Limits.TimeoutMS > 25000 {
		return fmt.Errorf("limits.timeout_ms 不能超过 25000（Invoker 墙钟 30s）")
	}
	if m.Limits.MaxRPC <= 0 {
		m.Limits.MaxRPC = 10
	}
	if m.Limits.MaxRPC > 50 {
		return fmt.Errorf("limits.max_rpc 不能超过 50")
	}
	if m.Limits.MaxResultBytes <= 0 {
		m.Limits.MaxResultBytes = 4 << 10
	}
	if m.Limits.MaxResultBytes > 24<<10 {
		return fmt.Errorf("limits.max_result_bytes 不能超过 24KB")
	}
	return nil
}

func defaultCapability(k Kind) string {
	switch k {
	case KindSkill:
		return CapSkillUse
	case KindComponent:
		return CapUIRender
	case KindMCP:
		return CapHTTPRequest
	case KindAgent:
		return CapAgentUse
	case KindSource:
		// 源本身不对模型暴露：它实现的是已有的 search.query 能力
		return CapSearchQuery
	}
	return CapPluginInvok
}

// checkParamsSize 限制数据源默认参数体积：它们会随每次取数一起进入工具调用。
func checkParamsSize(params map[string]any) error {
	if len(params) == 0 {
		return nil
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	if len(raw) > 2<<10 {
		return fmt.Errorf("默认参数超过 2KB")
	}
	return nil
}

func checkSchema(raw json.RawMessage, field string) error {
	if len(raw) == 0 {
		return nil
	}
	if len(raw) > maxSchemaBytes {
		return fmt.Errorf("%s 超过 %d 字节（会直接进入模型上下文）", field, maxSchemaBytes)
	}
	text := string(raw)
	if strings.Contains(text, `"$ref"`) {
		return fmt.Errorf("%s 不允许 $ref（远程引用会让 schema 不可控）", field)
	}
	if strings.Contains(text, `"patternProperties"`) {
		return fmt.Errorf("%s 不允许 patternProperties", field)
	}
	return nil
}

func validateCapability(c Capability) error {
	switch c.Method {
	case MethodHTTP:
		if len(c.Hosts) == 0 {
			return fmt.Errorf("http.request 必须声明 hosts 白名单")
		}
		for _, h := range c.Hosts {
			if err := validateHost(h); err != nil {
				return err
			}
		}
		for _, v := range c.Verbs {
			switch strings.ToUpper(v) {
			case "GET", "HEAD":
			case "POST", "PUT", "PATCH", "DELETE":
				// 允许，但会推导成 L3
			default:
				return fmt.Errorf("http.request 不支持动词 %q", v)
			}
		}
	case "fs.read", "fs.list", "fs.write":
		// 已经不是宿主能力了：文件由沙箱挂载 + deno 权限管，不走 RPC。
		return fmt.Errorf("%s 已移除：文件范围只写在顶层 fs 段（read / write / read_only），读写用 Deno 的 API", c.Method)
	case MethodWSConnect:
		// 隧道只能按域名授权：端口固定 443，路径与内容宿主看不到，
		// 所以这里除了 hosts 不允许再写别的，避免给出「写了就生效」的错觉。
		if len(c.Hosts) == 0 {
			return fmt.Errorf("ws.connect 必须声明 hosts 白名单")
		}
		for _, h := range c.Hosts {
			if err := validateHost(h); err != nil {
				return err
			}
		}
		if len(c.Verbs) > 0 {
			return fmt.Errorf("ws.connect 不接受 verbs：隧道里的方法宿主看不见，写了也不会生效")
		}
		if c.Name != "" {
			return fmt.Errorf("ws.connect 不接受 name：凭证请用 secret.use")
		}
	case MethodShell, MethodSearch:
		// 平台已实现的能力，不接受额外参数：范围由能力本身决定
	case MethodSecret:
		// 必须写清楚能取哪个凭证：没有名字就拒，不能退化成「整个 scope 可读」。
		if c.Name == "" {
			return fmt.Errorf("secret.use 必须声明 name（要取的那个凭证名），一个凭证一条能力")
		}
		if !secretNameRe.MatchString(c.Name) {
			return fmt.Errorf("secret.use 的 name %q 不合法：只允许字母、数字、下划线、连字符", c.Name)
		}
		if len(c.Hosts) > 0 || len(c.Verbs) > 0 {
			return fmt.Errorf("secret.use 不接受 hosts / verbs：凭证与出网是两条独立声明")
		}
	default:
		return fmt.Errorf("能力 method %q 不在宿主白名单内", c.Method)
	}
	return nil
}

// validateFS 只挡「越界」：路径必须相对、不能含 ..。
//
// 真正的越界在解析成绝对路径时还会再挡一次（resolveFSPaths），
// 这里检查是为了让作者在试运行 / 发布时就拿到错误，而不是等运行时。
func validateFS(fs *FSSpec) error {
	if fs == nil {
		return nil
	}
	lists := []struct {
		field string
		paths []string
	}{
		{"fs.read", fs.Read},
		{"fs.write", fs.Write},
	}
	for _, l := range lists {
		for _, p := range l.paths {
			raw := strings.TrimSpace(p)
			if raw == "" {
				return fmt.Errorf("%s 不能包含空路径", l.field)
			}
			if strings.HasPrefix(raw, "/") || filepath.IsAbs(raw) {
				return fmt.Errorf("%s 必须是相对空间工作区的路径：%q", l.field, p)
			}
			clean := path.Clean(raw)
			if clean == ".." || strings.HasPrefix(clean, "../") {
				return fmt.Errorf("%s 不能越出空间工作区：%q", l.field, p)
			}
			// 只允许声明子目录：`.` 等于整个空间，等于绕开这次收敛。
			if clean == "." {
				return fmt.Errorf("%s 必须是空间工作区下的子目录，不能用 %q 声明整个空间", l.field, p)
			}
		}
	}
	return nil
}

// validateHost 要求域名，不收 scheme / 端口 / 路径 / IP：白名单按域名边界匹配。
func validateHost(raw string) error {
	h := strings.TrimSpace(raw)
	if h == "" {
		return fmt.Errorf("hosts 不能包含空值")
	}
	if strings.Contains(h, "://") || strings.ContainsAny(h, "/?#") {
		return fmt.Errorf("hosts 只写域名，不要带协议、端口或路径：%q", raw)
	}
	if strings.Contains(h, ":") {
		return fmt.Errorf("hosts 不要带端口：%q", raw)
	}
	if net.ParseIP(h) != nil {
		return fmt.Errorf("hosts 只接受域名，不接受 IP：%q", raw)
	}
	if !strings.Contains(h, ".") {
		return fmt.Errorf("hosts 需要完整域名：%q", raw)
	}
	return nil
}

// ---------------------------------------------------------------- 安装级配置

const (
	// maxConfigItems 限制表单规模：安装页是一屏能看完的东西，
	// 超过 20 项更该考虑「这插件是不是该拆」。
	maxConfigItems = 20
	// maxConfigLabelRunes 是字段标题长度：标题是给人扫的，不是说明文档。
	maxConfigLabelRunes = 40
)

// validateConfigItems 校验清单里的安装级配置声明。
//
// 这些问题必须在发布时暴露：config 段最终会直接变成安装页的表单，
// 留到安装时才失败，安装方看到的只是一个「点了保存就报错」的对话框。
func validateConfigItems(m Manifest) error {
	if len(m.Config) == 0 {
		return nil
	}
	// 只有脚本工具消费配置（wa.config()）。内置件的执行体在 Go 里，声明了没人读；
	// 技能/组件/智能体没有执行器，同样没有消费点。
	if m.Kind != KindTool || m.Builtin {
		return fmt.Errorf("config 段只对脚本工具（kind=tool 且非内置）有效：%s 插件请把参数写进清单或正文", m.Kind)
	}
	if len(m.Config) > maxConfigItems {
		return fmt.Errorf("config 最多声明 %d 项", maxConfigItems)
	}
	seen := map[string]bool{}
	for i, item := range m.Config {
		if !configKeyRe.MatchString(item.Key) {
			return fmt.Errorf("config[%d].key 必须是 1-%d 位小写字母开头的 [a-z0-9_]：收到 %q", i, maxConfigKeyLen, item.Key)
		}
		if seen[item.Key] {
			return fmt.Errorf("config[%d].key 重复：%q", i, item.Key)
		}
		seen[item.Key] = true
		if reservedConfigKeys[item.Key] {
			return fmt.Errorf("config[%d].key %q 是平台保留键：它对应执行器行为（地址/鉴权/审批），清单不能重复声明", i, item.Key)
		}
		if n := utf8.RuneCountInString(strings.TrimSpace(item.Label)); n == 0 || n > maxConfigLabelRunes {
			return fmt.Errorf("config[%d].label 必填且不超过 %d 个字符", i, maxConfigLabelRunes)
		}
		if !configTypes[item.Type] {
			return fmt.Errorf("config[%d].type 只支持 text / textarea / number / boolean / select / url / list / secret_ref，收到 %q", i, item.Type)
		}
		if err := validateConfigShape(item); err != nil {
			return fmt.Errorf("config[%d]（%s）：%w", i, item.Key, err)
		}
		// required + default 是自相矛盾的组合：有默认值就永远不会为空，
		// required 拦不住任何人，作者多半想的是「必填但给个建议值」。
		if item.Required && item.Default != "" {
			return fmt.Errorf("config[%d]（%s）同时声明了 required 与 default：有默认值就永远不会为空", i, item.Key)
		}
		if item.Default != "" {
			if err := validateConfigValue(item, item.Default); err != nil {
				return fmt.Errorf("config[%d]（%s）的 default 不合法：%w", i, item.Key, err)
			}
		}
	}
	return nil
}

// validateConfigShape 校验「字段形态」：哪些属性适用于哪些类型。
//
// 不适用的属性一律拒绝而不是忽略：清单里写了 max: 10 却没生效，
// 作者会以为平台在按 10 校验，直到某天录进 999。
func validateConfigShape(item ConfigItem) error {
	switch item.Type {
	case ConfigTypeSelect:
		if len(item.Options) == 0 {
			return fmt.Errorf("select 必须声明 options")
		}
		values := map[string]bool{}
		for _, op := range item.Options {
			v := strings.TrimSpace(op.Value)
			if v == "" {
				return fmt.Errorf("options 里有空 value")
			}
			if values[v] {
				return fmt.Errorf("options 的 value 重复：%q", v)
			}
			values[v] = true
			if strings.TrimSpace(op.Label) == "" {
				return fmt.Errorf("options[%s] 缺少 label", v)
			}
		}
	case ConfigTypeNumber:
		if _, err := configRange(item); err != nil {
			return err
		}
	default:
		if len(item.Options) > 0 {
			return fmt.Errorf("%s 类型不支持 options（只有 select 有）", item.Type)
		}
		if item.Min != "" || item.Max != "" {
			return fmt.Errorf("%s 类型不支持 min/max（只有 number 有）", item.Type)
		}
	}
	if item.MaxLength > 0 && item.Type != ConfigTypeText && item.Type != ConfigTypeTextarea {
		return fmt.Errorf("max_length 只对 text / textarea 生效")
	}
	return nil
}

// Derive 由能力清单推导风险等级与副作用范围。
//
// 作者不能声明 risk：否则写一句 "risk": 1 就白拿了所有权限，
// 自报风险永远不可信，只能从能力声明推导。
func Derive(m Manifest) (int, string) {
	// 平台内置工具没有脚本、也没有清单可推导的副作用：
	// 它们的风险来自 Go 侧实现（tool.Manifest.Risk / SideEffects），
	// seed 时写进这两个非 JSON 字段，作者无论如何也塞不进来。
	if m.DeriveRisk > 0 {
		if m.DeriveEffects == "" {
			return m.DeriveRisk, policy.SideEffectsReadOnly
		}
		return m.DeriveRisk, m.DeriveEffects
	}

	risk := policy.RiskReadOnly
	effects := policy.SideEffectsReadOnly
	hasSecret, hasWriteHTTP := false, false

	for _, c := range m.Capabilities {
		switch c.Method {
		case MethodHTTP:
			if httpWrites(c) {
				hasWriteHTTP = true
				risk = maxRisk(risk, policy.RiskWrite)
				effects = policy.SideEffectsExternal
			} else {
				risk = maxRisk(risk, policy.RiskReadCost)
			}
		case MethodShell:
			risk = maxRisk(risk, policy.RiskWrite)
			if effects == policy.SideEffectsReadOnly {
				effects = policy.SideEffectsWorkspace
			}
		case MethodSearch:
			risk = maxRisk(risk, policy.RiskReadCost)
			if effects == policy.SideEffectsReadOnly {
				effects = policy.SideEffectsExternal
			}
		case MethodWSConnect:
			// 隧道里是任意协议：宿主看不见内容，也拦不住写，
			// 只能按「外部写」给等级，并在安装确认里写清楚。
			risk = maxRisk(risk, policy.RiskWrite)
			effects = policy.SideEffectsExternal
		case MethodSecret:
			hasSecret = true
			risk = maxRisk(risk, policy.RiskWrite)
		}
	}

	// 搜索源：读外部数据，按 L2 计；访问内网（allow_private）按 L3 处理，
	// 因为它能穿过「公网搜索源」这道边界去看内网接口。
	if m.Kind == KindSource {
		risk = maxRisk(risk, policy.RiskReadCost)
		if effects == policy.SideEffectsReadOnly {
			effects = policy.SideEffectsExternal
		}
		if m.Source != nil && m.Source.AllowPrivate {
			risk = maxRisk(risk, policy.RiskWrite)
		}
	}

	// MCP 是外部系统调用：它对平台内数据没有任何读写权限，平台也不该替远端
	// 判断「这次调用在它那边算不算写」。默认按「只读有成本」处理——副作用由
	// 远端的鉴权、只读账号、限流与审计负责。
	// 作者声明 tools.writes 时才抬到 L3：这时卡片要能一眼看出「这一步会改远端」，
	// 且每次调用都要过审批。
	if m.Kind == KindMCP {
		risk = maxRisk(risk, policy.RiskReadCost)
		if effects == policy.SideEffectsReadOnly {
			effects = policy.SideEffectsExternal
		}
		if m.Tools != nil && m.Tools.Writes {
			risk = maxRisk(risk, policy.RiskWrite)
		}
	}
	// 文件写权限由 fs 段声明：能写空间文件就是写操作，
	// 不能因为作者没在 capabilities 里重复写一行 fs.write 就按只读处理。
	if m.FSWritable() {
		risk = maxRisk(risk, policy.RiskWrite)
		if effects == policy.SideEffectsReadOnly {
			effects = policy.SideEffectsWorkspace
		}
	}
	if hasSecret && hasWriteHTTP {
		risk = maxRisk(risk, policy.RiskSensitive)
		effects = policy.SideEffectsExternal
	}
	return risk, effects
}

func httpWrites(c Capability) bool {
	if len(c.Verbs) == 0 {
		// 未声明动词按只读处理：声明 GET 的成本低于被默认按写处理
		return false
	}
	for _, v := range c.Verbs {
		switch strings.ToUpper(v) {
		case "GET", "HEAD":
		default:
			return true
		}
	}
	return false
}

func maxRisk(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Hosts 汇总所有 http.request 的域名白名单。
func (m Manifest) Hosts() []string {
	seen := map[string]bool{}
	out := make([]string, 0, 4)
	// 搜索源没有 capabilities：出网域名由 source.endpoint 唯一决定。
	if m.Kind == KindSource && m.Source != nil {
		if u, err := url.Parse(strings.TrimSpace(m.Source.Endpoint)); err == nil {
			if h := strings.ToLower(u.Hostname()); h != "" {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	for _, c := range m.Capabilities {
		if c.Method != MethodHTTP {
			continue
		}
		for _, h := range c.Hosts {
			h = strings.ToLower(strings.TrimSpace(h))
			if h != "" && !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	return out
}

// ExposedTools 返回安装后暴露给模型的工具名（不含 slug 前缀）。
func (m Manifest) ExposedTools() []string {
	switch m.Kind {
	case KindTool:
		return []string{m.Slug}
	case KindMCP:
		if m.Tools == nil {
			return nil
		}
		out := make([]string, 0, len(m.Tools.Allow))
		deny := map[string]bool{}
		for _, d := range m.Tools.Deny {
			deny[d] = true
		}
		for _, name := range m.Tools.Allow {
			if !deny[name] {
				out = append(out, name)
			}
		}
		return out
	}
	return nil
}

// ToolName 是模型可见的工具名：{slug}__{tool}，总长不超过 64。
func ToolName(slug, tool string) string {
	if tool == "" || tool == slug {
		return slug
	}
	name := slug + "__" + tool
	if len(name) <= 64 {
		return name
	}
	// 超长时保留可读前缀，用短哈希保证唯一
	sum := sha256.Sum256([]byte(name))
	return name[:48] + "_" + hex.EncodeToString(sum[:4])
}

// HashDir 计算目录内容指纹（排序后的路径 + 文件内容）。
func HashDir(files map[string][]byte) string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sortStrings(names)
	h := sha256.New()
	for _, name := range names {
		sum := sha256.Sum256(files[name])
		h.Write([]byte(name))
		h.Write([]byte{0})
		h.Write(sum[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
