package remote

import (
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/workagent/auto-agent/internal/egress"
	"github.com/workagent/auto-agent/internal/sandbox"
)

// Handler 是执行体对控制面暴露的接口。
//
// root 非空时只允许在 root 之下执行：执行体是特权容器，令牌泄露等于节点级
// 代码执行，所以在协议层再收一道「只能在存储根下跑」。真正的边界仍然是
// bwrap 的挂载白名单，这里防的是误用与越界路径。
//
// eg 是出网代理注册表：控制面过线的是白名单（Spec.Egress），代理由执行体
// 在自己的命名空间里建——容器之间不共享 /tmp，路径传过来没有意义。
func Handler(r sandbox.Runner, token, root string, facts sandbox.EnvFacts, eg *egress.Registry, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	s := &server{runner: r, token: token, root: filepath.Clean(root), facts: facts, egress: eg, logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc(healthPath, s.health)
	mux.HandleFunc(readyPath, s.auth(s.ready))
	mux.HandleFunc(runPath, s.auth(s.run))
	mux.HandleFunc(procPath, s.auth(s.start))
	return mux
}

type server struct {
	runner sandbox.Runner
	token  string
	root   string
	facts  sandbox.EnvFacts
	egress *egress.Registry
	logger *slog.Logger
}

type healthResponse struct {
	Backend  string            `json:"backend"`
	Isolated bool              `json:"isolated"`
	Env      *sandbox.EnvFacts `json:"env,omitempty"`
	EnvHash  string            `json:"env_hash,omitempty"`
}

// health 不鉴权：k8s 的 httpGet 探针没法带 header。
// 它只暴露后端名，不构成敏感信息泄露；真正的执行接口必须带令牌。
func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.status(false))
}

// ready 是控制面握手的入口（鉴权）：一次往返同时确认「连得上 / 令牌对 /
// 环境是什么」，省掉第二次请求，也避免两件事拿到互相矛盾的结果。
func (s *server) ready(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.status(true))
}

func (s *server) status(withEnv bool) healthResponse {
	name := "unknown"
	if s.runner != nil {
		name = s.runner.Name()
	}
	out := healthResponse{Backend: name, Isolated: name != sandbox.NameLocal}
	if withEnv && !s.facts.Empty() {
		out.Env = &s.facts
		out.EnvHash = s.facts.Hash()
	}
	return out
}

func (s *server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if s.token == "" {
			// 没配令牌直接拒绝：执行体可以跑任意命令，不能裸奔。
			writeJSON(w, http.StatusInternalServerError, wireError{Kind: "config", Msg: "执行体未配置鉴权令牌"})
			return
		}
		got := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, wireError{Kind: "auth", Msg: "鉴权失败"})
			return
		}
		next(w, req)
	}
}

func (s *server) run(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, wireError{Kind: "method", Msg: "只支持 POST"})
		return
	}
	var in runRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, req.Body, maxRequestBytes))
	if err := dec.Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, wireError{Kind: "decode", Msg: "解析执行请求失败: " + err.Error()})
		return
	}
	if err := s.checkPaths(in.Spec); err != nil {
		writeJSON(w, http.StatusForbidden, wireError{Kind: "path", Msg: err.Error()})
		return
	}
	spec, err := s.prepareEgress(in.Spec)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, wireError{Kind: "egress", Msg: err.Error()})
		return
	}

	res, err := s.runner.Run(req.Context(), spec)
	out := runResponse{Result: res}
	if err != nil {
		out.Error = classify(err)
	}
	writeJSON(w, http.StatusOK, out)
}

