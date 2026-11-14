package session

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/workagent/auto-agent/internal/agent"
	"github.com/workagent/auto-agent/internal/agentspec"
	"github.com/workagent/auto-agent/internal/platform"
	"github.com/workagent/auto-agent/internal/space"
	"github.com/workagent/auto-agent/internal/store"
	"github.com/workagent/auto-agent/internal/tool"
)

// UserResolver 从请求中解析当前登录用户。
type UserResolver func(r *http.Request) (store.User, error)

// Handler 提供会话 HTTP 端点。
// SSE 事件名统一在 agent 包定义，与方案 §7.3 一致；run 级事件由会话层补发。

type Handler struct {
	svc     *Service
	resolve UserResolver
	log     *slog.Logger
}

// NewHandler 构造 Handler。
func NewHandler(svc *Service, resolve UserResolver, log *slog.Logger) *Handler {
	return &Handler{svc: svc, resolve: resolve, log: log}
}

// Register 注册路由。
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /spaces/{id}/sessions", h.listBySpace)
	mux.HandleFunc("POST /spaces/{id}/sessions", h.create)
	// 会话创建时的智能体选择器：只返回清单，不含提示词正文
	mux.HandleFunc("GET /spaces/{id}/agents", h.listAgents)
	mux.HandleFunc("GET /sessions", h.listMine)
	mux.HandleFunc("PATCH /sessions/{id}", h.rename)
	mux.HandleFunc("DELETE /sessions/{id}", h.remove)
	mux.HandleFunc("GET /sessions/{id}/messages", h.messages)
	mux.HandleFunc("POST /sessions/{id}/messages", h.appendMessage)
	// 与 Agent 对话，流式返回（SSE）
	mux.HandleFunc("POST /sessions/{id}/chat", h.chat)
	// 绑定/清除会话技能：空的 path 表示清除
	mux.HandleFunc("PUT /sessions/{id}/skill", h.setSkill)
	// run 级端点：刷新后据此判断状态、续上与停止。断开连接不再等于停止（§15.12.1）。
	mux.HandleFunc("GET /runs/{id}", h.runState)
	mux.HandleFunc("GET /runs/{id}/stream", h.runStream)
	mux.HandleFunc("POST /runs/{id}/cancel", h.cancelRun)

	// 刷新后的第一问：这个会话现在有没有在跑的 run
	mux.HandleFunc("GET /sessions/{id}/active-run", h.activeRun)

	// 任务派发：列表、取消、催办（§15.12.4）
	mux.HandleFunc("GET /sessions/{id}/tasks", h.listTasks)
	mux.HandleFunc("POST /tasks/{id}/cancel", h.cancelTask)
	mux.HandleFunc("POST /tasks/{id}/remind", h.remindTask)

	// 排障只读链接：创建、列表、吊销；查看走公开路径，不需要登录（§15.12.3）
	mux.HandleFunc("POST /sessions/{id}/share", h.createShare)
	mux.HandleFunc("GET /sessions/{id}/share", h.listShares)
	mux.HandleFunc("DELETE /sessions/{id}/share/{shareID}", h.revokeShare)
	mux.HandleFunc("GET /share/{token}", h.viewShared)
}

