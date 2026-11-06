// Package confstore 读取系统空间根下的平台配置（config.json / net_whitelist.json）。
//
// 只在启动时读一次：改完需要重启。这样出网白名单与搜索接入点不会在一次请求之间
// 变化，也避免每次搜索都去读存储。读不到时返回零值，由调用方回退到环境变量。
package confstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/workagent/auto-agent/internal/space"
	"github.com/workagent/auto-agent/internal/store"
)

const (
	// ConfigFile 是平台配置
	ConfigFile = "config.json"
	// WhitelistFile 是出网白名单
	WhitelistFile = "net_whitelist.json"
	// readLimit 是单个配置文件的读取上限
	readLimit = 64 << 10
)

// Search 是 config.json 里的搜索配置。
//
// 只允许平台指定接入点：skill 与模型都无法传 endpoint，密钥用引用名而不是明文。
type Search struct {
	Provider string `json:"provider"`
	Endpoint string `json:"endpoint"`
	// Secret 是凭证存储里的名字，已废弃：留空即使用统一名 default。
	// 保留字段只为兼容老配置（DisallowUnknownFields 会让删掉字段的老配置启动失败）。
	Secret string `json:"secret,omitempty"`
	// Spec 是声明式搜索源定义（请求模板 + 响应字段映射），由 internal/search 解析。
	// 给了 spec 就不需要内置 provider 实现，接新搜索 API 只改这里。
	Spec json.RawMessage `json:"spec,omitempty"`
	// Script 是脚本搜索源的入口文件（.ts/.js），由 deno 在只读无网沙箱里执行。
	// 声明式表达不了的（请求签名、游标分页、非 JSON 响应）才需要它。
	// 优先级：script > spec > 内置 provider。
	Script string `json:"script,omitempty"`
}

// Config 是 config.json 的结构。
type Config struct {
	Search Search `json:"search"`
}

// Whitelist 是 net_whitelist.json 的结构。
type Whitelist struct {
	Domains []string `json:"domains"`
}

// Loader 从系统空间读取平台配置。
type Loader struct {
	spaces *space.Service
}

// NewLoader 构造加载器。
func NewLoader(spaces *space.Service) *Loader {
	return &Loader{spaces: spaces}
}

// Load 读取两份配置；文件不存在时返回零值且不报错。
func (l *Loader) Load(ctx context.Context) (Config, Whitelist, error) {
	var cfg Config
	var wl Whitelist
	if l == nil || l.spaces == nil {
		return cfg, wl, nil
	}

	sysID, err := l.spaces.SystemSpaceID(ctx)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return cfg, wl, nil
		}
		return cfg, wl, err
	}

	if err := l.decode(ctx, sysID, ConfigFile, &cfg); err != nil {
		return Config{}, Whitelist{}, err
	}
	if err := l.decode(ctx, sysID, WhitelistFile, &wl); err != nil {
		return Config{}, Whitelist{}, err
	}

	if cfg.Search.Endpoint != "" && !strings.HasPrefix(cfg.Search.Endpoint, "https://") {
		return Config{}, Whitelist{}, fmt.Errorf("%s 的 search.endpoint 必须是 https", ConfigFile)
	}
	if cfg.Search.Provider != "" && cfg.Search.Endpoint == "" {
		return Config{}, Whitelist{}, fmt.Errorf("%s 配了 search.provider 但缺少 search.endpoint", ConfigFile)
	}

	wl.Domains = normalizeDomains(wl.Domains)
	return cfg, wl, nil
}

func (l *Loader) decode(ctx context.Context, spaceID, name string, out any) error {
	raw, err := l.spaces.ReadSourceFile(ctx, spaceID, name, readLimit)
	if err != nil {
		if errors.Is(err, space.ErrFileNotFound) {
			return nil
		}
		return err
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	// 拼错字段名必须显式失败：配置静默失效比启动报错危险得多
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%s 不是合法 JSON: %w", name, err)
	}
	return nil
}

func normalizeDomains(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		v := strings.ToLower(strings.TrimSpace(raw))
		v = strings.TrimPrefix(v, "https://")
		v = strings.TrimPrefix(v, "http://")
		v = strings.Trim(v, "/")
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
