package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/workagent/auto-agent/internal/sandbox"
	"github.com/workagent/auto-agent/internal/search"
	"github.com/workagent/auto-agent/internal/storage"
)

// 单帧上限：正常一帧只有几 KB，超过基本意味着协议实现有问题。
const maxFrameBytes = 512 << 10

// 宿主 RPC 单次调用的墙钟，必须短于插件整体的 timeout。
const hostCallTimeout = 15 * time.Second

// maxHTTPBody 是 http.request 回给插件的响应体上限。
const maxHTTPBody = 1 << 20

// Secrets 是密钥来源。插件永远拿不到明文：manifest 里写 secret:name，
// 由宿主在出网前替换。
type Secrets interface {
	Get(ctx context.Context, scope, name string) (string, error)
}

// localPathStore 是可解析出本地绝对路径的存储（localfs 实现）。
// 换成对象存储时这条能力消失，脚本类插件不可用，但技能/组件不受影响。
type localPathStore interface {
	LocalPath(key string) (string, error)
}

// toolRuntime 执行 kind=tool 的插件：deno 进程 + NDJSON 双向协议。
type toolRuntime struct {
	runner     sandbox.Runner
	deno       string
	runtimeDir string
	secrets    Secrets
	fs         storage.Storage
	logger     *slog.Logger
}

type invokeInput struct {
	Dir     string
	Entry   string
	Guard   *search.Guard
	Limits  Limits
	SpaceID string
	UserID  string
	Slug    string
	// 能力清单是审批对象：脚本能调到哪些宿主方法由 plugin.json 决定。
	Allow     map[string]bool
	Verbs     map[string]bool
	HasSecret bool
}

type rpcFrame struct {
	Op     string          `json:"op"`
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Data   json.RawMessage `json:"data"`
	Args   json.RawMessage `json:"args"`
	Error  string          `json:"error"`
}

