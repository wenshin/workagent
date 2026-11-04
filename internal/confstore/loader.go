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
	// Secret 是凭证存储（{STORAGE_ROOT}/secrets/search/）里的名字
	Secret string `json:"secret"`
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
