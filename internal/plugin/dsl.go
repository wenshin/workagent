package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// 组件 DSL：小程序式声明式 UI。
//
// 组件不是可执行代码：`ui.json` 只描述「用哪些平台组件、绑哪个数据源」，
// 由前端宿主原生渲染。这样组件拿不到 window / cookie / 网络，XSS 面收敛到
// React 的文本转义；平台也不需要 iframe 或 Shadow DOM 这种重隔离手段。
//
// 与 plugin.json 的分工：能调哪些工具只为 data_sources（清单）说了算，
// DSL 只能引用已声明的 id，不能自己写工具名或接口地址。
const (
	// DSLVersion 是组件 DSL 的版本，不兼容变更才升版本。
	DSLVersion = "wa-ui/1"
	// DSLFile 是组件 DSL 的默认入口文件。
	DSLFile = "ui.json"

	maxDSLBytes     = 64 << 10
	maxDSLNodes     = 300
	maxDSLDepth     = 8
	maxDSLTextRunes = 2000
	maxDSLStateKeys = 20
	minDSLHeight    = 120
	maxDSLHeight    = 2000
)

// dslNodeFields 是每种节点的允许字段。多写一个字段直接拒绝，避免
// 「看起来生效了、其实被忽略」——组件作者与模型拿到的语义必须一致。
var dslNodeFields = map[string]map[string]bool{
	"Stack":    fieldSet("type", "gap", "children", "when"),
	"Row":      fieldSet("type", "gap", "align", "children", "when"),
	"Grid":     fieldSet("type", "columns", "gap", "children", "when"),
	"Card":     fieldSet("type", "title", "children", "when"),
	"Divider":  fieldSet("type", "when"),
	"Heading":  fieldSet("type", "text", "level", "when"),
	"Text":     fieldSet("type", "text", "size", "tone", "when"),
	"Badge":    fieldSet("type", "text", "tone", "when"),
	"Alert":    fieldSet("type", "title", "text", "tone", "when"),
	"Stat":     fieldSet("type", "label", "value", "delta", "tone", "when"),
	"KeyValue": fieldSet("type", "items", "when"),
	"List":     fieldSet("type", "source", "empty", "when"),
	"Table":    fieldSet("type", "columns", "source", "empty", "when"),
	"Progress": fieldSet("type", "label", "value", "max", "tone", "when"),
	"Button":   fieldSet("type", "text", "tone", "action", "when"),
	"Input":    fieldSet("type", "name", "label", "placeholder", "input_type", "required", "when"),
	"Select":   fieldSet("type", "name", "label", "options", "options_from", "multiple", "max", "label_name", "placeholder", "empty", "required", "when"),
	"Tabs":     fieldSet("type", "name", "options", "when"),
}

