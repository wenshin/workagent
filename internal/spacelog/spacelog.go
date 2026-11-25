// Package spacelog 记录空间内的运行日志：插件调用与 shell 命令。
//
// 位置是空间文件区的隐藏目录（files/.logs/），在 spacefs 里登记为平台保留项，
// 所以目录树、file_*、上传下载、wiki 统统看不到也读不到——暂不对用户与
// Agent 开放，先在后台把「刚才那次为什么失败」这件事留证。
//
// 按天分文件：当天写 <yyyy-mm-dd>.log（JSONL，一行一条），跨天后把历史文件
// 压成 <yyyy-mm-dd>.log.gz，并按保留期删除。压缩在写入路径上惰性触发，
// 每个空间每天最多扫一次目录，不额外起定时任务——日志很少时也无需付出
// 定时器与全量扫描的成本。
package spacelog

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/workagent/auto-agent/internal/storage"
)

const (
	// Dir 是空间文件区里存放运行日志的隐藏目录（files/.logs）。
	Dir = ".logs"
	// filesDir 与 space 包的 dirsRoot 一致；用字面量避免包间循环依赖。
	filesDir = "files"

	activeSuffix  = ".log"
	archiveSuffix = ".log.gz"
	dateLayout    = "2006-01-02"

	// DefaultMaxFileBytes 是单日文件上限。
	//
	// 有上限才敢说「日志不会吃满空间」：调用元数据一天几 MB 已经足够排障，
	// 再多也不会有新的结论，只会让配额与备份一起变大。
	DefaultMaxFileBytes = 2 << 20
	// DefaultDetailBytes 是单条正文的上限。
	DefaultDetailBytes = 2 << 10
	// maxCompressBytes 是压缩时读取的上限：防止历史遗留的超大文件把内存吃满。
	maxCompressBytes = 8 << 20
	// compactTimeout 是压缩一次的成本上限。
	compactTimeout = 2 * time.Minute
)

// Entry 是一条运行日志。
//
// 字段按「事后能不能定位」取舍：谁、在哪次对话、调了什么、成没成、多久、
// 输出尾巴是什么。不记录参数全文（可能含用户数据），shell 的命令除外——
// 命令本身就是排障的第一手材料。
type Entry struct {
	SpaceID string
	// Kind 取 plugin / shell。
	Kind string
	// Tool 是插件 slug 或内置工具名。
	Tool string
	// Mode 区分同一插件的执行形态（如 dry_run 试跑）；正式调用为空。
	Mode string
	// Status 取 ok / error / timeout / exit / denied / log。
	Status  string
	Level   string
	Latency time.Duration

	SessionID string
	RunID     string
	UserID    string

	// Command 仅 shell：原始命令，调用方负责脱敏与截断。
	Command string
	// ExitCode 仅 shell：非零即失败。
	ExitCode int
	// Output 是诊断正文：shell 的输出、插件的错误摘要或 wa.log 的内容。
	Output string
}

// Options 是空间日志的参数。
type Options struct {
	// RetainDays 是压缩后保留的天数，<=0 表示永久保留。
	RetainDays int
	// MaxFileBytes 是单日文件上限，<=0 用 DefaultMaxFileBytes。
	MaxFileBytes int64
	// DetailBytes 是单条正文上限，<=0 用 DefaultDetailBytes。
	DetailBytes int
	// Logger 用于报告日志写入自身的失败；nil 用 slog.Default()。
	Logger *slog.Logger
	// Now 仅测试用；nil 用 time.Now。
	Now func() time.Time
}

// Logger 是空间运行日志的写入端。
//
// 零值不可用；New 之后的实例并发安全，可作为单例注入各执行路径。
type Logger struct {
	st  storage.Storage
	opt Options
	log *slog.Logger

	mu         sync.Mutex
	compacted  map[string]string // spaceID → 已压缩到的日期
	compacting map[string]bool   // spaceID 是否正在压缩
	limitWarn  map[string]bool   // 超限告警只报一次，按 空间|日 记
}

