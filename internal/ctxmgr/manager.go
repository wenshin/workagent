// Package ctxmgr 实现上下文管理（技术方案 §5.7.2 摘要压缩、§5.7.4 记忆与按需检索）。
//
// 三段式组装：
//
//	[长期摘要]     更早的消息压缩成事实性摘要，避免上下文线性膨胀
//	[按需检索]     用本轮 query 的向量召回历史片段，补齐摘要遗漏的细节
//	[滑动窗口]     最近 N 条保留原文，保证指代与语气连续
package ctxmgr

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/workagent/auto-agent/internal/llm"
	"github.com/workagent/auto-agent/internal/store"
)

// Budget 控制上下文的规模。
type Budget struct {
	// Window 保留原文的最近消息条数
	Window int
	// MaxRunes 上下文总字数上限，超出后丢弃更早的原文
	MaxRunes int
	// TopK 检索补充的历史片段条数
	TopK int
	// SummaryRunes 摘要目标长度
	SummaryRunes int
	// MinScore 检索相似度阈值，低于此值视为不相关
	MinScore float64
}

// DefaultBudget 返回默认预算。
func DefaultBudget() Budget {
	return Budget{Window: 16, MaxRunes: 24000, TopK: 4, SummaryRunes: 600, MinScore: 0.35}
}

// Input 是一次上下文组装请求。
type Input struct {
	SessionID string
	History   []store.Message
	Query     string
}

// Manager 负责上下文压缩、检索与索引。
type Manager struct {
	store  store.Store
	router *llm.Router
	budget Budget
	log    *slog.Logger
}

// New 构造上下文管理器。
func New(st store.Store, router *llm.Router, log *slog.Logger, budget Budget) *Manager {
	if log == nil {
		log = slog.Default()
	}
	// 逐项补默认值，不整份替换：调用方只想改窗口条数时，
	// 不该把字数上限与摘要长度一起清成 0
	def := DefaultBudget()
	if budget.Window <= 0 {
		budget.Window = def.Window
	}
	if budget.MaxRunes <= 0 {
		budget.MaxRunes = def.MaxRunes
	}
	if budget.TopK <= 0 {
		budget.TopK = def.TopK
	}
	if budget.SummaryRunes <= 0 {
		budget.SummaryRunes = def.SummaryRunes
	}
	if budget.MinScore <= 0 {
		budget.MinScore = def.MinScore
	}
	return &Manager{store: st, router: router, budget: budget, log: log}
}

// EmbeddingEnabled 报告语义检索是否可用（需配置 LLM_EMBED_*）。
func (m *Manager) EmbeddingEnabled() bool {
	return m.router != nil && m.router.EmbeddingReady()
}

// Build 组装模型上下文。
func (m *Manager) Build(ctx context.Context, in Input) ([]llm.Message, error) {
	msgs := filterMessages(in.History)

	early := make([]store.Message, 0)
	recent := msgs
	if len(msgs) > m.budget.Window {
		early = msgs[:len(msgs)-m.budget.Window]
		recent = msgs[len(msgs)-m.budget.Window:]
	}

	out := make([]llm.Message, 0, len(recent)+2)

	if len(early) > 0 {
		if summary := m.summarize(ctx, in.SessionID, early); summary != "" {
			out = append(out, llm.Message{
				Role:    llm.RoleSystem,
				Content: "【更早对话的摘要】\n" + summary,
			})
		}
	}

	if m.EmbeddingEnabled() && strings.TrimSpace(in.Query) != "" {
		if snippets := m.retrieve(ctx, in.SessionID, in.Query, recent); len(snippets) > 0 {
			out = append(out, llm.Message{
				Role:    llm.RoleSystem,
				Content: "【与当前问题相关的历史片段】\n" + strings.Join(snippets, "\n---\n"),
			})
		}
	}

	// 最近消息按预算倒推，宁少勿超
	recentMsgs := make([]llm.Message, 0, len(recent))
	remaining := m.budget.MaxRunes
	for i := len(recent) - 1; i >= 0 && remaining > 0; i-- {
		content := recent[i].Content
		size := utf8.RuneCountInString(content)
		if size > remaining {
			// 最新一条自己就超预算（多半是用户粘了一整份日志）：截断保留头部，
			// 而不是整条丢掉。丢掉等于「用户说的话模型一个字都没听到」，
			// 而且不会报错，是最难排查的一类故障。
			if len(recentMsgs) == 0 {
				content = truncateRunes(content, remaining)
				recentMsgs = append(recentMsgs, llm.Message{Role: recent[i].Role, Content: content})
			}
			break
		}
		remaining -= size
		recentMsgs = append(recentMsgs, llm.Message{Role: recent[i].Role, Content: content})
	}
	for i, j := 0, len(recentMsgs)-1; i < j; i, j = i+1, j-1 {
		recentMsgs[i], recentMsgs[j] = recentMsgs[j], recentMsgs[i]
	}

	return append(out, recentMsgs...), nil
}

