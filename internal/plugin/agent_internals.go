package plugin

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// 智能体自带件（agent.skills / agent.tools）的解析与校验。
//
// 校验分两层：路径语法在 Parse 里（manifest.go，纯清单），文件是否真的存在
// 在这里（需要整个插件目录的快照），因此发布与 plugin_try 都走这一份。
// 运行期按同样的规则从安装副本重新读一遍——副本是可以被替换的，不能假设
// 发布时校验过就永远正确，坏掉的那一条跳过并告警，不拖垮整个智能体。

// validateAgentInternals 校验自带件在快照里确实存在、类型正确，返回内置工具的清单。
//
// 返回的清单只用于聚合风险（发布方要看到真实的最高风险），不参与执行。
func validateAgentInternals(b *Bundle) ([]Manifest, error) {
	if b == nil || b.Meta.Agent == nil {
		return nil, nil
	}
	for _, rel := range b.Meta.Agent.Skills {
		pages := agentSkillPages(b.Files, rel, maxAgentSkillFiles)
		if len(pages) == 0 {
			return nil, fmt.Errorf("agent.skills 里的 %q 没有任何 .md 页面", rel)
		}
		for _, p := range pages {
			if len(b.Files[p]) > maxAgentSkillBytes {
				return nil, fmt.Errorf("内置技能页 %s 超过 %dKB", p, maxAgentSkillBytes>>10)
			}
		}
	}

	var out []Manifest
	seen := map[string]bool{}
	for _, rel := range b.Meta.Agent.Tools {
		raw, ok := b.Files[path.Join(rel, ManifestFile)]
		if !ok {
			return nil, fmt.Errorf("agent.tools 里的 %q 缺少 %s", rel, ManifestFile)
		}
		meta, err := Parse(raw, false)
		if err != nil {
			return nil, fmt.Errorf("内置工具 %s 的清单不合法: %w", rel, err)
		}
		if meta.Kind != KindTool {
			return nil, fmt.Errorf("agent.tools 里的 %q 必须是 tool 插件，收到 %s", rel, meta.Kind)
		}
		// 内置件（file_write 之类）走系统空间那份安装记录，塞进智能体只会让
		// 「停用/审批档位在哪改」变得含糊，干脆不允许。
		if meta.Builtin {
			return nil, fmt.Errorf("agent.tools 里的 %q 是平台内置件，不能作为智能体内置工具", rel)
		}
		if seen[meta.Slug] {
			return nil, fmt.Errorf("agent.tools 里的 slug %q 重复", meta.Slug)
		}
		seen[meta.Slug] = true
		out = append(out, meta)
	}
	return out, nil
}

// agentSkillPages 把一条 agent.skills 声明展开成具体页面路径（相对插件目录）。
//
// 声明成 .md 就是一个文件；声明成目录只取该目录下一层 .md——内置件是智能体的
// 附带内容，允许随手写几页，不允许在里面再搭一棵需要导航的树。
func agentSkillPages(files map[string][]byte, rel string, limit int) []string {
	if strings.HasSuffix(strings.ToLower(rel), ".md") {
		if _, ok := files[rel]; ok {
			return []string{rel}
		}
		return nil
	}
	prefix := rel + "/"
	out := make([]string, 0, 8)
	for name := range files {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rest := strings.TrimPrefix(name, prefix)
		if rest == "" || strings.Contains(rest, "/") || strings.HasPrefix(rest, ".") ||
			!strings.HasSuffix(strings.ToLower(rest), ".md") {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// agentSkillMeta 从技能页正文里取展示用的名称与描述。
//
// 没有 frontmatter（技能正文不写元数据是平台既有约定），所以退化成「首个
// # 标题作名称、其后第一行非空正文作描述」；都没有就用文件名兜底。
// 名称与描述只服务检索排序和系统提示词，正文始终以文件为准。
func agentSkillMeta(rel, body string) (name, desc string) {
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if name == "" && strings.HasPrefix(t, "#") {
			name = strings.TrimSpace(strings.TrimLeft(t, "#"))
			continue
		}
		if desc == "" {
			desc = strings.TrimSpace(strings.TrimLeft(t, "#"))
		}
		if name != "" && desc != "" {
			break
		}
	}
	if name == "" {
		base := path.Base(rel)
		name = strings.TrimSuffix(base, path.Ext(base))
	}
	if desc == "" {
		desc = name
	}
	return clipRunes(name, 60), clipRunes(desc, maxAgentSkillDescrR)
}

// clipRunes 按字符截断，避免把中文切成半个字。
func clipRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