var (
	dslGapSet       = map[string]bool{"sm": true, "md": true, "lg": true}
	dslAlignSet     = map[string]bool{"start": true, "center": true, "end": true, "between": true}
	dslSizeSet      = map[string]bool{"sm": true, "md": true, "lg": true}
	dslToneSet      = map[string]bool{"default": true, "muted": true, "success": true, "warning": true, "danger": true, "info": true}
	dslOpSet        = map[string]bool{"eq": true, "ne": true, "gt": true, "lt": true, "in": true, "exists": true, "truthy": true, "empty": true}
	dslInputTypeSet = map[string]bool{"text": true, "number": true, "date": true}
	dslStateKey     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	dslSegment      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$|^[0-9]+$`)
	maxDSLSelectMax = 50
)

// dslOptionsFrom 让下拉的候选来自数据源而不是字面量。
//
// source 必须同时出现在 ui.json 的 data 段里——取数只有那一条管线，
// 只写在这里等于「声明了但没人加载」，渲染出来是空下拉且 dry-run 不报错。
type dslOptionsFrom struct {
	Source string `json:"source"`
	Items  string `json:"items,omitempty"`
	Label  string `json:"label"`
	Value  string `json:"value"`
	// Search 把下拉自带的搜索框接到某个 state 键，配合 data.watch 做远程搜索。
	// 不写就是一次性加载 + 前端过滤（候选 ≤200 时够用）。
	Search string `json:"search,omitempty"`
}

// dslCtx 是节点校验需要横穿的上下文：数据源声明、data 段、state 初值。
type dslCtx struct {
	sources map[string]DataSource
	data    map[string]dslQuery
	state   map[string]any
}

func fieldSet(keys ...string) map[string]bool {
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		out[k] = true
	}
	return out
}

type dslQuery struct {
	Params map[string]any `json:"params,omitempty"`
	Auto   *bool          `json:"auto,omitempty"`
	// Watch 里的 state 键变化后重新取数（搜索框、级联筛选）。
	Watch []string `json:"watch,omitempty"`
	// Reset 在 Watch 触发重取前清空指定 state 键，避免上游变了还留着下游的旧值。
	Reset []string `json:"reset,omitempty"`
}

// ValidateDSL 校验组件 DSL，返回渲染高度。
//
// sources 是 plugin.json 里的 data_sources：DSL 只能引用它们，
// 不能出现工具名、URL 或任何可执行内容。
func ValidateDSL(raw []byte, sources []DataSource) (int, error) {
	if len(raw) == 0 {
		return 0, fmt.Errorf("组件缺少 %s", DSLFile)
	}
	if len(raw) > maxDSLBytes {
		return 0, fmt.Errorf("%s 超过 %d 字节", DSLFile, maxDSLBytes)
	}
	var spec struct {
		DSL    string              `json:"dsl"`
		Height int                 `json:"height,omitempty"`
		State  map[string]any      `json:"state,omitempty"`
		Data   map[string]dslQuery `json:"data,omitempty"`
		View   json.RawMessage     `json:"view"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(&spec); err != nil {
		return 0, fmt.Errorf("%s 解析失败（不允许未知字段）：%w", DSLFile, err)
	}
	if spec.DSL != DSLVersion {
		return 0, fmt.Errorf("dsl 必须是 %s，收到 %q", DSLVersion, spec.DSL)
	}

	byID := make(map[string]DataSource, len(sources))
	for _, ds := range sources {
		byID[ds.ID] = ds
	}

	if len(spec.Data) > len(sources) {
		return 0, fmt.Errorf("data 里的数据源多于 data_sources 声明")
	}
	for id, q := range spec.Data {
		ds, ok := byID[id]
		if !ok {
			return 0, fmt.Errorf("data[%s] 未在 plugin.json 的 data_sources 里声明", id)
		}
		if ds.Mode == "action" {
			return 0, fmt.Errorf("data[%s] 是写数据源，不能自动加载（只能由按钮触发）", id)
		}
		if err := checkScalarMap(q.Params, "data."+id+".params"); err != nil {
			return 0, err
		}
		if err := checkBindings(q.Params, byID); err != nil {
			return 0, err
		}
		// watch / reset 里的键名拼错，表现是「输入没反应」，运行期几乎查不出来，
		// 所以发布时就必须能在 state 里对上。
		for _, key := range q.Watch {
			if !dslStateKey.MatchString(key) {
				return 0, fmt.Errorf("data[%s].watch 的 %q 不是合法 state 键", id, key)
			}
			if _, ok := spec.State[key]; !ok {
				return 0, fmt.Errorf("data[%s].watch 的 %q 未在 state 里声明", id, key)
			}
		}
		if len(q.Reset) > 0 && len(q.Watch) == 0 {
			return 0, fmt.Errorf("data[%s].reset 需要同时声明 watch", id)
		}
		for _, key := range q.Reset {
			if !dslStateKey.MatchString(key) {
				return 0, fmt.Errorf("data[%s].reset 的 %q 不是合法 state 键", id, key)
			}
			if _, ok := spec.State[key]; !ok {
				return 0, fmt.Errorf("data[%s].reset 的 %q 未在 state 里声明", id, key)
			}
		}
	}

	if len(spec.State) > maxDSLStateKeys {
		return 0, fmt.Errorf("state 最多 %d 个键", maxDSLStateKeys)
	}
	for k, v := range spec.State {
		if !dslStateKey.MatchString(k) {
			return 0, fmt.Errorf("state 键 %q 必须是 a-z 开头的 [a-z0-9_]", k)
		}
		if err := checkScalar(v, "state."+k); err != nil {
			return 0, err
		}
		// 初值只能引用 props：引用 state 自己会形成声明顺序依赖，引用 data 则
		// 取数发生在初值之前，都是不可解释的行为。
		if err := checkPropsOnly(v, "state."+k); err != nil {
			return 0, err
		}
	}

	if len(spec.View) == 0 {
		return 0, fmt.Errorf("view 必填")
	}
	count := 0
	ctx := &dslCtx{sources: byID, data: spec.Data, state: spec.State}
	if err := walkDSLNode(spec.View, 1, ctx, &count); err != nil {
		return 0, err
	}
	if count > maxDSLNodes {
		return 0, fmt.Errorf("节点数 %d 超过上限 %d", count, maxDSLNodes)
	}

	height := spec.Height
	if height == 0 {
		height = 360
	}
	if height < minDSLHeight || height > maxDSLHeight {
		return 0, fmt.Errorf("height 必须在 %d-%d 之间", minDSLHeight, maxDSLHeight)
	}
	return height, nil
}

