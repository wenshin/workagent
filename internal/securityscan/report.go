// Package securityscan 在插件发布前做静态检查：提示词风险、硬编码凭证、
// 危险代码，以及依赖的已知漏洞（CVE）。
//
// 设计约束见 docs/插件安全基线.md：
//   - 规则是外部数据，支持热加载，改规则不重启服务；
//   - 规则加载失败保留旧规则，绝不因为规则文件坏掉而阻断发布；
//   - 依赖扫描不可用（无外网 / 无 lockfile）时标记 unscanned，同样不阻断；
//   - 扫描结果是元数据，不修改发布件。
package securityscan

import "time"

// Severity 是规则的有效级别。未知值按 low 处理，避免规则里拼错一个词
// 就把所有发布卡死。
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityMedium   Severity = "medium"
	SeverityLow      Severity = "low"
)

func (s Severity) rank() int {
	switch s {
	case SeverityCritical:
		return 4
	case SeverityHigh:
		return 3
	case SeverityMedium:
		return 2
	default:
		return 1
	}
}

// ParseSeverity 把外部输入（规则文件、OSV 的 database_specific.severity）
// 归一成已知级别；不认识的值一律降级，不抛错。
func ParseSeverity(v string) Severity {
	switch s := Severity(v); s {
	case SeverityCritical:
		return SeverityCritical
	case SeverityHigh:
		return SeverityHigh
	case SeverityMedium:
		return SeverityMedium
	default:
		return SeverityLow
	}
}

// 扫描状态。
const (
	// StatusPassed 扫描完成且没有阻断项。
	StatusPassed = "passed"
	// StatusBlocked 命中阻断规则，本次发布必须失败。
	StatusBlocked = "blocked"
	// StatusUnscanned 依赖扫描无法进行（无 lockfile / 无漏洞库 / 外网不可达）。
	// 这是「不知道」，不是「安全」，必须在界面里如实展示。
	StatusUnscanned = "unscanned"
	// StatusError 扫描器自身出错。按 fail-open 处理：不阻断发布。
	StatusError = "error"
)

// 规则与命中的类别。
const (
	KindPrompt = "prompt"
	KindSecret = "secret"
	KindCode   = "code"
	KindDep    = "dep"
)

// Finding 是一条命中。Snippet 会脱敏：凭证类不回显原文。
type Finding struct {
	RuleID   string   `json:"rule_id"`
	Kind     string   `json:"kind"`
	Severity Severity `json:"severity"`
	Blocking bool     `json:"blocking"`
	File     string   `json:"file,omitempty"`
	Line     int      `json:"line,omitempty"`
	Column   int      `json:"column,omitempty"`
	Snippet  string   `json:"snippet,omitempty"`
	Message  string   `json:"message"`
	Ref      string   `json:"ref,omitempty"`
	// 依赖类命中的附加信息。
	Package string `json:"package,omitempty"`
	Version string `json:"version,omitempty"`
	CVE     string `json:"cve,omitempty"`
	FixedIn string `json:"fixed_in,omitempty"`
}

// Report 是一个版本的一次扫描结论。
type Report struct {
	Version      string    `json:"version,omitempty"`
	Status       string    `json:"status"`
	MaxSeverity  Severity  `json:"max_severity,omitempty"`
	Findings     []Finding `json:"findings"`
	RulesVersion string    `json:"rules_version"`
	Deps         int       `json:"deps"`
	Lockfile     string    `json:"lockfile,omitempty"`
	ScannedAt    time.Time `json:"scanned_at"`
	// BlockedReason 是给人看的一句话总结。
	BlockedReason string `json:"blocked_reason,omitempty"`
	// DepError 记录依赖扫描失败的原因（此时 Status=unscanned）。
	DepError string `json:"dep_error,omitempty"`
}

// Blocked 表示本次结论会拒绝发布。
func (r Report) Blocked() bool { return r.Status == StatusBlocked }

// Summary 返回界面上一行能放下的摘要。
func (r Report) Summary() string {
	if r.Status == StatusBlocked {
		return r.BlockedReason
	}
	if r.Status == StatusUnscanned {
		return "依赖未扫描（" + r.DepError + "）"
	}
	return ""
}
