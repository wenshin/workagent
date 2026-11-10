package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/workagent/auto-agent/internal/policy"
)

// maxSkillDocBytes 是单次生成 SKILL.md 的上限。
//
// 技能文档最终会被读进模型上下文，过大既浪费 token 也说明这个技能该拆，
// 所以比普通文件写（256KB）收紧到 64KB。
const maxSkillDocBytes = 64 * 1024

// 技能元数据限制，与 Agent Skills 规范对齐：name 最长 64 字符，description 最长 1024 字符。
const (
	maxSkillNameRunes = 64
	maxSkillDescRunes = 1024
)

// skillCreate 在当前空间创建一个技能插件草稿，并写入 SKILL.md 正文。
//
// 目录与元数据由 space.Service.CreateSkill 负责（名字校验、权限、幂等冲突提示），
// 工具只补写正文；这样 UI 新建技能与 Agent 新建技能走同一套规则。
// 元数据只在 plugins/<slug>/plugin.json：SKILL.md 不写 frontmatter。
func skillCreate() Tool {
	return Tool{
		Name:       "skill_create",
		Capability: "skill.create",
		Description: "在当前空间创建一个技能插件（kind=skill）：生成 plugins/<slug>/plugin.json 与 SKILL.md。" +
			"name 是技能展示名，description 是一句话说明（写清做什么 + 何时使用）。 " +
			"content 可选，直接作为 SKILL.md 正文，不传则写入待填充骨架；之后可用 file_write 继续完善。" +
			"用户说「帮我做一个技能」时用它。",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "技能名，同时作为空间内的目录名，如「周报生成」"},
    "description": {"type": "string", "description": "一句话说明技能解决什么问题"},
    "content": {"type": "string", "description": "可选，SKILL.md 的完整正文；不传则生成骨架"}
  },
  "required": ["name", "description"],
  "additionalProperties": false
}`),
		Risk: policy.RiskWrite,
		// 只改当前空间的文件区，策略按空间写权限放行
		SideEffects: policy.SideEffectsWorkspace,
		Runtime:     "builtin",
		Invoke: func(ctx context.Context, args json.RawMessage, env Env) (string, error) {
			if env.Storage == nil || env.SpaceID == "" || env.CreateSkill == nil {
				return "", fmt.Errorf("技能创建能力不可用")
			}
			var in struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				Content     string `json:"content"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", fmt.Errorf("参数解析失败：%w", err)
			}

			name := strings.TrimSpace(in.Name)
			if name == "" {
				return "", fmt.Errorf("name 不能为空")
			}
			// 规范：技能名同时是目录名，限制长度并禁止路径分隔符与隐藏目录
			if utf8.RuneCountInString(name) > maxSkillNameRunes {
				return "", fmt.Errorf("name 过长（%d 字符），上限 %d 字符", utf8.RuneCountInString(name), maxSkillNameRunes)
			}
			if strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
				return "", fmt.Errorf("name 不能包含 / 或 \\，也不能以 . 开头")
			}

			desc := strings.TrimSpace(in.Description)
			if desc == "" {
				return "", fmt.Errorf("description 不能为空：用一句话说明这个技能解决什么问题")
			}
			// 规范：description 要同时写清「做什么」和「何时使用」，上限 1024 字符
			if utf8.RuneCountInString(desc) > maxSkillDescRunes {
				return "", fmt.Errorf("description 过长（%d 字符），上限 %d 字符；写清「做什么 + 何时使用」即可",
					utf8.RuneCountInString(desc), maxSkillDescRunes)
			}

			doc := in.Content
			if strings.TrimSpace(doc) == "" {
				doc = defaultSkillDoc(name, desc)
			} else {
				// 单一定义点：元数据只在 plugin.json，正文里带 frontmatter 会变成第二处定义。
				doc = stripSkillFrontmatter(doc)
			}
			if len(doc) > maxSkillDocBytes {
				return "", fmt.Errorf("SKILL.md 过大（%d 字节），上限 %d 字节；建议把参考资料拆到子文件并用相对路径引用",
					len(doc), maxSkillDocBytes)
			}

			dir, err := env.CreateSkill(ctx, name, desc)
			if err != nil {
				return "", err
			}
			if dir == "" {
				dir = name
			}

			key := path.Join(env.Storage.SpaceKey(env.SpaceID), workspaceFilesDir, dir, "SKILL.md")
			if _, err := env.Storage.Put(ctx, key, strings.NewReader(doc), int64(len(doc))); err != nil {
				// 目录已建好，只差正文：告诉模型可重试，别让它再建一次目录
				return "", fmt.Errorf("技能目录已创建（%s），但 SKILL.md 写入失败：%w；请用 file_write 写入 %s/SKILL.md", dir, err, dir)
			}

			hint := ""
			if len(doc) > 16*1024 {
				hint = "；正文偏长，建议把模板/样例拆到子文件并在 SKILL.md 中相对路径引用"
			}
			return fmt.Sprintf("已创建技能插件 %s（%s），位于 %s；SKILL.md 已写入 %d 字节。"+
				"元数据（名称/描述）在 %s/plugin.json，正文用 file_write 继续完善 %s/SKILL.md%s",
				name, desc, dir, len(doc), dir, dir, hint), nil
		},
	}
}

// defaultSkillDoc 是新建技能的骨架：只写流程，长内容拆到同目录子文件。
func defaultSkillDoc(name, desc string) string {
	return fmt.Sprintf(`
# %s

## 何时使用

- 待补充：用户说什么话、处在什么场景时触发这个技能。

## 输入

| 参数 | 说明 | 必填 |
| --- | --- | --- |
| 待补充 |  |  |

## 步骤

1. 待补充：先确认哪些信息，再按顺序调用哪些工具。

## 输出与验收

- 待补充：产出什么结果，如何判断任务完成。

## 参考文件（可选，最多一层）

- 模板、样例、接口说明放同目录子文件，用相对路径引用，例如 `+"`templates.md`"+`。

> 元数据只在 plugin.json 的 name / description 里，请勿在正文写 YAML frontmatter。
`, name)
}

// stripSkillFrontmatter 去掉旧格式的 YAML frontmatter（导入/迁移时兜底）。
func stripSkillFrontmatter(body string) string {
	trimmed := strings.TrimSpace(body)
	if !strings.HasPrefix(trimmed, "---") {
		return body
	}
	rest := strings.TrimPrefix(trimmed, "---")
	rest = strings.TrimLeft(rest, "\r\n")
	for _, sep := range []string{"\n---\n", "\n---\r\n", "\r\n---\r\n"} {
		if i := strings.Index(rest, sep); i >= 0 {
			return strings.TrimLeft(rest[i+len(sep):], "\r\n")
		}
	}
	return body
}
