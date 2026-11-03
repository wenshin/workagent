package session

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/workagent/auto-agent/internal/agent"
	"github.com/workagent/auto-agent/internal/ctxmgr"
	"github.com/workagent/auto-agent/internal/llm"
	"github.com/workagent/auto-agent/internal/space"
	"github.com/workagent/auto-agent/internal/storage"
	"github.com/workagent/auto-agent/internal/store"
	"github.com/workagent/auto-agent/internal/tool"
)

var (
	ErrForbidden      = errors.New("没有该空间的权限")
	ErrTitleInvalid   = errors.New("会话标题需为 1-64 个字符")
	ErrMessageEmpty   = errors.New("消息内容不能为空")
	ErrMessageTooLong = errors.New("单条消息不能超过 8000 字")
	ErrLastSession    = errors.New("每个空间至少要保留一个会话")
	ErrAgentUnavailable = errors.New("模型服务未配置，请联系管理员配置 LLM_BASE_URL、LLM_API_KEY、LLM_MODEL")
)

const (
	defaultTitle    = "新会话"
	maxTitleRunes   = 64
	maxMessageRunes = 8000
	// 首条消息自动生成的标题长度（超出部分省略）
	autoTitleRunes = 20
)

// Service 承载聊天会话的业务规则。
type Service struct {
	store   store.Store
	spaces  *space.Service
	agent   *agent.Service
	storage storage.Storage
	memory  *ctxmgr.Manager
	log     *slog.Logger
}

// NewService 构造会话服务。
func NewService(st store.Store, spaces *space.Service, agents *agent.Service, fs storage.Storage, memory *ctxmgr.Manager, log *slog.Logger) *Service {
	return &Service{store: st, spaces: spaces, agent: agents, storage: fs, memory: memory, log: log}
}

// AgentReady 返回模型服务是否可用。
func (s *Service) AgentReady() bool { return s.agent != nil && s.agent.Ready() }

// AgentModel 返回当前模型名。
func (s *Service) AgentModel() string {
	if s.agent == nil {
		return ""
	}
	return s.agent.Model()
}

// List 返回空间下的会话。
func (s *Service) List(ctx context.Context, userID, spaceID string) ([]store.Session, error) {
	if _, err := s.spaces.Get(ctx, userID, spaceID); err != nil {
		return nil, err
	}
	return s.store.ListSessions(ctx, spaceID)
}

// ListMine 返回当前用户可见的全部会话（左侧树一次取数）。
func (s *Service) ListMine(ctx context.Context, userID string) ([]store.Session, error) {
	return s.store.ListUserSessions(ctx, userID)
}

// Create 在空间下新建会话，标题默认为「新会话」。
func (s *Service) Create(ctx context.Context, userID, spaceID, rawTitle string) (store.Session, error) {
	sp, err := s.spaces.Get(ctx, userID, spaceID)
	if err != nil {
		return store.Session{}, err
	}
	if !canWrite(sp.Perm) {
		return store.Session{}, ErrForbidden
	}

	title, err := normalizeTitle(rawTitle)
	if err != nil {
		return store.Session{}, err
	}

	sess, err := s.store.CreateSession(ctx, spaceID, userID, title)
	if err != nil {
		return store.Session{}, err
	}
	s.log.Info("会话已创建", "session_id", sess.ID, "space_id", spaceID, "by", userID)
	return sess, nil
}

// Rename 手动重命名会话；命名后不再被首条消息自动改写。
func (s *Service) Rename(ctx context.Context, userID, sessionID, rawTitle string) (store.Session, error) {
	sess, sp, err := s.load(ctx, userID, sessionID)
	if err != nil {
		return store.Session{}, err
	}
	if !canWrite(sp.Perm) {
		return store.Session{}, ErrForbidden
	}

	title, err := normalizeTitle(rawTitle)
	if err != nil {
		return store.Session{}, err
	}
	if err := s.store.RenameSession(ctx, sessionID, title, false); err != nil {
		return store.Session{}, err
	}
	sess.Title = title
	sess.TitleAuto = false
	return sess, nil
}

