package securityscan

import (
	"context"
	"log/slog"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// 默认值。单文件上限按「插件里最大可能是提示词或脚本」估计，超过就不像插件了。
const (
	DefaultMaxFileBytes   = 512 << 10
	DefaultReloadInterval = 30 * time.Second
	maxSnippetRunes       = 120
)

// Config 是扫描器的装配参数。零值可用：只用内置规则、依赖扫描关闭。
type Config struct {
	// RulesDir 是外部规则目录（<dir>/active.json + <dir>/<version>/rules.json）。
	// 为空时只用内置规则。
	RulesDir string
	// OsvMode / OsvDir 见 osv.go：off / remote / local。
	OsvMode OsvMode
	OsvDir  string
	Logger  *slog.Logger
	// Allow 是被豁免的规则 id（误报治理）。豁免只影响阻断，不影响记录。
	Allow map[string]bool
	// MaxFileBytes 是单文件扫描上限，超过则跳过（不阻断）。
	MaxFileBytes int
	// ReloadInterval 是规则轮询周期。
	ReloadInterval time.Duration
	// HTTPTimeout 是远程查询漏洞库的超时。
	HTTPTimeout time.Duration
}

// Input 是一次扫描的输入：一个待发布目录的全部文件。
type Input struct {
	Slug    string
	Version string
	// Files 是相对路径 → 内容，与 plugin.Bundle.Files 一致。
	Files map[string][]byte
}

// Engine 是可热更新规则的扫描器。
//
// 并发约定：Scan 只读一次 atomic 指针，规则替换永远整体生效，
// 不会出现「一半新规则、一半旧规则」的扫描结果。
type Engine struct {
	cfg   Config
	rules atomic.Pointer[RuleSet]
	osv   *osvClient

	mu   sync.Mutex
	sig  string // 最近一次处理过的规则文件指纹
	warn bool   // 本指纹是否已经告警过（避免每 30s 刷一次日志）
}

// New 构造扫描器。规则目录不可用时只告警、回退内置规则——规则是一份可换的
// 数据，不能成为服务启动的依赖。
func New(cfg Config) (*Engine, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MaxFileBytes <= 0 {
		cfg.MaxFileBytes = DefaultMaxFileBytes
	}
	if cfg.ReloadInterval <= 0 {
		cfg.ReloadInterval = DefaultReloadInterval
	}
	if cfg.HTTPTimeout <= 0 {
		cfg.HTTPTimeout = 10 * time.Second
	}
	switch cfg.OsvMode {
	case OsvRemote, OsvLocal, OsvOff:
	case "":
		cfg.OsvMode = OsvOff
	default:
		cfg.Logger.Warn("未知的漏洞库模式，按关闭处理", "mode", string(cfg.OsvMode))
		cfg.OsvMode = OsvOff
	}

	builtin, err := DefaultRuleSet()
	if err != nil {
		return nil, err
	}

	e := &Engine{
		cfg: cfg,
		osv: newOsvClient(cfg.OsvMode, cfg.OsvDir, cfg.HTTPTimeout),
	}
	rs := builtin
	if cfg.RulesDir != "" {
		sig, _ := rulesSignature(cfg.RulesDir)
		if loaded, loadErr := loadFromDir(cfg.RulesDir); loadErr != nil {
			cfg.Logger.Warn("加载外部扫描规则失败，回退内置规则", "dir", cfg.RulesDir, "err", loadErr)
			e.sig, e.warn = sig, true
		} else {
			rs, e.sig = loaded, sig
			cfg.Logger.Info("已加载外部扫描规则", "version", loaded.Version, "dir", cfg.RulesDir)
		}
	}
	e.rules.Store(rs)
	return e, nil
}

// RulesVersion 返回当前生效的规则版本，用于写进扫描报告。
func (e *Engine) RulesVersion() string {
	if rs := e.rules.Load(); rs != nil {
		return rs.Version
	}
	return ""
}

// OsvMode 返回依赖扫描模式，调用方据此判断「未扫描」是否可避免。
func (e *Engine) OsvMode() OsvMode { return e.cfg.OsvMode }

// Reload 检查外部规则是否变化并热替换，返回是否真的换了。
//
// 失败语义：解析失败时保留旧规则并告警，绝不因为规则写坏而让发布全部失败。
// 同一份坏文件只告警一次，改过之后再试。
func (e *Engine) Reload() bool {
	if e.cfg.RulesDir == "" {
		return false
	}
	sig, err := rulesSignature(e.cfg.RulesDir)
	if err != nil {
		e.mu.Lock()
		first := !e.warn
		e.warn = true
		e.mu.Unlock()
		if first {
			e.cfg.Logger.Warn("扫描规则目录不可读，沿用当前规则", "dir", e.cfg.RulesDir, "err", err)
		}
		return false
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if sig == e.sig {
		return false
	}
	rs, err := loadFromDir(e.cfg.RulesDir)
	if err != nil {
		if !e.warn {
			e.cfg.Logger.Warn("扫描规则加载失败，保留旧规则", "dir", e.cfg.RulesDir, "err", err)
			e.warn = true
		}
		e.sig = sig
		return false
	}
	e.rules.Store(rs)
	e.sig, e.warn = sig, false
	e.cfg.Logger.Info("扫描规则已热更新", "version", rs.Version)
	return true
}

// Watch 按周期重载规则，直到 ctx 结束。
func (e *Engine) Watch(ctx context.Context) {
	if e.cfg.RulesDir == "" {
		return
	}
	t := time.NewTicker(e.cfg.ReloadInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.Reload()
		}
	}
}

// Scan 扫描一个待发布目录。
//
// 返回值永远是可用的报告：规则命中的阻断与依赖扫描的失败分开记录，
// 调用方只看 Report.Blocked() 决定是否拒绝发布。
func (e *Engine) Scan(ctx context.Context, in Input) Report {
	rs := e.rules.Load()
	rep := Report{
		Version:      in.Version,
		Status:       StatusPassed,
		RulesVersion: rs.Version,
		ScannedAt:    time.Now(),
		Findings:     []Finding{},
	}

	names := make([]string, 0, len(in.Files))
	for name := range in.Files {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		body := in.Files[name]
		if len(body) == 0 || len(body) > e.cfg.MaxFileBytes || isBinary(body) {
			continue
		}
		kinds := kindsFor(name)
		if len(kinds) == 0 {
			continue
		}
		text := string(body)
		for _, rule := range rs.Rules {
			if !kinds[rule.Kind] || rule.re == nil {
				continue
			}
			for _, m := range rule.re.FindAllStringIndex(text, rule.MaxPerFile) {
				line, col := position(text, m[0])
				rep.Findings = append(rep.Findings, Finding{
					RuleID:   rule.ID,
					Kind:     rule.Kind,
					Severity: rule.Severity,
					Blocking: rule.Blocking,
					File:     name,
					Line:     line,
					Column:   col,
					Snippet:  snippet(text, m[0], m[1], rule.Kind),
					Message:  rule.Message,
					Ref:      rule.Ref,
				})
			}
		}
	}

	depFindings, deps, lockfile, depErr := e.scanDeps(ctx, in.Files)
	rep.Findings = append(rep.Findings, depFindings...)
	rep.Deps, rep.Lockfile = deps, lockfile
	if depErr != nil {
		rep.Status = StatusUnscanned
		rep.DepError = depErr.Error()
	}

	// 豁免只影响阻断：命中仍然记录，方便事后判断豁免是否还合理。
	var blocking []Finding
	for _, f := range rep.Findings {
		if f.Severity.rank() > rep.MaxSeverity.rank() {
			rep.MaxSeverity = f.Severity
		}
		if f.Blocking && !e.cfg.Allow[f.RuleID] {
			blocking = append(blocking, f)
		}
	}
	if len(blocking) > 0 {
		rep.Status = StatusBlocked
		rep.BlockedReason = blockReason(blocking)
	}
	return rep
}

// scanDeps 见 osv.go；这里只做「没有 lockfile 就跳过」的判断。
func (e *Engine) scanDeps(ctx context.Context, files map[string][]byte) ([]Finding, int, string, error) {
	lock, name := findLockfile(files)
	if name == "" {
		return nil, 0, "", errNoLockfile
	}
	deps, err := parseLockfile(name, lock)
	if err != nil {
		return nil, 0, name, err
	}
	if len(deps) == 0 {
		return nil, 0, name, nil
	}
	findings, err := e.osv.query(ctx, deps)
	if err != nil {
		return nil, len(deps), name, err
	}
	return findings, len(deps), name, nil
}

// blockReason 给出一句能直接展示给发布者的话：只列前三条，避免弹窗爆炸。
func blockReason(fs []Finding) string {
	parts := make([]string, 0, 3)
	seen := make(map[string]bool, len(fs))
	for _, f := range fs {
		key := f.RuleID
		if f.File != "" {
			key += "@" + f.File
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		if f.CVE != "" {
			parts = append(parts, f.CVE+"（"+f.Package+"）")
		} else {
			parts = append(parts, f.RuleID+"（"+f.File+"）")
		}
		if len(parts) == 3 {
			break
		}
	}
	more := ""
	if len(fs) > len(parts) {
		more = " 等 " + itoa(len(fs)) + " 项"
	}
	return "发布被安全扫描阻断：" + strings.Join(parts, "、") + more
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// kindsFor 决定一个文件要跑哪几类规则。
//
// secret 对所有文本文件生效（密钥可能写在 .md、.json、.ts 任何地方）；
// prompt 只扫文档；code 扫代码与配置。
func kindsFor(name string) map[string]bool {
	ext := strings.ToLower(path.Ext(name))
	out := map[string]bool{KindSecret: true}
	switch ext {
	case ".md", ".markdown", ".txt", ".mdx":
		out[KindPrompt] = true
	case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs",
		".json", ".yaml", ".yml", ".sh", ".toml":
		out[KindCode] = true
	default:
		// 未知扩展名（.env、LICENSE 等）只跑凭证规则
	}
	return out
}

// isBinary 用前 512 字节判断是否二进制：规则是文本正则，二进制只会浪费 CPU。
func isBinary(b []byte) bool {
	n := len(b)
	if n > 512 {
		n = 512
	}
	for i := 0; i < n; i++ {
		if b[i] == 0 {
			return true
		}
	}
	return false
}

// position 把字节偏移换算成 1 起始的行列号（列按 rune 计数）。
func position(text string, off int) (int, int) {
	if off > len(text) {
		off = len(text)
	}
	line := 1 + strings.Count(text[:off], "\n")
	last := strings.LastIndex(text[:off], "\n")
	col := utf8.RuneCountInString(text[last+1:off]) + 1
	return line, col
}

// snippet 截取命中所在行作为上下文。凭证类的命中不回显原文——
// 报告会被存进数据库、显示在界面上，不能把密钥再抄一份。
func snippet(text string, start, end int, kind string) string {
	if kind == KindSecret {
		return "已隐藏（命中 " + itoa(end-start) + " 字节）"
	}
	lineStart := strings.LastIndex(text[:start], "\n") + 1
	lineEnd := len(text)
	if i := strings.Index(text[end:], "\n"); i >= 0 {
		lineEnd = end + i
	}
	s := strings.TrimSpace(text[lineStart:lineEnd])
	if utf8.RuneCountInString(s) > maxSnippetRunes {
		r := []rune(s)
		s = string(r[:maxSnippetRunes]) + "…"
	}
	return s
}
