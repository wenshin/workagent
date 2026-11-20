// Package remote 把 sandbox.Runner 搬到另一个进程里执行（执行容器）。
//
// 动机：容器化之后，平台进程与执行体不在同一个文件系统命名空间，但上层
// （shell_exec、脚本插件、搜索脚本）只认识 sandbox.Runner。这一层把 Run 变成
// 一次 HTTP 调用，其余代码一行不改。
//
// 传输选 HTTP+JSON 而不是 gRPC：Run 是「一个请求一个结果」，没有流式需求；
// Start（常驻进程）需要双向流，留到下一步做——现在显式返回「不支持」，
// 而不是静默降级成一次性进程，否则常驻插件的性能语义会悄悄消失。
package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/workagent/auto-agent/internal/sandbox"
)

const (
	// healthPath 不鉴权，给 k8s 探针用（httpGet 没法带 header）
	healthPath = "/healthz"
	// readyPath 鉴权，控制面握手走这条：同时证明「连得上」和「令牌对」
	readyPath = "/v1/ready"
	runPath   = "/v1/run"

	// 单请求上限：Stdin 走 JSON 的 base64，插件不会在这里传大文件
	// （文件是直连存储的），8MB 足够且能挡住误用。
	maxRequestBytes = 8 << 20
)

// Client 是远端执行体的 sandbox.Runner 实现。
type Client struct {
	base     string
	token    string
	backend  string
	isolated bool
	env      sandbox.EnvFacts
	http     *http.Client
}

// New 连接执行体并完成握手：拿不到真实的沙箱后端名就报错。
//
// 这里必须握手而不是假设「远端一定隔离」——执行容器里 bwrap 起不来时
// Detect 会降级到 local，控制面如果不知情，就会把「裸跑」记成 bwrap。
func New(addr, token string) (*Client, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("远端沙箱缺少鉴权令牌（SANDBOX_RUNNER_TOKEN）")
	}
	base := strings.TrimRight(strings.TrimSpace(addr), "/")
	if base == "" {
		return nil, errors.New("远端沙箱地址为空（SANDBOX_RUNNER_ADDR）")
	}
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	c := &Client{base: base, token: token, http: &http.Client{}}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var h healthResponse
	if err := c.do(ctx, http.MethodGet, readyPath, nil, &h); err != nil {
		return nil, fmt.Errorf("连接执行体 %s 失败: %w", base, err)
	}
	if strings.TrimSpace(h.Backend) == "" {
		return nil, errors.New("执行体没有报告沙箱后端")
	}
	c.backend = h.Backend
	c.isolated = h.Isolated
	if h.Env != nil {
		c.env = *h.Env
	}
	return c, nil
}

// Name 返回执行体真实生效的后端名（bwrap / sandbox-exec / local）。
// 调用方据此判断「到底有没有隔离」，不要用字面量比较。
func (c *Client) Name() string { return c.backend }

// Env 返回执行容器自述的运行环境，实现 sandbox.EnvProvider。
//
// 只在握手时取一次：镜像基线是稳定的（换镜像必然重建执行体，也就必然重新握手），
// 每轮去问一次只是多一次往返。要在不重启控制面的情况下刷新，
// 重新 New 一个 Client 即可——旧实例没有状态需要迁移。
func (c *Client) Env() sandbox.EnvFacts { return c.env }

// EnvHash 返回环境自述的摘要，仅用于日志对账。
func (c *Client) EnvHash() string { return c.env.Hash() }

// Transport 返回传输形态，仅用于日志：backend 才是安全语义。
func (c *Client) Transport() string { return "remote" }

// Isolated 表示执行体报告自己不是 local。
func (c *Client) Isolated() bool { return c.isolated }

// Run 在远端执行一次命令。
func (c *Client) Run(ctx context.Context, spec sandbox.Spec) (sandbox.Result, error) {
	body, err := json.Marshal(runRequest{Spec: spec})
	if err != nil {
		return sandbox.Result{Sandbox: c.backend}, fmt.Errorf("序列化执行请求失败: %w", err)
	}
	var out runResponse
	if err := c.do(ctx, http.MethodPost, runPath, body, &out); err != nil {
		return out.Result, err
	}
	if out.Error != nil {
		return out.Result, out.Error.toError()
	}
	return out.Result, nil
}

// Start 需要双向流，当前版本未实现。
//
// 明确报错而不是回退成「一次性进程」：常驻插件按 runtime.resident 申请的是
// 进程复用，静默改成一次一进程会让冷启动从毫秒变秒级，而且问题只会在
// 生产上以「插件很慢」的形式出现。
func (c *Client) Start(ctx context.Context, spec sandbox.Spec) (*sandbox.Proc, error) {
	return nil, errors.New("远端执行体暂不支持常驻进程（Start）：请把插件 runtime.resident 关闭，或等待常驻进程通道上线")
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) error {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// 错误响应也可能是 JSON（wireError），先整体读出来再判断。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRequestBytes))
	if err != nil {
		return fmt.Errorf("读取执行体响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var we wireError
		if json.Unmarshal(raw, &we) == nil && we.Msg != "" {
			return we.toError()
		}
		return fmt.Errorf("执行体返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("解析执行体响应失败: %w", err)
	}
	return nil
}

type runRequest struct {
	Spec sandbox.Spec `json:"spec"`
}

type runResponse struct {
	Result sandbox.Result `json:"result"`
	Error  *wireError     `json:"error,omitempty"`
}

// wireError 把错误分类带回控制面，这样 tool 层「超时 / 输出超限是结果的一部分」
// 的判断在远端执行时仍然成立。
type wireError struct {
	Kind string `json:"kind"`
	Msg  string `json:"msg"`
}

func (e *wireError) toError() error {
	msg := strings.TrimSpace(e.Msg)
	if msg == "" {
		msg = "执行体返回未知错误"
	}
	switch e.Kind {
	case "timeout":
		return fmt.Errorf("%w: %s", sandbox.ErrTimeout, msg)
	case "output_too_large":
		return fmt.Errorf("%w: %s", sandbox.ErrOutputTooLarge, msg)
	default:
		return errors.New(msg)
	}
}
