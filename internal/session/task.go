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
	// baseAgentID 是平台默认智能体：父会话没绑定时，派发出去的会话用它。
	// 绑定了就继承「slug + 来源空间」，子会话能拿到同一套设定与 requires 里的能力。
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

	// 分段内容：任一段有值就能派发，全空说明模型什么也没交代，对方打开只会看到一句「找你确认」。
	background := strings.TrimSpace(in.Background)
	done := strings.TrimSpace(in.Done)
	brief := strings.TrimSpace(in.Brief)
	if background == "" && done == "" && len(normalizeChecks(in.Checks)) == 0 && brief == "" {
		return tool.TaskInfo{}, errors.New("缺少任务说明：background / done / checks 至少写一项，对方看不到本空间文件")
	}
	// dir 传空：父会话的目录是相对父空间 files/ 的路径，跨到个人空间后没有意义。
	// titleAuto=false：会话名就是任务内容，不能被对方回的第一句「好的」改掉。
	//
	// 智能体继承父会话的绑定：子会话里记下来源空间，运行时按「发起方空间 + slug」
	// 解析；提示词与版本留空，让智能体更新后子任务也能跟上。发起方的智能体不可用
	// （被停用/删掉）时回落到通用助理，不能因为一个可选绑定让派发失败。
	agentID, agentSpaceID, agentName := s.sessionAgent(ctx, sess)
	child, err := s.store.CreateSessionTitled(ctx, personalSpaceID, assignee.ID,
		truncate(title, 60), "", agentSpaceID, agentID, "", "", false)
	if err != nil {
		return tool.TaskInfo{}, err
	}

	// 子会话的第一条消息就是任务说明：被派发人打开就知道是谁派的、要做什么、回什么。
	// 话术以智能体为主语、按「需求 / 已做 / 待确认 / 确认后」分段，而不是平台旁白加一段流水账。
	from := userID
	if u, err := s.store.GetUser(ctx, userID); err == nil {
		from = u.Name
	}
	if _, err := s.store.CreateMessageFrom(ctx, child.ID, "user",
		composeTaskBrief(in, agentName, from), "task_brief"); err != nil {
		s.log.Warn("写入任务说明失败", "task_session", child.ID, "err", err)
	}

	// 任务列表只展示一句摘要：brief 优先，没有就退到需求背景。
	storedBrief := brief
	if storedBrief == "" {
		storedBrief = background
	}
	now := time.Now()
	task, err := s.store.CreateTask(ctx, store.Task{
		// space_id 记的是「任务从哪个空间派出」；子会话在别人的个人空间，
		// 需要它的空间时用 child_session_id 反查，不要用这一列。
		SpaceID:         sess.SpaceID,
		ParentSessionID: sessionID,
		ChildSessionID:  child.ID,
		Title:           title,
		Brief:           storedBrief,
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
	// 名册在这里补齐：session_task 只存 assignee 的 user id，前端没有「按 id 查人」的接口，
	// 不补就只能显示一串 uuid（或退化成拿状态当名字用）。同一个人只查一次。
	names := make(map[string]string, len(tasks))
	out := make([]tool.TaskInfo, 0, len(tasks))
	for _, t := range tasks {
		name, ok := names[t.Assignee]
		if !ok {
			if u, err := s.store.GetUser(ctx, t.Assignee); err == nil {
				name = u.Name
			}
			names[t.Assignee] = name
		}
		out = append(out, toTaskInfo(t, name))
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

// sessionAgent 解析会话绑定的智能体，返回 slug、来源空间与显示名。
//
// 子会话（agent_space_id 指向发起方空间）再往下派发时，来源空间要跟着传，否则按会话
// 自己的空间解析必然找不到——链式派发的第三层最容易漏。智能体被停用/删除时回落 base：
// 一个可选绑定不该让派发失败。
func (s *Service) sessionAgent(ctx context.Context, sess store.Session) (id, spaceID, name string) {
	id = baseAgentID
	if sess.AgentID == "" || sess.AgentID == baseAgentID || s.agentPlugins == nil {
		return id, "", ""
	}
	spaceID = sess.SpaceID
	if sess.AgentSpaceID != "" {
		spaceID = sess.AgentSpaceID
	}
	r, err := s.agentPlugins.ResolveAgentPlugin(ctx, spaceID, sess.AgentID)
	if err != nil {
		s.log.Warn("派发时智能体不可用，回落到通用助理",
			"agent_id", sess.AgentID, "space_id", spaceID, "err", err)
		return id, "", ""
	}
	return sess.AgentID, spaceID, r.Name
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
		// 当成 B 自己说的话，上下文就串了。口吻与首条任务说明保持一致。
		text := fmt.Sprintf("【催办】%s 想确认上面这件事的进展（第 %d 次）。方便的话请尽快回复；"+
			"如果已经不需要了，直接说一声即可。", from, task.DispatchCount)
		if parent, err := s.store.GetSession(ctx, task.ParentSessionID); err == nil {
			if _, _, agentName := s.sessionAgent(ctx, parent); agentName != "" {
				text = fmt.Sprintf("【催办】你好，我是「%s」智能体，再来问一下上面这件事的进展（第 %d 次）。"+
					"方便的话请尽快回复；如果已经不需要了，直接说一声即可。",
					agentName, task.DispatchCount)
			}
		}
		_, _ = s.store.CreateMessageFrom(ctx, task.ChildSessionID, "user", text, "task_remind")
	}
	name := ""
	if u, err := s.store.GetUser(ctx, task.Assignee); err == nil {
		name = u.Name
	}
	return toTaskInfo(task, name), nil
}

// ReplyTask 是子会话对被派发任务的正式回执：被派发人确认结论后由 Agent 调用。
//
// 平台不再在每轮 run 结束时自动回填：那样「我先确认一下」「能再说说是哪个表吗」
// 这一轮也会被当成结果，发起方看到的状态永远是「已完成」，而实际对方只是在反问。
// 现在的口径是「用户确认结论 → 调一次 task_reply → 任务才成功」。
func (s *Service) ReplyTask(ctx context.Context, userID, sessionID, summary string) (tool.TaskInfo, error) {
	if _, _, err := s.load(ctx, userID, sessionID); err != nil {
		return tool.TaskInfo{}, err
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return tool.TaskInfo{}, errors.New("回执内容不能为空：请先总结结论")
	}

	task, err := s.store.GetTaskByChild(ctx, sessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return tool.TaskInfo{}, errors.New("当前会话不是派发出来的任务会话，不需要回执")
		}
		return tool.TaskInfo{}, err
	}
	if task.Assignee != userID {
		return tool.TaskInfo{}, ErrForbidden
	}
	switch task.Status {
	case store.TaskSucceeded:
		return tool.TaskInfo{}, errors.New("这个任务已经回执过，不要重复提交")
	case store.TaskCancelled, store.TaskFailed:
		return tool.TaskInfo{}, errors.New("任务已结束，不能再回执")
	}

	task.Status = store.TaskSucceeded
	task.Result = summary
	if err := s.store.UpdateTask(ctx, task); err != nil {
		return tool.TaskInfo{}, err
	}
	s.notifyParent(ctx, task, summary)
	return toTaskInfo(task, ""), nil
}

// notifyParent 把回执写回父会话，并按配额自动续一轮。
//
// 回填后自动起一轮 run，否则派发方不再说话就永远不知道结果；
// 但密度受限：一分钟内最多触发 3 次，其余的回执留在消息里等下一轮一起读。
func (s *Service) notifyParent(ctx context.Context, task store.Task, content string) {
	notice := fmt.Sprintf("【任务回执】%s\n\n%s", task.Title, content)
	// 先落消息：即使这一轮 run 因为配额没起，用户刷新也能看到回执
	if _, err := s.store.CreateMessageFrom(ctx, task.ParentSessionID, "user", notice, "task_result"); err != nil {
		s.log.Warn("回执写入父会话失败", "task_id", task.ID, "err", err)
		return
	}

	// 父会话已经有 run 在跑：只排队，不打断（同会话串行）
	if _, active, err := s.store.GetActiveRun(ctx, task.ParentSessionID, s.idleTimeout); err == nil && active {
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

const (
	// 单段正文上限：跨空间看不到对方文件，内容只能靠正文，但一条消息不该无限长。
	taskBriefPartLimit = 4000
	// 待确认项太多，对方反而不知道先看哪个。
	taskBriefCheckLimit = 8
	taskBriefCheckRunes = 200
)

// composeTaskBrief 拼出子会话的第一条消息：以智能体为主语，按「需求 / 已做 / 待确认 / 确认后」分段。
//
// 这是被派发人打开会话看到的第一段话，它决定对方把这件事当「系统通知」还是「有人找我办事」。
// 分段也让模型不必把所有内容挤进一段流水账：背景归背景，代码归代码，要确认的点单列。
func composeTaskBrief(in tool.TaskDispatchInput, agentName, from string) string {
	var b strings.Builder
	// 名称本身就常以「智能体」结尾（「大数据分析智能体」），再补一个后缀会变成
	// 「我是「大数据分析智能体」智能体」，反而像机器模板。
	if agentName != "" {
		b.WriteString("你好，我是「" + agentName + "」。")
	} else {
		b.WriteString("你好，我是" + from + " 的助理。")
	}
	if title := strings.TrimSpace(in.Title); title != "" {
		if agentName != "" {
			b.WriteString("\n我接到 " + from + " 的一个需求：" + title + "。")
		} else {
			b.WriteString("\n他委托我找你确认一件事：" + title + "。")
		}
	}
	if v := clipTaskPart(in.Background, taskBriefPartLimit); v != "" {
		b.WriteString("\n\n" + v)
	}
	if v := clipTaskPart(in.Done, taskBriefPartLimit); v != "" {
		b.WriteString("\n\n我这边已经做到：\n" + v)
	}
	if checks := normalizeChecks(in.Checks); len(checks) > 0 {
		b.WriteString("\n\n辛苦帮忙确认：")
		for i, c := range checks {
			fmt.Fprintf(&b, "\n%d. %s", i+1, c)
		}
	}
	if v := clipTaskPart(in.Brief, taskBriefPartLimit); v != "" {
		b.WriteString("\n\n" + v)
	}
	after := strings.TrimRight(strings.TrimSpace(in.After), "。.!！")
	if after == "" {
		after = "把结论带回发起方会话"
	}
	b.WriteString("\n\n确认后，我会" + after + "。")
	return b.String()
}

// normalizeChecks 去掉空条目并限量。
func normalizeChecks(items []string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		out = append(out, clipTaskPart(item, taskBriefCheckRunes))
		if len(out) >= taskBriefCheckLimit {
			break
		}
	}
	return out
}

// clipTaskPart 限长并明确告知已截断：悄悄截断会让对方以为原文就这么短。
func clipTaskPart(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\n……（内容较长，已截断）"
}