// start 起一个常驻进程，并把这条 HTTP 连接变成它的 stdin/stdout。
//
// 连接即进程：上行 EOF（控制面关掉写端）或下行结束（进程退出）都会终止它。
// 这样常驻进程的生命周期不需要额外的注册表，也不会因为没有显式 Close
// 而在执行体里越积越多。
func (s *server) start(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, wireError{Kind: "method", Msg: "只支持 POST"})
		return
	}
	// 前缀帧：4 字节长度 + JSON Spec，之后紧跟上行 NDJSON 帧。
	var head [4]byte
	if _, err := io.ReadFull(req.Body, head[:]); err != nil {
		writeJSON(w, http.StatusBadRequest, wireError{Kind: "decode", Msg: "读取请求头失败: " + err.Error()})
		return
	}
	n := binary.BigEndian.Uint32(head[:])
	if n == 0 || int64(n) > maxRequestBytes {
		writeJSON(w, http.StatusBadRequest, wireError{Kind: "decode", Msg: fmt.Sprintf("请求头长度非法: %d", n)})
		return
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(req.Body, buf); err != nil {
		writeJSON(w, http.StatusBadRequest, wireError{Kind: "decode", Msg: "读取 Spec 失败: " + err.Error()})
		return
	}
	var in startRequest
	if err := json.Unmarshal(buf, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, wireError{Kind: "decode", Msg: "解析 Spec 失败: " + err.Error()})
		return
	}
	if err := s.checkPaths(in.Spec); err != nil {
		writeJSON(w, http.StatusForbidden, wireError{Kind: "path", Msg: err.Error()})
		return
	}
	spec, err := s.prepareEgress(in.Spec)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, wireError{Kind: "egress", Msg: err.Error()})
		return
	}

	proc, err := s.runner.Start(req.Context(), spec)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, classify(err))
		return
	}
	defer proc.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, wireError{Kind: "http", Msg: "执行体的 ResponseWriter 不支持流式输出"})
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Sandbox-Backend", proc.Name())
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	up := make(chan struct{})
	go func() {
		defer close(up)
		_, _ = io.Copy(proc.Stdin, req.Body)
		// 上行关闭就是「不再发帧了」：常驻进程没有别的退出信号，
		// 这里必须主动杀，否则连接一直挂着、进程也一直占着池名额。
		proc.Kill()
	}()

	_, _ = io.Copy(flushWriter{w: w, f: flusher}, proc.Stdout)
	// 下行结束说明进程已退出：关掉 stdin 让上面的 goroutine 返回，
	// 否则它会一直阻塞在「客户端还没关写端」上，handler 永远不退出。
	if proc.Stdin != nil {
		_ = proc.Stdin.Close()
	}
	<-up
	// 远端拿不到本地 Proc.Stderr()：进程死前说了什么只能落在执行体日志里，
	// 否则控制面只会看到一个 「EOF」，排查时无从下手。
	if out := strings.TrimSpace(proc.Stderr()); out != "" {
		s.logger.Warn("常驻进程有错误输出", "stderr", out)
	}
}

// flushWriter 每写一次就刷出去：不带它的话响应体攒在 4KB 缓冲里，
// 插件回一帧、控制面要等下一帧才看得到。
type flushWriter struct {
	w io.Writer
	f http.Flusher
}

func (fw flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if err == nil {
		fw.f.Flush()
	}
	return n, err
}

// prepareEgress 在执行体这一侧按白名单建出网代理。
//
// 控制面建的 socket 在另一个容器的 /tmp 里，路径传过来只会 bind 失败，
// 所以这里必须重放「按名单建代理」这一步。Proxy 仍是路径：只是从
// 「控制面建好的那一份」变成「这条路径在本容器里由我建」。
func (s *server) prepareEgress(spec sandbox.Spec) (sandbox.Spec, error) {
	if spec.Egress == nil {
		// 没有白名单就不允许带任何代理路径进来：执行体不该替控制面
		// 打开一条它没声明过的通道。
		spec.Proxy = ""
		return spec, nil
	}
	if !egress.IsSocketPath(spec.Proxy) {
		return spec, fmt.Errorf("出网代理路径 %q 不在允许的位置", spec.Proxy)
	}
	if s.egress == nil {
		return spec, errors.New("执行体未启用出网代理，无法满足插件的出网声明")
	}
	digest := egress.Digest(spec.Egress.Hosts, spec.Egress.WSHosts, spec.Egress.Verbs)
	path, err := s.egress.Ensure(spec.Proxy, egress.Options{
		Policy:       egress.NewAllowlist(spec.Egress.Hosts),
		ConnectHosts: spec.Egress.WSHosts,
		Verbs:        spec.Egress.Verbs,
		Logger:       s.logger,
		OnEvent: func(ev egress.Event) {
			if !ev.Allowed {
				s.logger.Warn("插件出网被拒", "host", ev.Host, "reason", ev.Reason, "err", ev.Err)
			}
		},
	}, digest)
	if err != nil {
		return spec, err
	}
	spec.Proxy = path
	return spec, nil
}

// checkPaths 保证请求里的路径不跳出存储根。
func (s *server) checkPaths(spec sandbox.Spec) error {
	if s.root == "" || s.root == "." {
		return nil
	}
	check := func(p string) error {
		if p == "" {
			return nil
		}
		clean := filepath.Clean(p)
		if clean == s.root || strings.HasPrefix(clean, s.root+string(filepath.Separator)) {
			return nil
		}
		return fmt.Errorf("路径 %q 不在执行体允许的根目录 %q 之下", p, s.root)
	}
	if err := check(spec.Dir); err != nil {
		return err
	}
	for _, p := range spec.FS.Write {
		if err := check(p); err != nil {
			return err
		}
	}
	for _, p := range spec.FS.Read {
		if err := check(p); err != nil {
			return err
		}
	}
	return nil
}

func classify(err error) *wireError {
	switch {
	case errors.Is(err, sandbox.ErrTimeout):
		return &wireError{Kind: "timeout", Msg: err.Error()}
	case errors.Is(err, sandbox.ErrOutputTooLarge):
		return &wireError{Kind: "output_too_large", Msg: err.Error()}
	default:
		return &wireError{Kind: "other", Msg: err.Error()}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
