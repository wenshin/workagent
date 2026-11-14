package space

import _ "embed"

// 「编程实践」技能的子手册用嵌入文件：正文里有大量 markdown 反引号，
// 写成 Go 原始字符串会互相打架（与 plugin_docs/source.md 同一处理）。
//
// 目录里的 SKILL.md 是总纲（Doc），其余是子手册（Files），模型按需 resource_read。

//go:embed coding_docs/SKILL.md
var codingSkillDoc string

//go:embed coding_docs/implement.md
var codingImplementDoc string

//go:embed coding_docs/debug.md
var codingDebugDoc string

//go:embed coding_docs/review.md
var codingReviewDoc string

//go:embed coding_docs/refactor.md
var codingRefactorDoc string

// codingSkill 是「编程实践」技能：写 / 改 / 调 / 查 / 重构代码的统一工作方法。
//
// 它要解决的问题是「模型凭经验自由发挥」：平台没有 IDE、也看不到用户仓库，
// 唯一的执行面是空间文件区与沙箱命令。因此正文只写这个边界内的做法、
// 以及「证据优先于声明」这条不能让的规矩；子手册按任务类型拆分，
// 避免总纲膨胀成一份没人读完的规范合集。
func codingSkill() BuiltinSkill {
	return BuiltinSkill{
		Slug:        "builtin_coding",
		DirName:     "编程实践",
		Name:        "编程实践",
		Description: "写代码、改代码、修 bug、调试、重构、代码审查、补测试时使用：小步实现与测试先行、四阶段根因调试、五维审查与严重级别、行为不变的重构，以及验证证据的写法",
		Doc:         codingSkillDoc,
		Files: map[string]string{
			"references/implement.md": codingImplementDoc,
			"references/debug.md":     codingDebugDoc,
			"references/review.md":    codingReviewDoc,
			"references/refactor.md":  codingRefactorDoc,
		},
	}
}
