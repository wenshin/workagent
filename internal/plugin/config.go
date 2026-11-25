package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/workagent/auto-agent/internal/agentspec"
	"github.com/workagent/auto-agent/internal/store"
	"github.com/workagent/auto-agent/internal/tool"
)

// 安装级配置键：MCP 插件在不同空间可能连不同服务、用不同凭证，
// 这些值属于「安装」而不是「发布」，所以放在 plugin_install.config。
const (
	ConfigURL      = "url"
	ConfigAuthName = "auth_name"
	ConfigSecret   = "secret"
	// ConfigApprovalCommands 是 shell 插件的「需确认命令」名单（逗号分隔）。
	// 空值表示不放宽：沿用平台默认；显式清空（只删内容不删键）等于全不审批。
	ConfigApprovalCommands = "approval_commands"
	// ConfigApprovalPolicy 是这个插件的审批档位（declared / always / off）。
	// 它是安装方对「这个插件整体要不要确认」的选择，覆盖清单声明，
	// 与风险等级无关——等级仍然只用于展示。
	//
	// 键名与读取侧共用一个常量：内置工具的档位由 tool.builtinPolicy 读，
	// 两边字面量漂移就会变成「配了但读不到」。
	ConfigApprovalPolicy = tool.ApprovalPolicyKey
	// ConfigAgentPrompt 是智能体正文的安装级覆盖（插件卡片上的「提示词覆盖」）。
	// 空值表示用插件包里的 AGENT.md；它只改行为设定，改不了平台基础规则。
	ConfigAgentPrompt = "prompt"
)

// 凭证名 / Header 名只允许标识符字符：不允许表达式、路径或空格的逃逸空间。
var configNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// configKeyRe 是清单配置键的命名规则：小写字母开头，只含 [a-z0-9_]。
//
// 键会原样变成 wa.config() 的字段名。允许大写与中划线的话，同一个声明在脚本里
// 会出现「approval-policy / approvalPolicy」这类猜名字的写法，最后表现成
// 「配了但读不到」，而且很难自查。
var configKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// maxConfigKeyLen 与 configKeyRe 的长度上限保持一致。
const maxConfigKeyLen = 64

// reservedConfigKeys 是平台占用的键：它们与执行器行为强绑定（MCP 连接、
// 审批档位、shell 名单），由平台按 kind/capability 生成字段。清单重复声明同名键
// 会让「表单里那个值到底谁生效」无法解释，所以发布时直接拒绝。
var reservedConfigKeys = map[string]bool{
	ConfigURL: true, ConfigAuthName: true, ConfigSecret: true,
	ConfigApprovalCommands: true, ConfigApprovalPolicy: true, ConfigAgentPrompt: true,
}

// ConfigOption 是 select 型字段的一个选项。
type ConfigOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
	// Help 说明选它之后会发生什么（前端渲染在下面一行）。
	Help string `json:"help,omitempty"`
}

// ConfigField 是一个可编辑项，前端据此渲染表单，不写死字段名。
type ConfigField struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Type        string `json:"type"`
	Required    bool   `json:"required"`
	Placeholder string `json:"placeholder,omitempty"`
	Help        string `json:"help,omitempty"`
	// Note 是字段下方的事实补充，不随选项变化。审批档位用它列出「这个插件
	// 被允许访问哪些网站」——用户做的不只是「问几次」的选择，而是「要不要继续
	// 信任这个插件」的决定，不知道它能连哪里，这个决定就没有依据。
	Note    string         `json:"note,omitempty"`
	Options []ConfigOption `json:"options,omitempty"`
	// Default 是安装方没填过时的生效值（前端可用来提示「留空即 xxx」）。
	Default string `json:"default,omitempty"`
}

// ConfigView 是配置页的数据：字段定义 + 当前生效值（凭证只回显名字，不回显密钥）。
type ConfigView struct {
	Slug   string            `json:"slug"`
	Kind   string            `json:"kind"`
	Fields []ConfigField     `json:"fields"`
	Values map[string]string `json:"values"`
	// Scope 说明这份配置的影响范围（space / global），前端据此提示，
	// 别把「改一次全局生效」说成「只影响当前空间」。
	Scope string `json:"scope"`
}

// 配置的作用范围。
const (
	// ScopeSpace：配置只影响当前空间（插件是按空间安装的）。
	ScopeSpace = "space"
	// ScopeGlobal：配置对全部空间生效。平台内置件只有系统空间那一份安装记录，
	// 工具加载时读的也是它（见 tool.builtinPolicy），改它等于改所有空间的行为。
	ScopeGlobal = "global"
)

