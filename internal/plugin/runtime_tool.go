package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/workagent/auto-agent/internal/sandbox"
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
//
// 默认一次性：每次调用起一个进程，跑完即退。
// 只有清单声明 runtime.resident 的插件才进常驻池，按键（空间 + slug + 目录）复用。
type toolRuntime struct {
	runner     sandbox.Runner
	deno       string
	runtimeDir string
	secrets    Secrets
	fs         storage.Storage
	logger     *slog.Logger
	pool       *residentPool
	// egress 给每个插件实例一条独立出网通道（SOCKS5 over unix socket）。
	// 没有声明 http.request 的插件拿不到 socket，也就完全没有出网。
	egress *egressPool
}

type invokeInput struct {
	Dir     string
	Entry   string
	Limits  Limits
	SpaceID string
	UserID  string
	Slug    string
	// FS 是文件访问白名单（空间工作区 + 清单 fs 收紧后的结果）。
	// 它同时喂给沙箱挂载和 deno 的 --allow-read/--allow-write：
	// 挂载决定「路径存不存在」，权限决定「deno 允不允许读」，缺一层都算没管。
	FS sandbox.FS
	// NetHosts 是清单声明的出网域名，用来建该插件的代理白名单。
	NetHosts []string
	// SpaceRoot 是空间工作区的绝对路径，以 WA_SPACE_DIR 注入：
	// 文件改直连后，脚本得知道自己的相对路径挂在哪。
	SpaceRoot string
	// OneShot 为 true 时不起常驻进程：试运行必须跑完就退，
	// 否则草稿代码会一直挂在池里，改一次就要等空闲回收。
	OneShot bool
	// WSHosts 是 ws.connect 声明的域名：非空才注入 HTTPS_PROXY 并放行 CONNECT。
	WSHosts []string
	// 能力清单是审批对象：脚本能调到哪些宿主方法由 plugin.json 决定。
	Allow   map[string]bool
	Verbs   map[string]bool
	Secrets map[string]bool
}

