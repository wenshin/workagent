package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/workagent/auto-agent/internal/store"
	"github.com/workagent/auto-agent/internal/tool"
)

const (
	// maxActiveTasks 是每会话同时未完成的异步任务上限。
	// 同步任务不占这个额度——它们仍然串行执行（§15.12.1）。
	maxActiveTasks = 50
	// autoRunPerMinute 限制「回执触发的 run」的密度：
	// 十个回执同时到达时不该催出十轮模型调用，剩下的留在消息里等下一轮一起读。
	autoRunPerMinute = 3
)

// ErrTaskQuota 表示会话内未完成的派发任务已经到上限。
var ErrTaskQuota = errors.New("当前会话未完成的任务太多，请先处理已有任务")

// autoRunBudget 是「系统消息触发的 run」的每分钟配额，纯内存、按会话计。
type autoRunBudget struct {
	mu sync.Mutex
	m  map[string][]time.Time
}

func newAutoRunBudget() *autoRunBudget {
	return &autoRunBudget{m: map[string][]time.Time{}}
}

func (b *autoRunBudget) allow(sessionID string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	cutoff := now.Add(-time.Minute)
	kept := b.m[sessionID][:0]
	for _, t := range b.m[sessionID] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= autoRunPerMinute {
		b.m[sessionID] = kept
		return false
	}
	b.m[sessionID] = append(kept, now)
	return true
}

// Dispatch 把一件事派发给子会话：子会话归属被派发人，父会话正常结束、继续可用。
//
// 只有会话创建者能派发；被派发人必须已经是空间成员——否则他连子会话都打不开，
// 「拉个人进空间」这种事不该由模型的一次工具调用悄悄完成。
func (s *Service) Dispatch(ctx context.Context, userID, sessionID string, in tool.TaskDispatchInput) (tool.TaskInfo, error) {
	sess, sp, err := s.load(ctx, userID, sessionID)
	if err != nil {
		return tool.TaskInfo{}, err
	}
	if !canWrite(sp.Perm) {
		return tool.TaskInfo{}, ErrForbidden
	}

	title := strings.TrimSpace(in.Title)
	if title == "" {
		return tool.TaskInfo{}, errors.New("缺少任务标题")
	}
	assignee, err := s.resolveAssignee(ctx, in.Assignee)
	if err != nil {
		return tool.TaskInfo{}, err
	}
	if assignee.ID == userID {
		return tool.TaskInfo{}, errors.New("不能把任务派发给自己：直接在当前会话里完成即可")
	}
	if perm, err := s.store.SpacePerm(ctx, sess.SpaceID, assignee.ID); err == nil && perm == "" {
		return tool.TaskInfo{}, fmt.Errorf("%s 不是本空间成员，无法接收任务", assignee.Name)
	}

	existing, err := s.store.ListTasks(ctx, sessionID)
	if err != nil {
		return tool.TaskInfo{}, err
	}
	active := 0
	for _, t := range existing {
		switch t.Status {
		case store.TaskPending, store.TaskRunning, store.TaskWaitingExternal:
			active++
		}
	}
	if active >= maxActiveTasks {
		return tool.TaskInfo{}, ErrTaskQuota
	}

	brief := strings.TrimSpace(in.Brief)
	child, err := s.store.CreateSession(ctx, sess.SpaceID, assignee.ID,
		truncate(title, 60), sess.Dir, sess.AgentID, sess.AgentVersion, sess.AgentPrompt)
	if err != nil {
		return tool.TaskInfo{}, err
	}

	// 子会话的第一条消息就是任务说明：被派发人打开就知道要做什么、回什么。
	if brief != "" {
		if _, err := s.store.CreateMessageFrom(ctx, child.ID, "user", brief, "task_brief"); err != nil {
			s.log.Warn("写入任务说明失败", "task_session", child.ID, "err", err)
		}
	}

	now := time.Now()
	task, err := s.store.CreateTask(ctx, store.Task{
		SpaceID:         sess.SpaceID,
		ParentSessionID: sessionID,
		ChildSessionID:  child.ID,
		Title:           title,
		Brief:           brief,
		Assignee:        assignee.ID,
		Status:          store.TaskWaitingExternal,
		WaitingSince:    &now,
		DispatchCount:   1,
		CreatedBy:       userID,
	})
	if err != nil {
		return tool.TaskInfo{}, err
	}

	s.log.Info("任务已派发",
		"task_id", task.ID, "parent", sessionID, "child", child.ID,
		"assignee", assignee.ID, "by", userID)

	return toTaskInfo(task, assignee.Name), nil
}

// Tasks 返回会话派发出的任务列表。
func (s *Service) Tasks(ctx context.Context, userID, sessionID string) ([]tool.TaskInfo, error) {
	if _, _, err := s.load(ctx, userID, sessionID); err != nil {
		return nil, err
	}
	tasks, err := s.store.ListTasks(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	out := make([]tool.TaskInfo, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, toTaskInfo(t, ""))
	}
	return out, nil
}