// configScope 判断这份配置改给谁看。
func configScope(meta Manifest) string {
	if meta.Builtin {
		return ScopeGlobal
	}
	return ScopeSpace
}

// approvalField 是「风险控制」：两档，调用前要不要人工审批。
//
// 标题下的说明是这个工具的能力清单——它要访问哪些域名、会不会写文件。用户
// 打开配置是为了决定「要不要继续让它碰我的东西」，看不到它能连哪儿、能写什么，
// 这个决定就没有依据；「允许的网站」这种抽象说法等于没说。
//
// 「需要用户审批」直接落到 declared（按清单声明拦有风险的调用），不再有单独的
// always 档：三档并存时用户既要理解「什么算风险」，又要猜这一档到底拦不拦，
// 最后没人能预期某次调用会不会被打断。always 仍被 ApprovalLevelOf 识别，旧配置
// 继续按「每次都问」执行——只会更严，界面统一显示成「需要用户审批」。
func approvalField(help string) ConfigField {
	return ConfigField{
		Key: ConfigApprovalPolicy, Label: "风险控制", Type: "select", Help: help,
		Options: []ConfigOption{
			{
				Value: tool.ApprovalDeclared,
				Label: "需要用户审批",
				Help:  "大模型调用该工具时，需要用户审批。",
			},
			{
				Value: tool.ApprovalOff,
				Label: "不需要用户审批",
				Help:  "大模型调用该工具时，不需要用户审批。",
			},
		},
	}
}

// configFields 返回安装级可配置项：平台按 kind/capability 生成的行为字段
// （审批档位、MCP 地址与鉴权、shell 名单），后面追加清单声明的通用配置项。
//
// 顺序固定为「平台字段在前」：它们决定插件能不能跑起来，清单字段是调优项，
// 排在后面符合「先接通、再调参」的填写顺序。
func configFields(meta Manifest) []ConfigField {
	fields := platformConfigFields(meta)
	for _, item := range meta.Config {
		fields = append(fields, ConfigField{
			Key: item.Key, Label: item.Label, Type: item.Type,
			Required:    item.Required,
			Placeholder: item.Placeholder,
			Help:        item.Help,
			Options:     item.Options,
			Default:     item.Default,
		})
	}
	return fields
}

