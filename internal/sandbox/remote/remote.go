// Package remote 把 sandbox.Runner 搬到另一个进程里执行（执行容器）。
//
// 动机：容器化之后，平台进程与执行体不在同一个文件系统命名空间，但上层
// （shell_exec、脚本插件、搜索脚本）只认识 sandbox.Runner。这一层把 Run 变成
// 一次 HTTP 调用，其余代码一行不改。
//
// 传输选 HTTP+JSON 而不是 gRPC：Run 是「一个请求一个结果」；Start 是
// 一条双工连接（请求体=上行帧，响应体=下行帧），用长度前缀把 Spec 与
// 后续的 NDJSON 流分开。够用，且不需要引入 gRPC 依赖。
package remote

import (
	"bytes"
	"context"
	"encoding/binary"
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
	// procPath 是常驻进程：一条连接同时承载上行帧与下行帧。
	//
	// 不用「建进程 → 轮询 → 关进程」那套：插件协议是持续的双向 NDJSON，
	// 拆成多次请求会把每一帧都变成一次往返，而且中间状态没有归属。
	procPath = "/v1/procs"

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
// 这里必须握手而不是假设「远端一定隔离」——执行体可能自报非隔离后端，
// 控制面如果不知情，就会把「裸跑」记成 bwrap。
func New(addr, token string) (*Client, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("远端沙箱缺少鉴权令牌（SANDBOX_RUNNER_TOKEN）")
	}
	base := strings.TrimRight(strings.TrimSpace(addr), "/")
	if base == "" {
		return nil, errors.New("远端沙箱地址为空（执行体地址未配置）")
	}
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	// 禁用自动解压：常驻通道是一路 NDJSON 流，gzip 会引入缓冲，
	// 表现为「插件回了结果但控制面过一会儿才收到」。
	c := &Client{base: base, token: token, http: &http.Client{
		Transport: &http.Transport{DisableCompression: true},
	}}

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

// Name 返回执行体真实生效的后端名（bwrap / local）。
// 调用方据此判断「到底有没有隔离」，不要用字面量比较。
func (c *Client) Name() string { return c.backend }

// Executable 返回裸命令名：真正的解析发生在执行体里，控制面的 PATH 与容器无关。
//
// 这条线只传「命令名」，不传宿主绝对路径——容器化部署下控制面可能在 macOS 上，
// 本机解析出来的 /Users/.../deno 在执行容器里必然不存在。
func (c *Client) Executable(name string) string { return name }

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

// wire 清掉只在本机有意义的字段：宿主路径不出这条线。
//
// Mount.Source 是控制面看到的真实目录，执行体有它自己的根与 Key 解析；
// Proxy 是建 socket 那一侧的临时路径，断网时由执行体自己重建。
// 这样过线的只有「相对根 + 沙箱内挂载点」，两边不再需要同路径，
// 报错里也不会出现对方机器的目录结构。
func wire(spec sandbox.Spec) sandbox.Spec {
	mounts := make([]sandbox.Mount, len(spec.Mounts))
	copy(mounts, spec.Mounts)
	for i := range mounts {
		// 只有「空间数据」这一路需要由执行体重新解析：
		// 系统挂载（Key 为空）在两边本来就是同一个容器内路径。
		if mounts[i].Key != "" || mounts[i].Root == sandbox.RootCache {
			mounts[i].Source = ""
		}
	}
	spec.Mounts = mounts
	spec.Proxy = ""
	return spec
}

// Run 在远端执行一次命令。
func (c *Client) Run(ctx context.Context, spec sandbox.Spec) (sandbox.Result, error) {
	body, err := json.Marshal(runRequest{Spec: wire(spec)})
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

// Start 在远端执行体里起一个常驻进程，并用一条双工连接与它通信。
//
// 协议：请求体是「4 字节大端长度 + JSON Spec + 后续上行帧」，响应体是
// 一路下行帧。为什么不用「先 POST 创建、再分别读写」：那要在建进程与
// 收帧之间插入一次往返，而插件在启动阶段就可能在等 stdin，状态容易错位。
//
// 连接断开即进程终止（服务端读到上行 EOF 会杀掉进程），所以 Close /
// Kill 都退化成关连接。
func (c *Client) Start(ctx context.Context, spec sandbox.Spec) (*sandbox.Proc, error) {
	payload, err := json.Marshal(startRequest{Spec: wire(spec)})
	if err != nil {
		return nil, fmt.Errorf("序列化常驻进程请求失败: %w", err)
	}
	if len(payload) > maxRequestBytes {
		return nil, fmt.Errorf("常驻进程请求过大（%d 字节）", len(payload))
	}

	pr, pw := io.Pipe()
	go func() {
		var head [4]byte
		binary.BigEndian.PutUint32(head[:], uint32(len(payload)))
		if _, err := pw.Write(head[:]); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if _, err := pw.Write(payload); err != nil {
			_ = pw.CloseWithError(err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+procPath, pr)
	if err != nil {
		_ = pw.CloseWithError(err)
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/x-ndjson")
	// 必须显式要求关闭连接复用。
	//
	// net/http 在首次 Flush 的时候会尝试把请求体的剩余部分读掉（为了把连接
	// 还给连接池复用），而这里的请求体是一条持续的双向流：服务端读它、
	// 客户端在等响应头，双方一起挂住。标记 close 之后服务端跳过这段清理。
	req.Close = true

	resp, err := c.http.Do(req)
	if err != nil {
		_ = pw.CloseWithError(err)
		return nil, fmt.Errorf("启动常驻进程失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		_ = pw.CloseWithError(errors.New("常驻进程启动被拒绝"))
		var we wireError
		if json.Unmarshal(raw, &we) == nil && we.Msg != "" {
			return nil, we.toError()
		}
		return nil, fmt.Errorf("执行体返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	backend := resp.Header.Get("X-Sandbox-Backend")
	if backend == "" {
		// 老执行体不回这个头：宁可标成控制面握手时的后端，
		// 也不要让审计里出现「none」这种看起来像没隔离的名字。
		backend = c.backend
	}
	closeFn := func() {
		_ = resp.Body.Close()
		_ = pw.Close()
	}
	return sandbox.NewProc(backend, resp.Body, pw, closeFn), nil
}

type startRequest struct {
	Spec sandbox.Spec `json:"spec"`
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