// summarize 生成或复用滚动摘要。
func (m *Manager) summarize(ctx context.Context, sessionID string, early []store.Message) string {
	last := early[len(early)-1].ID

	if cached, ok, err := m.store.GetSessionSummary(ctx, sessionID); err == nil && ok {
		// 覆盖范围已包含这批早期消息时直接复用，避免每轮重复调用模型
		if cached.UptoMessageID == last && cached.Content != "" {
			return cached.Content
		}
	}

	if m.router == nil {
		return fallbackSummary(early, m.budget.SummaryRunes)
	}

	var b strings.Builder
	for _, msg := range early {
		role := "用户"
		if msg.Role == llm.RoleAssistant {
			role = "助手"
		}
		fmt.Fprintf(&b, "%s：%s\n", role, truncateRunes(msg.Content, 400))
	}

	prompt := fmt.Sprintf(`把下面的历史对话压缩成不超过 %d 字的事实性摘要，保留：
1. 用户目标与约束
2. 已确认的结论与关键数据
3. 未完成或待确认的事项
只输出摘要正文，不要加评论，不要推测。

<对话>
%s
</对话>`, m.budget.SummaryRunes, b.String())

	sumCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	res, provider, err := m.router.Chat(sumCtx, llm.RouteSummary, llm.ChatRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: prompt}},
	}, nil)
	if err != nil {
		m.log.Warn("生成对话摘要失败，使用截断兜底", "provider", provider, "err", err)
		return fallbackSummary(early, m.budget.SummaryRunes)
	}

	summary := strings.TrimSpace(res.Content)
	if summary == "" {
		return fallbackSummary(early, m.budget.SummaryRunes)
	}

	if err := m.store.SaveSessionSummary(ctx, store.SessionSummary{
		SessionID:     sessionID,
		UptoMessageID: last,
		Content:       summary,
		MessageCount:  len(early),
		Model:         res.Model,
	}); err != nil {
		m.log.Warn("保存对话摘要失败", "session_id", sessionID, "err", err)
	}
	return summary
}

// retrieve 用向量相似度召回与当前问题相关的历史片段。
func (m *Manager) retrieve(ctx context.Context, sessionID, query string, recent []store.Message) []string {
	vectors, err := m.router.Embed(ctx, []string{query})
	if err != nil || len(vectors) == 0 || len(vectors[0]) == 0 {
		m.log.Debug("查询向量生成失败，跳过检索", "err", err)
		return nil
	}
	queryVec := vectors[0]

	records, err := m.store.ListMessageEmbeddings(ctx, sessionID, 2000)
	if err != nil {
		m.log.Warn("读取消息向量失败", "err", err)
		return nil
	}
	if len(records) == 0 {
		return nil
	}

	// 最近窗口里的消息本就在上下文里，不重复召回
	recentIDs := make(map[string]struct{}, len(recent))
	for _, msg := range recent {
		recentIDs[msg.ID] = struct{}{}
	}

	type hit struct {
		score   float64
		content string
	}
	hits := make([]hit, 0, len(records))
	for _, rec := range records {
		if _, ok := recentIDs[rec.MessageID]; ok {
			continue
		}
		if len(rec.Vector) != len(queryVec) {
			continue
		}
		score := cosine(queryVec, rec.Vector)
		if score < m.budget.MinScore {
			continue
		}
		hits = append(hits, hit{score: score, content: truncateRunes(rec.Content, 300)})
	}

	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	if len(hits) > m.budget.TopK {
		hits = hits[:m.budget.TopK]
	}

	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.content)
	}
	return out
}

// Index 为消息生成向量，供后续检索；未配置向量模型时直接返回。
func (m *Manager) Index(ctx context.Context, sessionID string, msgs []store.Message) error {
	if !m.EmbeddingEnabled() || len(msgs) == 0 {
		return nil
	}

	texts := make([]string, 0, len(msgs))
	for _, msg := range msgs {
		if strings.TrimSpace(msg.Content) == "" {
			continue
		}
		texts = append(texts, truncateRunes(msg.Content, 2000))
	}
	if len(texts) == 0 {
		return nil
	}

	vectors, err := m.router.Embed(ctx, texts)
	if err != nil {
		return err
	}

	idx := 0
	for _, msg := range msgs {
		if strings.TrimSpace(msg.Content) == "" {
			continue
		}
		if idx >= len(vectors) || len(vectors[idx]) == 0 {
			idx++
			continue
		}
		if err := m.store.SaveMessageEmbedding(ctx, store.EmbeddingRecord{
			MessageID: msg.ID,
			SessionID: sessionID,
			Model:     m.router.Model(llm.RouteEmbedding),
			Vector:    vectors[idx],
		}); err != nil {
			return err
		}
		idx++
	}
	return nil
}

func filterMessages(msgs []store.Message) []store.Message {
	out := make([]store.Message, 0, len(msgs))
	for _, msg := range msgs {
		if msg.Role != llm.RoleUser && msg.Role != llm.RoleAssistant {
			continue
		}
		if strings.TrimSpace(msg.Content) == "" {
			continue
		}
		out = append(out, msg)
	}
	return out
}

func fallbackSummary(msgs []store.Message, limit int) string {
	if len(msgs) == 0 {
		return ""
	}
	// 模型不可用时退化为「首尾拼接」，保证至少保留关键上下文
	head := msgs[0]
	tail := msgs[len(msgs)-1]
	text := fmt.Sprintf("更早共 %d 条消息。起始：%s；最近：%s",
		len(msgs), truncateRunes(head.Content, limit/3), truncateRunes(tail.Content, limit/3))
	return truncateRunes(text, limit)
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func truncateRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	runes := []rune(s)
	return string(runes[:limit]) + "…"
}