type rpcReply struct {
	Op    string `json:"op"`
	ID    int    `json:"id"`
	OK    bool   `json:"ok"`
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

// Call 执行一次插件调用。进程一次一启：冷启动约 100ms，换来的是
// 「一次调用失败不会污染下一次」以及超时语义干净。
func (r *toolRuntime) Call(ctx context.Context, in invokeInput, input json.RawMessage) (string, error) {
	if r.deno == "" {
		return "", errors.New("当前环境没有 deno，无法执行脚本插件")
	}
	lp, ok := r.fs.(localPathStore)
	if !ok {
		return "", errors.New("当前存储不支持插件执行（需要本地文件系统）")
	}
	abs, err := lp.LocalPath(in.Dir)
	if err != nil {
		return "", fmt.Errorf("解析插件目录失败: %w", err)
	}
	entry := filepath.Join(abs, filepath.FromSlash(in.Entry))
	entryURL := (&url.URL{Scheme: "file", Path: entry}).String()
	bootstrap := filepath.Join(r.runtimeDir, "bootstrap.ts")

	timeout := time.Duration(in.Limits.TimeoutMS) * time.Millisecond
	proc, err := r.runner.Start(ctx, sandbox.Spec{
		Cmd: []string{
			r.deno, "run",
			"--quiet", "--no-prompt",
			// 沙箱里没有可写工作目录，锁文件与配置会变成噪音甚至失败
			"--no-lock", "--no-config",
			bootstrap, entryURL,
		},
		Dir:       abs,
		Mode:      sandbox.ModeReadOnly,
		Network:   false,
		Timeout:   timeout,
		MaxOutput: maxFrameBytes * 4,
	})
	if err != nil {
		return "", fmt.Errorf("启动插件失败: %w", err)
	}
	defer proc.Close()

	if err := writeFrame(proc, map[string]any{
		"op":   "invoke",
		"data": json.RawMessage(input),
		"args": map[string]any{"space_id": in.SpaceID, "user_id": in.UserID, "slug": in.Slug},
	}); err != nil {
		return "", fmt.Errorf("发送调用帧失败: %w", err)
	}

	reader := bufio.NewReaderSize(proc.Stdout, 64<<10)
	maxRPC := in.Limits.MaxRPC
	rpc := 0
	deadline := time.Now().Add(timeout)

	for {
		if time.Now().After(deadline) {
			return "", fmt.Errorf("插件执行超时（%dms）", in.Limits.TimeoutMS)
		}
		line, readErr := readLine(reader, maxFrameBytes)
		if readErr != nil {
			// 进程提前退出：给出 stderr，不然只剩一个 io.EOF 无从排查。
			proc.Kill()
			res, _ := proc.Wait()
			msg := strings.TrimSpace(proc.Stderr())
			if msg == "" {
				msg = strings.TrimSpace(res.Stderr)
			}
			if msg != "" {
				return "", fmt.Errorf("插件进程异常退出（%s）", clipText(msg, 500))
			}
			return "", fmt.Errorf("插件没有返回结果: %w", readErr)
		}
		var frame rpcFrame
		if err := json.Unmarshal(line, &frame); err != nil {
			return "", fmt.Errorf("插件输出了非法帧: %s", clipText(string(line), 200))
		}
		switch frame.Op {
		case "return":
			if frame.Error != "" {
				return "", errors.New(frame.Error)
			}
			return frameData(frame.Data), nil
		case "error":
			msg := frame.Error
			if msg == "" {
				var e struct {
					Message string `json:"message"`
				}
				_ = json.Unmarshal(frame.Data, &e)
				msg = e.Message
			}
			if msg == "" {
				msg = "插件返回错误"
			}
			return "", errors.New(msg)
		case "call":
			rpc++
			if rpc > maxRPC {
				return "", fmt.Errorf("插件单次调用超过宿主 RPC 上限（%d 次）", maxRPC)
			}
			data, callErr := r.hostCall(ctx, in, frame)
			reply := rpcReply{Op: "result", ID: frame.ID, OK: callErr == nil, Data: data}
			if callErr != nil {
				reply.Error = callErr.Error()
			}
			if err := writeFrame(proc, reply); err != nil {
				return "", fmt.Errorf("回写宿主结果失败: %w", err)
			}
		default:
			return "", fmt.Errorf("插件输出了未知帧 %q", frame.Op)
		}
	}
}

// hostCall 实现宿主系统调用表。每一条都对应已有的一份策略实现。
func (r *toolRuntime) hostCall(ctx context.Context, in invokeInput, frame rpcFrame) (any, error) {
	callCtx, cancel := context.WithTimeout(ctx, hostCallTimeout)
	defer cancel()

	// 未在 plugin.json 里声明的宿主能力直接拒绝：审批看到的是清单，
	// 脚本不能靠「写一行 host.http」把未授权的调用拿到手。
	if !in.Allow[frame.Method] {
		return nil, fmt.Errorf("插件未声明能力 %s，请在 plugin.json 的 capabilities 里补充", frame.Method)
	}

	switch frame.Method {
	case MethodHTTP:
		return r.httpRequest(callCtx, in, frame.Args)
	case MethodFSRead:
		var a struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(frame.Args, &a); err != nil {
			return nil, fmt.Errorf("fs.read 参数非法: %w", err)
		}
		return r.readFile(callCtx, in.SpaceID, a.Path)
	case MethodFSList:
		var a struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(frame.Args, &a); err != nil {
			return nil, fmt.Errorf("fs.list 参数非法: %w", err)
		}
		return r.listFiles(callCtx, in.SpaceID, a.Path)
	case MethodFSWr:
		var a struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(frame.Args, &a); err != nil {
			return nil, fmt.Errorf("fs.write 参数非法: %w", err)
		}
		return r.writeFile(callCtx, in.SpaceID, a.Path, a.Content)
	case MethodLog:
		var a struct {
			Level   string `json:"level"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(frame.Args, &a)
		r.logger.Info("插件日志", "plugin", in.Slug, "space", in.SpaceID, "level", a.Level, "message", clipText(a.Message, 500))
		return map[string]any{"ok": true}, nil
	default:
		return nil, fmt.Errorf("宿主不支持 method %q", frame.Method)
	}
}

func (r *toolRuntime) httpRequest(ctx context.Context, in invokeInput, raw json.RawMessage) (any, error) {
	var a struct {
		URL     string            `json:"url"`
		Method  string            `json:"method"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("http.request 参数非法: %w", err)
	}
	method := strings.ToUpper(strings.TrimSpace(a.Method))
	if method == "" {
		method = http.MethodGet
	}
	if !in.Verbs[method] {
		return nil, fmt.Errorf("插件未声明 HTTP 动词 %s（capabilities.verbs）", method)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.URL, strings.NewReader(a.Body))
	if err != nil {
		return nil, fmt.Errorf("请求地址非法: %w", err)
	}
	for k, v := range a.Headers {
		// 密钥只在出网这一步被替换，插件进程内存里从来没有明文。
		req.Header.Set(k, r.resolveSecrets(ctx, v, in.HasSecret))
	}
	resp, err := in.Guard.DoRaw(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBody))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}
	headers := make(map[string]string, len(resp.Header))
	for k := range resp.Header {
		headers[k] = resp.Header.Get(k)
	}
	return map[string]any{
		"status":    resp.StatusCode,
		"body":      string(body),
		"headers":   headers,
		"truncated": resp.ContentLength > maxHTTPBody,
	}, nil
}

