package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/workagent/auto-agent/internal/sandbox"
)

// 单段脚本的墙钟：两段加起来要明显短于工具层 30s 的调用预算。
const denoPhaseTimeout = 8 * time.Second

// denoMaxOutput 是脚本 stdout 的上限；正常输出只有几百字节，留足余量即可。
const denoMaxOutput = 512 << 10

// doer 是出网通道。生产中由 Guard 实现（仅 https、域名白名单、拨号期 IP 校验），
// 测试里可以换成假的，避免为了测映射逻辑去起一个真 HTTPS 服务。
type doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Deno 是脚本驱动的搜索源：请求怎么拼、响应怎么读，都写在外部脚本里。
//
// 之所以是两段式而不是「脚本自己 fetch」：出网必须留在 Go 侧的 Guard，
// 否则域名白名单、拨号期拒绝私网 IP、不跟随 30x 这些约束在脚本里全都要重做一遍，
// 而且做不对。所以脚本只做纯变换，拿不到网络权限：
//
//	plan：{phase,query,limit,endpoint,secret} → {request:{method,url,headers,body}}
//	map ：{phase,status,body}                → {results:[{title,url,snippet,published_at}]}
//
// 密钥只走 stdin：argv 会进 ps，环境变量会进 /proc/<pid>/environ。
type Deno struct {
	name     string
	script   string
	deno     string
	endpoint string
	keyFunc  func(context.Context) (string, error)
	client   doer
	shell    sandbox.Runner
}

// NewDeno 构造脚本搜索源。script 是 .ts/.js 文件的绝对路径（相对路径按当前目录解析）。
func NewDeno(name, script, endpoint string, keyFunc func(context.Context) (string, error), client doer, shell sandbox.Runner) (*Deno, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("搜索脚本缺少 provider 名")
	}
	if client == nil {
		return nil, fmt.Errorf("搜索脚本缺少出网通道")
	}
	if keyFunc == nil {
		return nil, fmt.Errorf("搜索脚本缺少密钥来源")
	}
	if shell == nil {
		return nil, fmt.Errorf("搜索脚本缺少执行沙箱")
	}
	abs, err := filepath.Abs(script)
	if err != nil {
		return nil, fmt.Errorf("解析脚本路径失败: %w", err)
	}
	if info, statErr := os.Stat(abs); statErr != nil || info.IsDir() {
		return nil, fmt.Errorf("搜索脚本不存在或不是文件: %s", abs)
	}
	// Deno 不在时直接报错，让装配层回落到 spec / 内置 provider，而不是等到第一次搜索才炸。
	denoPath, err := exec.LookPath("deno")
	if err != nil {
		return nil, fmt.Errorf("找不到 deno 可执行文件: %w", err)
	}
	return &Deno{
		name: name, script: abs, deno: denoPath, endpoint: endpoint,
		keyFunc: keyFunc, client: client, shell: shell,
	}, nil
}

func (d *Deno) Name() string { return d.name }

type denoPlanRequest struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

type denoPlanResponse struct {
	Request denoPlanRequest `json:"request"`
	Error   string          `json:"error"`
}

type denoMapResponse struct {
	Results []Result `json:"results"`
	Error   string   `json:"error"`
}

