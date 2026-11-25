package ctxmgr

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/workagent/auto-agent/internal/llm"
	"github.com/workagent/auto-agent/internal/store"
)

// fakeStore 只覆盖摘要读写；其余方法走嵌入接口，测试里不会调用。
type fakeStore struct {
	store.Store

	mu      sync.Mutex
	summary store.SessionSummary
	has     bool
	saves   int
}

func (f *fakeStore) GetSessionSummary(context.Context, string) (store.SessionSummary, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.summary, f.has, nil
}

func (f *fakeStore) SaveSessionSummary(_ context.Context, s store.SessionSummary) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.summary, f.has = s, true
	f.saves++
	return nil
}

func (f *fakeStore) snapshot() (store.SessionSummary, bool, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.summary, f.has, f.saves
}

// fakeProvider 记录摘要调用的提示词，可注入耗时与失败。
type fakeProvider struct {
	mu      sync.Mutex
	prompts []string
	calls   int
	delay   time.Duration
	// fail 返回 true 时这次折叠失败
	fail func(prompt string) bool
	// reply 生成摘要正文，缺省返回「摘要」
	reply func(prompt string) string
}

func (p *fakeProvider) Info() llm.ProviderInfo {
	return llm.ProviderInfo{Name: "fake", Kind: "openai", Model: "fake-model"}
}

func (p *fakeProvider) Chat(ctx context.Context, req llm.ChatRequest, _ llm.DeltaFunc) (llm.Result, error) {
	p.mu.Lock()
	p.calls++
	prompt := ""
	if len(req.Messages) > 0 {
		prompt = req.Messages[0].Content
	}
	p.prompts = append(p.prompts, prompt)
	delay, fail, reply := p.delay, p.fail, p.reply
	p.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return llm.Result{}, ctx.Err()
		}
	}
	if fail != nil && fail(prompt) {
		return llm.Result{}, errors.New("模型调用失败")
	}
	content := "摘要"
	if reply != nil {
		content = reply(prompt)
	}
	return llm.Result{Content: content, Model: "fake-model"}, nil
}

func (p *fakeProvider) Embed(context.Context, []string) ([][]float32, error) {
	return nil, llm.ErrUnsupportedProvider
}

func (p *fakeProvider) lastPrompt() (string, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.prompts) == 0 {
		return "", p.calls
	}
	return p.prompts[len(p.prompts)-1], p.calls
}

func testManager(maxRunes int) *Manager {
	return New(nil, nil, nil, Budget{Window: 8, MaxRunes: maxRunes, TopK: 3, SummaryRunes: 200})
}

// history 造 n 条交替的用户/助手消息，ID 为 m1..mn。
func history(n int, content string) []store.Message {
	out := make([]store.Message, 0, n)
	for i := 1; i <= n; i++ {
		role := "user"
		if i%2 == 0 {
			role = "assistant"
		}
		out = append(out, store.Message{ID: fmt.Sprintf("m%d", i), Role: role, Content: content})
	}
	return out
}

func withProvider(st store.Store, p *fakeProvider, b Budget) *Manager {
	router := llm.NewRouter(nil)
	router.Register(p)
	router.SetRoute(llm.RouteSummary, llm.Route{Primary: "fake"})
	return New(st, router, nil, b)
}

// 最新一条自己就超预算时，必须截断保留，而不是整条丢掉。
// 丢掉等于「用户刚发的内容模型一个字都没看到」，而且不报错。
func TestBuildKeepsOversizeLatestMessage(t *testing.T) {
	m := testManager(100)
	long := strings.Repeat("很长的内容", 500) // 2500 字，远超 100 字预算
	msgs, err := m.Build(context.Background(), Input{
		History: []store.Message{{Role: "user", Content: long}},
		Query:   long,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("应保留 1 条消息，实际 %d 条", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Fatalf("角色不对: %s", msgs[0].Role)
	}
	runes := []rune(msgs[0].Content)
	if len(runes) > 101 { // 100 字 + 省略号
		t.Fatalf("截断后仍然过长: %d 字", len(runes))
	}
	if !strings.HasPrefix(long, strings.TrimSuffix(msgs[0].Content, "…")) {
		t.Fatal("截断应保留头部原文")
	}
}

// 正常预算下按「从新到旧」装满，旧的被丢掉。
func TestBuildKeepsNewestWithinBudget(t *testing.T) {
	m := testManager(10)
	hist := []store.Message{
		{Role: "user", Content: strings.Repeat("旧", 6)},
		{Role: "assistant", Content: strings.Repeat("中", 6)},
		{Role: "user", Content: strings.Repeat("新", 6)},
	}
	msgs, err := m.Build(context.Background(), Input{History: hist})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("10 字预算只装得下 1 条，实际 %d 条", len(msgs))
	}
	if !strings.HasPrefix(msgs[0].Content, "新") {
		t.Fatalf("应保留最新一条，实际: %s", msgs[0].Content)
	}
}

// 没超预算时原样返回，顺序不变。
func TestBuildKeepsOrder(t *testing.T) {
	m := testManager(1000)
	hist := []store.Message{
		{Role: "user", Content: "一"},
		{Role: "assistant", Content: "二"},
		{Role: "user", Content: "三"},
	}
	msgs, err := m.Build(context.Background(), Input{History: hist})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("应保留 3 条，实际 %d", len(msgs))
	}
	for i, want := range []string{"一", "二", "三"} {
		if msgs[i].Content != want {
			t.Fatalf("第 %d 条顺序不对: %s", i, msgs[i].Content)
		}
	}
}

