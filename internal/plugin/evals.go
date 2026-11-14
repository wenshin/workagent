package plugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/workagent/auto-agent/internal/agent"
	"github.com/workagent/auto-agent/internal/golden"
)

// EvalRunner 跑 agent 类黄金用例（需要模型）。
// 由宿主注入，插件层不直接依赖 LLM——发布门禁在「没模型」的环境里也必须能跑。
type EvalRunner interface {
	RunAgentCases(ctx context.Context, suite golden.Suite) []golden.CaseResult
}

// EvalReport 是一次发布门禁的用例执行报告，写进审计。
type EvalReport struct {
	// Present 表示插件目录里带了 evals.json
	Present bool
	// Tool 是确定性用例数（已执行）
	Tool int
	// Agent 是行为用例数（执行或被跳过）
	Agent int
	// Skipped 是因缺少模型能力而跳过的行为用例数
	Skipped int
	// Failures 是失败描述，非空即拒绝发布
	Failures []string
}

// OK 判断用例是否全部通过。
func (r EvalReport) OK() bool { return len(r.Failures) == 0 }

// Summary 返回一行审计用的摘要。
func (r EvalReport) Summary() string {
	return fmt.Sprintf("present=%t tool=%d agent=%d skipped=%d failed=%d",
		r.Present, r.Tool, r.Agent, r.Skipped, len(r.Failures))
}

// EvalFailedError 表示黄金用例未通过，发布被拒绝。
// 串到 ErrConflict 上，HTTP 层据此返回 409；调用方也能用 errors.As 拿到明细。
type EvalFailedError struct {
	Slug     string
	Failures []string
}

func (e *EvalFailedError) Error() string {
	return fmt.Sprintf("%v：插件 %s 的黄金用例未通过（%d 条）：%s",
		ErrConflict, e.Slug, len(e.Failures), strings.Join(e.Failures, "；"))
}

func (e *EvalFailedError) Unwrap() error { return ErrConflict }

// evalDraft 跑草稿目录里的 evals.json。
//
// 只跑确定性用例（kind=tool）：发布门禁不能依赖外部模型可用，
// 否则「模型服务抖动」会变成「插件发不出去」。行为用例交给 `go test`
// 或宿主注入的 EvalRunner。
func (m *Manager) evalDraft(ctx context.Context, b *Bundle, spaceID, slug, userID string) (EvalReport, error) {
	raw, ok := b.Files[golden.FileName]
	if !ok {
		return EvalReport{}, nil
	}
	suite, err := golden.Load(bytes.NewReader(raw))
	if err != nil {
		// 用例文件写坏同样是作者的问题，且会让门禁静默失效，必须拦住
		return EvalReport{Present: true, Failures: []string{err.Error()}},
			&EvalFailedError{Slug: slug, Failures: []string{err.Error()}}
	}

	report := EvalReport{Present: true}
	// 插件正文注入给行为用例：技能靠正文生效，不注入就等于在测一个不存在的技能
	skill, prompt := bundleBody(b)
	for _, c := range suite.Cases {
		switch c.Kind {
		case golden.KindTool:
			report.Tool++
			if failures := m.runToolCase(ctx, spaceID, slug, userID, c); len(failures) > 0 {
				report.Failures = append(report.Failures, failures...)
			}
		case golden.KindAgent:
			report.Agent++
			if m.o.Evals == nil {
				report.Skipped++
				continue
			}
			c.InjectSkill = skill
			c.InjectPrompt = prompt
			for _, r := range m.o.Evals.RunAgentCases(ctx, golden.Suite{Version: suite.Version, Cases: []golden.Case{c}}) {
				if !r.Passed {
					report.Failures = append(report.Failures,
						fmt.Sprintf("%s：%s", r.Name, strings.Join(r.Failures, "，")))
				}
			}
		}
	}
	if len(report.Failures) > 0 {
		return report, &EvalFailedError{Slug: slug, Failures: report.Failures}
	}
	return report, nil
}

// bundleBody 取出插件正文：技能取 SKILL.md，智能体取 AGENT.md。
// 其他类型返回空——工具的行为由脚本与清单决定，正文注入没有意义。
func bundleBody(b *Bundle) (*agent.ActiveSkill, string) {
	switch b.Meta.Kind {
	case KindSkill:
		entry := "SKILL.md"
		if b.Meta.Skill != nil && b.Meta.Skill.Entry != "" {
			entry = b.Meta.Skill.Entry
		}
		body, ok := b.Files[entry]
		if !ok || len(bytes.TrimSpace(body)) == 0 {
			return nil, ""
		}
		name := b.Meta.Name
		if strings.TrimSpace(name) == "" {
			name = b.Meta.Slug
		}
		return &agent.ActiveSkill{Name: name, Path: b.Meta.Slug + "/" + entry, Doc: string(body)}, ""
	case KindAgent:
		entry := AgentEntry
		if b.Meta.Agent != nil && b.Meta.Agent.Entry != "" {
			entry = b.Meta.Agent.Entry
		}
		if body, ok := b.Files[entry]; ok {
			return nil, string(body)
		}
	}
	return nil, ""
}

// runToolCase 用插件自带的确定性用例跑一次 dry-run 并断言。
func (m *Manager) runToolCase(ctx context.Context, spaceID, slug, userID string, c golden.Case) []string {
	res, err := m.DryRun(ctx, spaceID, slug, userID, c.Input)
	if c.Expect.ErrorExpected {
		if err == nil {
			return []string{fmt.Sprintf("%s：期望失败，实际成功", c.Name)}
		}
		return nil
	}
	if err != nil {
		return []string{fmt.Sprintf("%s：执行失败（%v）", c.Name, err)}
	}
	var failures []string
	for _, want := range c.Expect.Contains {
		if !strings.Contains(res.Output+res.Detail, want) {
			failures = append(failures, fmt.Sprintf("%s：输出缺少 %q", c.Name, want))
		}
	}
	for _, banned := range c.Expect.NotContains {
		if strings.Contains(res.Output+res.Detail, banned) {
			failures = append(failures, fmt.Sprintf("%s：输出不应包含 %q", c.Name, banned))
		}
	}
	return failures
}

// 编译期断言：EvalFailedError 必须能被 errors.Is(err, ErrConflict) 命中。
var _ = func() bool { return errors.Is(&EvalFailedError{}, ErrConflict) }
