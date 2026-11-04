package platform

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// SSE 长连接的时限策略（业内通行取值，改动前先看这里）：
//
//   - 服务端不设全局 WriteTimeout（见 cmd/server/main.go）。Go 的 WriteTimeout 是「整条响应」的
//     绝对写截止时间，无法按路由豁免，会把流式长连接按秒掐断，表现是「回答到一半停住、无任何报错」。
//     写下限由本类型在每次写前刷新，超时即熔断。
//   - 心跳间隔必须小于链路上最短的空闲超时：nginx `proxy_read_timeout` 与主流云 LB 默认都是 60s，
//     所以取 15s（经验规则：最短空闲超时的 1/4 ~ 1/3）。
//   - 单次写入阻塞容忍 30s：慢客户端不能占住 goroutine 与模型配额。
//   - 流的生命周期上限由应用自己定（run 的墙钟预算 LLM_MAX_WALL_CLOCK），不依赖中间件的超时。
//
// 注意：心跳只能重置「空闲超时」，对「整响应总超时」无效（如 GCP 外部 ALB 默认 30s、
// Envoy route 默认 15s），那类计时器只能改配置。另外首字节也要尽早发出：
// Cloudflare 等 CDN 有约 100s 的首字节超时（报 524），模型 thinking 期间可能长时间没有增量。
const (
	// StreamHeartbeatInterval 是保活注释行的发送间隔。
	StreamHeartbeatInterval = 15 * time.Second
	// StreamWriteTimeout 是单次写入的阻塞容忍时间，超过按连接不可用处理。
	StreamWriteTimeout = 30 * time.Second
)

// ErrStreamBroken 表示连接已不可写（客户端断开或写阻塞超时），调用方应停止生成。
var ErrStreamBroken = errors.New("SSE 连接已断开")

// Stream 是 SSE 服务端推送器：统一响应头、首字节、心跳、写截止时间与并发写入。
//
// Send / Comment 可被多个 goroutine 调用（心跳与业务事件会并发），内部串行化；
// 一旦写入失败即进入破断状态，后续写入直接返回 ErrStreamBroken，调用方可据此停止生成。
type Stream struct {
	w  http.ResponseWriter
	rc *http.ResponseController

	mu     sync.Mutex
	broken error
	closed bool

	heartbeat time.Duration
	stopOnce  sync.Once
	stop      chan struct{}
	done      chan struct{}
}

// NewStream 构造推送器；Start 之前不会写出任何字节。
func NewStream(w http.ResponseWriter) *Stream {
	return &Stream{
		w:         w,
		rc:        http.NewResponseController(w),
		heartbeat: StreamHeartbeatInterval,
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// Start 写响应头与首个字节，并启动心跳。
func (s *Stream) Start() error {
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	// 关闭反向代理的响应缓冲，保证增量及时到达（nginx 认这个头，等价于 proxy_buffering off）
	h.Set("X-Accel-Buffering", "no")
	// 不设 Content-Length，也不设 Connection：HTTP/1.1 长连接是默认行为，
	// 而 Connection 在 HTTP/2 下属于非法头，会被某些栈直接拒绝
	s.w.WriteHeader(http.StatusOK)

	// 立刻吐出第一个字节，穿越 CDN / 代理的首字节超时
	if err := s.Comment("open"); err != nil {
		return err
	}
	go s.heartbeatLoop()
	return nil
}

// Send 推送一条具名事件。
func (s *Stream) Send(event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return s.write(fmt.Sprintf("event: %s\ndata: %s\n\n", event, data))
}

// Comment 推送一条注释行（客户端忽略），用于保活。
func (s *Stream) Comment(text string) error {
	return s.write(": " + text + "\n\n")
}

// Broken 返回连接是否已破断（nil 表示仍可写）。
func (s *Stream) Broken() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.broken
}

// Close 停止心跳并等待其退出；应在 handler 返回前调用。
func (s *Stream) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.stopHeartbeat()
	<-s.done
}

func (s *Stream) write(chunk string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broken != nil {
		return s.broken
	}
	if s.closed {
		return ErrStreamBroken
	}

	// 每次写前刷新写截止时间：全局 WriteTimeout 已置 0，写阻塞由这里熔断。
	// 部分 ResponseWriter（如单测 recorder）不支持，返回 ErrNotSupported，忽略即可。
	_ = s.rc.SetWriteDeadline(time.Now().Add(StreamWriteTimeout))

	if _, err := io.WriteString(s.w, chunk); err != nil {
		return s.failLocked(err)
	}
	if err := s.rc.Flush(); err != nil {
		return s.failLocked(err)
	}
	return nil
}

// failLocked 记录连接不可用并停止心跳；调用方需持有 s.mu。
func (s *Stream) failLocked(err error) error {
	s.broken = fmt.Errorf("%w: %v", ErrStreamBroken, err)
	s.stopHeartbeat()
	return s.broken
}

func (s *Stream) stopHeartbeat() {
	s.stopOnce.Do(func() { close(s.stop) })
}

func (s *Stream) heartbeatLoop() {
	defer close(s.done)
	ticker := time.NewTicker(s.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			if err := s.Comment("keep-alive " + time.Now().Format("15:04:05")); err != nil {
				return
			}
		}
	}
}