func (d *Deno) Search(ctx context.Context, req Request) ([]Result, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}

	// 密钥取不到不直接失败：有些源不需要密钥，让脚本自己决定要不要报错。
	secret, secretErr := d.keyFunc(ctx)
	if secretErr != nil {
		secret = ""
	}

	planIn := map[string]any{
		"phase":    "plan",
		"query":    req.Query,
		"limit":    limit,
		"endpoint": d.endpoint,
	}
	if secret != "" {
		planIn["secret"] = secret
	}
	if secretErr != nil {
		planIn["secret_error"] = secretErr.Error()
	}

	planOut, err := d.run(ctx, planIn, secret)
	if err != nil {
		return nil, err
	}
	var plan denoPlanResponse
	if err := json.Unmarshal(planOut, &plan); err != nil {
		return nil, fmt.Errorf("搜索脚本的 plan 输出无法解析: %w", err)
	}
	if plan.Error != "" {
		return nil, fmt.Errorf("搜索脚本拒绝了本次请求: %s", plan.Error)
	}
	if strings.TrimSpace(plan.Request.URL) == "" {
		return nil, fmt.Errorf("搜索脚本没有给出请求地址")
	}

	method := strings.ToUpper(strings.TrimSpace(plan.Request.Method))
	if method == "" {
		method = http.MethodPost
	}
	var body io.Reader
	if plan.Request.Body != "" {
		body = strings.NewReader(plan.Request.Body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, plan.Request.URL, body)
	if err != nil {
		return nil, fmt.Errorf("搜索脚本给出的请求无法构造: %w", err)
	}
	for k, v := range plan.Request.Headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := d.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("搜索请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("读取搜索响应失败: %w", err)
	}

	mapOut, err := d.run(ctx, map[string]any{
		"phase":  "map",
		"status": resp.StatusCode,
		"body":   string(raw),
	}, secret)
	if err != nil {
		return nil, err
	}
	var mapped denoMapResponse
	if err := json.Unmarshal(mapOut, &mapped); err != nil {
		return nil, fmt.Errorf("搜索脚本的 map 输出无法解析: %w", err)
	}
	if mapped.Error != "" {
		return nil, fmt.Errorf("搜索失败（HTTP %d）：%s", resp.StatusCode, mapped.Error)
	}
	return normalizeResults(mapped.Results, limit), nil
}

// run 执行一次脚本：JSON 进 stdin、JSON 出 stdout，全程在只读无网沙箱里。
func (d *Deno) run(ctx context.Context, in map[string]any, secret string) ([]byte, error) {
	payload, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("序列化脚本入参失败: %w", err)
	}
	res, err := d.shell.Run(ctx, sandbox.Spec{
		Cmd: []string{
			d.deno, "run",
			"--quiet", "--no-prompt",
			// 沙箱里没有可写的工作目录，锁文件与配置会变成噪音甚至失败
			"--no-lock", "--no-config",
			d.script,
		},
		Dir:       filepath.Dir(d.script),
		Stdin:     payload,
		Mode:      sandbox.ModeReadOnly,
		Network:   false,
		Timeout:   denoPhaseTimeout,
		MaxOutput: denoMaxOutput,
	})
	if err != nil {
		return nil, fmt.Errorf("搜索脚本执行失败: %w%s", err, stderrSuffix(res.Stderr, secret))
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("搜索脚本退出码 %d%s", res.ExitCode, stderrSuffix(res.Stderr, secret))
	}
	out := strings.TrimSpace(res.Stdout)
	if out == "" {
		return nil, fmt.Errorf("搜索脚本没有输出")
	}
	if !json.Valid([]byte(out)) {
		return nil, fmt.Errorf("搜索脚本输出不是合法 JSON: %s", maskSecret(clip(out, 300), secret))
	}
	return []byte(out), nil
}

// normalizeResults 做最后一道归一化：脚本写错字段名也不该把脏数据带给模型。
func normalizeResults(items []Result, limit int) []Result {
	out := make([]Result, 0, len(items))
	for _, item := range items {
		url := strings.TrimSpace(item.URL)
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			continue
		}
		title := strings.TrimSpace(item.Title)
		if title == "" {
			title = url
		}
		out = append(out, Result{
			Title:       clip(title, 300),
			URL:         clip(url, 2048),
			Snippet:     clip(strings.TrimSpace(item.Snippet), 2000),
			PublishedAt: clip(strings.TrimSpace(item.PublishedAt), 64),
			Source:      clip(strings.TrimSpace(item.Source), 64),
		})
		if len(out) >= limit {
			break
		}
	}
	return out
}

func stderrSuffix(stderr, secret string) string {
	s := strings.TrimSpace(stderr)
	if s == "" {
		return ""
	}
	return "（stderr: " + maskSecret(clip(s, 500), secret) + "）"
}

// maskSecret 抹掉错误信息里可能回显的密钥，避免它经审计或日志二次扩散。
func maskSecret(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "***")
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