// platformConfigFields 是平台内建字段：它们的值会被执行器直接消费
// （审批档位进 tool.ApprovalLevel、MCP 地址进 Transport、shell 名单进 Tool），
// 所以不能交给清单声明，只能由平台按能力生成。
func platformConfigFields(meta Manifest) []ConfigField {
	// shell 的特殊之处：它是唯一「命令内容在运行时才知道」的能力，
	// 哪些命令需要确认只能由使用方按自己的数据来定。
	if meta.Capability == CapShellExec {
		return []ConfigField{
			// shell 的站点在命令里，配置页列不出来——说清它的动作形态。
			approvalField("该工具会在本机执行命令，写本空间的文件。"),
			{
				Key: ConfigApprovalCommands, Label: "需确认的命令", Type: "list",
				Placeholder: "rm, rmdir, dd, truncate, shred, chmod, chown, sudo, su, mount",
				Help:        "逗号或换行分隔。命中这些命令时先弹确认框；输入框里是当前生效的名单，清空并保存表示都不确认（沙箱仍限制只能写当前空间、默认无网）",
			},
		}
	}
	// 工具类插件都能配档位：脚本工具的出网动词来自发布快照、安装方改不了，
	// 但能改「要不要拦」；内置工具（file_write / wiki_write 等）的档位由
	// tool.builtinPolicy 从系统空间那份安装配置读，配置也写在同一处。
	if meta.Kind == KindTool {
		// 脚本工具与内置工具的出网动词都来自发布快照（安装方改不了），
		// 能改的只有「要不要拦」，所以这里说清「按声明」到底拦什么。
		//
		// 域名同样来自清单，必须列出来：安装方要判断「这个插件能不能碰我的
		// 钉钉数据」，唯一的依据就是它会连哪几个站。只写「外部系统」等于没说。
		hosts, _ := meta.HTTPTargets()
		// 「能碰什么」按事实写：有域名就列域名，没有的按能力说清是读还是写。
		// 内置工具（file_read / file_write / wiki_*）没有出网声明，不能套用
		// 「访问外部网络」，否则用户会以为一个读文件工具会联网。
		help := "该工具会访问外部网络，写本空间的文件。"
		switch {
		case len(hosts) > 0:
			help = "该工具需要访问 " + strings.Join(hosts, "、") + " 域名，写本空间的文件。"
		case meta.Capability == CapFSWrite:
			help = "该工具会写本空间的文件。"
		case meta.Capability == CapFSRead:
			help = "该工具会读本空间的文件。"
		}
		return []ConfigField{approvalField(help)}
	}
	// 智能体只暴露正文覆盖：行为设定是发布方的意图，安装方可以按空间改，
	// 但只能改这一段追加正文——平台基础规则不在插件里，改不到。
	if meta.Kind == KindAgent {
		return []ConfigField{{
			Key:         ConfigAgentPrompt,
			Label:       "提示词覆盖",
			Type:        ConfigTypeTextarea,
			Placeholder: "留空表示使用插件内置的 AGENT.md",
			Help:        "只覆盖这个智能体的行为设定，改不了平台基础规则。保存后对新请求立即生效；已经固化了提示词的历史会话不受影响。",
		}}
	}
	if meta.Kind != KindMCP || meta.Transport == nil {
		return nil
	}
	help := "安装级覆盖，不影响发布快照；只能填 https（本机调试可用 http://127.0.0.1）"
	if meta.Transport.AllowHTTP {
		// 清单允许明文，配置才跟着允许：这个开关由发布方持有，部署方只是用地址。
		help = "安装级覆盖，不影响发布快照；清单已允许明文，内网 http 地址可以填"
	}
	fields := []ConfigField{
		// 远端工具算不算写由清单 tools.writes 决定，档位覆盖它：
		// 声明了写入但天天被打扰时，这是安装方的出口。
		// MCP 的站点藏在远端：平台只看到「服务地址」这一跳，列不出它代发请求的目标。
		approvalField("该工具会调用远端服务，写本空间的文件。"),
		{
			Key: ConfigURL, Label: "服务地址", Type: "url", Required: true,
			Placeholder: "https://mcp.example.com/mcp",
			Help:        help,
		},
	}
	if meta.Auth != nil {
		fields = append(fields, ConfigField{
			Key: ConfigAuthName, Label: "鉴权 Header", Type: "text",
			Placeholder: "Authorization",
			Help:        "留空表示沿用插件清单里的设置",
		}, ConfigField{
			Key: ConfigSecret, Label: "凭证名称", Type: "text",
			Help: "密钥本身在「密钥配置」里维护，这里只填引用名，不存明文",
		})
	}
	return fields
}

// ApprovalLevelOf 从安装配置里读审批档位。
//
// 缺省、空串与非法值都按 declared：旧安装记录没有这个键，它们的行为不能被
// 一次解析失败改变。
func ApprovalLevelOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return tool.ApprovalDeclared
	}
	var cfg map[string]string
	if json.Unmarshal(raw, &cfg) != nil {
		return tool.ApprovalDeclared
	}
	return tool.NormalizeApprovalLevel(cfg[ConfigApprovalPolicy])
}

// hasApprovalField 判断这个插件的配置里有没有审批档位（回显默认值用）。
func hasApprovalField(meta Manifest) bool {
	for _, f := range configFields(meta) {
		if f.Key == ConfigApprovalPolicy {
			return true
		}
	}
	return false
}

func configValues(meta Manifest, raw json.RawMessage) map[string]string {
	values := map[string]string{}
	if meta.Transport != nil {
		values[ConfigURL] = meta.Transport.URL
	}
	if meta.Auth != nil {
		values[ConfigAuthName] = meta.Auth.Name
		values[ConfigSecret] = meta.Auth.Secret
	}
	var stored map[string]string
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &stored)
	}
	// 档位没配过时回显默认档：界面空着会让人以为「没设过就是不拦」，
	// 而实际生效的是「有风险的调用需要审批」。
	//
	// 这里回显存储值本身（declared），不归一成 always：保存是整表提交，把
	// declared 改写成 always 会让「只改了 AppKey」的保存顺带把插件变成每次都问。
	if hasApprovalField(meta) {
		values[ConfigApprovalPolicy] = tool.ApprovalDeclared
	}
	// 清单声明的字段同理：没配过时回显 default。这里是运行时真正的生效值，
	// 空框会让安装方以为「这个参数现在是空的」。
	for _, item := range meta.Config {
		values[item.Key] = item.Default
	}
	for k, v := range stored {
		if v != "" {
			values[k] = v
		}
	}
	// shell 的审批名单没配过时回显平台默认，而不是留一个空框。
	//
	// 空框看着像「没有名单」，但保存下去就是「全不确认」——用户只是打开看了一眼
	// 配置、顺手点了保存，确认机制就被静默关掉了。库里有这个键（哪怕值是空串）
	// 才算显式配置过，那时才尊重用户的清空。
	if meta.Capability == CapShellExec {
		if _, configured := stored[ConfigApprovalCommands]; !configured {
			values[ConfigApprovalCommands] = strings.Join(tool.DefaultApprovalCommands, ", ")
		}
	}
	return values
}

