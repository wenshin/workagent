package tool

import "context"

// PluginEntry 是插件目录里的一条能力：技能 / 工具 / MCP / 组件各一条。
type PluginEntry struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Path 相对空间 files/，可直接交给 ReadPlugin
	Path string `json:"path"`
	// Source 标记来源：current（当前空间）或 system（系统空间）
	Source string `json:"source"`
}

// PluginLoader 是「当前空间 + 系统空间」合并后的插件目录视图。
//
// 由会话层注入（实现见 session.pluginLoader），工具包不依赖 space 包。
// 合并规则：同名（Name）条目当前空间优先，系统空间只补当前空间没有的。
type PluginLoader interface {
	// Plugins 返回合并后的 skills / tools / mcps / components
	Plugins(ctx context.Context) ([]PluginEntry, error)
	// ReadPlugin 读取插件文件：先当前空间，再系统空间
	ReadPlugin(ctx context.Context, rel string) (string, error)
	// RefreshIndex 重建当前空间索引（写操作后调用）
	RefreshIndex(ctx context.Context) error
}
