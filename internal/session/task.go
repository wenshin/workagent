package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/workagent/auto-agent/internal/store"
	"github.com/workagent/auto-agent/internal/tool"
)

const (
	// maxActiveTasks 是每会话同时未完成的异步任务上限。
	// 同步任务不占这个额度——它们仍然串行执行（§15.12.1）。
	maxActiveTasks = 50
	// baseAgentID 是平台默认智能体：派发出去的会话用它，不继承发起方的智能体。
	// B 要的是一个通用助理帮他把话说清楚，A 那个特定角色的行为设定对他没有意义；
	// A 的上下文在回执回到父会话时自然恢复，两边各用各的。
	baseAgentID = "base"
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
// 子会话**建在被派发人的个人空间**，不建在父会话所在空间：他可能根本无权进入那个
// 空间，而派发只是「请他做件事」，不该顺带给他开一片空间的读权限（那是可见的权限
// 变更，不该由一次工具调用悄悄完成）。个人空间他天然 manage，任务一落地就看得见。
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
	// 标题会直接成为对方会话列表里的会话名：「任务」「咨询」这种名字堆几条就分不清了。
	// 这里只卡下限，具体写什么由模型决定。
	if utf8.RuneCountInString(title) < 4 {
		return tool.TaskInfo{}, errors.New("任务标题太短，对方会话列表里会分不清：请写成包含对象和事项的具体标题")
	}
	assignee, err := s.resolveAssignee(ctx, in.Assignee)
	if err != nil {
		return tool.TaskInfo{}, err
	}
	if assignee.ID == userID {
		return tool.TaskInfo{}, errors.New("不能把任务派发给自己：直接在当前会话里完成即可")
	}
	// 被派发人可能从没登录过平台：先把个人空间备好，否则子会话无处安放
	personalSpaceID, err := s.ensurePersonalSpace(ctx, assignee.ID)
	if err != nil {
		return tool.TaskInfo{}, fmt.Errorf("为 %s 准备个人空间失败：%w", assignee.Name, err)
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
			// 同一个被派发人 + 同一个标题 + 还没结束 = 几乎必然是重复派发。
			// 只拦这一种确定的重复，把「到底是不是同一件事」的判断交回模型：
			// 真要是另一件事，让它把 title 写具体，重复自然消失。
			if t.Assignee == assignee.ID && strings.TrimSpace(t.Title) == title {
				return tool.TaskInfo{}, fmt.Errorf(
					"已经给 %s 派过同名任务（task_id=%s，状态=%s）。同一件事请用 task_remind 催办；"+
						"如果确实是另一件事，请把 title 写得更具体以便区分",
					assignee.Name, t.ID, t.Status)
			}
		}
	}
	if active >= maxActiveTasks {
		return tool.TaskInfo{}, ErrTaskQuota
	}

	brief := strings.TrimSpace(in.Brief)
	// dir 传空：父会话的目录是相对父空间 files/ 的路径，跨到个人空间后没有意义。
	// 智能体不继承：用平台默认的通用助理，提示词与版本都留空，由运行时按 B 的空间解析。
	// titleAuto=false：会话名就是任务内容，不能被对方回的第一句「好的」改掉。
	child, err := s.store.CreateSessionTitled(ctx, personalSpaceID, assignee.ID,
		truncate(title, 60), "", baseAgentID, "", "", false)
	if err != nil {
		return tool.TaskInfo{}, err
	}

	// 子会话的第一条消息就是任务说明：被派发人打开就知道是谁派的、要做什么、回什么。
	if brief != "" {
		from := userID
		if u, err := s.store.GetUser(ctx, userID); err == nil {
			from = u.Name
		}
		intro := fmt.Sprintf("%s 通过智能体派了个任务给你，直接在本会话回复即可，结果会自动回传给他。", from)
		if _, err := s.store.CreateMessageFrom(ctx, child.ID, "user", intro+"\n\n"+brief, "task_brief"); err != nil {
			s.log.Warn("写入任务说明失败", "task_session", child.ID, "err", err)
		}
	}

	now := time.Now()
	task, err := s.store.CreateTask(ctx, store.Task{
		// space_id 记的是「任务从哪个空间派出」；子会话在别人的个人空间，
		// 需要它的空间时用 child_session_id 反查，不要用这一列。
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
		from := task.CreatedBy
		if u, err := s.store.GetUser(ctx, task.CreatedBy); err == nil {
			from = u.Name
		}
		// 这条消息由平台代发起人写入：正文里必须点名，否则 B 的助理会把它
		// 当成 B 自己说的话，上下文就串了。
		_, _ = s.store.CreateMessageFrom(ctx, task.ChildSessionID, "user",
			fmt.Sprintf("【催办】%s 想确认上面这件事的进展（第 %d 次）。方便的话请尽快回复；"+
				"如果已经不需要了，直接说一声即可。", from, task.DispatchCount),
			"task_remind")
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

// ensurePersonalSpace 返回用户的个人空间 ID，没有就现建一个。
//
// 与登录共用 store.EnsurePersonalSpace：内部按 user_id 取 advisory lock，
// 并发派发两次也只会建出一个空间。被派发人没用过平台时，他的个人空间
// 就在这里被建出来——这是派发能覆盖「同事还没上过平台」的前提。
func (s *Service) ensurePersonalSpace(ctx context.Context, userID string) (string, error) {
	spaces, _, err := s.store.EnsurePersonalSpace(ctx, userID)
	if err != nil {
		return "", err
	}
	for _, sp := range spaces {
		if sp.Type == "personal" && sp.OwnerID == userID && sp.ParentID == "" {
			return sp.ID, nil
		}
	}
	return "", errors.New("个人空间不可用（可能已停用），请联系管理员")
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
