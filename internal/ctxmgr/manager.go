// Package ctxmgr 实现上下文管理（技术方案 §5.7.2 摘要压缩、§5.7.4 记忆与按需检索）。
//
// 三段式组装：
//
//	[长期摘要]     更早的消息折叠成事实性摘要，避免上下文线性膨胀
//	[按需检索]     用本轮 query 的向量召回历史片段，补齐摘要遗漏的细节
//	[滑动窗口]     最近 N 条保留原文，保证指代与语气连续
//
// 摘要是「后台增量折叠」，不是「请求内同步全量重算」：Build 只读快照，
// 需要推进覆盖水位时把折叠丢给后台 goroutine（同会话同飞去重）。
// 这样摘要模型再慢也只影响「更早对话」的完整度，不会把用户挡在一分钟的空白里。
package ctxmgr

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/workagent/auto-agent/internal/llm"
	"github.com/workagent/auto-agent/internal/store"
)

// summaryHeader 是摘要段在上下文里的固定前缀，前端与提示词都靠它识别。
const summaryHeader = "【更早对话的摘要】\n"

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
	// FoldEvery 触发一次增量折叠的最小新增消息条数：摘要已经覆盖到哪里就推进到哪里，
	// 没必要每轮都折。折叠落后于窗口时不丢消息——落后的那一段按原文进上下文。
	FoldEvery int
	// SummaryInputTokens 是单次折叠输入的 token 预算，超了按段连续折叠，
	// 避免「一次塞进全部早期消息」把摘要模型也顶到超时。
	SummaryInputTokens int
	// FoldTimeout 是后台折叠的整体超时：它不在用户等待路径上，可以给得宽一些。
	FoldTimeout time.Duration
	// RetrieveTimeout 是向量检索在请求路径上的最大等待。
	// 检索只是「锦上添花」，让它拖住首 token 不值得。
	RetrieveTimeout time.Duration
}

// DefaultBudget 返回默认预算。
func DefaultBudget() Budget {
	return Budget{
		Window:             16,
		MaxRunes:           24000,
		TopK:               4,
		SummaryRunes:       600,
		MinScore:           0.35,
		FoldEvery:          4,
		SummaryInputTokens: 6000,
		FoldTimeout:        90 * time.Second,
		RetrieveTimeout:    5 * time.Second,
	}
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

	// 同会话同时只跑一个折叠：两个 goroutine 并行读同一个基线摘要，
	// 后写的那份会把水位往回拨，摘要原地打转。
	mu      sync.Mutex
	folding map[string]bool
	// wg 让测试（以及未来的优雅关闭）能等后台折叠收尾。
	wg sync.WaitGroup
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
	if budget.FoldEvery <= 0 {
		budget.FoldEvery = def.FoldEvery
	}
	if budget.SummaryInputTokens <= 0 {
		budget.SummaryInputTokens = def.SummaryInputTokens
	}
	if budget.FoldTimeout <= 0 {
		budget.FoldTimeout = def.FoldTimeout
	}
	if budget.RetrieveTimeout <= 0 {
		budget.RetrieveTimeout = def.RetrieveTimeout
	}
	return &Manager{
		store:   st,
		router:  router,
		budget:  budget,
		log:     log,
		folding: make(map[string]bool),
	}
}

// EmbeddingEnabled 报告语义检索是否可用（需配置 LLM_EMBED_*）。
func (m *Manager) EmbeddingEnabled() bool {
	return m.router != nil && m.router.EmbeddingReady()
}

// Build 组装模型上下文。
//
// 只有一次「读摘要快照」的库操作在请求路径上；摘要模型一次都不等。
func (m *Manager) Build(ctx context.Context, in Input) ([]llm.Message, error) {
	msgs := filterMessages(in.History)

	snap, hasSummary := m.loadSummary(ctx, in.SessionID)

	// 摘要覆盖到哪（不含）与窗口起点：两者取小，保证「摘要没追上」的那一段
	// 以原文出现在上下文里，而不是被窗口挤掉、又不被摘要记住。
	covered, cut := 0, len(msgs)-m.budget.Window
	if cut < 0 {
		cut = 0
	}
	if hasSummary {
		covered = indexAfter(msgs, snap.UptoMessageID)
	}
	keepFrom := covered
	if keepFrom > cut {
		keepFrom = cut
	}

	out := make([]llm.Message, 0, len(msgs)-keepFrom+2)
	if hasSummary && snap.Content != "" {
		out = append(out, llm.Message{
			Role:    llm.RoleSystem,
			Content: summaryHeader + snap.Content,
		})
	}

	if m.EmbeddingEnabled() && strings.TrimSpace(in.Query) != "" {
		if snippets := m.retrieve(ctx, in.SessionID, in.Query, msgs[keepFrom:]); len(snippets) > 0 {
			out = append(out, llm.Message{
				Role:    llm.RoleSystem,
				Content: "【与当前问题相关的历史片段】\n" + strings.Join(snippets, "\n---\n"),
			})
		}
	}

	recent := msgs[keepFrom:]
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

	// 窗口外的这一段交给后台折叠；本轮先用现有摘要，绝不在这里等模型
	if covered < cut {
		m.scheduleFold(ctx, in.SessionID, msgs[:cut], msgs[covered:cut], snap, hasSummary)
	}

	return append(out, recentMsgs...), nil
}

