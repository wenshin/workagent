package securityscan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// OsvMode 决定漏洞数据从哪来。
//
// 私有化部署的默认是 local：没有外网时 remote 一定失败，而「扫不了」不能
// 变成「发不了」，所以本地镜像是一等公民，不是降级方案。
type OsvMode string

const (
	// OsvOff 关闭依赖扫描，报告状态为 unscanned。
	OsvOff OsvMode = "off"
	// OsvRemote 直接查 api.osv.dev（需要出网）。
	OsvRemote OsvMode = "remote"
	// OsvLocal 查本地 OSV 镜像目录（*.json，OSV 官方格式）。
	OsvLocal OsvMode = "local"
)

var (
	// errNoLockfile 表示包里没有 lockfile：没有依赖清单就无从扫描。
	errNoLockfile = errors.New("包内没有 lockfile，无法生成依赖清单")
)

const osvBatchURL = "https://api.osv.dev/v1/querybatch"

// Dep 是一个待查依赖。
type Dep struct {
	Name    string
	Version string
}

// findLockfile 按优先级找 lockfile。只认包根目录，不递归——
// 依赖必须由发布者固化在包根，不能藏在子目录里躲过扫描。
func findLockfile(files map[string][]byte) ([]byte, string) {
	for _, name := range []string{"deno.lock", "package-lock.json", "npm-shrinkwrap.json"} {
		if body, ok := files[name]; ok && len(body) > 0 {
			return body, name
		}
	}
	return nil, ""
}

func parseLockfile(name string, body []byte) ([]Dep, error) {
	if name == "deno.lock" {
		return parseDenoLock(body)
	}
	return parsePackageLock(body)
}

// parseDenoLock 解析 deno.lock 的 npm 段。
//
// 兼容 key 的三种写法：lodash@4.17.21、/lodash@4.17.21、npm:lodash@4.17.21；
// scope 包（@scope/pkg@1.0.0）用最后一个 @ 切分。
func parseDenoLock(body []byte) ([]Dep, error) {
	var lf struct {
		Npm map[string]json.RawMessage `json:"npm"`
	}
	if err := json.Unmarshal(body, &lf); err != nil {
		return nil, fmt.Errorf("解析 deno.lock 失败: %w", err)
	}
	out := make([]Dep, 0, len(lf.Npm))
	for key := range lf.Npm {
		key = strings.TrimPrefix(strings.TrimPrefix(key, "/"), "npm:")
		i := strings.LastIndex(key, "@")
		if i <= 0 || i == len(key)-1 {
			continue
		}
		if name := key[:i]; name != "" {
			out = append(out, Dep{Name: name, Version: key[i+1:]})
		}
	}
	return dedupDeps(out), nil
}

// parsePackageLock 解析 package-lock v2/v3（packages）与 v1（dependencies）。
// dev 依赖跳过：它们不进发布件，报出来只是噪音。
func parsePackageLock(body []byte) ([]Dep, error) {
	type entry struct {
		Version string `json:"version"`
		Dev     bool   `json:"dev"`
	}
	var lf struct {
		Packages     map[string]entry `json:"packages"`
		Dependencies map[string]entry `json:"dependencies"`
	}
	if err := json.Unmarshal(body, &lf); err != nil {
		return nil, fmt.Errorf("解析 package-lock.json 失败: %w", err)
	}

	out := make([]Dep, 0, len(lf.Packages))
	if len(lf.Packages) > 0 {
		for key, p := range lf.Packages {
			if p.Dev || p.Version == "" {
				continue
			}
			// 嵌套依赖的 key 形如 node_modules/a/node_modules/b，
			// 真实包名取最后一个 node_modules/ 之后的部分。
			idx := strings.LastIndex(key, "node_modules/")
			if idx < 0 {
				continue // 根包 / 工作区条目，不是依赖
			}
			name := key[idx+len("node_modules/"):]
			if name == "" {
				continue
			}
			out = append(out, Dep{Name: name, Version: p.Version})
		}
	} else {
		for name, d := range lf.Dependencies {
			if d.Dev || d.Version == "" {
				continue
			}
			out = append(out, Dep{Name: name, Version: d.Version})
		}
	}
	return dedupDeps(out), nil
}

