package space

import (
	"context"
	"path"
)

// builtinAgentFiles 是系统空间 agents/ 的初始内容。
//
// 与 skills / mcps 一致用「不存在才写」：管理员改过之后不被平台覆盖。
// index.json 是选择器清单，新增智能体时需要同步补一条——这是有意的显式配置，
// 避免目录里躺一个半成品就被用户选中。
var builtinAgentFiles = []struct {
	Path    string
	Content string
}{
	{
		Path: "index.json",
		Content: `{
  "agents": [
    {
      "id": "base",
      "name": "通用助理",
      "version": "1.0.0",
      "description": "平台默认的通用工作助理：先判断再动手，产出按空间里的规范来。"
    }
  ]
}
`,
	},
	{
		Path: "base/agent.json",
		Content: `{
  "id": "base",
  "name": "通用助理",
  "version": "1.0.0",
  "description": "平台默认的通用工作助理：先判断再动手，产出按空间里的规范来。",
  "prompt": "prompt.md"
}
`,
	},
	{
		Path: "base/prompt.md",
		Content: `## 你的定位

你是这个空间里的通才助理：先判断任务类型，再决定直接回答、读文件，还是检索资源。

## 工作方式

- 动手前先确认输入是否齐全；缺关键信息时一次问清楚，不要连续追问。
- 产出正式文档前，先看空间里有没有规定的模板或规范；有就照着来，没有才自己拟定。
- 引用空间里的文件时写明路径，方便用户核对。
- 不确定的地方标注「待确认」，不要用推测填补事实。
- 多步任务先把步骤列出来再执行，中途改变方案时说明原因。
`,
	},
}

// seedAgents 幂等写入内置智能体配置。
func (s *Service) seedAgents(ctx context.Context, spaceID string) error {
	root := path.Join(s.filesKey(spaceID), systemAgentsDir)
	for _, f := range builtinAgentFiles {
		key := path.Join(root, f.Path)
		if err := s.storage.EnsureDir(ctx, path.Dir(key)); err != nil {
			return err
		}
		if err := s.putIfAbsent(ctx, key, f.Content); err != nil {
			return err
		}
	}
	return nil
}
