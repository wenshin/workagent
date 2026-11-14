package securityscan

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// 内置规则集：外部规则目录不存在时的兜底，保证开箱即可用。
//
//go:embed default_rules.json
var defaultRulesFS embed.FS

const defaultRulesName = "default_rules.json"

// activeFile 是规则目录里的指针文件，内容形如
//
//	{"version": "2026.11.1", "sha256": "..."}
//
// 用指针文件而不是直接读固定路径，是为了让「规则版本」可命名、可回滚：
// 换一个 version 就是换一套规则，旧目录留着即可回退。
const activeFile = "active.json"

// Rule 是一条扫描规则。Pattern 是 Go 正则，kind 决定它作用于哪些文件。
type Rule struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"` // prompt | secret | code
	Severity Severity `json:"severity"`
	// Blocking 为真时命中即拒绝发布；否则只出警告。
	Blocking bool   `json:"blocking"`
	Pattern  string `json:"pattern"`
	Message  string `json:"message"`
	Ref      string `json:"ref,omitempty"`
	// MaxPerFile 限制同一条规则在单个文件里的命中数，<=0 用默认值 5，
	// 避免一个文件里几百处同样的问题把报告刷爆。
	MaxPerFile int `json:"max_per_file,omitempty"`

	re *regexp.Regexp
}

// RuleSet 是一份完整的规则集。
type RuleSet struct {
	Version string `json:"version"`
	Rules   []Rule `json:"rules"`
}

// parseRuleSet 解析并编译规则集。任何一条规则坏掉都整份拒绝——
// 半份规则比没有规则更危险，因为没人知道漏了哪条。
func parseRuleSet(body []byte) (*RuleSet, error) {
	var rs RuleSet
	if err := json.Unmarshal(body, &rs); err != nil {
		return nil, fmt.Errorf("解析规则集失败: %w", err)
	}
	if strings.TrimSpace(rs.Version) == "" {
		return nil, fmt.Errorf("规则集缺少 version")
	}
	if len(rs.Rules) == 0 {
		return nil, fmt.Errorf("规则集为空")
	}
	seen := make(map[string]bool, len(rs.Rules))
	for i := range rs.Rules {
		r := &rs.Rules[i]
		if strings.TrimSpace(r.ID) == "" {
			return nil, fmt.Errorf("第 %d 条规则缺少 id", i+1)
		}
		if seen[r.ID] {
			return nil, fmt.Errorf("规则 id 重复: %s", r.ID)
		}
		seen[r.ID] = true
		switch r.Kind {
		case KindPrompt, KindSecret, KindCode:
		default:
			return nil, fmt.Errorf("规则 %s 的 kind 非法: %q（只能是 prompt / secret / code）", r.ID, r.Kind)
		}
		if strings.TrimSpace(r.Pattern) == "" {
			return nil, fmt.Errorf("规则 %s 缺少 pattern", r.ID)
		}
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return nil, fmt.Errorf("规则 %s 的正则无法编译: %w", r.ID, err)
		}
		r.re = re
		r.Severity = ParseSeverity(string(r.Severity))
		if r.MaxPerFile <= 0 {
			r.MaxPerFile = 5
		}
	}
	return &rs, nil
}

// DefaultRuleSet 返回内置规则集。内置规则解析失败属于编译期错误（规则随包发布），
// 调用方可以直接把 error 当成启动失败。
func DefaultRuleSet() (*RuleSet, error) {
	body, err := defaultRulesFS.ReadFile(defaultRulesName)
	if err != nil {
		return nil, fmt.Errorf("读取内置规则失败: %w", err)
	}
	return parseRuleSet(body)
}

// rulesSignature 给出规则目录的指纹：active.json 的内容指纹 + 被指向的
// rules.json 的 mtime / 大小。
//
// 两者都要，因为运维有两种改法：换指针（新版本）或就地改文件（既没有换
// 指针、sha256 也留空）。只看其中一个会漏掉另一种。
func rulesSignature(dir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, activeFile))
	if err != nil {
		return "", err
	}
	var ref activeRef
	if err := json.Unmarshal(raw, &ref); err != nil {
		return "", err
	}
	ref.Version = strings.TrimSpace(ref.Version)
	if ref.Version == "" || strings.ContainsAny(ref.Version, `/\`) {
		return "", fmt.Errorf("%s 的 version 非法: %q", activeFile, ref.Version)
	}
	st, err := os.Stat(filepath.Join(dir, ref.Version, "rules.json"))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]) + ":" +
		strconv.FormatInt(st.ModTime().UnixNano(), 10) + ":" +
		strconv.FormatInt(st.Size(), 10), nil
}

// activeRef 是 active.json 的内容。
type activeRef struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256,omitempty"`
}

// loadFromDir 按 active.json 的指向加载规则目录。
// 目录结构：<dir>/active.json + <dir>/<version>/rules.json。
func loadFromDir(dir string) (*RuleSet, error) {
	raw, err := os.ReadFile(filepath.Join(dir, activeFile))
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", activeFile, err)
	}
	var ref activeRef
	if err := json.Unmarshal(raw, &ref); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", activeFile, err)
	}
	ref.Version = strings.TrimSpace(ref.Version)
	if ref.Version == "" {
		return nil, fmt.Errorf("%s 缺少 version", activeFile)
	}
	// 版本号会被拼进路径，先挡掉路径穿越：规则目录是运维写入的，但不能因此
	// 允许它读到任意文件。
	if strings.ContainsAny(ref.Version, `/\`) || ref.Version == "." || ref.Version == ".." {
		return nil, fmt.Errorf("%s 的 version 非法: %q", activeFile, ref.Version)
	}

	body, err := os.ReadFile(filepath.Join(dir, ref.Version, "rules.json"))
	if err != nil {
		return nil, fmt.Errorf("读取规则 %s 失败: %w", ref.Version, err)
	}
	if ref.SHA256 != "" {
		sum := sha256.Sum256(body)
		if !strings.EqualFold(hex.EncodeToString(sum[:]), ref.SHA256) {
			return nil, fmt.Errorf("规则 %s 的 sha256 与 %s 不符（文件被改动或指针未更新）", ref.Version, activeFile)
		}
	}
	rs, err := parseRuleSet(body)
	if err != nil {
		return nil, err
	}
	// 以文件里声明的版本为准，避免 active.json 与 rules.json 版本不一致时
	// 报告里记错版本号。
	if rs.Version != ref.Version {
		return nil, fmt.Errorf("规则版本不一致：%s 指向 %s，但 rules.json 声明的是 %s",
			activeFile, ref.Version, rs.Version)
	}
	return rs, nil
}
