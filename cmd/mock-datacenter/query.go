package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// 任务状态机：pending -> running -> succeeded | failed。
const (
	statusPending   = "pending"
	statusRunning   = "running"
	statusSucceeded = "succeeded"
	statusFailed    = "failed"
)

// maxResultRows 限制单任务结果行数，避免下载接口被拿来灌数据。
const maxResultRows = 200

var (
	writePattern = regexp.MustCompile(`(?i)^\s*(insert|update|delete|drop|create|truncate|alter|merge)\b`)
	fromPattern  = regexp.MustCompile(`(?i)\bfrom\b`)
)

// findTable 按表名解析：支持 t / db.t，也接受反引号与大小写差异。
func findTable(name string) *Table {
	raw := strings.Trim(strings.TrimSpace(name), "`\"")
	short := raw
	if i := strings.LastIndex(raw, "."); i >= 0 {
		short = raw[i+1:]
	}
	for _, t := range mockTables() {
		if strings.EqualFold(t.Name, short) || strings.EqualFold(t.FullName, raw) {
			return t
		}
	}
	return nil
}

// Task 是一次 Spark SQL 提交。
type Task struct {
	ID         string
	Engine     string
	SQL        string
	Status     string
	Progress   int
	Message    string
	Columns    []string
	Rows       []map[string]any
	RowCount   int
	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time
}

// snapshot 返回一份拷贝，避免并发读写。
func (t *Task) snapshot() Task {
	cp := *t
	cp.Columns = append([]string(nil), t.Columns...)
	cp.Rows = append([]map[string]any(nil), t.Rows...)
	return cp
}

func (t *Task) done() bool {
	return t.Status == statusSucceeded || t.Status == statusFailed
}

// taskStore 是内存任务表：mock 服务不落盘，进程重启即丢。
type taskStore struct {
	mu    sync.RWMutex
	tasks map[string]*Task
	delay time.Duration
	seq   int
}

func newTaskStore(delay time.Duration) *taskStore {
	return &taskStore{tasks: make(map[string]*Task), delay: delay}
}

// submit 创建任务并异步执行，立即返回任务快照。
func (s *taskStore) submit(sql, engine string) Task {
	if engine == "" {
		engine = "spark"
	}
	s.mu.Lock()
	s.seq++
	task := &Task{
		ID:        fmt.Sprintf("spark-%d-%04d", time.Now().Unix(), s.seq%10000),
		Engine:    engine,
		SQL:       sql,
		Status:    statusPending,
		Progress:  0,
		CreatedAt: time.Now(),
	}
	s.tasks[task.ID] = task
	s.mu.Unlock()

	go s.run(task.ID)
	return task.snapshot()
}

// run 模拟 Spark 提交：pending -> running(进度递增) -> 成功/失败。
func (s *taskStore) run(id string) {
	s.patch(id, func(t *Task) {
		t.Status = statusRunning
		t.Progress = 10
		t.StartedAt = time.Now()
	})

	for _, p := range []int{35, 60} {
		time.Sleep(s.delay / 3)
		s.patch(id, func(t *Task) { t.Progress = p })
	}
	time.Sleep(s.delay / 3)

	cols, rows, execErr := executeSQL(s.sqlOf(id))
	s.patch(id, func(t *Task) {
		t.FinishedAt = time.Now()
		t.Progress = 100
		if execErr != nil {
			t.Status = statusFailed
			t.Message = execErr.Error()
			return
		}
		t.Status = statusSucceeded
		t.Columns = cols
		t.Rows = rows
		t.RowCount = len(rows)
		t.Message = fmt.Sprintf("任务执行成功，返回 %d 行", len(rows))
	})
}

func (s *taskStore) sqlOf(id string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if t, ok := s.tasks[id]; ok {
		return t.SQL
	}
	return ""
}

func (s *taskStore) patch(id string, fn func(*Task)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.tasks[id]; ok {
		fn(t)
	}
}

// get 取任务快照。
func (s *taskStore) get(id string) (Task, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tasks[id]
	if !ok {
		return Task{}, false
	}
	return t.snapshot(), true
}

// wait 阻塞等待任务结束或超时，返回最新快照。
func (s *taskStore) wait(ctx context.Context, id string, timeout time.Duration) (Task, bool) {
	deadline := time.Now().Add(timeout)
	for {
		t, ok := s.get(id)
		if !ok {
			return Task{}, false
		}
		if t.done() || timeout <= 0 || time.Now().After(deadline) {
			return t, true
		}
		select {
		case <-ctx.Done():
			return t, true
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// 单任务默认返回行数：不写 limit 时按这个值截断，避免一次拉回整个表。
const defaultResultRows = 50
