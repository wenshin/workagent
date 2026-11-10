package search

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Spec 是「声明式搜索源」：请求怎么拼、响应怎么取，全写在插件的 search.json 里。
//
// 为什么需要它：REST 搜索 API 没有统一规范——博查是 data.webPages.value[]，
// Tavily 是 results[]，Serper 是 organic[]，字段名也各不相同。但差异几乎都能归结为
// 「请求参数名」和「响应字段路径」两件事，于是接新搜索源不必写 Go、不必发版。
//
// 覆盖不到的少数情况（游标分页、非 JSON 响应、请求签名）走脚本源（kind=source,
// protocol=script），两者共用同一套注册与出网边界。
type Spec struct {
	Request  RequestSpec  `json:"request"`
	Response ResponseSpec `json:"response"`
}

// RequestSpec 描述如何构造请求。
type RequestSpec struct {
	// Method 只支持 GET / POST，省略按 POST 处理
	Method string `json:"method"`
	// Headers 是固定请求头，Content-Type 默认 application/json
	Headers map[string]string `json:"headers"`
	// Auth 决定密钥放在哪；为空表示这个源不需要密钥
	Auth *AuthSpec `json:"auth"`
	// Body 是 POST 的 JSON 模板，字符串里的 {{query}} {{limit}} {{freshness}} {{secret}} 会被替换
	Body map[string]any `json:"body"`
	// Params 是 GET 的查询参数模板，占位符规则同上
	Params map[string]string `json:"params"`
}

// AuthSpec 描述密钥的携带方式。
type AuthSpec struct {
	// In 取值 header / query / body
	In string `json:"in"`
	// Name 是头名、参数名或 body 字段名
	Name string `json:"name"`
	// Prefix 是前缀，如 "Bearer "（注意尾随空格）；为空表示裸密钥
	Prefix string `json:"prefix"`
}

// PathList 是一组候选字段路径，按顺序取第一个「有内容」的值。
//
// 写成单个字符串或字符串数组都行：摘要字段在哪家 API 里叫什么、有没有值并不稳定
// （博查开了 summary 就把正文放 summary，snippet 可能是空的），只取一个字段会静默
// 丢掉内容。数组形式让「回退」成为配置的一部分，而不是在 Go 里给某个厂商写特例。
type PathList []string

// UnmarshalJSON 同时接受 "a.b" 与 ["a.b", "a.c"]；空字符串视为「不取这个字段」。
func (p *PathList) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		if strings.TrimSpace(one) == "" {
			*p = nil
			return nil
		}
		*p = PathList{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return fmt.Errorf("字段路径只能是字符串或字符串数组，收到 %s", string(b))
	}
	out := make(PathList, 0, len(many))
	for _, m := range many {
		if strings.TrimSpace(m) != "" {
			out = append(out, m)
		}
	}
	*p = out
	return nil
}

// ResponseSpec 描述从响应 JSON 里取哪些字段。
//
// 路径只支持「对象键 + 数组下标」，如 data.webPages.value 或 results[0].title。
type ResponseSpec struct {
	// Items 是结果数组路径，必填；给了多个时取第一个能解析成数组的
	Items PathList `json:"items"`
	Title PathList `json:"title"`
	// URL 必填：没有 URL 的结果无法回溯，直接丢弃
	URL         PathList `json:"url"`
	Snippet     PathList `json:"snippet"`
	PublishedAt PathList `json:"published_at"`
	// ErrorCode 可选：响应里的业务错误码字段；取到的值不在 ErrorOK 里即视为失败
	ErrorCode    string `json:"error_code"`
	ErrorOK      []any  `json:"error_ok"`
	ErrorMessage string `json:"error_message"`
}