// New 构造写入端。storage 为空时返回 nil，调用方按「没有日志」处理即可。
func New(st storage.Storage, opt Options) *Logger {
	if st == nil {
		return nil
	}
	if opt.MaxFileBytes <= 0 {
		opt.MaxFileBytes = DefaultMaxFileBytes
	}
	if opt.DetailBytes <= 0 {
		opt.DetailBytes = DefaultDetailBytes
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Logger{
		st:         st,
		opt:        opt,
		log:        opt.Logger,
		compacted:  map[string]string{},
		compacting: map[string]bool{},
		limitWarn:  map[string]bool{},
	}
}

// Write 追加一条日志。
//
// 永远不返回错误：日志是旁路，写不进去也不能让工具调用或插件执行失败。
// 失败只告警一次，避免存储故障时把服务日志刷满。
func (l *Logger) Write(ctx context.Context, e Entry) {
	if l == nil || l.st == nil || e.SpaceID == "" {
		return
	}
	now := l.opt.Now()
	day := now.Format(dateLayout)
	key := path.Join(l.dir(e.SpaceID), day+activeSuffix)

	// 调用方的超时/取消不该带走这条日志：它记录的恰恰是那次失败。
	ctx = context.WithoutCancel(ctx)

	if err := l.append(ctx, key, l.encode(e, now)); err != nil {
		l.log.Warn("写空间运行日志失败", "space", e.SpaceID, "key", key, "err", err)
		return
	}
	l.compactAsync(e.SpaceID, day)
}

// Compact 同步压缩某个空间的历史日志，并清理过期归档。
//
// today 是「当前日期」（yyyy-mm-dd），当天文件不压缩——它还在被追加。
// Write 内部异步调用它；导出是为了测试与将来的运维入口。
func (l *Logger) Compact(ctx context.Context, spaceID, today string) error {
	if l == nil || l.st == nil {
		return nil
	}
	now, err := time.Parse(dateLayout, today)
	if err != nil {
		return fmt.Errorf("非法日期 %q: %w", today, err)
	}
	dir := l.dir(spaceID)
	entries, err := l.st.List(ctx, dir)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		return err
	}

	var firstErr error
	for _, e := range entries {
		name := path.Base(e.Key)
		switch {
		case strings.HasSuffix(name, activeSuffix):
			if strings.TrimSuffix(name, activeSuffix) == today {
				continue
			}
			if err := l.compress(ctx, dir, name); err != nil && firstErr == nil {
				firstErr = err
			}
		case strings.HasSuffix(name, archiveSuffix):
			if l.opt.RetainDays <= 0 {
				continue
			}
			day, err := time.Parse(dateLayout, strings.TrimSuffix(name, archiveSuffix))
			if err != nil {
				continue
			}
			// 保留完整的 RetainDays 天：today-30 当天仍在，再往前一天才删。
			if !day.AddDate(0, 0, l.opt.RetainDays).Before(now) {
				continue
			}
			if err := l.st.Delete(ctx, path.Join(dir, name)); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// compactAsync 每个空间每天只触发一次扫描。
func (l *Logger) compactAsync(spaceID, day string) {
	l.mu.Lock()
	if l.compacted[spaceID] == day || l.compacting[spaceID] {
		l.mu.Unlock()
		return
	}
	l.compacting[spaceID] = true
	l.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), compactTimeout)
		defer cancel()
		if err := l.Compact(ctx, spaceID, day); err != nil {
			l.log.Warn("压缩空间运行日志失败", "space", spaceID, "day", day, "err", err)
		}
		l.mu.Lock()
		l.compacting[spaceID] = false
		l.compacted[spaceID] = day
		l.mu.Unlock()
	}()
}

func (l *Logger) append(ctx context.Context, key string, line []byte) error {
	// 按「写完还不超」判断，上限才是真上限。第一条总是写进去：
	// 单条就超过上限时留空文件没有意义，而「日志一条都没有」更难排查。
	if e, err := l.st.Stat(ctx, key); err == nil && e.Size > 0 &&
		e.Size+int64(len(line)) > l.opt.MaxFileBytes {
		l.limitWarnOnce(key, e.Size)
		return nil
	}
	if app, ok := l.st.(storage.Appender); ok {
		_, err := app.Append(ctx, key, bytes.NewReader(line))
		return err
	}
	return l.appendByRewrite(ctx, key, line)
}