func dedupDeps(in []Dep) []Dep {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, d := range in {
		key := d.Name + "@" + d.Version
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Version < out[j].Version
	})
	return out
}

// osvVuln 是 OSV 记录的裁剪版，只保留报告要用的字段。
type osvVuln struct {
	ID      string   `json:"id"`
	Aliases []string `json:"aliases"`
	Summary string   `json:"summary"`
	Details string   `json:"details"`
	// 已解析的严重度（远程响应的 database_specific.severity）。
	Severity         []struct{ Type, Score string } `json:"severity"`
	Affected         []osvAffected                  `json:"affected"`
	DatabaseSpecific struct {
		Severity string `json:"severity"`
	} `json:"database_specific"`
}

type osvAffected struct {
	Package  struct{ Name, Ecosystem string } `json:"package"`
	Ranges   []osvRange                       `json:"ranges"`
	Versions []string                         `json:"versions"`
}

type osvRange struct {
	Type   string `json:"type"`
	Events []struct {
		Introduced string `json:"introduced"`
		Fixed      string `json:"fixed"`
	} `json:"events"`
}

// osvClient 查漏洞库。remote / local 两种模式共用后面的匹配与报告构造。
type osvClient struct {
	mode    OsvMode
	dir     string
	http    *http.Client
	lang    string // 目标生态，目前只有 npm（deno 的依赖也是 npm 包）
	mu      sync.Mutex
	local   map[string][]osvVuln
	loading bool
}

func newOsvClient(mode OsvMode, dir string, timeout time.Duration) *osvClient {
	return &osvClient{
		mode: mode,
		dir:  dir,
		http: &http.Client{Timeout: timeout},
		lang: "npm",
	}
}

func (c *osvClient) query(ctx context.Context, deps []Dep) ([]Finding, error) {
	switch c.mode {
	case OsvRemote:
		return c.queryRemote(ctx, deps)
	case OsvLocal:
		return c.queryLocal(ctx, deps)
	default:
		return nil, errors.New("依赖扫描未启用（SCAN_OSV_MODE=off）")
	}
}