func walkDSLNode(raw json.RawMessage, depth int, ctx *dslCtx, count *int) error {
	if depth > maxDSLDepth {
		return fmt.Errorf("组件嵌套超过 %d 层", maxDSLDepth)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return fmt.Errorf("节点必须是 JSON 对象：%w", err)
	}
	typ, err := nodeString(obj, "type", false)
	if err != nil || typ == "" {
		return fmt.Errorf("节点缺少 type")
	}
	allowed := dslNodeFields[typ]
	if allowed == nil {
		return fmt.Errorf("不支持的节点类型 %q（可用：%s）", typ, strings.Join(dslNodeTypes(), ", "))
	}
	for k := range obj {
		if !allowed[k] {
			return fmt.Errorf("节点 %s 不支持字段 %q", typ, k)
		}
	}
	*count++

	switch typ {
	case "Stack", "Row", "Grid":
		if err := checkChildren(obj, depth, ctx, count); err != nil {
			return err
		}
		if typ == "Grid" {
			cols, err := nodeInt(obj, "columns")
			if err != nil || cols < 1 || cols > 4 {
				return fmt.Errorf("Grid.columns 必须是 1-4")
			}
		}
		if err := checkEnumField(obj, "gap", dslGapSet); err != nil {
			return err
		}
		if err := checkEnumField(obj, "align", dslAlignSet); err != nil {
			return err
		}
	case "Card":
		if _, err := nodeString(obj, "title", false); err != nil {
			return err
		}
		if err := checkChildren(obj, depth, ctx, count); err != nil {
			return err
		}
	case "Heading":
		if err := requireText(obj); err != nil {
			return err
		}
		level, err := nodeInt(obj, "level")
		if err != nil || level == 0 {
			level = 3
		}
		if level < 1 || level > 4 {
			return fmt.Errorf("Heading.level 必须是 1-4")
		}
	case "Text", "Badge":
		if err := requireText(obj); err != nil {
			return err
		}
		if err := checkEnumField(obj, "size", dslSizeSet); err != nil {
			return err
		}
		if err := checkEnumField(obj, "tone", dslToneSet); err != nil {
			return err
		}
	case "Alert":
		if err := requireText(obj); err != nil {
			return err
		}
		if err := checkEnumField(obj, "tone", dslToneSet); err != nil {
			return err
		}
	case "Stat":
		label, _ := nodeString(obj, "label", false)
		value, _ := nodeString(obj, "value", false)
		if label == "" || value == "" {
			return fmt.Errorf("Stat 需要 label 与 value")
		}
		if err := checkEnumField(obj, "tone", dslToneSet); err != nil {
			return err
		}
	case "Divider":
	case "KeyValue":
		items, err := nodeArray(obj, "items")
		if err != nil || len(items) == 0 {
			return fmt.Errorf("KeyValue.items 必须是非空数组")
		}
		for i, it := range items {
			if err := checkItemObject(it, []string{"label", "value"}, fmt.Sprintf("items[%d]", i)); err != nil {
				return err
			}
		}
	case "List":
		if err := requireBindingField(obj, "source"); err != nil {
			return err
		}
	case "Table":
		cols, err := nodeArray(obj, "columns")
		if err != nil || len(cols) == 0 || len(cols) > 8 {
			return fmt.Errorf("Table.columns 必须是 1-8 个列定义")
		}
		for i, c := range cols {
			if err := checkItemObject(c, []string{"key", "label", "align"}, fmt.Sprintf("columns[%d]", i)); err != nil {
				return err
			}
			key, _ := nodeStringFrom(c, "key")
			if key == "" {
				return fmt.Errorf("Table.columns[%d].key 必填", i)
			}
		}
		if err := requireBindingField(obj, "source"); err != nil {
			return err
		}
	case "Progress":
		if _, err := nodeNumber(obj, "value"); err != nil {
			return fmt.Errorf("Progress.value 必填且必须是数字或绑定")
		}
		if err := checkEnumField(obj, "tone", dslToneSet); err != nil {
			return err
		}
	case "Button":
		if err := requireText(obj); err != nil {
			return err
		}
		if _, ok := obj["action"]; !ok {
			return fmt.Errorf("Button.action 必填")
		}
	case "Input":
		if err := requireStateRef(obj); err != nil {
			return err
		}
		if err := checkEnumField(obj, "input_type", dslInputTypeSet); err != nil {
			return err
		}
		if err := checkOptionalBool(obj, "required"); err != nil {
			return err
		}
		if err := checkOptionalText(obj, "placeholder", 200); err != nil {
			return err
		}
	case "Select":
		if err := requireStateRef(obj); err != nil {
			return err
		}
		if err := checkSelectOptions(obj, ctx); err != nil {
			return err
		}
		if err := checkOptionalBool(obj, "required"); err != nil {
			return err
		}
		if err := checkOptionalText(obj, "placeholder", 200); err != nil {
			return err
		}
		if err := checkOptionalText(obj, "empty", 200); err != nil {
			return err
		}
	case "Tabs":
		if err := requireStateRef(obj); err != nil {
			return err
		}
		if err := checkOptions(obj); err != nil {
			return err
		}
	}

	if rawWhen, ok := obj["when"]; ok {
		if err := checkCond(rawWhen); err != nil {
			return err
		}
	}
	if rawAction, ok := obj["action"]; ok {
		if err := checkAction(rawAction, ctx.sources); err != nil {
			return err
		}
	}

	// 所有字符串都要过一遍绑定语法校验：`{{...}}` 里只能出现受支持的数据路径，
	// 这样前端就不需要实现任何表达式求值，也就没有注入面。
	var anyVal any
	if err := json.Unmarshal(raw, &anyVal); err != nil {
		return err
	}
	return checkBindings(anyVal, ctx.sources)
}

