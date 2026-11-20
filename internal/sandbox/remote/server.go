package remote

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/workagent/auto-agent/internal/sandbox"
)

// Handler 是执行体对控制面暴露的接口。
//
// root 非空时只允许在 root 之下执行：执行体是特权容器，令牌泄露等于节点级
// 代码执行，所以在协议层再收一道「只能在存储根下跑」。真正的边界仍然是
// bwrap 的挂载白名单，这里防的是误用与越界路径。
func Handler(r sandbox.Runner, token, root string, facts sandbox.EnvFacts, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	s := &server{runner: r, token: token, root: filepath.Clean(root), facts: facts, logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc(healthPath, s.health)
	mux.HandleFunc(readyPath, s.auth(s.ready))
	mux.HandleFunc(runPath, s.auth(s.run))
	return mux
}

type server struct {
	runner sandbox.Runner
	token  string
	root   string
	facts  sandbox.EnvFacts
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

	res, err := s.runner.Run(req.Context(), in.Spec)
	out := runResponse{Result: res}
	if err != nil {
		out.Error = classify(err)
	}
	writeJSON(w, http.StatusOK, out)
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