// Delete 删除会话，需要空间写权限；消息记录保留但不再展示。
func (s *Service) Delete(ctx context.Context, userID, sessionID string) error {
	sess, sp, err := s.load(ctx, userID, sessionID)
	if err != nil {
		return err
	}
	if !canWrite(sp.Perm) {
		return ErrForbidden
	}

	// 空间至少要保留一个会话，否则进入空间后没有对话入口
	if list, err := s.store.ListSessions(ctx, sess.SpaceID); err != nil {
		return err
	} else if len(list) <= 1 {
		return ErrLastSession
	}

	if err := s.store.DeleteSession(ctx, sessionID); err != nil {
		return err
	}
	s.log.Info("会话已删除", "session_id", sess.ID, "space_id", sess.SpaceID, "by", userID)
	return nil
}

// ValidateChat 校验一次对话请求，供 SSE 开始前返回常规错误码。
func (s *Service) ValidateChat(ctx context.Context, userID, sessionID, content string) (store.Session, store.Space, error) {
	sess, sp, err := s.load(ctx, userID, sessionID)
	if err != nil {
		return store.Session{}, store.Space{}, err
	}
	if !canWrite(sp.Perm) {
		return store.Session{}, store.Space{}, ErrForbidden
	}
	if _, err := normalizeMessage(content); err != nil {
		return store.Session{}, store.Space{}, err
	}
	if !s.AgentReady() {
		return store.Session{}, store.Space{}, ErrAgentUnavailable
	}
	return sess, sp, nil
}

// ChatOutcome 是一轮对话的落库结果与运行统计，供 SSE 收尾事件使用。
type ChatOutcome struct {
	Message store.Message
	Session store.Session
	RunID   string
	Usage   llm.Usage
	Status  string
}

