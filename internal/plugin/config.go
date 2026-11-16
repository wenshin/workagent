package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/workagent/auto-agent/internal/store"
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
)

// 凭证名 / Header 名只允许标识符字符：不允许表达式、路径或空格的逃逸空间。
var configNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ConfigField 是一个可编辑项，前端据此渲染表单，不写死字段名。
type ConfigField struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Type        string `json:"type"`
	Required    bool   `json:"required"`
	Placeholder string `json:"placeholder,omitempty"`
	Help        string `json:"help,omitempty"`
}

// ConfigView 是配置页的数据：字段定义 + 当前生效值（凭证只回显名字，不回显密钥）。
type ConfigView struct {
	Slug   string            `json:"slug"`
	Kind   string            `json:"kind"`
	Fields []ConfigField     `json:"fields"`
	Values map[string]string `json:"values"`
}

// configFields 目前只有 MCP 有空间级配置：服务地址与鉴权。
// 其它类型的参数属于清单/内容，不该在安装页改（否则等于绕过发布校验）。
func configFields(meta Manifest) []ConfigField {
	// shell 的特殊之处：它是唯一「命令内容在运行时才知道」的能力，
	// 哪些命令需要确认只能由使用方按自己的数据来定。
	if meta.Capability == "shell.exec" {
		return []ConfigField{{
			Key: ConfigApprovalCommands, Label: "需确认的命令", Type: "list",
			Placeholder: "rm, rmdir, dd, truncate, shred, chmod, chown, sudo, su, mount",
			Help:        "逗号或换行分隔。命中这些命令时先弹确认框；清空表示都不确认（沙箱仍限制只能写当前空间、默认无网）",
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
	fields := []ConfigField{{
		Key: ConfigURL, Label: "服务地址", Type: "url", Required: true,
		Placeholder: "https://mcp.example.com/mcp",
		Help:        help,
	}}
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
	for k, v := range stored {
		if v != "" {
			values[k] = v
		}
	}
	return values
}

// ConfigView 返回安装级配置；未安装时返回冲突错误（配置只对安装生效）。
func (m *Manager) ConfigView(ctx context.Context, spaceID, slug string) (ConfigView, error) {
	in, err := m.installOf(ctx, spaceID, slug)
	if err != nil {
		return ConfigView{}, err
	}
	meta, err := m.installedManifest(ctx, in)
	if err != nil {
		return ConfigView{}, err
	}
	return ConfigView{
		Slug: slug, Kind: string(meta.Kind),
		Fields: configFields(meta), Values: configValues(meta, in.Config),
	}, nil
}

// UpdateConfig 保存安装级配置：只改配置、不动版本，保存后立刻重建运行时。
func (m *Manager) UpdateConfig(ctx context.Context, userID, spaceID, slug string, values map[string]string) (ConfigView, error) {
	in, err := m.installOf(ctx, spaceID, slug)
	if err != nil {
		return ConfigView{}, err
	}
	meta, err := m.installedManifest(ctx, in)
	if err != nil {
		return ConfigView{}, err
	}
	if len(configFields(meta)) == 0 {
		return ConfigView{}, fmt.Errorf("%w：该插件没有可配置项", ErrConflict)
	}
	clean, err := normalizeConfig(values, allowsPlainHTTP(meta))
	if err != nil {
		return ConfigView{}, err
	}
	raw, err := json.Marshal(clean)
	if err != nil {
		return ConfigView{}, err
	}
	if err := m.o.Store.UpdatePluginInstallConfig(ctx, spaceID, in.PluginID, raw); err != nil {
		return ConfigView{}, err
	}

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
	m.recordAudit(ctx, userID, "plugin.config", slug, in.Version,
		map[string]any{"space": spaceID, "fields": keys})

	updated, err := m.installOf(ctx, spaceID, slug)
	if err != nil {
		return ConfigView{}, err
	}
	return ConfigView{
		Slug: slug, Kind: string(meta.Kind),
		Fields: configFields(meta), Values: configValues(meta, updated.Config),
	}, nil
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

// applyInstallConfig 把安装级覆盖合并进清单；发布快照本身保持不变。
func (m *Manager) applyInstallConfig(ctx context.Context, spaceID, slug string, meta Manifest) Manifest {
	if meta.Kind != KindMCP {
		return meta
	}
	in, err := m.o.Store.GetPluginInstall(ctx, spaceID, slug)
	if err != nil || len(in.Config) == 0 {
		return meta
	}
	var cfg map[string]string
	if json.Unmarshal(in.Config, &cfg) != nil {
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

// normalizeConfig 校验并裁剪覆盖值：空值表示「沿用清单」，未知键直接拒绝。
func normalizeConfig(values map[string]string, allowHTTP bool) (map[string]string, error) {
	allowed := map[string]bool{
		ConfigURL: true, ConfigAuthName: true, ConfigSecret: true, ConfigApprovalCommands: true,
	}
	clean := map[string]string{}
	for key, raw := range values {
		if !allowed[key] {
			return nil, fmt.Errorf("未知配置项：%s", key)
		}
		v := strings.TrimSpace(raw)
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
		}
	}
	return clean, nil
}