// CancelTask 取消一个派发任务：父会话与子会话都能调，只有参与方可以。
func (s *Service) CancelTask(ctx context.Context, userID, taskID string) error {
	task, err := s.store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	// 参与方校验：派发人（父会话创建者）或被派发人都能取消
	if task.CreatedBy != userID && task.Assignee != userID {
		return ErrForbidden
	}
	if task.Status == store.TaskSucceeded || task.Status == store.TaskCancelled || task.Status == store.TaskFailed {
		return ErrRunFinished
	}
	task.Status = store.TaskCancelled
	if err := s.store.UpdateTask(ctx, task); err != nil {
		return err
	}
	// 在子会话里留个记号：被派发人再打开时不会以为还需要处理
	if task.ChildSessionID != "" {
		_, _ = s.store.CreateMessageFrom(ctx, task.ChildSessionID, "system",
			"任务已被发起人取消，无需再回复。", "task_result")
	}
	return nil
}

// RemindTask 是「再次咨询」：向同一子会话再催一次，不新建会话。
func (s *Service) RemindTask(ctx context.Context, userID, taskID string) (tool.TaskInfo, error) {
	task, err := s.store.GetTask(ctx, taskID)
	if err != nil {
		return tool.TaskInfo{}, err
	}
	if task.CreatedBy != userID {
		return tool.TaskInfo{}, ErrForbidden
	}
	switch task.Status {
	case store.TaskSucceeded, store.TaskCancelled, store.TaskFailed:
		return tool.TaskInfo{}, ErrRunFinished
	}

	task.DispatchCount++
	if err := s.store.UpdateTask(ctx, task); err != nil {
		return tool.TaskInfo{}, err
	}
	if task.ChildSessionID != "" {
		_, _ = s.store.CreateMessageFrom(ctx, task.ChildSessionID, "user",
			fmt.Sprintf("（第 %d 次催办）请尽快回复上面的任务；如果已经不需要了，请直接说明。", task.DispatchCount),
			"task_result")
	}
	return toTaskInfo(task, ""), nil
}

// completeParentTask 在子会话跑完一轮后把结果回填父会话。
//
// 回填后自动起一轮 run，否则派发方不再说话就永远不知道结果；
// 但密度受限：一分钟内最多触发 3 次，其余的回执留在消息里等下一轮一起读。
func (s *Service) completeParentTask(ctx context.Context, childSessionID, content string) {
	if s.store == nil || strings.TrimSpace(content) == "" {
		return
	}
	task, err := s.store.GetTaskByChild(ctx, childSessionID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Warn("反查派发任务失败", "session_id", childSessionID, "err", err)
		}
		return
	}
	if task.Status == store.TaskSucceeded || task.Status == store.TaskCancelled {
		return
	}

	task.Status = store.TaskSucceeded
	task.Result = content
	if err := s.store.UpdateTask(ctx, task); err != nil {
		s.log.Warn("更新任务状态失败", "task_id", task.ID, "err", err)
	}

	notice := fmt.Sprintf("【任务回执】%s\n\n%s", task.Title, content)
	// 先落消息：即使这一轮 run 因为配额没起，用户刷新也能看到回执
	if _, err := s.store.CreateMessageFrom(ctx, task.ParentSessionID, "user", notice, "task_result"); err != nil {
		s.log.Warn("回执写入父会话失败", "task_id", task.ID, "err", err)
		return
	}

	// 父会话已经有 run 在跑：只排队，不打断（同会话串行）
	if _, active, err := s.store.GetActiveRun(ctx, task.ParentSessionID); err == nil && active {
		return
	}
	if !s.autoBudget.allow(task.ParentSessionID, time.Now()) {
		return
	}

	go func() {
		runCtx, cancel := context.WithTimeout(context.Background(), s.runBudget)
		defer cancel()
		if _, err := s.chat(runCtx, task.CreatedBy, task.ParentSessionID, notice,
			chatOptions{source: "task_result", skipUser: true}, nil); err != nil {
			s.log.Warn("回执触发父会话失败", "task_id", task.ID, "err", err)
		}
	}()
}

// resolveAssignee 把模型给的「人」解析成用户：先按 id，再按工号，最后按姓名/邮箱模糊匹配。
func (s *Service) resolveAssignee(ctx context.Context, raw string) (store.User, error) {
	key := strings.TrimSpace(raw)
	if key == "" {
		return store.User{}, errors.New("缺少被派发人")
	}
	if u, err := s.store.GetUser(ctx, key); err == nil {
		return u, nil
	}
	if u, err := s.store.FindUserByJobNumber(ctx, key); err == nil {
		return u, nil
	}
	users, err := s.store.SearchUsers(ctx, key, 5)
	if err != nil {
		return store.User{}, err
	}
	switch len(users) {
	case 0:
		return store.User{}, fmt.Errorf("找不到用户 %q，请改用工号或完整姓名", raw)
	case 1:
		return users[0], nil
	default:
		names := make([]string, 0, len(users))
		for _, u := range users {
			names = append(names, u.Name+"("+u.JobNumber+")")
		}
		return store.User{}, fmt.Errorf("「%s」匹配到多个人：%s，请用工号指定", raw, strings.Join(names, "、"))
	}
}

func toTaskInfo(t store.Task, assigneeName string) tool.TaskInfo {
	return tool.TaskInfo{
		ID:            t.ID,
		Title:         t.Title,
		Assignee:      t.Assignee,
		AssigneeName:  assigneeName,
		Status:        string(t.Status),
		WaitingSince:  t.WaitingSince,
		DispatchCount: t.DispatchCount,
		Result:        t.Result,
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
