package tool

import (
	"context"
	"fmt"

	"github.com/workagent/auto-agent/internal/store"
)

// RegisterCapabilities 把内置工具的三层模型写入数据库（§5.6.1）。
//
// Capability 描述能力语义与 IO，Tool 指向具体实现；两者启动时幂等注册，
// 这样绑定、审计、计量都有稳定的外键可引用。
func RegisterCapabilities(ctx context.Context, st store.Store) error {
	for _, t := range builtinTools() {
		if err := st.UpsertCapability(ctx, store.Capability{
			Name:        t.Capability,
			Description: t.Description,
			InputSchema: t.Parameters,
			RiskLevel:   t.Risk,
			Idempotent:  true,
			SideEffects: "read-only",
		}); err != nil {
			return fmt.Errorf("注册能力 %s 失败: %w", t.Capability, err)
		}

		if err := st.UpsertTool(ctx, store.ToolRecord{
			Name:       t.Name,
			Capability: t.Capability,
			Runtime:    "builtin",
			RiskLevel:  t.Risk,
		}); err != nil {
			return fmt.Errorf("注册工具 %s 失败: %w", t.Name, err)
		}
	}
	return nil
}
