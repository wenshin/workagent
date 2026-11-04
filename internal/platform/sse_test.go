package platform

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// waitForLine 在超时前等待满足条件的行，避免测试依赖固定 sleep。
func waitForLine(t *testing.T, lines <-chan string, pred func(string) bool, timeout time.Duration) bool {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				return false
			}
			if pred(line) {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

func streamLines(resp *http.Response) <-chan string {
	lines := make(chan string, 128)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	return lines
}

// TestStreamStartSendsOpenEventAndHeartbeat 覆盖开流首字节（穿越首字节超时）与 15s 心跳的机制。
func TestStreamStartSendsOpenEventAndHeartbeat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := NewStream(w)
		s.heartbeat = 20 * time.Millisecond // 测试用短心跳
		if err := s.Start(); err != nil {
			t.Errorf("Start失败：%v", err)
			return
		}
		defer s.Close()
		if err := s.Send("run.started", map[string]string{"run_id": "r1"}); err != nil {
			t.Errorf("Send失败：%v", err)
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type 应为 text/event-stream，实际 %q", ct)
	}
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering 应为 no，实际 %q", got)
	}
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "no-cache") {
		t.Fatalf("Cache-Control 应含 no-cache，实际 %q", got)
	}

	lines := streamLines(resp)
	// 首个字节必须是注释行：早于任何事件，用于穿越 CDN 的首字节超时
	first, ok := <-lines
	if !ok {
		t.Fatal("流未返回任何内容")
	}
	if first != ": open" {
		t.Fatalf("首行应为 \": open\"，实际 %q", first)
	}
	if !waitForLine(t, lines, func(l string) bool { return l == "event: run.started" }, time.Second) {
		t.Fatal("未收到 run.started 事件")
	}
	if !waitForLine(t, lines, func(l string) bool { return l == `data: {"run_id":"r1"}` }, time.Second) {
		t.Fatal("run.started 载荷不正确")
	}
	if !waitForLine(t, lines, func(l string) bool { return strings.HasPrefix(l, ": keep-alive") }, 2*time.Second) {
		t.Fatal("未收到心跳注释行，空闲超时无法被重置")
	}
}

// TestStreamRejectsWriteAfterClose 覆盖收尾：Close 之后不得再写，避免 handler 返回后仍往连接里写。
func TestStreamRejectsWriteAfterClose(t *testing.T) {
	sendErr := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := NewStream(w)
		s.heartbeat = 10 * time.Millisecond
		if err := s.Start(); err != nil {
			sendErr <- err
			return
		}
		s.Close()
		sendErr <- s.Send("run.finished", map[string]string{"status": "succeeded"})
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	defer resp.Body.Close()

	select {
	case err := <-sendErr:
		if !errors.Is(err, ErrStreamBroken) {
			t.Fatalf("Close 之后写入应返回 ErrStreamBroken，实际 %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close 之后写入没有返回")
	}
}

// TestStreamSendFailsAfterClientDisconnect 覆盖客户端断开的探测：写失败要让调用方停下来，
// 而不是继续生成（这是「用户点停止后仍在烧 token」的根因）。
func TestStreamSendFailsAfterClientDisconnect(t *testing.T) {
	sendErr := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := NewStream(w)
		s.heartbeat = time.Hour // 本用例只观察业务写入
		if err := s.Start(); err != nil {
			sendErr <- err
			return
		}
		defer s.Close()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if err := s.Comment("tick"); err != nil {
				sendErr <- err
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		sendErr <- errors.New("客户端已断开，但写入始终成功")
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	if _, err := io.ReadAtLeast(resp.Body, make([]byte, 1), 1); err != nil {
		t.Fatalf("读取首个字节失败：%v", err)
	}
	_ = resp.Body.Close() // 模拟客户端断开

	select {
	case err := <-sendErr:
		if !errors.Is(err, ErrStreamBroken) {
			t.Fatalf("客户端断开后写入应返回 ErrStreamBroken，实际 %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("客户端断开后未检测到写失败")
	}
}