// appendByRewrite 是没有追加能力时的降级：读出来、接上、整份写回。
//
// 只对不超过上限的文件做，否则读的是几十 MB 的历史，代价远大于这条日志的价值。
func (l *Logger) appendByRewrite(ctx context.Context, key string, line []byte) error {
	var buf bytes.Buffer
	rc, err := l.st.Get(ctx, key)
	switch {
	case err == nil:
		defer rc.Close()
		if _, err := io.Copy(&buf, io.LimitReader(rc, l.opt.MaxFileBytes)); err != nil {
			return err
		}
	case !errors.Is(err, storage.ErrNotFound):
		return err
	}
	buf.Write(line)
	_, err = l.st.Put(ctx, key, bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	return err
}

func (l *Logger) limitWarnOnce(key string, size int64) {
	l.mu.Lock()
	seen := l.limitWarn[key]
	l.limitWarn[key] = true
	l.mu.Unlock()
	if !seen {
		l.log.Warn("空间运行日志达到单日上限，后写入的记录被丢弃",
			"key", key, "size", size, "limit", l.opt.MaxFileBytes)
	}
}

func (l *Logger) dir(spaceID string) string {
	return path.Join(l.st.SpaceKey(spaceID), filesDir, Dir)
}

// record 是落盘的一行。
type record struct {
	TS        string `json:"ts"`
	Kind      string `json:"kind"`
	Tool      string `json:"tool,omitempty"`
	Mode      string `json:"mode,omitempty"`
	Status    string `json:"status,omitempty"`
	Level     string `json:"level,omitempty"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
	SessionID string `json:"session,omitempty"`
	RunID     string `json:"run,omitempty"`
	UserID    string `json:"user,omitempty"`
	Command   string `json:"cmd,omitempty"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	Output    string `json:"output,omitempty"`
}

func (l *Logger) encode(e Entry, now time.Time) []byte {
	r := record{
		TS:        now.Format("2006-01-02T15:04:05.000Z07:00"),
		Kind:      e.Kind,
		Tool:      clip(e.Tool, 128),
		Mode:      e.Mode,
		Status:    e.Status,
		Level:     clip(e.Level, 16),
		LatencyMS: e.Latency.Milliseconds(),
		SessionID: e.SessionID,
		RunID:     e.RunID,
		UserID:    e.UserID,
		Command:   clip(e.Command, l.opt.DetailBytes),
		// 0 也是有效退出码，用指针区分「没有这个字段」。
		ExitCode: exitCodePtr(e),
		// 保留尾部：错误与失败的堆栈总在最后。
		Output: tail(e.Output, l.opt.DetailBytes),
	}
	b, err := json.Marshal(r)
	if err != nil {
		// 字段都是字符串与整数，实际到不了这里；保底写一条说明而不是丢日志。
		b = []byte(`{"kind":"log","status":"encode_failed"}`)
	}
	return append(b, '\n')
}

func exitCodePtr(e Entry) *int {
	if e.Kind != "shell" {
		return nil
	}
	v := e.ExitCode
	return &v
}

// compress 把一份明文日志压成 .log.gz：先写新文件再删源，
// 中途失败最多留下两份，不会丢内容。
func (l *Logger) compress(ctx context.Context, dir, name string) error {
	src := path.Join(dir, name)
	rc, err := l.st.Get(ctx, src)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		return err
	}
	defer rc.Close()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := io.Copy(gz, io.LimitReader(rc, maxCompressBytes)); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}

	dst := path.Join(dir, strings.TrimSuffix(name, activeSuffix)+archiveSuffix)
	if _, err := l.st.Put(ctx, dst, bytes.NewReader(buf.Bytes()), int64(buf.Len())); err != nil {
		return err
	}
	return l.st.Delete(ctx, src)
}

// clip 按字节从头截断（命令与标签用）。
func clip(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// tail 保留末尾 n 字节，且不从一个多字节字符中间开始。
func tail(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	i := 0
	for i < 3 && i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return "…" + s[i:]
}