// loadSummary 读摘要快照。读失败按「还没有摘要」处理：宁可这几轮多带点原文，
// 也不该让一个读库错误把整轮对话打掉。
func (m *Manager) loadSummary(ctx context.Context, sessionID string) (store.SessionSummary, bool) {
	if m.store == nil || sessionID == "" {
		return store.SessionSummary{}, false
	}
	snap, ok, err := m.store.GetSessionSummary(ctx, sessionID)
	if err != nil {
		m.log.Warn("读取对话摘要失败，本轮按无摘要处理", "session_id", sessionID, "err", err)
		return store.SessionSummary{}, false
	}
	if !ok || strings.TrimSpace(snap.Content) == "" {
		return store.SessionSummary{}, false
	}
	return snap, true
}

// scheduleFold 把「窗口外的新增消息」交给后台折叠。
//
// 判据用的是「距离上次水位新增了多少条」而不是「有没有早期消息」：
// 后者会让每个新会话的每一轮都白跑一次模型调用。
func (m *Manager) scheduleFold(
	ctx context.Context,
	sessionID string,
	history, delta []store.Message,
	snap store.SessionSummary,
	hasSummary bool,
) {
	if m.router == nil || len(delta) == 0 || sessionID == "" {
		return
	}
	// 已有摘要时攒够一批再折；还没有摘要（首次跨过窗口）就立刻补一份，
	// 否则那一段永远只有原文、迟早被预算挤掉。
	if hasSummary && len(delta) < m.budget.FoldEvery {
		return
	}
	if !m.beginFold(sessionID) {
		return
	}

	job := foldJob{
		sessionID: sessionID,
		goal:      sessionGoal(history),
		delta:     append([]store.Message(nil), delta...),
	}
	if hasSummary {
		job.base = snap.Content
		job.count = snap.MessageCount
	}

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer m.endFold(sessionID)

		// 与请求脱钩：用户关页面、点停止都不该让折叠半途而废。
		// 但超时要有——后台失败只损失一次增量，不损失已有摘要。
		foldCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.budget.FoldTimeout)
		defer cancel()
		if err := m.fold(foldCtx, job); err != nil {
			m.log.Warn("折叠对话摘要失败，沿用旧摘要",
				"session_id", sessionID, "messages", len(job.delta), "err", err)
		}
	}()
}

// foldJob 是一次增量折叠的全部输入。
type foldJob struct {
	sessionID string
	// base 是已覆盖部分（旧摘要），空表示从零开始
	base  string
	count int
	// goal 是会话起点（用户最初的诉求），每轮都钉住，避免摘要越滚越偏离目标
	goal  string
	delta []store.Message
}

// fold 按 token 预算分段连续折叠，每段落一次库。
//
// 分段落库是刻意的：一次折叠可能因为输入太长而超时，已折好的水位要留住，
// 下一轮从断点接着折，而不是每次都从头再来。
func (m *Manager) fold(ctx context.Context, job foldJob) error {
	for _, seg := range m.segments(job.delta) {
		summary, model, err := m.foldOnce(ctx, job.base, job.goal, seg)
		if err != nil {
			return err
		}
		job.base = summary
		job.count += len(seg)
		if err := m.store.SaveSessionSummary(ctx, store.SessionSummary{
			SessionID:     job.sessionID,
			UptoMessageID: seg[len(seg)-1].ID,
			Content:       summary,
			MessageCount:  job.count,
			Model:         model,
		}); err != nil {
			return err
		}
	}
	return nil
}

