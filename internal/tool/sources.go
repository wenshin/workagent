package tool

import "context"

// SourceEntry 是能力源（skills / tools / mcps）中的一条记录。
type SourceEntry struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Path 相对空间 files/，可直接交给 ReadSource
	Path string `json:"path"`
	// Source 标记来源：current（当前空间）或 system（系统空间）
	Source string `json:"source"`
}

// SourceLoader 是「当前空间 + 系统空间」合并后的能力源视图。
//
// 由会话层注入（实现见 space.Service），工具包不依赖 space 包。
// 合并规则：同名（Name）条目当前空间优先，系统空间只补当前空间没有的。
type SourceLoader interface {
	// Sources 返回合并后的 skills / tools / mcps
	Sources(ctx context.Context) ([]SourceEntry, error)
	// ReadSource 读取资源文件：先当前空间，再系统空间
	ReadSource(ctx context.Context, rel string) (string, error)
	// RefreshIndex 重建当前空间索引（写操作后调用）
	RefreshIndex(ctx context.Context) error
}