type sessionDTO struct {
	ID      string `json:"id"`
	SpaceID string `json:"space_id"`
	// Dir 是会话所属目录（相对空间 files/），为空表示直接挂在空间根
	Dir       string `json:"dir,omitempty"`
	Title     string `json:"title"`
	TitleAuto bool   `json:"title_auto"`
	// SkillPath 是会话绑定的技能（SKILL.md 路径），空表示未绑定
	SkillPath string `json:"skill_path,omitempty"`
	// AgentID 是会话绑定的智能体，空表示未绑定
	AgentID string `json:"agent_id,omitempty"`
	// AgentVersion 是创建时绑定的版本
	AgentVersion  string  `json:"agent_version,omitempty"`
	Status        string  `json:"status"`
	LastMessageAt *string `json:"last_message_at"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
}

type messageDTO struct {
	ID        string          `json:"id"`
	SessionID string          `json:"session_id"`
	Role      string          `json:"role"`
	Content   string          `json:"content"`
	Reasoning string          `json:"reasoning,omitempty"`
	Parts     json.RawMessage `json:"parts"`
	// 产生这条助手消息的那一轮的 token 消耗与总耗时；用户消息为 0
	InTokens   int    `json:"in_tokens"`
	OutTokens  int    `json:"out_tokens"`
	DurationMS int    `json:"duration_ms"`
	// Streaming 表示还在生成中：刷新后据此渲染「生成中」骨架而不是空消息
	Streaming   bool   `json:"streaming,omitempty"`
	Interrupted bool   `json:"interrupted,omitempty"`
	// Source 区分用户输入、任务回执与外部回信，前端据此换一种样式
	Source    string `json:"source,omitempty"`
	CreatedAt string `json:"created_at"`
}

const timeLayout = "2006-01-02T15:04:05Z07:00"

func toDTO(s store.Session) sessionDTO {
	dto := sessionDTO{
		ID:           s.ID,
		SpaceID:      s.SpaceID,
		Dir:          s.Dir,
		Title:        s.Title,
		TitleAuto:    s.TitleAuto,
		SkillPath:    s.SkillPath,
		AgentID:      s.AgentID,
		AgentVersion: s.AgentVersion,
		Status:       s.Status,
		CreatedAt:    s.CreatedAt.Format(timeLayout),
		UpdatedAt:    s.UpdatedAt.Format(timeLayout),
	}
	if s.LastMessageAt != nil {
		v := s.LastMessageAt.Format(timeLayout)
		dto.LastMessageAt = &v
	}
	return dto
}

func toMessageDTO(m store.Message) messageDTO {
	parts := json.RawMessage(m.Parts)
	if len(parts) == 0 {
		parts = json.RawMessage("[]")
	}
	return messageDTO{
		ID:         m.ID,
		SessionID:  m.SessionID,
		Role:       m.Role,
		Content:    m.Content,
		Reasoning:  m.Reasoning,
		Parts:      parts,
		InTokens:    m.Stats.InTokens,
		OutTokens:   m.Stats.OutTokens,
		DurationMS:  m.Stats.DurationMS,
		Streaming:   m.Streaming,
		Interrupted: m.Interrupted,
		Source:      m.Source,
		CreatedAt:   m.CreatedAt.In(time.Local).Format(timeLayout),
	}
}

func (h *Handler) listBySpace(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	list, err := h.svc.List(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"sessions": sessionDTOs(list)})
}

func (h *Handler) listMine(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	list, err := h.svc.ListMine(r.Context(), user.ID)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"sessions": sessionDTOs(list)})
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}

	var req struct {
		Title string `json:"title"`
		// Dir 是空间内目录路径（相对 files/），为空表示挂在空间根
		Dir string `json:"dir"`
		// AgentID 是 agents/ 里的智能体 id，空表示用默认 base
		AgentID string `json:"agent_id"`
	}
	if r.ContentLength != 0 {
		if err := platform.DecodeJSON(r, &req); err != nil {
			platform.WriteError(w, r, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
			return
		}
	}

	sess, err := h.svc.Create(r.Context(), user.ID, r.PathValue("id"), req.Title, req.Dir, req.AgentID)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusCreated, map[string]any{"session": toDTO(sess)})
}

// listAgents 返回当前空间可用的智能体清单（只含元信息，不含提示词）。
func (h *Handler) listAgents(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	list, err := h.svc.Agents(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	if list == nil {
		list = []agentspec.Summary{}
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"agents": list})
}

func (h *Handler) rename(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}

	var req struct {
		Title string `json:"title"`
	}
	if err := platform.DecodeJSON(r, &req); err != nil {
		platform.WriteError(w, r, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
		return
	}

	sess, err := h.svc.Rename(r.Context(), user.ID, r.PathValue("id"), req.Title)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"session": toDTO(sess)})
}

// setSkill 绑定会话技能；path 为空表示清除。
func (h *Handler) setSkill(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}

	var req struct {
		Path string `json:"path"`
	}
	if err := platform.DecodeJSON(r, &req); err != nil {
		platform.WriteError(w, r, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
		return
	}

	sess, err := h.svc.SetSkill(r.Context(), user.ID, r.PathValue("id"), req.Path)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"session": toDTO(sess)})
}

func (h *Handler) remove(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), user.ID, r.PathValue("id")); err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) messages(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	list, err := h.svc.Messages(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}

	out := make([]messageDTO, 0, len(list))
	for _, m := range list {
		out = append(out, toMessageDTO(m))
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"messages": out})
}

func (h *Handler) appendMessage(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}

	var req struct {
		Content string `json:"content"`
	}
	if err := platform.DecodeJSON(r, &req); err != nil {
		platform.WriteError(w, r, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
		return
	}

	msg, sess, err := h.svc.AppendMessage(r.Context(), user.ID, r.PathValue("id"), req.Content)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusCreated, map[string]any{
		"message": toMessageDTO(msg),
		"session": toDTO(sess),
	})
}

// chat 与 Agent 对话：先做常规校验（错误以 JSON 返回），再切换为 SSE 流式输出。
func (h *Handler) chat(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}

	var req struct {
		Content string `json:"content"`
		// References 是用户在图里勾选的引用文件（相对空间 files/ 的路径）。
		// 只传路径，不传正文：是否读、读多少由 Agent 按任务决定。
		References []string `json:"references"`
	}
	if err := platform.DecodeJSON(r, &req); err != nil {
		platform.WriteError(w, r, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
		return
	}

	sessionID := r.PathValue("id")
	refs, err := h.svc.ResolveReferences(r.Context(), user.ID, sessionID, req.References)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	// 引用清单拼在正文末尾一起落库：历史回放与后续轮次都能看到「参考了哪些文件」，
	// 前端按标记把清单切出来渲染成文件标签。
	content := ComposeContent(req.Content, refs)
	if _, _, err := h.svc.ValidateChat(r.Context(), user.ID, sessionID, content); err != nil {
		h.writeErr(w, r, err)
		return
	}

	// 不支持 flush 的 ResponseWriter 做不了 SSE，在写响应头之前返回统一错误
	if _, ok := w.(http.Flusher); !ok {
		platform.WriteError(w, r, http.StatusInternalServerError, "stream_unsupported", "当前服务不支持流式响应")
		return
	}

	// 响应头、首字节、心跳与写截止时间都交给推送器（§7.3 时限约定）
	stream := platform.NewStream(w)
	if err := stream.Start(); err != nil {
		// 响应头已发出，状态码无法回改，只能记录后结束
		h.log.Warn("SSE 初始化失败",
			"session_id", sessionID, "request_id", platform.RequestIDFrom(r.Context()), "err", err)
		return
	}
	defer stream.Close()

	emit := func(ev agent.Event, offset int) error {
		return sendRunEvent(stream, ev, offset)
	}

	out, err := h.svc.Chat(r.Context(), user.ID, sessionID, content, emit)
	if err != nil {
		switch {
		case r.Context().Err() != nil:
			// 客户端已断开（用户点了停止或关掉页面）：没有接收方，事件写不出去；
			// 已生成的部分已落库，中断原因由会话层记录
			return
		case out.Status == store.RunCancelled:
			// 连接仍然可用（到点或预算用尽，而不是用户断开）：补一个可解释的收尾
			_ = stream.Send(agent.EventRunFinished, map[string]any{
				"run_id": out.RunID, "status": out.Status, "reason": out.Reason,
				"error": userFacingCancelError(err),
			})
			return
		}

		h.log.Warn("对话失败", "session_id", sessionID, "run_id", out.RunID, "err", err)
		// 失败也以 run.finished 收尾，保证一次 run 的事件流始终有终点
		payload := map[string]any{"status": "failed", "error": userFacingAgentError(err)}
		if out.RunID != "" {
			payload["run_id"] = out.RunID
		}
		_ = stream.Send(agent.EventRunFinished, payload)
		return
	}

	_ = stream.Send(agent.EventMessageDone, map[string]any{
		"role": "assistant",
		"usage": map[string]int{
			"in":  out.Usage.InputTokens,
			"out": out.Usage.OutputTokens,
		},
		"message": toMessageDTO(out.Message),
		"session": toDTO(out.Session),
	})
	finished := map[string]any{"run_id": out.RunID, "status": out.Status}
	if out.Reason != "" {
		finished["reason"] = out.Reason
	}
	_ = stream.Send(agent.EventRunFinished, finished)
}

// sendRunEvent 把一条 run 事件写成 SSE。
//
// chat 与 /runs/{id}/stream 共用同一份实现：前端重连后收到的载荷形状与首次
// 一致，不需要区分数据来自哪条链路。offset 是这条事件之后的累计字符数，
// 客户端把它带回 after_offset 即可精确续传。
func sendRunEvent(stream *platform.Stream, ev agent.Event, offset int) error {
	switch ev.Type {
	case agent.EventRunStarted:
		return stream.Send(ev.Type, map[string]any{"run_id": ev.RunID, "offset": offset})
	case agent.EventMessageDelta:
		// 思考过程与正文共用 message.delta，用字段区分，保持会话顺序不乱
		payload := map[string]any{"role": ev.Role, "offset": offset}
		if ev.Reasoning {
			payload["reasoning"] = ev.Delta
		} else {
			payload["content"] = ev.Delta
		}
		return stream.Send(ev.Type, payload)
	case agent.EventToolCall, agent.EventToolResult:
		payload := ev.Tool.Payload()
		payload["offset"] = offset
		return stream.Send(ev.Type, payload)
	case agent.EventApprovalReq:
		return stream.Send(ev.Type, ev.Approval)
	default:
		return nil
	}
}

type runDTO struct {
	ID        string          `json:"id"`
	SessionID string          `json:"session_id"`
	Status    string          `json:"status"`
	Provider  string          `json:"provider,omitempty"`
	Model     string          `json:"model,omitempty"`
	Error     string          `json:"error,omitempty"`
	Usage     json.RawMessage `json:"usage"`
	StartedAt string          `json:"started_at"`
	EndedAt   *string         `json:"ended_at"`
}

func toRunDTO(r store.Run) runDTO {
	usage := json.RawMessage(r.Usage)
	if len(usage) == 0 {
		usage = json.RawMessage("{}")
	}
	dto := runDTO{
		ID: r.ID, SessionID: r.SessionID, Status: r.Status,
		Provider: r.Provider, Model: r.Model, Error: r.Error,
		Usage:     usage,
		StartedAt: r.StartedAt.In(time.Local).Format(timeLayout),
	}
	if r.EndedAt != nil {
		v := r.EndedAt.In(time.Local).Format(timeLayout)
		dto.EndedAt = &v
	}
	return dto
}

// runState 返回一次执行的状态：刷新后先问「还在不在跑」，再决定是否续流。
func (h *Handler) runState(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	run, err := h.svc.RunState(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, toRunDTO(run))
}

// cancelRun 是唯一能停掉一轮执行的入口。
func (h *Handler) cancelRun(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	runID := r.PathValue("id")
	if err := h.svc.CancelRun(r.Context(), user.ID, runID); err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]string{"run_id": runID, "status": store.RunCancelled})
}

// runStream 续传一次执行的事件流：先补发缓冲，再跟着直播走。
//
// 客户端断开只结束这一次订阅，run 继续跑（§15.12.1）；再次连上来时
// 用最后一帧的 offset 作为 after_offset，就能从断开处接着往下看。
func (h *Handler) runStream(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	runID := r.PathValue("id")
	after, _ := strconv.Atoi(r.URL.Query().Get("after_offset"))
	if after < 0 {
		after = 0
	}

	feed, err := h.svc.SubscribeRun(r.Context(), user.ID, runID, after)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	defer feed.Close()

	stream := platform.NewStream(w)
	if err := stream.Start(); err != nil {
		h.log.Warn("SSE 初始化失败",
			"run_id", runID, "request_id", platform.RequestIDFrom(r.Context()), "err", err)
		return
	}
	defer stream.Close()

	for _, f := range feed.Backlog {
		if err := sendRunEvent(stream, f.Event, f.Offset); err != nil {
			return
		}
	}
	if feed.Frames == nil {
		// run 已经不在本进程：让客户端去取状态与消息，而不是干等一条不会再来的流
		_ = stream.Send(agent.EventRunFinished, map[string]any{"run_id": runID, "detached": true})
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-feed.Done:
			return
		case f := <-feed.Frames:
			if err := sendRunEvent(stream, f.Event, f.Offset); err != nil {
				return
			}
		}
	}
}

// activeRun 回答「这个会话现在有没有在跑」——刷新后的第一问。
func (h *Handler) activeRun(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	run, found, err := h.svc.ActiveRun(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	if !found {
		platform.WriteJSON(w, http.StatusOK, map[string]any{"active": false})
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"active": true, "run": toRunDTO(run)})
}

type taskDTO struct {
	ID            string  `json:"id"`
	Title         string  `json:"title"`
	Assignee      string  `json:"assignee"`
	AssigneeName  string  `json:"assignee_name,omitempty"`
	Status        string  `json:"status"`
	WaitingSince  *string `json:"waiting_since"`
	DispatchCount int     `json:"dispatch_count"`
	Result        string  `json:"result,omitempty"`
}

func toTaskDTO(t tool.TaskInfo) taskDTO {
	dto := taskDTO{
		ID: t.ID, Title: t.Title, Assignee: t.Assignee, AssigneeName: t.AssigneeName,
		Status: t.Status, DispatchCount: t.DispatchCount, Result: t.Result,
	}
	if t.WaitingSince != nil {
		v := t.WaitingSince.In(time.Local).Format(timeLayout)
		dto.WaitingSince = &v
	}
	return dto
}

func (h *Handler) listTasks(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	tasks, err := h.svc.Tasks(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	out := make([]taskDTO, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, toTaskDTO(t))
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"tasks": out})
}

func (h *Handler) cancelTask(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	if err := h.svc.CancelTask(r.Context(), user.ID, r.PathValue("id")); err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]string{"status": string(store.TaskCancelled)})
}

// remindTask 是「再次咨询」：向同一子会话催办，不新建任务。
func (h *Handler) remindTask(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	task, err := h.svc.RemindTask(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, toTaskDTO(task))
}

type shareDTO struct {
	ID        string  `json:"id"`
	ExpiresAt string  `json:"expires_at"`
	RevokedAt *string `json:"revoked_at"`
	ViewCount int     `json:"view_count"`
	CreatedAt string  `json:"created_at"`
	// Token 只在创建时返回一次，列表里永远为空
	Token string `json:"token,omitempty"`
	URL   string `json:"url,omitempty"`
}

func toShareDTO(s store.Share) shareDTO {
	dto := shareDTO{
		ID:        s.ID,
		ExpiresAt: s.ExpiresAt.In(time.Local).Format(timeLayout),
		ViewCount: s.ViewCount,
		CreatedAt: s.CreatedAt.In(time.Local).Format(timeLayout),
	}
	if s.RevokedAt != nil {
		v := s.RevokedAt.In(time.Local).Format(timeLayout)
		dto.RevokedAt = &v
	}
	return dto
}

// createShare 生成排障只读链接：默认 24h，最多 7 天，可随时吊销。
func (h *Handler) createShare(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	var in struct {
		TTLHours int `json:"ttl_hours"`
	}
	if r.ContentLength > 0 {
		if err := platform.DecodeJSON(r, &in); err != nil {
			h.writeErr(w, r, err)
			return
		}
	}
	share, token, err := h.svc.CreateShare(r.Context(), user.ID, r.PathValue("id"),
		time.Duration(in.TTLHours)*time.Hour)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	dto := toShareDTO(share)
	dto.Token = token
	dto.URL = "/share/" + token
	platform.WriteJSON(w, http.StatusOK, dto)
}

func (h *Handler) listShares(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	shares, err := h.svc.Shares(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	out := make([]shareDTO, 0, len(shares))
	for _, s := range shares {
		out = append(out, toShareDTO(s))
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"shares": out})
}

func (h *Handler) revokeShare(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	if err := h.svc.RevokeShare(r.Context(), user.ID, r.PathValue("id"), r.PathValue("shareID")); err != nil {
		h.writeErr(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// viewShared 是唯一不要求登录的读取路径：拿着用户授权的链接看一段对话。
//
// 不返回任何可操作能力（不能发消息、不能跑 run），也不返回会话所属空间等元数据，
// 只给「这段对话长什么样」，够运维定位问题即可。
func (h *Handler) viewShared(w http.ResponseWriter, r *http.Request) {
	sess, msgs, err := h.svc.ViewShared(r.Context(), r.PathValue("token"),
		clientIP(r), r.UserAgent())
	if err != nil {
		if errors.Is(err, ErrShareGone) {
			platform.WriteError(w, r, http.StatusNotFound, "share_gone", "链接无效或已过期")
			return
		}
		platform.WriteError(w, r, http.StatusInternalServerError, "internal", "读取失败")
		return
	}

	out := make([]messageDTO, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, toMessageDTO(m))
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{
		"title":      sess.Title,
		"created_at": sess.CreatedAt.In(time.Local).Format(timeLayout),
		"messages":   out,
		"read_only":  true,
	})
}

// clientIP 取来源地址：代理场景优先用 X-Forwarded-For 的第一跳。
// 它只进审计，不参与任何鉴权判断——来源 IP 不是可信身份。
func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.IndexByte(v, ','); i > 0 {
			v = v[:i]
		}
		return strings.TrimSpace(v)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// userFacingCancelError 说明一次中断的原因，避免前端把它当成「失败，请重试」。
// 预算用尽由实现给出具体说明（如「本轮对话超过时间预算（10m0s）」），其余给通用文案。
func userFacingCancelError(err error) string {
	// 预算与停滞都由实现给出了具体原因（「已用完 8 轮工具调用」「连续 2 轮没有新信息」），
	// 直接透传比换成一句通用文案更有用：用户据此决定是改问法还是换个做法。
	if errors.Is(err, agent.ErrBudgetExceeded) || errors.Is(err, agent.ErrStalled) {
		return err.Error()
	}
	return RunBudgetExceededMessage
}

// timeoutError 识别上游超时：http.Client 的 Timeout 会包成实现了 Timeout() 的错误。
type timeoutError interface{ Timeout() bool }

// userFacingAgentError 把内部错误转成可读文案，避免把密钥等细节暴露给前端。
func userFacingAgentError(err error) string {
	var timeout timeoutError
	switch {
	case errors.As(err, &timeout) && timeout.Timeout(), errors.Is(err, context.DeadlineExceeded):
		return "模型响应超时，可调大 LLM_TIMEOUT 或把问题拆小后重试"
	case errors.Is(err, ErrAgentUnavailable):
		return err.Error()
	case errors.Is(err, ErrForbidden):
		return "没有该空间的权限"
	default:
		return "生成回复失败：" + err.Error()
	}
}

func sessionDTOs(list []store.Session) []sessionDTO {
	out := make([]sessionDTO, 0, len(list))
	for _, s := range list {
		out = append(out, toDTO(s))
	}
	return out
}

func (h *Handler) current(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	user, err := h.resolve(r)
	if err != nil {
		platform.WriteError(w, r, http.StatusUnauthorized, "unauthenticated", "未登录或登录已过期")
		return store.User{}, false
	}
	return user, true
}

func (h *Handler) writeErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		platform.WriteError(w, r, http.StatusNotFound, "not_found", "会话或空间不存在")
	case errors.Is(err, space.ErrForbidden), errors.Is(err, ErrForbidden):
		platform.WriteError(w, r, http.StatusForbidden, "forbidden", "没有该空间的权限")
	case errors.Is(err, ErrLastSession):
		platform.WriteError(w, r, http.StatusConflict, "last_session", err.Error())
	case errors.Is(err, ErrRunFinished):
		platform.WriteError(w, r, http.StatusConflict, "run_finished", err.Error())
	case errors.Is(err, ErrAgentUnavailable):
		platform.WriteError(w, r, http.StatusServiceUnavailable, "agent_unavailable", err.Error())
	case errors.Is(err, space.ErrDirNotFound):
		platform.WriteError(w, r, http.StatusNotFound, "dir_not_found", "目录不存在，可能已被删除")
	case errors.Is(err, ErrTitleInvalid), errors.Is(err, ErrMessageEmpty), errors.Is(err, ErrMessageTooLong),
		errors.Is(err, space.ErrNameInvalid), errors.Is(err, space.ErrDirTooDeep):
		platform.WriteError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		h.log.Error("会话接口失败", "err", err, "request_id", platform.RequestIDFrom(r.Context()))
		platform.WriteError(w, r, http.StatusInternalServerError, "internal_error", "操作失败，请重试")
	}
}