// Validate 在启动时检查配置，写错就启动失败，而不是等到第一次搜索才报。
func (s Spec) Validate() error {
	method := strings.ToUpper(strings.TrimSpace(s.Request.Method))
	if method != "" && method != http.MethodGet && method != http.MethodPost {
		return fmt.Errorf("request.method 只支持 GET / POST，收到 %q", s.Request.Method)
	}
	if a := s.Request.Auth; a != nil {
		switch strings.ToLower(strings.TrimSpace(a.In)) {
		case "header", "query", "body":
		default:
			return fmt.Errorf("request.auth.in 只支持 header / query / body，收到 %q", a.In)
		}
		if strings.TrimSpace(a.Name) == "" {
			return fmt.Errorf("request.auth.name 不能为空")
		}
	}
	if len(s.Response.Items) == 0 {
		return fmt.Errorf("response.items 不能为空：不知道去哪取结果数组")
	}
	if len(s.Response.URL) == 0 {
		return fmt.Errorf("response.url 不能为空：没有 URL 的结果无法回溯")
	}
	return nil
}

// needsSecret 判断这个源是否需要取密钥：要么配了 auth，要么模板里显式引用了 {{secret}}。
func (s Spec) needsSecret() bool {
	if s.Request.Auth != nil {
		return true
	}
	for _, v := range s.Request.Headers {
		if strings.Contains(v, "{{secret}}") {
			return true
		}
	}
	for _, v := range s.Request.Params {
		if strings.Contains(v, "{{secret}}") {
			return true
		}
	}
	return containsSecret(s.Request.Body)
}

func containsSecret(v any) bool {
	switch x := v.(type) {
	case string:
		return strings.Contains(x, "{{secret}}")
	case map[string]any:
		for _, item := range x {
			if containsSecret(item) {
				return true
			}
		}
	case []any:
		for _, item := range x {
			if containsSecret(item) {
				return true
			}
		}
	}
	return false
}

// expand 把模板里的占位符替换成实际值。
// 整个字符串正好是占位符时保留原始类型（{{limit}} 要变成数字 123，而不是 "123"）。
func expand(tpl any, vars map[string]any) any {
	switch x := tpl.(type) {
	case string:
		// 整个字符串正好是占位符时保留原始类型（{{limit}} → 数字），
		// 混在句子里时按字符串替换。
		for k, v := range vars {
			if x == "{{"+k+"}}" {
				return v
			}
		}
		return expandString(x, vars)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, v := range x {
			out[k] = expand(v, vars)
		}
		return out
	case []any:
		out := make([]any, 0, len(x))
		for _, v := range x {
			out = append(out, expand(v, vars))
		}
		return out
	default:
		return tpl
	}
}

func expandString(tpl string, vars map[string]any) string {
	out := tpl
	for k, v := range vars {
		out = strings.ReplaceAll(out, "{{"+k+"}}", fmt.Sprint(v))
	}
	return out
}

// lookup 按 "a.b[0].c" 从解析后的 JSON 里取值，取不到返回 nil。
func lookup(root any, path string) any {
	cur := root
	for _, seg := range strings.Split(strings.TrimSpace(path), ".") {
		if seg == "" || cur == nil {
			return nil
		}
		key := seg
		var indexes []int
		if i := strings.IndexByte(seg, '['); i >= 0 {
			key = seg[:i]
			for _, part := range strings.Split(strings.TrimSuffix(seg[i:], "]"), "][") {
				n, err := strconv.Atoi(strings.Trim(part, "[] "))
				if err != nil {
					return nil
				}
				indexes = append(indexes, n)
			}
		}
		if key != "" {
			m, ok := cur.(map[string]any)
			if !ok {
				return nil
			}
			cur = m[key]
		}
		for _, n := range indexes {
			arr, ok := cur.([]any)
			if !ok || n < 0 || n >= len(arr) {
				return nil
			}
			cur = arr[n]
		}
	}
	return cur
}

// lookupFirst 按候选顺序取第一个「有内容」的值：空字符串与 null 都算没取到。
// 这是多字段回退的唯一实现点，Items / Title / URL / Snippet / PublishedAt 共用。
func lookupFirst(root any, paths PathList) any {
	for _, p := range paths {
		v := lookup(root, p)
		if v == nil {
			continue
		}
		if s, ok := v.(string); ok && strings.TrimSpace(s) == "" {
			continue
		}
		return v
	}
	return nil
}

// text 把 JSON 值转成字符串；数字按整数输出，避免 2024 变成 2024.000000。
func text(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return ""
		}
		return string(b)
	}
}