// Chat 执行一次完整对话：保存用户消息 → 建 run → 调用 Agent（含工具循环）→ 保存助手消息。
//
// 每个执行步骤都写 run_step 做 checkpoint（§5.7.5）；流式增量通过 emit 推给调用方，
// 即使客户端中途断开，已生成的内容与 checkpoint 也会落库。
func (s *Service) Chat(
	ctx context.Context,
	userID, sessionID, content string,
	emit func(agent.Event) error,
) (ChatOutcome, error) {
	out := ChatOutcome{}

	sess, sp, err := s.ValidateChat(ctx, userID, sessionID, content)
	if err != nil {
		return out, err
	}
	text := strings.TrimSpace(content)

	// 先读历史，再写入本条用户消息，避免同一条消息重复进入上下文
	history, err := s.store.ListMessages(ctx, sessionID)
	if err != nil {
		return out, err
	}

	userMsg, err := s.store.CreateMessage(ctx, sessionID, "user", text)
	if err != nil {
		return out, err
	}
	if err := s.store.TouchSession(ctx, sessionID); err != nil {
		s.log.Warn("更新会话活跃时间失败", "session_id", sessionID, "err", err)
	}

	// 首条消息决定会话名
	if sess.TitleAuto {
		if title := summarize(text); title != "" {
			if err := s.store.RenameSession(ctx, sessionID, title, false); err != nil {
				s.log.Warn("会话标题自动生成失败", "session_id", sessionID, "err", err)
			} else {
				sess.Title = title
				sess.TitleAuto = false
			}
		}
	}

	history = append(history, store.Message{Role: "user", Content: text})

	// 上下文三段式组装：摘要 + 按需检索 + 滑动窗口（§5.7.2 / §5.7.4）
	contextMessages, err := s.memory.Build(ctx, ctxmgr.Input{
		SessionID: sessionID,
		History:   history,
		Query:     text,
	})
	if err != nil {
		return out, err
	}

	// 一次 run 为根，预算与步骤都落库，进程重启后可据此恢复（§5.7.5）
	budget, marshalErr := json.Marshal(s.agent.Budget())
	if marshalErr != nil {
		budget = []byte(`{}`)
	}
	run, err := s.store.CreateRun(ctx, store.Run{SessionID: sessionID, Budget: budget})
	if err != nil {
		return out, err
	}
	out.RunID = run.ID

	// checkpoint 与事件推送共用一条链路，保证「推给前端的」与「落库的」一致
	seq := 0
	appendStep := func(typ string, payload any) {
		data, err := json.Marshal(payload)
		if err != nil {
			return
		}
		seq++
		stepCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		_, err = s.store.AppendRunStep(stepCtx, store.RunStep{
			RunID: run.ID, Seq: seq, Type: typ, Payload: data,
		})
		cancel()
		if err != nil {
			s.log.Warn("写入 run checkpoint 失败", "run_id", run.ID, "seq", seq, "err", err)
		}
	}

	// run.started 由会话层补发：此时 run 已落库，前端可以据此做断线重连与取消
	_ = emit(agent.Event{Type: agent.EventRunStarted, RunID: run.ID})

	emitEvent := func(ev agent.Event) error {
		switch ev.Type {
		case agent.EventToolCall, agent.EventToolResult:
			appendStep(ev.Type, ev.Tool.Payload())
		case agent.EventApprovalReq:
			appendStep(ev.Type, ev.Approval)
		}
		return emit(ev)
	}

	runCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	res, runErr := s.agent.Run(runCtx, agent.RunInput{
		Env: tool.Env{
			UserID:    userID,
			SpaceID:   sess.SpaceID,
			SessionID: sessionID,
			RunID:     run.ID,
			SpacePerm: sp.Perm,
			Store:     s.store,
			Storage:   s.storage,
			Log:       s.log,
		},
		History: contextMessages,
		Query:   text,
	}, emitEvent)

	// 客户端主动停止或连接断开时，仍要把已生成的部分保存下来
	saveCtx := ctx
	if ctx.Err() != nil {
		saveCtx = context.WithoutCancel(ctx)
	}

	status := store.RunSucceeded
	switch {
	case ctx.Err() != nil:
		status = store.RunCancelled
	case runErr != nil:
		status = store.RunFailed
	}

	finish := func(errMsg string) {
		usage, err := json.Marshal(map[string]int{
			"in":  res.Usage.InputTokens,
			"out": res.Usage.OutputTokens,
		})
		if err != nil {
			usage = []byte(`{}`)
		}
		if err := s.store.FinishRun(saveCtx, store.Run{
			ID: run.ID, Status: status, Usage: usage,
			Provider: res.Provider, Model: res.Model, Error: errMsg,
		}); err != nil {
			s.log.Warn("更新 run 状态失败", "run_id", run.ID, "err", err)
		}
	}

	if res.Content == "" && len(res.Steps) == 0 {
		status = store.RunFailed
		if runErr != nil {
			finish(runErr.Error())
			return out, runErr
		}
		err := errors.New("模型没有返回内容，请重试")
		finish(err.Error())
		return out, err
	}

	parts, marshalErr := json.Marshal(res.Steps)
	if marshalErr != nil {
		parts = []byte("[]")
	}

	msg, err := s.store.CreateAssistantMessage(saveCtx, sessionID, res.Content, res.Reasoning, parts)
	if err != nil {
		status = store.RunFailed
		finish(err.Error())
		return out, err
	}
	appendStep("message", map[string]any{
		"message_id":      msg.ID,
		"chars":           utf8.RuneCountInString(res.Content),
		"reasoning_chars": utf8.RuneCountInString(res.Reasoning),
		"tools":           len(res.Steps),
		"need_approval":   res.NeedApproval,
	})
	finish("")

	if err := s.store.TouchSession(saveCtx, sessionID); err != nil {
		s.log.Warn("更新会话活跃时间失败", "session_id", sessionID, "err", err)
	}

	sess.LastMessageAt = &msg.CreatedAt
	if latest, err := s.store.GetSession(saveCtx, sessionID); err == nil {
		sess = latest
	}

	// 向量索引异步执行，失败不影响本次对话
	if s.memory.EmbeddingEnabled() {
		go func() {
			indexCtx, indexCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer indexCancel()
			if err := s.memory.Index(indexCtx, sessionID, []store.Message{userMsg, msg}); err != nil {
				s.log.Warn("写入消息向量失败", "session_id", sessionID, "err", err)
			}
		}()
	}

	s.log.Info("对话完成",
		"session_id", sessionID, "space_id", sess.SpaceID, "by", userID, "run_id", run.ID,
		"status", status, "provider", res.Provider, "model", res.Model,
		"chars", utf8.RuneCountInString(res.Content), "tools", len(res.Steps))

	out.Message = msg
	out.Session = sess
	out.Usage = res.Usage
	out.Status = status
	return out, runErr
}