// 只改窗口条数时，不该把其他预算一起清成 0。
func TestNewFillsDefaultsPerField(t *testing.T) {
	m := New(nil, nil, nil, Budget{Window: 4})
	def := DefaultBudget()
	if m.budget.MaxRunes != def.MaxRunes || m.budget.SummaryRunes != def.SummaryRunes {
		t.Fatalf("缺省值没补上: %+v", m.budget)
	}
	if m.budget.Window != 4 {
		t.Fatalf("显式配置的窗口被覆盖了: %d", m.budget.Window)
	}
	if m.budget.FoldEvery != def.FoldEvery || m.budget.SummaryInputTokens != def.SummaryInputTokens {
		t.Fatalf("折叠预算缺省值没补上: %+v", m.budget)
	}
	if m.budget.FoldTimeout != def.FoldTimeout || m.budget.RetrieveTimeout != def.RetrieveTimeout {
		t.Fatalf("超时缺省值没补上: %+v", m.budget)
	}
}

// 关键回归：摘要模型再慢也不许挡在 Build 里。
// 旧实现在这里同步调模型，摘要超时 60s 就是用户干等 60s。
func TestBuildDoesNotWaitForSummary(t *testing.T) {
	st := &fakeStore{}
	p := &fakeProvider{delay: 2 * time.Second}
	m := withProvider(st, p, Budget{Window: 4, MaxRunes: 100000, FoldEvery: 1})

	start := time.Now()
	msgs, err := m.Build(context.Background(), Input{
		SessionID: "s1",
		History:   history(10, "内容"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Build 被摘要调用挡住了：%s", elapsed)
	}
	// 还没有摘要时，窗口外的消息以原文进上下文，一条都不能少
	if len(msgs) != 10 {
		t.Fatalf("没有摘要时应带上全部原文，实际 %d 条", len(msgs))
	}

	m.wg.Wait()
	snap, has, saves := st.snapshot()
	if !has || saves != 1 {
		t.Fatalf("后台应折叠一次并落库，saves=%d has=%v", saves, has)
	}
	if snap.UptoMessageID != "m6" { // 窗口 4 → 折叠 m1..m6
		t.Fatalf("水位应停在窗口边界 m6，实际 %s", snap.UptoMessageID)
	}
}

// 有摘要时：摘要段 + 窗口原文；水位之后、窗口之前的那一段仍按原文带上。
func TestBuildUsesSnapshotAndKeepsBacklogRaw(t *testing.T) {
	st := &fakeStore{
		has:     true,
		summary: store.SessionSummary{SessionID: "s1", UptoMessageID: "m2", Content: "旧摘要", MessageCount: 2},
	}
	p := &fakeProvider{}
	m := withProvider(st, p, Budget{Window: 4, MaxRunes: 100000, FoldEvery: 1})

	msgs, err := m.Build(context.Background(), Input{SessionID: "s1", History: history(10, "内容")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(msgs[0].Content, summaryHeader) || !strings.Contains(msgs[0].Content, "旧摘要") {
		t.Fatalf("首条应是摘要段，实际: %q", msgs[0].Content)
	}
	// m3..m10 共 8 条：m3..m6 是待折叠的积压，也要原样带上
	if got := len(msgs); got != 9 {
		t.Fatalf("应为 1 条摘要 + 8 条原文，实际 %d", got)
	}
	if msgs[1].Content != "内容" || msgs[len(msgs)-1].Role != "assistant" {
		t.Fatal("原文段丢了消息")
	}

	m.wg.Wait()
	snap, _, _ := st.snapshot()
	if snap.UptoMessageID != "m6" {
		t.Fatalf("折叠应推进到 m6，实际 %s", snap.UptoMessageID)
	}
	prompt, calls := p.lastPrompt()
	if calls != 1 {
		t.Fatalf("应只折一次，实际 %d", calls)
	}
	// 增量折叠：旧摘要作为合并基线进提示词，而不是从零重写
	if !strings.Contains(prompt, "旧摘要") {
		t.Fatalf("提示词没带上旧摘要: %s", prompt)
	}
	if !strings.Contains(prompt, "<已有摘要>") || !strings.Contains(prompt, "<新增对话>") {
		t.Fatalf("提示词缺少增量折叠结构: %s", prompt)
	}
}

// 攒不够 FoldEvery 就不折：每轮都调一次模型纯属浪费。
func TestFoldThrottledByFoldEvery(t *testing.T) {
	st := &fakeStore{
		has:     true,
		summary: store.SessionSummary{SessionID: "s1", UptoMessageID: "m2", Content: "旧摘要"},
	}
	p := &fakeProvider{}
	m := withProvider(st, p, Budget{Window: 4, MaxRunes: 100000, FoldEvery: 3})

	if _, err := m.Build(context.Background(), Input{SessionID: "s1", History: history(8, "内容")}); err != nil {
		t.Fatal(err)
	}
	m.wg.Wait()
	if _, calls := p.lastPrompt(); calls != 0 {
		t.Fatalf("新增 1 条不该触发折叠，实际调用 %d 次", calls)
	}
}

// 输入超预算时按段连续折叠，每段落库，水位逐段推进。
func TestFoldChunksByTokenBudget(t *testing.T) {
	st := &fakeStore{}
	p := &fakeProvider{reply: func(prompt string) string { return "分段摘要" }}
	m := withProvider(st, p, Budget{
		Window:             2,
		MaxRunes:           1000000,
		FoldEvery:          1,
		SummaryInputTokens: 30, // 约 5 个汉字，8 条消息必然切成多段
	})

	if _, err := m.Build(context.Background(), Input{SessionID: "s1", History: history(12, strings.Repeat("消", 20))}); err != nil {
		t.Fatal(err)
	}
	m.wg.Wait()
	if _, calls := p.lastPrompt(); calls < 2 {
		t.Fatalf("超预算应分段折叠，实际只调用 %d 次", calls)
	}
	snap, has, _ := st.snapshot()
	if !has {
		t.Fatal("应落库")
	}
	if snap.UptoMessageID != "m10" { // 窗口 2 → 折叠 m1..m10
		t.Fatalf("水位应推进到最后一段 m10，实际 %s", snap.UptoMessageID)
	}
}

// 折叠失败保留旧摘要，不能把已经积累的上下文替换成兜底文本。
func TestFoldFailureKeepsOldSummary(t *testing.T) {
	st := &fakeStore{
		has:     true,
		summary: store.SessionSummary{SessionID: "s1", UptoMessageID: "m2", Content: "旧摘要"},
	}
	p := &fakeProvider{fail: func(string) bool { return true }}
	m := withProvider(st, p, Budget{Window: 4, MaxRunes: 100000, FoldEvery: 1, FoldTimeout: 2 * time.Second})

	if _, err := m.Build(context.Background(), Input{SessionID: "s1", History: history(10, "内容")}); err != nil {
		t.Fatal(err)
	}
	m.wg.Wait()
	snap, _, saves := st.snapshot()
	if saves != 0 {
		t.Fatalf("失败不该写库，实际写入 %d 次", saves)
	}
	if snap.Content != "旧摘要" || snap.UptoMessageID != "m2" {
		t.Fatalf("旧摘要被改写了: %+v", snap)
	}
}

// 折叠提示词要钉住会话起点，避免摘要越滚越脱离用户最初的目标。
func TestFoldPromptPinsSessionGoal(t *testing.T) {
	st := &fakeStore{}
	p := &fakeProvider{}
	m := withProvider(st, p, Budget{Window: 2, MaxRunes: 100000, FoldEvery: 1})

	hist := history(6, "内容")
	hist[0].Content = "最初的诉求：把数据建模技能内置进智能体"
	if _, err := m.Build(context.Background(), Input{SessionID: "s1", History: hist}); err != nil {
		t.Fatal(err)
	}
	m.wg.Wait()
	prompt, _ := p.lastPrompt()
	if !strings.Contains(prompt, "会话起点") || !strings.Contains(prompt, "最初的诉求") {
		t.Fatalf("提示词没有钉住会话起点: %s", prompt)
	}
}