// segments 把增量按 token 预算切段。
func (m *Manager) segments(delta []store.Message) [][]store.Message {
	limit := m.budget.SummaryInputTokens
	out := make([][]store.Message, 0, 1+len(delta)/8)
	cur := make([]store.Message, 0, 8)
	used := 0
	for _, msg := range delta {
		// 每条多算 8 token：角色名、换行与「用户：」这类前缀也要占位置
		t := llm.EstimateTokens(msg.Content) + 8
		if len(cur) > 0 && used+t > limit {
			out = append(out, cur)
			cur = make([]store.Message, 0, 8)
			used = 0
		}
		cur = append(cur, msg)
		used += t
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// foldOnce 做一次「旧摘要 + 新消息 → 新摘要」的模型调用。
func (m *Manager) foldOnce(ctx context.Context, base, goal string, seg []store.Message) (string, string, error) {
	var b strings.Builder
	for _, msg := range seg {
		role := "用户"
		if msg.Role == llm.RoleAssistant {
			role = "助手"
		}
		fmt.Fprintf(&b, "%s：%s\n", role, msg.Content)
	}

	if strings.TrimSpace(base) == "" {
		base = "（无，这是第一份摘要）"
	}
	goalBlock := ""
	if goal != "" {
		goalBlock = fmt.Sprintf("\n<会话起点>\n用户：%s\n</会话起点>\n（会话起点是用户最初的目标，摘要必须始终保留它的核心约束）\n", goal)
	}

	prompt := fmt.Sprintf(`你在维护一段长对话的滚动摘要：已有摘要覆盖更早的内容，下面额外给出需要合并进来的新增对话。
请输出合并后的完整摘要，规则：
1. 在旧摘要的基础上合并与修正，不要重写无关部分；旧摘要的事实除非被新内容明确推翻，否则保留。
2. 固定分节输出：用户目标与约束 / 已确认结论与关键数据 / 未决事项 / 关键引用（文件、接口、表名、路径）。
3. 只写对话里出现过的事实，不推测、不加评论，总长不超过 %d 字。

<已有摘要>
%s
</已有摘要>
%s
<新增对话>
%s
</新增对话>`, m.budget.SummaryRunes, base, goalBlock, b.String())

	// 给足输出空间：思考也算在 max_tokens 里，卡太紧会出现「只有思考、没有正文」
	maxTokens := m.budget.SummaryRunes * 8
	if maxTokens < 4096 {
		maxTokens = 4096
	}
	res, provider, err := m.router.Chat(ctx, llm.RouteSummary, llm.ChatRequest{
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: prompt}},
		MaxTokens: maxTokens,
	}, nil)
	if err != nil {
		return "", provider, err
	}
	summary := strings.TrimSpace(res.Content)
	if summary == "" {
		return "", provider, fmt.Errorf("摘要模型返回空正文")
	}
	return summary, res.Model, nil
}

func (m *Manager) beginFold(sessionID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.folding[sessionID] {
		return false
	}
	m.folding[sessionID] = true
	return true
}

func (m *Manager) endFold(sessionID string) {
	m.mu.Lock()
	delete(m.folding, sessionID)
	m.mu.Unlock()
}

// indexAfter 返回水位消息之后的位置；水位不在历史里（消息被删）时返回 0，
// 表示「整段重折」——宁可多花一次模型调用，也不要留下覆盖空洞。
func indexAfter(msgs []store.Message, upto string) int {
	if upto == "" {
		return 0
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].ID == upto {
			return i + 1
		}
	}
	return 0
}

// sessionGoal 取会话起点（第一条用户消息），作为折叠时始终钉住的锚点。
func sessionGoal(history []store.Message) string {
	for _, msg := range history {
		if msg.Role != llm.RoleUser || strings.TrimSpace(msg.Content) == "" {
			continue
		}
		return truncateRunes(msg.Content, 800)
	}
	return ""
}

// retrieve 用向量相似度召回与当前问题相关的历史片段。
func (m *Manager) retrieve(ctx context.Context, sessionID, query string, recent []store.Message) []string {
	// 检索在请求路径上，得有自己的上限：向量服务打嗝不该拖住首 token
	retrieveCtx, cancel := context.WithTimeout(ctx, m.budget.RetrieveTimeout)
	defer cancel()

	vectors, err := m.router.Embed(retrieveCtx, []string{query})
	if err != nil || len(vectors) == 0 || len(vectors[0]) == 0 {
		m.log.Debug("查询向量生成失败，跳过检索", "err", err)
		return nil
	}
	queryVec := vectors[0]

	records, err := m.store.ListMessageEmbeddings(retrieveCtx, sessionID, 2000)
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