// residentKey 是常驻进程的复用键：目录变化就是另一份代码，不能复用。
func residentKey(spaceID, slug, dir string) string {
	return spaceID + "|" + slug + "|" + dir
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

// Call 执行一次插件调用。
//
// 声明了 runtime.resident 的插件复用常驻进程：冷启动约 50–180ms，
// 连接、缓存、模块级状态跨调用保留，代价是常占内存与一个池名额。
// OneShot 走一次性执行，用于试运行与不方便持有进程的场景。
func (r *toolRuntime) Call(ctx context.Context, in invokeInput, input json.RawMessage) (string, error) {
	if r.deno == "" {
		return "", errors.New("当前环境没有 deno，无法执行脚本插件")
	}
	if in.OneShot || r.pool == nil {
		return r.callOnce(ctx, in, input)
	}
	key := residentKey(in.SpaceID, in.Slug, in.Dir)
	rp, err := r.pool.acquire(key, r, func() (*sandbox.Proc, error) {
		return r.startProc(ctx, in, true)
	})
	if err != nil {
		return "", err
	}
	out, err := rp.invoke(ctx, in, input)
	if err != nil && rp.dead() {
		// 进程已经死了就从池里摘掉，下一次调用重新起一个，
		// 否则每个后续调用都会先撞一次「进程已退出」。
		r.pool.drop(key, rp)
	}
	return out, err
}

// procSpec 是启动插件进程的静态参数，一次性与常驻共用。
func (r *toolRuntime) procSpec(in invokeInput, resident bool) (sandbox.Spec, error) {
	lp, ok := r.fs.(localPathStore)
	if !ok {
		return sandbox.Spec{}, errors.New("当前存储不支持插件执行（需要本地文件系统）")
	}
	abs, err := lp.LocalPath(in.Dir)
	if err != nil {
		return sandbox.Spec{}, fmt.Errorf("解析插件目录失败: %w", err)
	}
	entry := filepath.Join(abs, filepath.FromSlash(in.Entry))
	entryURL := (&url.URL{Scheme: "file", Path: entry}).String()
	bootstrap := filepath.Join(r.runtimeDir, "bootstrap.ts")

	// 出网：声明了 http.request 或 ws.connect 才有通道。两张名单都空就没有代理，
	// 沙箱又保持 --unshare-net，等于完全断网。
	var proxy string
	if r.egress != nil && (len(in.NetHosts) > 0 || len(in.WSHosts) > 0) {
		p, err := r.egress.proxyFor(in.SpaceID, in.Slug, in.NetHosts, in.WSHosts, in.Verbs)
		if err != nil {
			return sandbox.Spec{}, fmt.Errorf("建立出网通道失败: %w", err)
		}
		proxy = p
	}

	// 运行时要可读：bootstrap.ts 与 host_sdk.ts 都在 runtimeDir 里，
	// 不在挂载白名单里的话连启动都做不到（exec 直接 ENOENT）。
	fs := in.FS
	fs.Read = dedupePaths(append(append([]string{}, fs.Read...), r.runtimeDir))

	spec := sandbox.Spec{
		Cmd: append(
			[]string{
				r.deno, "run",
				"--quiet", "--no-prompt",
				// 沙箱里没有可写工作目录，锁文件与配置会变成噪音甚至失败
				"--no-lock", "--no-config",
			},
			append(denoPermissionFlags(fs, proxy, len(in.WSHosts) > 0),
				bootstrap, entryURL)...,
		),
		Dir:       abs,
		Mode:      sandbox.ModeWorkspaceWrite,
		FS:        fs,
		Network:   false,
		Proxy:     proxy,
		Timeout:   time.Duration(in.Limits.TimeoutMS) * time.Millisecond,
		Resident:  resident,
		MaxOutput: maxFrameBytes * 4,
	}
	// 进程环境被 --clearenv 清空过，只有这里列出来的变量能进沙箱。
	if in.SpaceRoot != "" {
		spec.Env = append(spec.Env, "WA_SPACE_DIR="+in.SpaceRoot)
	}
	if proxy != "" {
		spec.Env = append(spec.Env, "WA_EGRESS_PROXY="+proxy)
	}
	// wss:// 只能靠 Deno 自己的代理支持：它读 HTTPS_PROXY，
	// 且认 unix:/path 这种写法（实测 2.9.4）。
	// 不声明 ws.connect 就不注入，插件连 new WebSocket 都会直连失败。
	if proxy != "" && len(in.WSHosts) > 0 {
		spec.Env = append(spec.Env, "HTTPS_PROXY=unix:"+proxy)
	}
	return spec, nil
}

// denoPermissionFlags 把同一份 fs 白名单翻译成 deno 的权限参数。
//
// 沙箱挂载决定「路径存不存在」，这里决定「deno 允不允许读」。只做挂载的话，
// 一个 --allow-read 全开就能读到挂载进来的所有东西；只做权限的话，
// 挂载层漏掉的路径又会被 deno 放出去。两层都要，才是同一份声明的两个执行点。
func denoPermissionFlags(fs sandbox.FS, proxy string, allowWS bool) []string {
	read := append([]string{}, fs.Read...)
	write := append([]string{}, fs.Write...)
	if proxy != "" {
		// Deno 对 unix socket 的检查是「读 + 写 + net」三件套，
		// 少一个就是 NotCapable，而不是「连不上」这种能猜到的错。
		read = append(read, proxy)
		write = append(write, proxy)
	}
	flags := make([]string, 0, 4)
	if len(read) > 0 {
		flags = append(flags, "--allow-read="+strings.Join(read, ","))
	}
	if len(write) > 0 {
		flags = append(flags, "--allow-write="+strings.Join(write, ","))
	}
	envs := []string{"WA_SPACE_DIR"}
	if proxy != "" {
		envs = append(envs, "WA_EGRESS_PROXY")
		if allowWS {
			envs = append(envs, "HTTPS_PROXY")
		}
		// 放开 --allow-net 不会变成直连：沙箱是 --unshare-net，
		// 除了挂进来的 socket 没有第二个出口。
		flags = append(flags, "--allow-net")
	}
	return append(flags, "--allow-env="+strings.Join(envs, ","))
}

func dedupePaths(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

func (r *toolRuntime) startProc(ctx context.Context, in invokeInput, resident bool) (*sandbox.Proc, error) {
	spec, err := r.procSpec(in, resident)
	if err != nil {
		return nil, err
	}
	// 常驻进程的父 ctx 必须是长生命周期的：用调用方的 ctx 会在请求结束时
	// 把插件连根拔掉，常驻就名存实亡了。
	if resident {
		ctx = context.Background()
	}
	proc, err := r.runner.Start(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("启动插件失败: %w", err)
	}
	return proc, nil
}

// callOnce 是一次性执行：起进程、跑一次、关闭。
func (r *toolRuntime) callOnce(ctx context.Context, in invokeInput, input json.RawMessage) (string, error) {
	if r.deno == "" {
		return "", errors.New("当前环境没有 deno，无法执行脚本插件")
	}
	proc, err := r.startProc(ctx, in, false)
	if err != nil {
		return "", err
	}
	defer proc.Close()
	timeout := time.Duration(in.Limits.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = defaultInvokeTimeout
	}

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

// removedMethods 记录已下线的宿主方法及替代方案。
// 存在的意义只是报错可读：这些名字在 allow 表里永远不出现。
var removedMethods = map[string]string{
	"fs.read":      "文件用 Deno.readTextFile(wa.path(\"data/x\"))，范围由清单的 fs 段决定",
	"fs.list":      "目录用 Deno.readDir(wa.path(\"data/\"))",
	"fs.write":     "文件用 Deno.writeTextFile(wa.path(\"data/x\"))，写权限要在 fs 段声明",
	"http.request": "出网用 wa.fetch；凭证用 host.secret(名称) 取明文自己拼 header",
}

// hostCall 实现宿主系统调用表。每一条都对应已有的一份策略实现。
func (r *toolRuntime) hostCall(ctx context.Context, in invokeInput, frame rpcFrame) (any, error) {
	callCtx, cancel := context.WithTimeout(ctx, hostCallTimeout)
	defer cancel()

	// 已经从宿主 ABI 里删掉的方法先说清楚去哪：走到下面那条通用报错，
	// 作者会以为「补进 capabilities 就能用」，而补进去是解析失败。
	if hint, ok := removedMethods[frame.Method]; ok {
		return nil, fmt.Errorf("%s 已从宿主 ABI 移除：%s", frame.Method, hint)
	}

	// 未在 plugin.json 里声明的宿主能力直接拒绝：审批看到的是清单，
	// 脚本不能靠「写一行 host.http」把未授权的调用拿到手。
	if !in.Allow[frame.Method] {
		return nil, fmt.Errorf("插件未声明能力 %s，请在 plugin.json 的 capabilities 里补充", frame.Method)
	}

	switch frame.Method {
	case MethodSecretGet:
		return r.secretGet(callCtx, in, frame.Args)
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

// secretGet 是插件唯一能拿到凭证明文的地方。
//
// 只认清单里逐个声明过的名字（capabilities 里 {"method":"secret.use","name":"x"}）：
// 只说「有 secret.use」不等于「能读整个 plugin scope」，否则一个通用插件就能把
// 平台上所有插件的凭证都取走。
func (r *toolRuntime) secretGet(ctx context.Context, in invokeInput, raw json.RawMessage) (any, error) {
	var a struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("secret.get 参数非法: %w", err)
	}
	name := strings.TrimSpace(a.Name)
	if name == "" {
		return nil, errors.New("secret.get 需要 name")
	}
	if !in.Secrets[name] {
		return nil, fmt.Errorf("未在清单声明凭证 %q：请加一条 {\"method\":\"secret.use\",\"name\":%q}", name, name)
	}
	if r.secrets == nil {
		return nil, errors.New("凭证存储未启用（缺少 SECRET_KEK）")
	}
	value, err := r.secrets.Get(ctx, "plugin", name)
	if err != nil {
		return nil, fmt.Errorf("凭证 %s 不存在或读取失败", name)
	}
	// 只记名字，不记值：审计日志本身也是会被导出的一份文件。
	r.logger.Info("插件读取凭证", "plugin", in.Slug, "space", in.SpaceID, "name", name)
	return map[string]any{"name": name, "value": value}, nil
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