func checkChildren(obj map[string]json.RawMessage, depth int, ctx *dslCtx, count *int) error {
	kids, err := nodeArray(obj, "children")
	if err != nil {
		return err
	}
	if len(kids) == 0 {
		return fmt.Errorf("布局节点必须至少有一个子节点")
	}
	for _, k := range kids {
		if err := walkDSLNode(k, depth+1, ctx, count); err != nil {
			return err
		}
	}
	return nil
}

func checkCond(raw json.RawMessage) error {
	var c struct {
		Path  string `json:"path"`
		Op    string `json:"op"`
		Value any    `json:"value,omitempty"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return fmt.Errorf("when 解析失败：%w", err)
	}
	if !dslOpSet[c.Op] {
		return fmt.Errorf("when.op 只能是 eq / ne / gt / lt / in / exists / truthy / empty")
	}
	if err := checkPath(c.Path); err != nil {
		return fmt.Errorf("when.path: %w", err)
	}
	if c.Value != nil {
		if err := checkScalar(c.Value, "when.value"); err != nil {
			return err
		}
	}
	return nil
}

func checkAction(raw json.RawMessage, sources map[string]DataSource) error {
	var a struct {
		Type      string         `json:"type"`
		Target    string         `json:"target,omitempty"`
		Source    string         `json:"source,omitempty"`
		Params    map[string]any `json:"params,omitempty"`
		Payload   map[string]any `json:"payload,omitempty"`
		OmitEmpty bool           `json:"omit_empty,omitempty"`
		Text      string         `json:"text,omitempty"`
		Confirm   string         `json:"confirm,omitempty"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return fmt.Errorf("action 解析失败：%w", err)
	}
	if utf8.RuneCountInString(a.Confirm) > 200 {
		return fmt.Errorf("action.confirm 不能超过 200 字")
	}
	switch a.Type {
	case "reload":
		ds, ok := sources[a.Target]
		if !ok || a.Target == "" {
			return fmt.Errorf("reload 的 target 必须是已声明的数据源")
		}
		if ds.Mode == "action" {
			return fmt.Errorf("reload 不能指向写数据源 %q", a.Target)
		}
	case "invoke":
		ds, ok := sources[a.Source]
		if !ok || a.Source == "" {
			return fmt.Errorf("invoke 的 source 必须是已声明的数据源")
		}
		if ds.Mode != "action" {
			return fmt.Errorf("invoke 只能指向 mode=action 的数据源")
		}
		if err := checkScalarMap(a.Params, "action.params"); err != nil {
			return err
		}
	case "message":
		if strings.TrimSpace(a.Text) == "" {
			return fmt.Errorf("message 动作需要 text")
		}
		if utf8.RuneCountInString(a.Text) > maxDSLTextRunes {
			return fmt.Errorf("message.text 不能超过 %d 字", maxDSLTextRunes)
		}
	case "submit":
		// submit 把 state 里的选择整理成结构化参数，填进输入框由用户确认发送，
		// 不发请求、不进后端，所以这里只校验形状。
		if len(a.Payload) == 0 {
			return fmt.Errorf("submit 动作需要 payload")
		}
		if err := checkScalarMap(a.Payload, "action.payload"); err != nil {
			return err
		}
		if strings.TrimSpace(a.Text) == "" && len(a.Payload) == 0 {
			return fmt.Errorf("submit 动作需要 payload")
		}
		if utf8.RuneCountInString(a.Text) > maxDSLTextRunes {
			return fmt.Errorf("submit.text 不能超过 %d 字", maxDSLTextRunes)
		}
	default:
		return fmt.Errorf("action.type 只能是 reload / invoke / message / submit")
	}
	return nil
}