// ConfigView 返回这份插件在当前空间的配置：已安装读安装记录，未发布读草稿。
func (m *Manager) ConfigView(ctx context.Context, spaceID, slug string) (ConfigView, error) {
	t, err := m.configTargetOf(ctx, spaceID, slug)
	if err != nil {
		return ConfigView{}, err
	}
	return ConfigView{
		Slug: slug, Kind: string(t.Meta.Kind),
		Fields: configFields(t.Meta), Values: configValues(t.Meta, t.Values),
		Scope: configScope(t.Meta),
	}, nil
}

// configTarget 回答「这份配置该读哪里、写哪里」。
//
// 为什么草稿也要有配置：启用的本地草稿就是这个空间真正在用的插件——进检索池、
// 进工具列表、起常驻进程，和已安装件没有区别。配置却只存在 plugin_install 上，
// 于是需要参数的插件（钉钉的 AppKey、MCP 的服务地址、审批档位）在草稿态永远
// 「未就绪」，作者必须先发布、安装，才能填自己插件要用的参数。
//
// 不是「两处合并」：同一 slug 的草稿与安装副本互斥，安装记录优先只是兜底顺序。
type configTarget struct {
	Meta      Manifest
	Values    json.RawMessage
	PluginID  string
	Version   string
	Installed bool
}

func (m *Manager) configTargetOf(ctx context.Context, spaceID, slug string) (configTarget, error) {
	in, err := m.installOf(ctx, spaceID, slug)
	if err == nil {
		meta, merr := m.installedManifest(ctx, in)
		if merr != nil {
			return configTarget{}, merr
		}
		return configTarget{
			Meta: meta, Values: in.Config, PluginID: in.PluginID,
			Version: in.Version, Installed: true,
		}, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return configTarget{}, err
	}
	c, err := m.o.Store.GetPluginCandidate(ctx, spaceID, slug)
	if err != nil {
		return configTarget{}, err
	}
	meta, err := m.readDraft(ctx, spaceID, slug)
	if err != nil {
		return configTarget{}, err
	}
	return configTarget{Meta: meta, Values: c.Config}, nil
}

// storedConfig 是运行时读配置的唯一入口：安装记录优先，草稿兜底。
//
// 读不到、或者这条记录没有配置，都返回 nil，调用方按清单默认值处理——
// 配置解析失败不该让插件直接不可用。
func (m *Manager) storedConfig(ctx context.Context, spaceID, slug string) json.RawMessage {
	if in, err := m.o.Store.GetPluginInstall(ctx, spaceID, slug); err == nil {
		return in.Config
	}
	c, err := m.o.Store.GetPluginCandidate(ctx, spaceID, slug)
	if err != nil {
		return nil
	}
	return c.Config
}

