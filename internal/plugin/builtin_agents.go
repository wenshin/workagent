package plugin

// BuiltinAgent 是平台预置智能体。
//
// 智能化之前这些内容写在系统空间的 agents/<id>/ 目录里；现在统一走插件：
// 发布为 origin=system 的 kind=agent，安装到系统空间后对所有空间可选。
type BuiltinAgent struct {
	Slug        string
	Name        string
	Description string
	// Extends 指父智能体 slug，空表示只追加到平台基础规则
	Extends string
	// Doc 是提示词正文，落成 AGENT.md
	Doc string
}

// builtinAgents 是平台内置智能体。
//
// 只放「平台默认」的那一个：它是会话不指定智能体时的落点，必须永远存在且稳定。
func builtinAgents() []BuiltinAgent {
	return []BuiltinAgent{
		{
			Slug:        "base",
			Name:        "通用助理",
			Description: "平台默认的通用工作助理：先判断再动手，产出按空间里的规范来。",
			Doc: `## 你的定位

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
}
