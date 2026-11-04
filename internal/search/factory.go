package search

import (
	"sort"
	"strings"
	"sync"
)

// Factory 按平台配置构造 provider：endpoint 来自配置，密钥由 KeyFunc 注入，
// 出网统一走 Guard。
type Factory func(endpoint string, key KeyFunc, guard *Guard) Provider

var (
	factoryMu sync.RWMutex
	factories = map[string]Factory{}
)

// RegisterFactory 登记 provider 构造函数。
//
// 这是新增搜索源唯一的接线点：实现 Provider，在自己的文件里 init 注册即可，
// 装配层（internal/app）不需要任何按名字的 case 分支——否则每加一个源都要
// 去改启动逻辑，provider 名就成了散落在业务代码里的魔法字符串。
func RegisterFactory(name string, f Factory) {
	name = strings.TrimSpace(name)
	if name == "" || f == nil {
		return
	}
	factoryMu.Lock()
	defer factoryMu.Unlock()
	factories[name] = f
}

// Build 按名字构造 provider；未注册时返回 false，由调用方决定降级还是报错。
func Build(name, endpoint string, key KeyFunc, guard *Guard) (Provider, bool) {
	factoryMu.RLock()
	f, ok := factories[strings.TrimSpace(name)]
	factoryMu.RUnlock()
	if !ok {
		return nil, false
	}
	return f(endpoint, key, guard), true
}

// ProviderNames 返回已登记的 provider 名（排序），供启动日志与配置校验使用。
func ProviderNames() []string {
	factoryMu.RLock()
	defer factoryMu.RUnlock()
	out := make([]string, 0, len(factories))
	for name := range factories {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
