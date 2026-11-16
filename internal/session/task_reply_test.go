package session

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/workagent/auto-agent/internal/space"
	"github.com/workagent/auto-agent/internal/storage/localfs"
	"github.com/workagent/auto-agent/internal/store"
)

// fakeReplyStore 只实现回执路径会用到的几个方法，其余走嵌入接口（测试里不会调到）。
type fakeReplyStore struct {
	store.Store
	sess     store.Session
	sp       store.Space
	perm     string
	task     store.Task
	taskErr  error
	updated  []store.Task
	messages []store.Message
	// active 为 true 时父会话被当作「有 run 在跑」，只落消息、不起 goroutine
	active bool
}

func (f *fakeReplyStore) GetSession(context.Context, string) (store.Session, error) {
	return f.sess, nil
}

func (f *fakeReplyStore) GetSpace(context.Context, string) (store.Space, error) {
	return f.sp, nil
}

func (f *fakeReplyStore) SpacePerm(context.Context, string, string) (string, error) {
	return f.perm, nil
}

func (f *fakeReplyStore) GetTaskByChild(context.Context, string) (store.Task, error) {
	return f.task, f.taskErr
}

func (f *fakeReplyStore) UpdateTask(_ context.Context, t store.Task) error {
	f.updated = append(f.updated, t)
	return nil
}

func (f *fakeReplyStore) CreateMessageFrom(_ context.Context, sessionID, role, content, source string) (store.Message, error) {
	f.messages = append(f.messages, store.Message{SessionID: sessionID, Role: role, Content: content, Source: source})
	return store.Message{}, nil
}

func (f *fakeReplyStore) GetActiveRun(context.Context, string, time.Duration) (store.Run, bool, error) {
	return store.Run{}, f.active, nil
}

func newReplyService(t *testing.T, st store.Store) *Service {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}
	return &Service{
		store:       st,
		spaces:      space.NewService(st, fs, log, nil),
		log:         log,
		autoBudget:  newAutoRunBudget(),
		idleTimeout: time.Minute,
		runBudget:   time.Minute,
	}
}

func waitingTask(status store.TaskStatus) store.Task {
	return store.Task{
		ID:              "t1",
		ParentSessionID: "parent",
		ChildSessionID:  "child",
		Title:           "确认指标口径",
		Assignee:        "u1",
		Status:          status,
	}
}

// 回执只能由被派发人在任务子会话里提交，且必须带确认过的结论。
func TestReplyTaskGuards(t *testing.T) {
	ctx := context.Background()
	base := func() *fakeReplyStore {
		return &fakeReplyStore{
			sess: store.Session{ID: "child", SpaceID: "sp1", CreatedBy: "u1"},
			sp:   store.Space{ID: "sp1", Type: "personal"},
			perm: "manage",
			task: waitingTask(store.TaskWaitingExternal),
		}
	}

	t.Run("不是任务会话", func(t *testing.T) {
		st := base()
		st.taskErr = store.ErrNotFound
		_, err := newReplyService(t, st).ReplyTask(ctx, "u1", "child", "结论")
		if err == nil || !strings.Contains(err.Error(), "不是派发出来的任务会话") {
			t.Fatalf("应提示不是任务会话，实际: %v", err)
		}
	})

	t.Run("已经回执过", func(t *testing.T) {
		st := base()
		st.task = waitingTask(store.TaskSucceeded)
		_, err := newReplyService(t, st).ReplyTask(ctx, "u1", "child", "结论")
		if err == nil || !strings.Contains(err.Error(), "已经回执过") {
			t.Fatalf("应拒绝重复回执，实际: %v", err)
		}
	})

	t.Run("空结论", func(t *testing.T) {
		st := base()
		_, err := newReplyService(t, st).ReplyTask(ctx, "u1", "child", "  ")
		if err == nil || !strings.Contains(err.Error(), "回执内容不能为空") {
			t.Fatalf("应拒绝空内容，实际: %v", err)
		}
	})

	t.Run("不是被派发人", func(t *testing.T) {
		st := base()
		_, err := newReplyService(t, st).ReplyTask(ctx, "u2", "child", "结论")
		if !errors.Is(err, ErrForbidden) {
			t.Fatalf("非被派发人应被拒绝，实际: %v", err)
		}
	})
}

// 确认后的回执要把结论写进任务、并作为 task_result 消息送回父会话。
func TestReplyTaskDelivers(t *testing.T) {
	ctx := context.Background()
	st := &fakeReplyStore{
		sess:   store.Session{ID: "child", SpaceID: "sp1", CreatedBy: "u1"},
		sp:     store.Space{ID: "sp1", Type: "personal"},
		perm:   "manage",
		task:   waitingTask(store.TaskWaitingExternal),
		active: true, // 父会话有 run 在跑：不触发新 run，测试不需要等 goroutine
	}

	info, err := newReplyService(t, st).ReplyTask(ctx, "u1", "child", "口径按 v2 表计算")
	if err != nil {
		t.Fatalf("回执失败: %v", err)
	}
	if info.Status != string(store.TaskSucceeded) {
		t.Fatalf("任务应置为 succeeded，实际: %s", info.Status)
	}
	if len(st.updated) != 1 || st.updated[0].Result != "口径按 v2 表计算" {
		t.Fatalf("任务结果没有落库: %+v", st.updated)
	}
	if len(st.messages) != 1 {
		t.Fatalf("父会话应收到一条回执，实际 %d 条", len(st.messages))
	}
	msg := st.messages[0]
	if msg.SessionID != "parent" || msg.Source != "task_result" ||
		!strings.Contains(msg.Content, "【任务回执】确认指标口径") ||
		!strings.Contains(msg.Content, "口径按 v2 表计算") {
		t.Fatalf("回执消息不符合预期: %+v", msg)
	}
}
