package main

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
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
	tableRefPattern = regexp.MustCompile(`(?i)\b(?:from|join)\s+([a-zA-Z0-9_."]+)`)
	limitPattern    = regexp.MustCompile(`(?i)\blimit\s+(\d+)`)
	writePattern    = regexp.MustCompile(`(?i)^\s*(insert|update|delete|drop|create|truncate|alter|merge)\b`)
)

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

	cols, rows, execErr := executeMockSQL(s.sqlOf(id))
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

// executeMockSQL 根据 SQL 里引用的表生成稳定的 mock 结果。
//
// 规则（覆盖智能体的正常与异常路径）：
//   - 写操作直接失败，提示只读账号（真实数仓的分析账号也是这样）；
//   - SQL 里出现 error / fail / boom 时失败，便于测试错误分支；
//   - from 引用了不存在的表时失败，报 Table or view not found；
//   - 其他情况按表列返回样例行，行数取 LIMIT，缺省 5 行。
func executeMockSQL(sql string) ([]string, []map[string]any, error) {
	trimmed := strings.TrimSpace(sql)
	if trimmed == "" {
		return nil, nil, fmt.Errorf("SQL 不能为空")
	}
	if writePattern.MatchString(trimmed) {
		return nil, nil, fmt.Errorf("当前账号为只读账号，不允许执行 DDL/DML，请只提交 SELECT 查询")
	}
	lower := strings.ToLower(trimmed)
	if strings.Contains(lower, "fail") || strings.Contains(lower, "error") || strings.Contains(lower, "boom") {
		return nil, nil, fmt.Errorf("执行失败：mock 服务命中故意失败的关键字（fail/error/boom）")
	}

	tableName := ""
	for _, m := range tableRefPattern.FindAllStringSubmatch(trimmed, -1) {
		if len(m) > 1 {
			tableName = strings.Trim(m[1], `"`)
			break
		}
	}

	limit := 5
	if m := limitPattern.FindStringSubmatch(trimmed); len(m) > 1 {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > maxResultRows {
		limit = maxResultRows
	}

	if tableName == "" {
		// select 1 / select current_date 这类无源查询
		return []string{"result"}, []map[string]any{{"result": 1}}, nil
	}

	table := findTable(tableName)
	if table == nil {
		return nil, nil, fmt.Errorf("Table or view not found: %s", tableName)
	}

	cols := make([]string, 0, len(table.Columns))
	for _, c := range table.Columns {
		cols = append(cols, c.Name)
	}
	if len(table.rows) == 0 {
		return cols, nil, nil
	}

	// 样例行循环填充到目标行数。故意不改写任何字段值：
	// 跨表关联（user_id / series_id）在 mock 里必须自洽，
	// 否则智能体做 join 时会拿到对不上的数据。
	rows := make([]map[string]any, 0, limit)
	for i := 0; i < limit; i++ {
		src := table.rows[i%len(table.rows)]
		row := make(map[string]any, len(cols))
		for k, v := range src {
			row[k] = v
		}
		rows = append(rows, row)
	}
	return cols, rows, nil
}