// UpdateConfig 保存配置：只改配置、不动版本与启用状态，保存后立刻重建运行时。
//
// 已安装写 plugin_install，未发布的草稿写 plugin_candidate——两者的差别只是
// 「配置存在哪张表」，对插件来说都是「这个空间给我的参数」。
func (m *Manager) UpdateConfig(ctx context.Context, userID, spaceID, slug string, values map[string]string) (ConfigView, error) {
	t, err := m.configTargetOf(ctx, spaceID, slug)
	if err != nil {
		return ConfigView{}, err
	}
	if len(configFields(t.Meta)) == 0 {
		return ConfigView{}, fmt.Errorf("%w：该插件没有可配置项", ErrConflict)
	}
	clean, err := normalizeConfig(t.Meta, values)
	if err != nil {
		return ConfigView{}, err
	}
	raw, err := json.Marshal(clean)
	if err != nil {
		return ConfigView{}, err
	}
	if t.Installed {
		err = m.o.Store.UpdatePluginInstallConfig(ctx, spaceID, t.PluginID, raw)
	} else {
		err = m.o.Store.UpdatePluginCandidateConfig(ctx, spaceID, slug, raw)
	}
	if err != nil {
		return ConfigView{}, err
	}

	// 常驻进程的启动参数里捕获了配置（invokeInput），配置一变必须换进程，
	// 否则改完配置、跑的还是旧值，而且怎么看都不像配置没生效。
	m.closeSlugProcs(spaceID, slug)
	// 配置变了就整空间重载：只删单个 slug 会让 ensureLoaded 误以为已加载。
	m.mu.Lock()
	delete(m.inst, spaceID)
	for key := range m.mcpCs {
		if strings.HasPrefix(key, spaceID+"/") {
			delete(m.mcpCs, key)
		}
	}
	m.mu.Unlock()
	if err := m.ensureLoaded(ctx, spaceID); err != nil {
		m.o.Logger.Warn("按新配置重建插件运行时失败", "space", spaceID, "slug", slug, "err", err)
	}

	keys := make([]string, 0, len(clean))
	for k := range clean {
		keys = append(keys, k)
	}
	m.recordAudit(ctx, userID, "plugin.config", slug, t.Version,
		map[string]any{"space": spaceID, "fields": keys, "draft": !t.Installed})

	updated, err := m.configTargetOf(ctx, spaceID, slug)
	if err != nil {
		return ConfigView{}, err
	}
	return ConfigView{
		Slug: slug, Kind: string(updated.Meta.Kind),
		Fields: configFields(updated.Meta), Values: configValues(updated.Meta, updated.Values),
		Scope: configScope(updated.Meta),
	}, nil
}

// ---------------------------------------------------------------- 清单配置项校验

// configNumRange 是 number 字段的取值范围。
//
// HasMin / HasMax 用来区分「没写」和「写了 0」：只靠零值判断的话，
// min: 0 会被当成「没限制」。
type configNumRange struct {
	Min, Max       int64
	HasMin, HasMax bool
}

func configRange(item ConfigItem) (configNumRange, error) {
	var r configNumRange
	if raw := strings.TrimSpace(item.Min); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return r, fmt.Errorf("min 必须是十进制整数：%q", raw)
		}
		r.Min, r.HasMin = v, true
	}
	if raw := strings.TrimSpace(item.Max); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return r, fmt.Errorf("max 必须是十进制整数：%q", raw)
		}
		r.Max, r.HasMax = v, true
	}
	if r.HasMin && r.HasMax && r.Min > r.Max {
		return r, fmt.Errorf("min(%d) 不能大于 max(%d)", r.Min, r.Max)
	}
	return r, nil
}

// validateConfigValue 校验一个安装方填的值能不能被这个字段接受。
//
// 空值只对 required 字段报错：可选字段的清空是合法输入（「这条不用配」），
// 必填字段的清空必须拦住——否则存进去的值在运行时是「没配」，
// 而作者以为平台会替他兜底。
func validateConfigValue(item ConfigItem, value string) error {
	if value == "" {
		if item.Required {
			return fmt.Errorf("必填")
		}
		return nil
	}
	switch item.Type {
	case ConfigTypeText, ConfigTypeTextarea:
		limit := item.MaxLength
		if limit <= 0 {
			limit = 256
			if item.Type == ConfigTypeTextarea {
				limit = 4096
			}
		}
		if n := utf8.RuneCountInString(value); n > limit {
			return fmt.Errorf("最多 %d 个字符（当前 %d 个）", limit, n)
		}
		if strings.ContainsRune(value, 0) {
			return fmt.Errorf("不能包含空字符")
		}
	case ConfigTypeNumber:
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return fmt.Errorf("必须是整数：%q", value)
		}
		r, err := configRange(item)
		if err != nil {
			return err
		}
		if r.HasMin && v < r.Min {
			return fmt.Errorf("不能小于 %d", r.Min)
		}
		if r.HasMax && v > r.Max {
			return fmt.Errorf("不能大于 %d", r.Max)
		}
	case ConfigTypeBoolean:
		if value != "true" && value != "false" {
			return fmt.Errorf("只能是 true 或 false")
		}
	case ConfigTypeSelect:
		names := make([]string, 0, len(item.Options))
		for _, op := range item.Options {
			if op.Value == value {
				return nil
			}
			names = append(names, op.Value)
		}
		return fmt.Errorf("只能是 %s 之一", strings.Join(names, " / "))
	case ConfigTypeURL:
		return validateConfigURL(value)
	case ConfigTypeList:
		items := splitConfigList(value)
		if len(items) > 40 {
			return fmt.Errorf("最多 40 项")
		}
		for _, it := range items {
			if n := utf8.RuneCountInString(it); n > 128 {
				return fmt.Errorf("单项最多 128 个字符：%q", it)
			}
			if strings.IndexFunc(it, unicode.IsControl) >= 0 {
				return fmt.Errorf("不能包含控制字符")
			}
		}
	case ConfigTypeSecretRef:
		if !configNameRe.MatchString(value) {
			return fmt.Errorf("凭证名只能是字母、数字、点、下划线或中划线（最多 64 位）")
		}
	}
	return nil
}