func (c *osvClient) queryRemote(ctx context.Context, deps []Dep) ([]Finding, error) {
	type pkg struct{ Name, Ecosystem string }
	reqBody := struct {
		Queries []struct {
			Package pkg    `json:"package"`
			Version string `json:"version"`
		} `json:"queries"`
	}{}
	reqBody.Queries = make([]struct {
		Package pkg    `json:"package"`
		Version string `json:"version"`
	}, 0, len(deps))
	for _, d := range deps {
		reqBody.Queries = append(reqBody.Queries, struct {
			Package pkg    `json:"package"`
			Version string `json:"version"`
		}{Package: pkg{Name: d.Name, Ecosystem: c.lang}, Version: d.Version})
	}

	raw, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, osvBatchURL, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("查询漏洞库失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("读取漏洞库响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("漏洞库返回 %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var out struct {
		Results []struct {
			Vulns []osvVuln `json:"vulns"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("解析漏洞库响应失败: %w", err)
	}
	if len(out.Results) != len(deps) {
		return nil, fmt.Errorf("漏洞库返回条数与请求不一致（%d != %d）", len(out.Results), len(deps))
	}

	findings := make([]Finding, 0, 8)
	for i, r := range out.Results {
		for _, v := range r.Vulns {
			findings = append(findings, buildDepFinding(deps[i], v))
		}
	}
	return findings, nil
}

// queryLocal 从本地镜像目录读取漏洞记录。文件可以是单个对象或数组。
func (c *osvClient) queryLocal(ctx context.Context, deps []Dep) ([]Finding, error) {
	index, err := c.loadLocal()
	if err != nil {
		return nil, err
	}
	findings := make([]Finding, 0, 8)
	for _, d := range deps {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, v := range index[d.Name] {
			if !v.affects(d.Version) {
				continue
			}
			findings = append(findings, buildDepFinding(d, v))
		}
	}
	return findings, nil
}

func (c *osvClient) loadLocal() (map[string][]osvVuln, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.local != nil {
		return c.local, nil
	}
	if strings.TrimSpace(c.dir) == "" {
		return nil, errors.New("未配置本地漏洞库目录（SCAN_OSV_DIR）")
	}
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return nil, fmt.Errorf("读取本地漏洞库失败: %w", err)
	}
	index := make(map[string][]osvVuln, 1024)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(c.dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("读取 %s 失败: %w", e.Name(), err)
		}
		var vulns []osvVuln
		trimmed := bytes.TrimSpace(body)
		if len(trimmed) > 0 && trimmed[0] == '[' {
			if err := json.Unmarshal(trimmed, &vulns); err != nil {
				continue // 坏文件跳过：一份镜像里混进坏数据不该让整体不可用
			}
		} else {
			var one osvVuln
			if err := json.Unmarshal(trimmed, &one); err != nil {
				continue
			}
			vulns = []osvVuln{one}
		}
		for _, v := range vulns {
			for _, a := range v.Affected {
				if a.Package.Ecosystem != "" && a.Package.Ecosystem != "npm" {
					continue
				}
				if a.Package.Name != "" {
					index[a.Package.Name] = append(index[a.Package.Name], v)
				}
			}
		}
	}
	c.local = index
	return index, nil
}

// affects 判断某个版本是否受影响：先看显式版本列表，再看范围。
func (v osvVuln) affects(version string) bool {
	for _, a := range v.Affected {
		for _, ver := range a.Versions {
			if ver == version {
				return true
			}
		}
		for _, r := range a.Ranges {
			introduced, fixed := "", ""
			for _, ev := range r.Events {
				if ev.Introduced != "" {
					introduced = ev.Introduced
				}
				if ev.Fixed != "" {
					fixed = ev.Fixed
				}
			}
			if introduced == "" || introduced == "0" {
				if fixed == "" || cmpVersion(version, fixed) < 0 {
					return true
				}
				continue
			}
			if cmpVersion(version, introduced) >= 0 && (fixed == "" || cmpVersion(version, fixed) < 0) {
				return true
			}
		}
	}
	return false
}

// buildDepFinding 把一条 OSV 记录转成报告条目。
//
// 阻断口径故意收窄：高危以上**且**有修复版本才阻断。没有修复版本的漏洞
// 阻断发布也解决不了问题，只会让所有人学会关掉扫描。
func buildDepFinding(dep Dep, v osvVuln) Finding {
	f := Finding{
		RuleID:   "DEP-" + v.ID,
		Kind:     KindDep,
		Severity: ParseSeverity(v.DatabaseSpecific.Severity),
		Package:  dep.Name,
		Version:  dep.Version,
		Message:  strings.TrimSpace(v.Summary),
	}
	if f.Message == "" {
		f.Message = "依赖存在已知漏洞"
	}
	for _, a := range v.Aliases {
		if strings.HasPrefix(a, "CVE-") {
			f.CVE = a
			break
		}
	}
	for _, af := range v.Affected {
		for _, r := range af.Ranges {
			for _, ev := range r.Events {
				if ev.Fixed != "" && f.FixedIn == "" {
					f.FixedIn = ev.Fixed
				}
			}
		}
	}
	if f.Severity == SeverityLow && f.CVE == "" && v.DatabaseSpecific.Severity == "" {
		// 库里没有严重度信息：按中危记录，不阻断。
		f.Severity = SeverityMedium
	}
	f.Blocking = f.Severity.rank() >= SeverityHigh.rank() && f.FixedIn != ""
	return f
}

// cmpVersion 是够用的 semver 比较：按 . 和 - 切段逐段比数字，
// 非数字段退化成字符串比较。漏洞范围判断不需要完整 semver 语义。
func cmpVersion(a, b string) int {
	if a == b {
		return 0
	}
	as := splitVersion(a)
	bs := splitVersion(b)
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y string
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		xn, xErr := strconv.Atoi(x)
		yn, yErr := strconv.Atoi(y)
		switch {
		case xErr == nil && yErr == nil:
			if xn != yn {
				if xn < yn {
					return -1
				}
				return 1
			}
		default:
			if x != y {
				if x < y {
					return -1
				}
				return 1
			}
		}
	}
	return 0
}

func splitVersion(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '-' || r == '+' })
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