// resolveSecrets 把 secret:name 替换成真值；取不到时按空处理并留日志。
func (r *toolRuntime) resolveSecrets(ctx context.Context, value string, allowed bool) string {
	const prefix = "secret:"
	if !strings.HasPrefix(value, prefix) {
		return value
	}
	if !allowed {
		r.logger.Warn("插件未声明 secret.use，密钥引用被忽略")
		return ""
	}
	name := strings.TrimSpace(strings.TrimPrefix(value, prefix))
	if name == "" || r.secrets == nil {
		return ""
	}
	secret, err := r.secrets.Get(ctx, "plugin", name)
	if err != nil {
		r.logger.Warn("插件引用的密钥不存在", "name", name, "error", err)
		return ""
	}
	return secret
}

func (r *toolRuntime) spaceFilePath(spaceID, p string) (string, string, error) {
	clean := path.Clean(strings.TrimPrefix(strings.TrimSpace(p), "/"))
	if clean == "." || clean == "" {
		clean = ""
	}
	if strings.Contains(clean, "..") {
		return "", "", errors.New("路径不能包含 ..")
	}
	key := path.Join(r.fs.SpaceKey(spaceID), "files", clean)
	return key, clean, nil
}

func (r *toolRuntime) readFile(ctx context.Context, spaceID, p string) (any, error) {
	key, clean, err := r.spaceFilePath(spaceID, p)
	if err != nil {
		return nil, err
	}
	f, err := r.fs.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", clean, err)
	}
	defer func() { _ = f.Close() }()
	body, err := io.ReadAll(io.LimitReader(f, maxBundleBytes))
	if err != nil {
		return nil, err
	}
	return map[string]any{"path": clean, "content": string(body)}, nil
}

func (r *toolRuntime) listFiles(ctx context.Context, spaceID, p string) (any, error) {
	key, _, err := r.spaceFilePath(spaceID, p)
	if err != nil {
		return nil, err
	}
	entries, err := r.fs.List(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("列目录失败: %w", err)
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]any{
			"name":   path.Base(e.Key),
			"is_dir": e.IsDir,
			"size":   e.Size,
		})
		if len(out) >= 200 {
			break
		}
	}
	return map[string]any{"entries": out}, nil
}

func (r *toolRuntime) writeFile(ctx context.Context, spaceID, p, content string) (any, error) {
	if len(content) > maxBundleBytes {
		return nil, errors.New("写入内容超过 5MB")
	}
	key, clean, err := r.spaceFilePath(spaceID, p)
	if err != nil {
		return nil, err
	}
	if clean == "" {
		return nil, errors.New("必须给出文件路径")
	}
	if err := r.fs.EnsureDir(ctx, path.Dir(key)); err != nil {
		return nil, err
	}
	if _, err := r.fs.Put(ctx, key, strings.NewReader(content), int64(len(content))); err != nil {
		return nil, fmt.Errorf("写入 %s 失败: %w", clean, err)
	}
	return map[string]any{"path": clean, "bytes": len(content)}, nil
}

func writeFrame(proc *sandbox.Proc, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return proc.Write(append(body, '\n'))
}

func readLine(r *bufio.Reader, limit int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > limit {
			return nil, fmt.Errorf("单帧超过 %d 字节", limit)
		}
		if err == nil {
			return buf[:len(buf)-1], nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && len(buf) > 0 {
			return buf, nil
		}
		return nil, err
	}
}

func frameData(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