func checkBindings(v any, sources map[string]DataSource) error {
	switch val := v.(type) {
	case string:
		return checkTemplate(val, sources)
	case []any:
		for _, item := range val {
			if err := checkBindings(item, sources); err != nil {
				return err
			}
		}
	case map[string]any:
		for _, item := range val {
			if err := checkBindings(item, sources); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkTemplate 只接受 `{{path}}`，且 path 必须是 state / props / data.<id> 下的字段路径。
func checkTemplate(s string, sources map[string]DataSource) error {
	return eachBinding(s, func(path string) error {
		if err := checkPath(path); err != nil {
			return err
		}
		if strings.HasPrefix(path, "data.") {
			id := strings.SplitN(strings.TrimPrefix(path, "data."), ".", 2)[0]
			if _, ok := sources[id]; !ok {
				return fmt.Errorf("绑定 %q 引用了未声明的数据源 %q", path, id)
			}
		}
		return nil
	})
}

// eachBinding 逐段扫描 `{{...}}`，把路径交给 fn 判断。
//
// 只做语法扫描，不做求值：前端因此不需要表达式引擎，也就没有注入面。
func eachBinding(s string, fn func(path string) error) error {
	rest := s
	for {
		i := strings.Index(rest, "{{")
		if i < 0 {
			if strings.Contains(rest, "}}") {
				return fmt.Errorf("绑定语法错误：多余的 }}（只支持 {{path}}）")
			}
			return nil
		}
		if strings.Contains(rest[:i], "}}") {
			return fmt.Errorf("绑定语法错误：多余的 }}（只支持 {{path}}）")
		}
		j := strings.Index(rest[i+2:], "}}")
		if j < 0 {
			return fmt.Errorf("绑定语法错误：{{ 未闭合")
		}
		path := strings.TrimSpace(rest[i+2 : i+2+j])
		if err := fn(path); err != nil {
			return err
		}
		rest = rest[i+2+j+2:]
	}
}

// checkPropsOnly 用于 state 初值：只允许引用 props，且路径必须是 props.x。
func checkPropsOnly(v any, field string) error {
	switch val := v.(type) {
	case string:
		return eachBinding(val, func(path string) error {
			if !strings.HasPrefix(path, "props.") || len(path) <= len("props.") {
				return fmt.Errorf("%s 的初值只能引用 props.x，收到 {{%s}}", field, path)
			}
			return checkPath(path)
		})
	case []any:
		for _, item := range val {
			if err := checkPropsOnly(item, field); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkSelectOptions 处理下拉候选的两种来源：字面量 options 或数据源 options_from。
func checkSelectOptions(obj map[string]json.RawMessage, ctx *dslCtx) error {
	_, hasOptions := obj["options"]
	_, hasFrom := obj["options_from"]
	if hasOptions == hasFrom {
		return fmt.Errorf("Select 必须在 options 与 options_from 中二选一")
	}
	if hasOptions {
		return checkOptions(obj)
	}

	var from dslOptionsFrom
	dec := json.NewDecoder(bytes.NewReader(obj["options_from"]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&from); err != nil {
		return fmt.Errorf("options_from 解析失败（不允许未知字段）：%w", err)
	}
	if from.Source == "" {
		return fmt.Errorf("options_from.source 必填")
	}
	ds, ok := ctx.sources[from.Source]
	if !ok {
		return fmt.Errorf("options_from.source %q 未在 data_sources 里声明", from.Source)
	}
	if ds.Mode == "action" {
		return fmt.Errorf("options_from 不能指向写数据源 %q", from.Source)
	}
	if _, ok := ctx.data[from.Source]; !ok {
		return fmt.Errorf("options_from.source %q 必须同时在 ui.json 的 data 里声明，否则没人取数", from.Source)
	}
	if err := checkFieldPath(from.Value, "options_from.value"); err != nil {
		return err
	}
	if err := checkFieldPath(from.Label, "options_from.label"); err != nil {
		return err
	}
	if from.Items != "" {
		if err := checkFieldPath(from.Items, "options_from.items"); err != nil {
			return err
		}
	}
	if from.Search != "" {
		if !dslStateKey.MatchString(from.Search) {
			return fmt.Errorf("options_from.search 必须是合法 state 键")
		}
		if _, ok := ctx.state[from.Search]; !ok {
			return fmt.Errorf("options_from.search 的 %q 未在 state 里声明", from.Search)
		}
		// 搜索框改了 state 却没人监听，表现就是「输入没反应」，这里提前拦住。
		watched := false
		for _, key := range ctx.data[from.Source].Watch {
			if key == from.Search {
				watched = true
				break
			}
		}
		if !watched {
			return fmt.Errorf("options_from.search 的 %q 必须出现在 data[%s].watch 里", from.Search, from.Source)
		}
	}

	name, _ := nodeString(obj, "name", false)
	multiple, err := nodeBool(obj, "multiple")
	if err != nil {
		return err
	}
	if maxVal, err := nodeInt(obj, "max"); err != nil {
		return err
	} else if maxVal != 0 {
		if !multiple {
			return fmt.Errorf("Select.max 只在 multiple 为 true 时有意义")
		}
		if maxVal < 1 || maxVal > maxDSLSelectMax {
			return fmt.Errorf("Select.max 必须在 1-%d 之间", maxDSLSelectMax)
		}
	}
	if multiple {
		// 多选的 state 必须是数组：初值写成 "" 会让第一次选择直接替换类型，
		// 前端 `asList` 也要兜底，但发布时先拒绝更省事。
		init, ok := ctx.state[name]
		if !ok {
			return fmt.Errorf("Select %q 多选时 state 必须声明初始值（建议 []）", name)
		}
		switch val := init.(type) {
		case []any:
		case string:
			if !strings.HasPrefix(strings.TrimSpace(val), "{{props.") {
				return fmt.Errorf("Select %q 多选时 state 初始值必须是数组或 {{props.x}}", name)
			}
		default:
			return fmt.Errorf("Select %q 多选时 state 初始值必须是数组或 {{props.x}}", name)
		}
	}
	if labelName, ok := obj["label_name"]; ok {
		raw := string(labelName)
		var s string
		if err := json.Unmarshal(labelName, &s); err != nil || !dslStateKey.MatchString(s) {
			return fmt.Errorf("label_name 必须是合法 state 键，收到 %s", raw)
		}
		if _, ok := ctx.state[s]; !ok {
			return fmt.Errorf("label_name %q 未在 state 里声明", s)
		}
	}
	return nil
}

// checkFieldPath 校验「从一条记录里取字段」的路径，支持 a.b 与 rows[0].x 的数组下标写法。
func checkFieldPath(path, field string) error {
	if path == "" {
		return fmt.Errorf("%s 必填", field)
	}
	if strings.Contains(path, "{{") || strings.Contains(path, "}}") {
		return fmt.Errorf("%s 必须是字段名，不能是绑定", field)
	}
	for _, p := range strings.Split(path, ".") {
		if !dslSegment.MatchString(p) {
			return fmt.Errorf("%s 含非法字段名 %q", field, p)
		}
	}
	return nil
}

func checkPath(path string) error {
	parts := strings.Split(path, ".")
	if len(parts) < 2 {
		return fmt.Errorf("路径 %q 必须形如 state.x / props.x / data.<id>.x", path)
	}
	switch parts[0] {
	case "state", "props", "data":
	default:
		return fmt.Errorf("路径 %q 必须以 state / props / data 开头", path)
	}
	for _, p := range parts[1:] {
		if !dslSegment.MatchString(p) {
			return fmt.Errorf("路径 %q 含非法字段名 %q", path, p)
		}
	}
	return nil
}

func checkScalarMap(m map[string]any, field string) error {
	if len(m) > 20 {
		return fmt.Errorf("%s 最多 20 个参数", field)
	}
	for k, v := range m {
		if !dslSegment.MatchString(k) {
			return fmt.Errorf("%s 的参数名 %q 非法", field, k)
		}
		if err := checkScalar(v, field+"."+k); err != nil {
			return err
		}
	}
	return nil
}

// checkScalar 限制状态/参数只能是标量或标量数组：嵌套结构交给数据源工具自己表达。
func checkScalar(v any, field string) error {
	switch val := v.(type) {
	case nil, string, bool, json.Number, float64:
		return nil
	case []any:
		if len(val) > 50 {
			return fmt.Errorf("%s 数组最多 50 项", field)
		}
		for _, item := range val {
			if err := checkScalar(item, field); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("%s 只能是字符串/数字/布尔或它们的数组", field)
	}
}

func checkItemObject(raw json.RawMessage, allowed []string, field string) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return fmt.Errorf("%s 必须是对象", field)
	}
	set := fieldSet(allowed...)
	for k := range obj {
		if !set[k] {
			return fmt.Errorf("%s 不支持字段 %q", field, k)
		}
	}
	return nil
}

func checkOptions(obj map[string]json.RawMessage) error {
	opts, err := nodeArray(obj, "options")
	if err != nil || len(opts) == 0 || len(opts) > 30 {
		return fmt.Errorf("options 必须是 1-30 项数组")
	}
	for i, o := range opts {
		if err := checkItemObject(o, []string{"label", "value"}, fmt.Sprintf("options[%d]", i)); err != nil {
			return err
		}
		label, _ := nodeStringFrom(o, "label")
		value, _ := nodeStringFrom(o, "value")
		if label == "" || value == "" {
			return fmt.Errorf("options[%d] 需要 label 与 value", i)
		}
	}
	return nil
}

func requireText(obj map[string]json.RawMessage) error {
	text, err := nodeString(obj, "text", true)
	if err != nil {
		return err
	}
	if utf8.RuneCountInString(text) > maxDSLTextRunes {
		return fmt.Errorf("text 不能超过 %d 字", maxDSLTextRunes)
	}
	return nil
}

func requireBindingField(obj map[string]json.RawMessage, field string) error {
	raw, ok := obj[field]
	if !ok {
		return fmt.Errorf("%s 必填", field)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("%s 必须是字符串绑定（如 {{data.orders.items}}）", field)
	}
	if !strings.Contains(s, "{{") {
		return fmt.Errorf("%s 必须是数据绑定（如 {{data.orders.items}}）", field)
	}
	return nil
}

func requireStateRef(obj map[string]json.RawMessage) error {
	name, err := nodeString(obj, "name", true)
	if err != nil {
		return err
	}
	if !dslStateKey.MatchString(name) {
		return fmt.Errorf("name %q 必须是 a-z 开头的 [a-z0-9_]，它对应 state 里的键", name)
	}
	if _, ok := obj["label"]; ok {
		label, err := nodeString(obj, "label", false)
		if err != nil {
			return err
		}
		if utf8.RuneCountInString(label) > 60 {
			return fmt.Errorf("label 不能超过 60 字")
		}
	}
	return nil
}

func checkEnumField(obj map[string]json.RawMessage, field string, allowed map[string]bool) error {
	if _, ok := obj[field]; !ok {
		return nil
	}
	value, err := nodeString(obj, field, false)
	if err != nil {
		return err
	}
	if value == "" {
		return nil
	}
	if !allowed[value] {
		return fmt.Errorf("%s 的取值 %q 不在允许范围", field, value)
	}
	return nil
}

// checkOptionalBool 只校验类型：字段缺失合法，写了就必须是布尔。
func checkOptionalBool(obj map[string]json.RawMessage, field string) error {
	raw, ok := obj[field]
	if !ok {
		return nil
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return fmt.Errorf("%s 必须是布尔值", field)
	}
	return nil
}

// checkOptionalText 校验可选文案字段的长度。
func checkOptionalText(obj map[string]json.RawMessage, field string, limit int) error {
	s, err := nodeString(obj, field, false)
	if err != nil {
		return err
	}
	if utf8.RuneCountInString(s) > limit {
		return fmt.Errorf("%s 不能超过 %d 字", field, limit)
	}
	return nil
}

func nodeBool(obj map[string]json.RawMessage, field string) (bool, error) {
	raw, ok := obj[field]
	if !ok {
		return false, nil
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false, fmt.Errorf("%s 必须是布尔值", field)
	}
	return b, nil
}

func nodeString(obj map[string]json.RawMessage, field string, required bool) (string, error) {
	raw, ok := obj[field]
	if !ok {
		if required {
			return "", fmt.Errorf("%s 必填", field)
		}
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("%s 必须是字符串", field)
	}
	return s, nil
}

func nodeStringFrom(raw json.RawMessage, field string) (string, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", err
	}
	return nodeString(obj, field, false)
}

func nodeInt(obj map[string]json.RawMessage, field string) (int, error) {
	raw, ok := obj[field]
	if !ok {
		return 0, nil
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, fmt.Errorf("%s 必须是整数", field)
	}
	return n, nil
}

func nodeNumber(obj map[string]json.RawMessage, field string) (float64, error) {
	raw, ok := obj[field]
	if !ok {
		return 0, fmt.Errorf("%s 必填", field)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if strings.Contains(s, "{{") {
			return 0, nil // 绑定值由前端求值，这里只校验语法（后面统一走 checkTemplate）
		}
		return strconv.ParseFloat(s, 64)
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, fmt.Errorf("%s 必须是数字或绑定", field)
	}
	return n, nil
}

func nodeArray(obj map[string]json.RawMessage, field string) ([]json.RawMessage, error) {
	raw, ok := obj[field]
	if !ok {
		return nil, fmt.Errorf("%s 必填", field)
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("%s 必须是数组", field)
	}
	return arr, nil
}

func dslNodeTypes() []string {
	out := make([]string, 0, len(dslNodeFields))
	for k := range dslNodeFields {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
