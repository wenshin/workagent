// Package plugin 实现插件体系（docs/插件体系设计.md）。
//
// 四类插件共用一份清单（plugin.json）、一套能力声明、一条生命周期
// （开发 → 发布 → 安装 → 运行 → 卸载），差异只在执行器：
//
//	skill      无执行器，正文按需注入
//	tool       Deno 进程 + 宿主 ABI（无网络、无文件写权限）
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
	"regexp"
	"strings"
	"unicode/utf8"

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
const (
	MethodHTTP   = "http.request"
	MethodFSRead = "fs.read"
	MethodFSList = "fs.list"
	MethodFSWr   = "fs.write"
	MethodSecret = "secret.use"
	MethodLog    = "log"
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
	// Paths 是 fs.* 的相对路径前缀，空表示整个空间工作区。
	Paths []string `json:"paths,omitempty"`
}

// RuntimeSpec 是 tool 的执行参数。
type RuntimeSpec struct {
	Entry     string `json:"entry,omitempty"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
	MaxRPC    int    `json:"max_rpc,omitempty"`
}

// Transport 是 mcp 的连接方式。
type Transport struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

// Auth 是 mcp 的鉴权方式。
type Auth struct {
	In     string `json:"in,omitempty"`
	Name   string `json:"name,omitempty"`
	Secret string `json:"secret,omitempty"`
	Mode   string `json:"mode,omitempty"`
}

// AllowedTools 是 mcp 的工具白名单。空清单视为非法：等价于开放全部。
type AllowedTools struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny,omitempty"`
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
	// Builtin 表示「平台已在 Go 侧实现」的工具：没有脚本运行时，
	// 只借插件体系做全局安装与市场展示，执行仍走平台能力注册表。
	Builtin bool `json:"builtin,omitempty"`
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
type AgentSpec struct {
	// Entry 是提示词正文文件，默认 AGENT.md
	Entry string `json:"entry,omitempty"`
	// Extends 是父智能体 slug，只能指向另一个 agent 插件；空表示只追加到平台基础规则
	Extends string `json:"extends,omitempty"`
	// Requires 是随智能体一并安装的插件 slug（技能/工具/MCP）
	Requires []string `json:"requires,omitempty"`
}

var (
	slugRe    = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)
	versionRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	// secretNameRe 是凭证名：只允许名字，不允许把密钥本身写进清单。
	secretNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.\-]{0,63}$`)
)

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

// HostAllow 把能力清单翻译成宿主方法白名单与 HTTP 动词集合。
//
// 脚本侧看到的 SDK 是一样的，能不能调到由这张表决定；verbs 为空时只放行 GET/HEAD，
// 与 Derive 把「只读 HTTP」判成 L2 的规则保持一致。
func (m Manifest) HostAllow() (allow, verbs map[string]bool, hasSecret bool) {
	allow = map[string]bool{}
	verbs = map[string]bool{}
	for _, c := range m.Capabilities {
		allow[c.Method] = true
		if c.Method == MethodHTTP {
			for _, v := range c.Verbs {
				verbs[strings.ToUpper(v)] = true
			}
		}
	}
	if allow[MethodHTTP] && len(verbs) == 0 {
		verbs["GET"] = true
		verbs["HEAD"] = true
	}
	return allow, verbs, allow[MethodSecret]
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
			break
		}
		if m.Runtime.Entry == "" {
			return fmt.Errorf("tool 插件必须声明 runtime.entry")
		}
		if strings.Contains(m.Runtime.Entry, "..") {
			return fmt.Errorf("runtime.entry 不能包含 ..")
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
		if !strings.HasPrefix(m.Transport.URL, "https://") {
			return fmt.Errorf("transport.url 只允许 https")
		}
		if m.Tools == nil || len(m.Tools.Allow) == 0 {
			return fmt.Errorf("tools.allow 必填且非空：默认不信任 MCP Server 的全部工具")
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
	case MethodFSRead, MethodFSList, MethodFSWr:
		for _, p := range c.Paths {
			if strings.HasPrefix(p, "/") || strings.Contains(p, "..") {
				return fmt.Errorf("fs 路径必须是空间内相对路径：%q", p)
			}
		}
	case MethodShell, MethodSearch:
		// 平台已实现的能力，不接受额外参数：范围由能力本身决定
	case MethodSecret:
		// 只允许引用名字，不允许明文
	default:
		return fmt.Errorf("能力 method %q 不在宿主白名单内", c.Method)
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

// Derive 由能力清单推导风险等级与副作用范围。
//
// 作者不能声明 risk：否则写一句 "risk": 1 就白拿了所有权限，
// 和 tool_binding 里「不能自己降级」是同一个坑。
func Derive(m Manifest) (int, string) {
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
		case MethodFSWr, MethodShell:
			risk = maxRisk(risk, policy.RiskWrite)
			if effects == policy.SideEffectsReadOnly {
				effects = policy.SideEffectsWorkspace
			}
		case MethodSearch:
			risk = maxRisk(risk, policy.RiskReadCost)
			if effects == policy.SideEffectsReadOnly {
				effects = policy.SideEffectsExternal
			}
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

	// MCP 的工具集由远端决定，平台无法静态判断是否会写；按最保守处理。
	if m.Kind == KindMCP {
		risk = maxRisk(risk, policy.RiskWrite)
		if effects == policy.SideEffectsReadOnly {
			effects = policy.SideEffectsExternal
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