// Messages 返回会话消息。
func (s *Service) Messages(ctx context.Context, userID, sessionID string) ([]store.Message, error) {
	if _, _, err := s.load(ctx, userID, sessionID); err != nil {
		return nil, err
	}
	return s.store.ListMessages(ctx, sessionID)
}

// AppendMessage 追加一条用户消息；若标题仍是自动状态，用首条消息生成标题。
func (s *Service) AppendMessage(ctx context.Context, userID, sessionID, content string) (store.Message, store.Session, error) {
	sess, sp, err := s.load(ctx, userID, sessionID)
	if err != nil {
		return store.Message{}, store.Session{}, err
	}
	if !canWrite(sp.Perm) {
		return store.Message{}, store.Session{}, ErrForbidden
	}

	text, err := normalizeMessage(content)
	if err != nil {
		return store.Message{}, store.Session{}, err
	}

	msg, err := s.store.CreateMessage(ctx, sessionID, "user", text)
	if err != nil {
		return store.Message{}, store.Session{}, err
	}
	if err := s.store.TouchSession(ctx, sessionID); err != nil {
		s.log.Warn("更新会话活跃时间失败", "session_id", sessionID, "err", err)
	}

	now := time.Now()
	sess.LastMessageAt = &now

	// 首条消息决定会话名：命名完成后 title_auto 置为 false，后续消息不再覆盖
	if sess.TitleAuto {
		if title := summarize(text); title != "" {
			if err := s.store.RenameSession(ctx, sessionID, title, false); err != nil {
				s.log.Warn("会话标题自动生成失败", "session_id", sessionID, "err", err)
			} else {
				sess.Title = title
				sess.TitleAuto = false
			}
		}
	}

	// 回读一次，保证返回的 updated_at / last_message_at 与实际一致
	if latest, err := s.store.GetSession(ctx, sessionID); err == nil {
		sess = latest
	}
	return msg, sess, nil
}

// load 读取会话并校验当前用户对所属空间的可见性。
func (s *Service) load(ctx context.Context, userID, sessionID string) (store.Session, store.Space, error) {
	sess, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return store.Session{}, store.Space{}, err
	}

	sp, err := s.spaces.Get(ctx, userID, sess.SpaceID)
	if err != nil {
		if errors.Is(err, space.ErrForbidden) {
			return store.Session{}, store.Space{}, ErrForbidden
		}
		return store.Session{}, store.Space{}, err
	}
	return sess, sp, nil
}

func normalizeMessage(raw string) (string, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return "", ErrMessageEmpty
	}
	if utf8.RuneCountInString(text) > maxMessageRunes {
		return "", ErrMessageTooLong
	}
	return text, nil
}

func normalizeTitle(raw string) (string, error) {
	title := strings.TrimSpace(raw)
	if title == "" {
		return defaultTitle, nil
	}
	if utf8.RuneCountInString(title) > maxTitleRunes || strings.ContainsAny(title, "\r\n\t") {
		return "", ErrTitleInvalid
	}
	return title, nil
}

// summarize 把首条消息压成标题：合并空白，截断到固定字数。
func summarize(text string) string {
	flat := strings.Join(strings.Fields(text), " ")
	runes := []rune(flat)
	if len(runes) <= autoTitleRunes {
		return flat
	}
	return string(runes[:autoTitleRunes]) + "…"
}

func canWrite(perm string) bool {
	return perm == "write" || perm == "manage"
}