// validateConfigURL 只做形态校验：http / https + 有主机。
//
// 不在这里套 MCP 的 https-only 规则：那是出网策略，由 capabilities 的白名单管；
// 通用配置项可能只是一个内网地址，平台不该替插件假设它的安全边界。
func validateConfigURL(raw string) error {
	if n := utf8.RuneCountInString(raw); n > 512 {
		return fmt.Errorf("最多 512 个字符")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("不是合法的地址：%q", raw)
	}
	switch u.Scheme {
	case "http", "https":
		return nil
	}
	return fmt.Errorf("只支持 http / https 地址")
}

// splitConfigList 按逗号、中文逗号与换行切分多值字段。
//
// 刻意不按空格切：与 shell 的命令名单不同，通用 list 的单项可能本身就含空格
// （例如一个完整的 header 值），按空格切会把一项拆成两项。
func splitConfigList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		switch r {
		case ',', '，', '\n', '\r':
			return true
		}
		return false
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if s := strings.TrimSpace(f); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// commandTokenRe 限制命令名本身：不允许空格、引号与通配符，避免把整条命令塞进来。
var commandTokenRe = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,32}$`)

// validateCommandList 校验「需确认命令」名单；空串合法，表示清空（全不确认）。
func validateCommandList(raw string) error {
	tokens := splitCommandList(raw)
	if len(tokens) > 40 {
		return fmt.Errorf("需确认命令最多 40 个")
	}
	for _, t := range tokens {
		if !commandTokenRe.MatchString(t) {
			return fmt.Errorf("命令名不合法：%q（只允许字母、数字、点、下划线、中划线、斜杠）", t)
		}
	}
	return nil
}

// splitCommandList 按逗号、中文逗号、换行与空白切分，去重保序。
func splitCommandList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		switch r {
		case ',', '，', '\n', '\r', '\t', ' ':
			return true
		}
		return false
	})
	seen := map[string]bool{}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

// installedManifest 读取安装版本对应的清单（配置字段以它为准）。
func (m *Manager) installedManifest(ctx context.Context, in store.PluginInstall) (Manifest, error) {
	p, err := m.o.Store.GetPluginBySlug(ctx, in.Slug)
	if err != nil {
		return Manifest{}, err
	}
	v, err := m.o.Store.GetPluginVersion(ctx, p.ID, in.Version)
	if err != nil {
		return Manifest{}, err
	}
	return Parse(v.Manifest, true)
}

// ConfigDefaults 把清单声明的默认值摊成一张表：未配置时的生效值就是它。
func (m Manifest) ConfigDefaults() map[string]string {
	out := map[string]string{}
	for _, item := range m.Config {
		if item.Default != "" {
			out[item.Key] = item.Default
		}
	}
	return out
}

// effectiveConfig 是插件运行时看到的配置：清单默认值打底，安装值覆盖。
//
// 只对脚本工具有意义（wa.config()）：内置件的参数在 Go 侧，MCP 的地址与鉴权
// 早就在 applyInstallConfig 里并进清单了，都不经过这里。
//
// 每次都现读安装记录，不缓存：保存配置会整空间重载，常驻进程也会被杀掉重建，
// 所以不存在「进程活着、读到的还是旧配置」这种窗口。
func (m *Manager) effectiveConfig(ctx context.Context, spaceID, slug string, meta Manifest) map[string]string {
	cfg := meta.ConfigDefaults()
	// 智能体的「提示词覆盖」是平台字段（不属于清单声明），这里要一并读出来：
	// 否则内置 base 的覆盖永远读不到，卡片上改了也不生效。
	//
	// 提前返回必须也注入平台事实：清单里一条 config 都没写，不等于脚本
	// 不需要宿主地址。渠道插件完全可以只声明 im.reply、不做任何可配置项。
	needStored := len(meta.Config) > 0 || meta.Kind == KindAgent
	raw := []byte(nil)
	if needStored {
		raw = m.storedConfig(ctx, spaceID, slug)
	}
	if len(raw) == 0 {
		m.injectHostFacts(cfg, spaceID, slug, meta)
		return cfg
	}
	var stored map[string]string
	if json.Unmarshal(raw, &stored) != nil {
		m.injectHostFacts(cfg, spaceID, slug, meta)
		return cfg
	}
	for _, item := range meta.Config {
		if v, ok := stored[item.Key]; ok && v != "" {
			cfg[item.Key] = v
		}
	}
	// 提示词覆盖是平台字段，不在 meta.Config 里，必须单独取一次：
	// 否则卡片上存进去了，ResolveAgentPlugin 读到的还是内置正文。
	if meta.Kind == KindAgent {
		if v, ok := stored[ConfigAgentPrompt]; ok && v != "" {
			cfg[ConfigAgentPrompt] = v
		}
	}
	m.injectHostFacts(cfg, spaceID, slug, meta)
	return cfg
}

// 平台自己知道的事实，统一在这里注入给脚本，不让使用者填。
//
// 目前是两项：宿主地址（插件在沙箱里，不知道平台在哪）与所在空间 id
// （插件实例本来就挂在某个空间下）。这两个值都写在部署环境里，手抄一遍只会
// 抄错——而抄错的表现是投递 401，排查方向全是反的。
const (
	ConfigHostBase  = "host_base"
	ConfigHostSpace = "host_space_id"
	// ConfigHostToken 是宿主签发给这个插件的身份令牌，随 host_base 一起注入。
	//
	// 插件没有登录态，也不该有人的身份；它能证明的只有「我是这个空间里的这个
	// 插件」，所以令牌由 (空间, slug) 派生，宿主用同一份密钥复算校验。作者既
	// 配不出、也改不了这个值。
	ConfigHostToken = "host_token"
)

func (m *Manager) injectHostFacts(cfg map[string]string, spaceID, slug string, meta Manifest) {
	// 只注入给声明了 im.reply 的渠道插件：普通工具拿到宿主地址只会多一个
	// 「能调平台接口」的入口，而它并不需要。能力清单是审批对象，注入面跟着走。
	if !meta.HasCapability(CapIMReply) {
		return
	}
	if m.o.HostBase != "" {
		// 强制覆盖而不是「没填才补」：使用者填的地址多半是浏览器地址
		// （127.0.0.1），在沙箱里不可达。部署事实以部署为准。
		cfg[ConfigHostBase] = strings.TrimRight(m.o.HostBase, "/")
	}
	if spaceID != "" {
		cfg[ConfigHostSpace] = spaceID
	}
	if token := m.PluginToken(spaceID, slug); token != "" {
		cfg[ConfigHostToken] = token
	}
}

// platformHostPorts 把宿主地址补进这个插件的出网白名单（精确到端口）。
//
// 为什么必须由平台补：插件在沙箱里要回调宿主（im.reply 的 inbox），而宿主
// 地址是部署事实，作者写不出来——清单的 hosts 只收域名，写 IP 直接解析失败；
// 就算允许写 IP，按域名放行 127.0.0.1 也等于把本机所有端口交给插件。
//
// 注入面跟着能力走，与 injectHostFacts 同一口径：只有声明 im.reply 的插件
// 拿得到。没有它，白名单里就永远差一个「宿主在哪」的答案。
func (m *Manager) platformHostPorts(meta Manifest) []string {
	if !meta.HasCapability(CapIMReply) {
		return nil
	}
	hp, ok := hostPortOf(m.o.HostBase)
	if !ok {
		return nil
	}
	return []string{hp}
}

// hostPortOf 从部署地址里取出「精确 host:port」。
//
// 端口必须显式算出来：APP_BASE_URL 常常写成 http://host:8321，但也可以省端口，
// 而代理判定的是真实端口（80/443），不补上就永远对不上。
func hostPortOf(base string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Hostname() == "" {
		return "", false
	}
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "https":
			port = "443"
		case "http":
			port = "80"
		default:
			return "", false
		}
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), port), true
}

// approvalLevelOf 读该空间这份安装的审批档位。
//
// 没装、读不到、或旧记录里没有这个键，都按默认档（declared）——档位是安装方的
// 显式选择，解析不出来时必须退回「按声明判断」，不能悄悄放行。
func (m *Manager) approvalLevelOf(ctx context.Context, spaceID, slug string) string {
	return ApprovalLevelOf(m.storedConfig(ctx, spaceID, slug))
}

// applyInstallConfig 把安装级覆盖合并进清单；发布快照本身保持不变。
func (m *Manager) applyInstallConfig(ctx context.Context, spaceID, slug string, meta Manifest) Manifest {
	if meta.Kind != KindMCP {
		return meta
	}
	raw := m.storedConfig(ctx, spaceID, slug)
	if len(raw) == 0 {
		return meta
	}
	var cfg map[string]string
	if json.Unmarshal(raw, &cfg) != nil {
		return meta
	}
	if v := strings.TrimSpace(cfg[ConfigURL]); v != "" {
		transport := Transport{Type: "http"}
		if meta.Transport != nil {
			transport.Type = meta.Transport.Type
		}
		transport.URL = v
		meta.Transport = &transport
	}
	auth := Auth{}
	if meta.Auth != nil {
		auth = *meta.Auth
	}
	if v := strings.TrimSpace(cfg[ConfigAuthName]); v != "" {
		auth.Name = v
	}
	if v := strings.TrimSpace(cfg[ConfigSecret]); v != "" {
		auth.Secret = v
	}
	if auth != (Auth{}) {
		meta.Auth = &auth
	}
	return meta
}

// allowsPlainHTTP：清单是否允许明文连接。安装级配置不放宽这个决定——
// 发布方没声明 allow_http，部署方就不能靠改地址把 https 绕成 http。
func allowsPlainHTTP(meta Manifest) bool {
	return meta.Transport != nil && meta.Transport.AllowHTTP
}

// normalizeConfig 校验并裁剪覆盖值。
//
// 两类键走两套规则：
//   - 平台键：空值表示「沿用清单」，各自有专门的校验（地址、名单、档位）；
//   - 清单声明的键：按声明类型校验，空值表示「回到默认」，不落库。
//
// 未知键直接拒绝：安装页只提交 schema 里的字段，多出来的键说明有人绕过前端
// 直接调接口——那不是「多存一个值」，而是「平台不知道这个值该怎么校验」。
func normalizeConfig(meta Manifest, values map[string]string) (map[string]string, error) {
	custom := map[string]ConfigItem{}
	for _, item := range meta.Config {
		custom[item.Key] = item
	}
	allowHTTP := allowsPlainHTTP(meta)
	clean := map[string]string{}
	for key, raw := range values {
		v := strings.TrimSpace(raw)
		if item, ok := custom[key]; ok {
			if err := validateConfigValue(item, v); err != nil {
				return nil, fmt.Errorf("%s：%w", item.Label, err)
			}
			if v == "" {
				// 可选字段清空 = 回到声明里的 default（运行时生效值就是它），
				// 不落库；required 字段的空值上面已经报错了。
				continue
			}
			clean[key] = v
			continue
		}
		if !reservedConfigKeys[key] {
			return nil, fmt.Errorf("未知配置项：%s", key)
		}
		switch key {
		case ConfigURL:
			if v == "" {
				continue
			}
			if err := validateMCPURL(v, allowHTTP); err != nil {
				return nil, err
			}
			clean[key] = v
		case ConfigAuthName, ConfigSecret:
			if v == "" {
				continue
			}
			if !configNameRe.MatchString(v) {
				return nil, fmt.Errorf("%s 只能是字母、数字、点、下划线或中划线（最多 64 位）", key)
			}
			clean[key] = v
		case ConfigApprovalCommands:
			if err := validateCommandList(v); err != nil {
				return nil, err
			}
			clean[key] = v
		case ConfigApprovalPolicy:
			// 非法值直接拒绝，不静默降级：把 off 写成 "none" 的人，
			// 应该看到报错，而不是以为关掉了确认。
			level := tool.NormalizeApprovalLevel(v)
			if v != "" && level != v {
				return nil, fmt.Errorf("审批级别只支持 %s / %s / %s",
					tool.ApprovalDeclared, tool.ApprovalAlways, tool.ApprovalOff)
			}
			clean[key] = level
		case ConfigAgentPrompt:
			// 空值 = 恢复默认，不落库；上限与 agentspec.MaxPromptBytes 对齐，
			// 否则这里存得进去、读取时才报超长，用户看不出是哪一步错的。
			if v == "" {
				continue
			}
			if len(v) > agentspec.MaxPromptBytes {
				return nil, fmt.Errorf("提示词不能超过 %d 字节", agentspec.MaxPromptBytes)
			}
			clean[key] = v
		}
	}
	return clean, nil
}
