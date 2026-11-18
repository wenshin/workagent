package main

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// 迷你 SQL 执行器。
//
// mock 数仓的价值在于「筛选条件真的生效」：分析智能体拼出来的
// `where city_code in (...) and stat_date between ...`、`group by`、
// `join dim_city` 必须能跑出对得上的结果，否则筛选组件的闭环测试没有意义。
//
// 支持范围（够跑通分析链路，不追求完备）：
//
//	select [distinct] 列|聚合 [as 别名], ...
//	from 表 [[left|inner] join 表 on a.x = b.y]...
//	[where 条件 and/or ...]
//	[group by 列, ...]
//	[order by 列|别名 [asc|desc], ...]
//	[limit n [offset m]]
//
// 条件：= != <> > >= < <=、in (...)、like '%x%'、between a and b、is [not] null。
// 聚合：count(*|列)、sum、avg、min、max。
// 不支持子查询、窗口函数、case when —— 命中时给出明确报错，而不是静默返回错数据。

type sqlRow map[string]any

type sqlQuery struct {
	distinct bool
	items    []sqlSelectItem
	from     string
	fromAs   string
	joins    []sqlJoin
	where    *sqlExpr
	groupBy  []*sqlExpr
	orderBy  []sqlOrder
	limit    int
	offset   int
}

type sqlSelectItem struct {
	star  bool
	expr  *sqlExpr
	alias string
}

type sqlJoin struct {
	kind  string // inner | left
	table string
	as    string
	on    *sqlExpr
}

type sqlOrder struct {
	expr *sqlExpr
	desc bool
}

type sqlExpr struct {
	kind string // col | lit | and | or | not | cmp | in | like | between | isnull | agg
	col  string
	lit  any
	sym  string
	args []*sqlExpr
	fn   string
	neg  bool
}

// ---------------------------------------------------------------------------
// 词法
// ---------------------------------------------------------------------------

type sqlToken struct {
	kind string // ident | string | number | symbol
	text string
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func lexSQL(s string) ([]sqlToken, error) {
	var out []sqlToken
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case c == '\'' || c == '"' || c == '`':
			quote := c
			i++
			var b strings.Builder
			for i < len(s) && s[i] != quote {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				b.WriteByte(s[i])
				i++
			}
			if i >= len(s) {
				return nil, fmt.Errorf("字符串/标识符未闭合")
			}
			i++
			kind := "string"
			if quote == '`' {
				kind = "ident"
			}
			out = append(out, sqlToken{kind, b.String()})
		case isDigit(c):
			start := i
			for i < len(s) && (isDigit(s[i]) || s[i] == '.') {
				i++
			}
			out = append(out, sqlToken{"number", s[start:i]})
		case isIdentStart(c):
			start := i
			for i < len(s) && (isIdentStart(s[i]) || isDigit(s[i]) || s[i] == '.' || s[i] == '$') {
				i++
			}
			out = append(out, sqlToken{"ident", s[start:i]})
		default:
			two := ""
			if i+1 < len(s) {
				two = s[i : i+2]
			}
			switch two {
			case ">=", "<=", "!=", "<>", "==":
				out = append(out, sqlToken{"symbol", two})
				i += 2
			default:
				out = append(out, sqlToken{"symbol", string(c)})
				i++
			}
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 语法
// ---------------------------------------------------------------------------

type sqlParser struct {
	tokens []sqlToken
	pos    int
}

func (p *sqlParser) peek() sqlToken {
	if p.pos >= len(p.tokens) {
		return sqlToken{}
	}
	return p.tokens[p.pos]
}

func (p *sqlParser) next() sqlToken {
	t := p.peek()
	p.pos++
	return t
}

func (p *sqlParser) eof() bool { return p.pos >= len(p.tokens) }

func (p *sqlParser) matchIdent(word string) bool {
	t := p.peek()
	if t.kind == "ident" && strings.EqualFold(t.text, word) {
		p.pos++
		return true
	}
	return false
}

func (p *sqlParser) matchSymbol(sym string) bool {
	t := p.peek()
	if t.kind == "symbol" && t.text == sym {
		p.pos++
		return true
	}
	return false
}

func (p *sqlParser) expectIdent(word string) error {
	if !p.matchIdent(word) {
		return fmt.Errorf("语法错误：期望 %s，收到 %q", strings.ToUpper(word), p.peek().text)
	}
	return nil
}

func parseSQL(sql string) (*sqlQuery, error) {
	tokens, err := lexSQL(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(sql), ";")))
	if err != nil {
		return nil, err
	}
	p := &sqlParser{tokens: tokens}
	if err := p.expectIdent("select"); err != nil {
		return nil, fmt.Errorf("只支持 SELECT 查询")
	}
	q := &sqlQuery{}
	if p.matchIdent("distinct") {
		q.distinct = true
	}
	for {
		item, err := p.parseSelectItem()
		if err != nil {
			return nil, err
		}
		q.items = append(q.items, item)
		if !p.matchSymbol(",") {
			break
		}
	}
	if err := p.expectIdent("from"); err != nil {
		return nil, err
	}
	q.from = p.next().text
	if p.matchIdent("as") {
		q.fromAs = p.next().text
	} else if t := p.peek(); t.kind == "ident" && !isSQLKeyword(t.text) {
		q.fromAs = p.next().text
	}
	for {
		kind := ""
		switch {
		case p.matchIdent("left"):
			p.matchIdent("outer")
			if err := p.expectIdent("join"); err != nil {
				return nil, err
			}
			kind = "left"
		case p.matchIdent("inner"):
			if err := p.expectIdent("join"); err != nil {
				return nil, err
			}
			kind = "inner"
		case p.matchIdent("join"):
			kind = "inner"
		default:
			kind = ""
		}
		if kind == "" {
			break
		}
		j := sqlJoin{kind: kind, table: p.next().text}
		if p.matchIdent("as") {
			j.as = p.next().text
		} else if t := p.peek(); t.kind == "ident" && !isSQLKeyword(t.text) {
			j.as = p.next().text
		}
		if err := p.expectIdent("on"); err != nil {
			return nil, err
		}
		on, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		j.on = on
		q.joins = append(q.joins, j)
	}
	if p.matchIdent("where") {
		expr, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		q.where = expr
	}
	if p.matchIdent("group") {
		if err := p.expectIdent("by"); err != nil {
			return nil, err
		}
		for {
			expr, err := p.parseOperand()
			if err != nil {
				return nil, err
			}
			q.groupBy = append(q.groupBy, expr)
			if !p.matchSymbol(",") {
				break
			}
		}
	}
	if p.matchIdent("order") {
		if err := p.expectIdent("by"); err != nil {
			return nil, err
		}
		for {
			expr, err := p.parseOperand()
			if err != nil {
				return nil, err
			}
			o := sqlOrder{expr: expr}
			if p.matchIdent("desc") {
				o.desc = true
			} else {
				p.matchIdent("asc")
			}
			q.orderBy = append(q.orderBy, o)
			if !p.matchSymbol(",") {
				break
			}
		}
	}
	if p.matchIdent("limit") {
		n, err := strconv.Atoi(p.next().text)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("limit 必须是正整数")
		}
		q.limit = n
	}
	if p.matchIdent("offset") {
		n, err := strconv.Atoi(p.next().text)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("offset 必须是非负整数")
		}
		q.offset = n
	}
	if !p.eof() {
		return nil, fmt.Errorf("SQL 解析失败：无法识别 %q 之后的内容（mock 不支持子查询/窗口函数）", p.peek().text)
	}
	return q, nil
}

func isSQLKeyword(word string) bool {
	switch strings.ToLower(word) {
	case "select", "distinct", "from", "where", "group", "by", "order", "limit", "offset",
		"join", "left", "outer", "inner", "on", "having", "union", "as",
		"and", "or", "not", "in", "like", "between", "is", "null", "asc", "desc":
		return true
	}
	return false
}

func (p *sqlParser) parseSelectItem() (sqlSelectItem, error) {
	if p.matchSymbol("*") {
		return sqlSelectItem{star: true}, nil
	}
	// 聚合函数：name(args)
	if t := p.peek(); t.kind == "ident" && isAggName(t.text) && p.isFuncCall() {
		fn := strings.ToLower(p.next().text)
		if !p.matchSymbol("(") {
			return sqlSelectItem{}, fmt.Errorf("语法错误：%s 后需要 (", fn)
		}
		var arg *sqlExpr
		if p.matchSymbol("*") {
			arg = &sqlExpr{kind: "lit", lit: "*"}
		} else {
			a, err := p.parseOperand()
			if err != nil {
				return sqlSelectItem{}, err
			}
			arg = a
		}
		if !p.matchSymbol(")") {
			return sqlSelectItem{}, fmt.Errorf("语法错误：%s(...) 缺少 )", fn)
		}
		expr := &sqlExpr{kind: "agg", fn: fn, args: []*sqlExpr{arg}}
		return p.finishSelectItem(expr)
	}
	expr, err := p.parseOperand()
	if err != nil {
		return sqlSelectItem{}, err
	}
	return p.finishSelectItem(expr)
}

func (p *sqlParser) finishSelectItem(expr *sqlExpr) (sqlSelectItem, error) {
	item := sqlSelectItem{expr: expr}
	if p.matchIdent("as") {
		item.alias = p.next().text
	} else if t := p.peek(); t.kind == "ident" && !isSQLKeyword(t.text) {
		item.alias = p.next().text
	}
	return item, nil
}

func (p *sqlParser) isFuncCall() bool {
	if p.pos+1 >= len(p.tokens) {
		return false
	}
	next := p.tokens[p.pos+1]
	return next.kind == "symbol" && next.text == "("
}

func isAggName(name string) bool {
	switch strings.ToLower(name) {
	case "count", "sum", "avg", "min", "max":
		return true
	}
	return false
}

func (p *sqlParser) parseExpr() (*sqlExpr, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.matchIdent("or") {
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &sqlExpr{kind: "or", args: []*sqlExpr{left, right}}
	}
	return left, nil
}

func (p *sqlParser) parseAnd() (*sqlExpr, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.matchIdent("and") {
		right, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		left = &sqlExpr{kind: "and", args: []*sqlExpr{left, right}}
	}
	return left, nil
}

func (p *sqlParser) parseNot() (*sqlExpr, error) {
	if p.matchIdent("not") {
		inner, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return &sqlExpr{kind: "not", args: []*sqlExpr{inner}}, nil
	}
	return p.parsePredicate()
}

func (p *sqlParser) parsePredicate() (*sqlExpr, error) {
	left, err := p.parseOperand()
	if err != nil {
		return nil, err
	}
	if p.matchIdent("is") {
		neg := p.matchIdent("not")
		if err := p.expectIdent("null"); err != nil {
			return nil, err
		}
		return &sqlExpr{kind: "isnull", neg: neg, args: []*sqlExpr{left}}, nil
	}
	neg := false
	if p.matchIdent("not") {
		neg = true
	}
	if p.matchIdent("in") {
		if !p.matchSymbol("(") {
			return nil, fmt.Errorf("语法错误：in 后需要 (")
		}
		var vals []*sqlExpr
		for {
			v, err := p.parseOperand()
			if err != nil {
				return nil, err
			}
			vals = append(vals, v)
			if !p.matchSymbol(",") {
				break
			}
		}
		if !p.matchSymbol(")") {
			return nil, fmt.Errorf("语法错误：in (...) 缺少 )")
		}
		return &sqlExpr{kind: "in", neg: neg, args: []*sqlExpr{left}, fn: "in", lit: vals}, nil
	}
	if p.matchIdent("like") {
		v, err := p.parseOperand()
		if err != nil {
			return nil, err
		}
		return &sqlExpr{kind: "like", neg: neg, args: []*sqlExpr{left, v}}, nil
	}
	if p.matchIdent("between") {
		lo, err := p.parseOperand()
		if err != nil {
			return nil, err
		}
		if err := p.expectIdent("and"); err != nil {
			return nil, err
		}
		hi, err := p.parseOperand()
		if err != nil {
			return nil, err
		}
		return &sqlExpr{kind: "between", neg: neg, args: []*sqlExpr{left, lo, hi}}, nil
	}
	t := p.peek()
	if t.kind == "symbol" {
		switch t.text {
		case "=", "==", "!=", "<>", ">", ">=", "<", "<=":
			p.next()
			right, err := p.parseOperand()
			if err != nil {
				return nil, err
			}
			return &sqlExpr{kind: "cmp", sym: t.text, args: []*sqlExpr{left, right}}, nil
		}
	}
	if neg {
		return nil, fmt.Errorf("语法错误：not 后缺少 in / like / between")
	}
	// 单列布尔：where is_new_user 这类写法容忍为「非空即真」
	return left, nil
}

func (p *sqlParser) parseOperand() (*sqlExpr, error) {
	t := p.peek()
	switch t.kind {
	case "string":
		p.next()
		return &sqlExpr{kind: "lit", lit: t.text}, nil
	case "number":
		p.next()
		if strings.Contains(t.text, ".") {
			f, err := strconv.ParseFloat(t.text, 64)
			if err != nil {
				return nil, fmt.Errorf("非法数字 %q", t.text)
			}
			return &sqlExpr{kind: "lit", lit: f}, nil
		}
		n, err := strconv.ParseInt(t.text, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("非法数字 %q", t.text)
		}
		return &sqlExpr{kind: "lit", lit: n}, nil
	case "ident":
		p.next()
		switch strings.ToLower(t.text) {
		case "null":
			return &sqlExpr{kind: "lit", lit: nil}, nil
		case "true":
			return &sqlExpr{kind: "lit", lit: true}, nil
		case "false":
			return &sqlExpr{kind: "lit", lit: false}, nil
		}
		if isAggName(t.text) && p.peek().kind == "symbol" && p.peek().text == "(" {
			p.next()
			var arg *sqlExpr
			if p.matchSymbol("*") {
				arg = &sqlExpr{kind: "lit", lit: "*"}
			} else {
				a, err := p.parseOperand()
				if err != nil {
					return nil, err
				}
				arg = a
			}
			if !p.matchSymbol(")") {
				return nil, fmt.Errorf("语法错误：%s(...) 缺少 )", t.text)
			}
			return &sqlExpr{kind: "agg", fn: strings.ToLower(t.text), args: []*sqlExpr{arg}}, nil
		}
		return &sqlExpr{kind: "col", col: strings.ToLower(t.text)}, nil
	case "symbol":
		if t.text == "(" {
			p.next()
			inner, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if !p.matchSymbol(")") {
				return nil, fmt.Errorf("语法错误：缺少 )")
			}
			return inner, nil
		}
	}
	return nil, fmt.Errorf("语法错误：无法解析 %q", t.text)
}

// ---------------------------------------------------------------------------
// 执行
// ---------------------------------------------------------------------------

func executeSQL(sql string) ([]string, []map[string]any, error) {
	trimmed := strings.TrimSpace(sql)
	if trimmed == "" {
		return nil, nil, fmt.Errorf("SQL 不能为空")
	}
	lower := strings.ToLower(trimmed)
	if writePattern.MatchString(trimmed) {
		return nil, nil, fmt.Errorf("当前账号为只读账号，不允许执行 DDL/DML，请只提交 SELECT 查询")
	}
	if strings.Contains(lower, "fail") || strings.Contains(lower, "error") || strings.Contains(lower, "boom") {
		return nil, nil, fmt.Errorf("执行失败：mock 服务命中故意失败的关键字（fail/error/boom）")
	}
	if !fromPattern.MatchString(lower) {
		// select 1 / select current_date 这类无源查询
		return []string{"result"}, []map[string]any{{"result": 1}}, nil
	}

	q, err := parseSQL(trimmed)
	if err != nil {
		return nil, nil, err
	}
	base := findTable(q.from)
	if base == nil {
		return nil, nil, fmt.Errorf("Table or view not found: %s", q.from)
	}
	alias := q.fromAs
	if alias == "" {
		alias = base.Name
	}

	rows, err := joinRows(q, base, alias)
	if err != nil {
		return nil, nil, err
	}
	if err := validateExpr(q.where, base, alias); err != nil {
		return nil, nil, err
	}

	// where
	if q.where != nil {
		filtered := rows[:0:0]
		for _, row := range rows {
			ok, err := evalBool(q.where, row)
			if err != nil {
				return nil, nil, err
			}
			if ok {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}

	hasAgg := false
	for _, it := range q.items {
		if it.expr != nil && it.expr.kind == "agg" {
			hasAgg = true
		}
		if it.expr != nil && containsAgg(it.expr) {
			hasAgg = true
		}
	}

	var cols []string
	var out []map[string]any
	if hasAgg || len(q.groupBy) > 0 {
		cols, out, err = aggregateRows(q, rows)
	} else {
		cols, out, err = projectRows(q, rows, base)
	}
	if err != nil {
		return nil, nil, err
	}

	// order by
	if len(q.orderBy) > 0 {
		sort.SliceStable(out, func(i, j int) bool {
			for _, o := range q.orderBy {
				a, b := orderValue(o.expr, out[i]), orderValue(o.expr, out[j])
				c := compareValues(a, b)
				if c == 0 {
					continue
				}
				if o.desc {
					return c > 0
				}
				return c < 0
			}
			return false
		})
	}

	if q.distinct {
		out = dedupeRows(out)
	}

	// 分页：limit 缺省时按 mock 上限截断，写清楚在 AGENT.md 里
	offset := q.offset
	if offset > len(out) {
		offset = len(out)
	}
	limit := q.limit
	if limit <= 0 {
		limit = defaultResultRows
	}
	if offset+limit > len(out) {
		limit = len(out) - offset
	}
	out = out[offset : offset+limit]
	if len(out) > maxResultRows {
		out = out[:maxResultRows]
	}
	return cols, out, nil
}

func joinRows(q *sqlQuery, base *Table, alias string) ([]sqlRow, error) {
	rows := make([]sqlRow, 0, len(base.rows))
	baseCols := columnNames(base)
	for _, src := range base.rows {
		row := sqlRow{}
		for k, v := range src {
			row[k] = v
			row[alias+"."+k] = v
		}
		for _, c := range baseCols {
			if _, ok := row[c]; !ok {
				row[c] = nil
			}
		}
		rows = append(rows, row)
	}
	for _, j := range q.joins {
		right := findTable(j.table)
		if right == nil {
			return nil, fmt.Errorf("Table or view not found: %s", j.table)
		}
		rightAlias := j.as
		if rightAlias == "" {
			rightAlias = right.Name
		}
		rightCols := columnNames(right)
		var next []sqlRow
		for _, left := range rows {
			matched := false
			for _, src := range right.rows {
				merged := sqlRow{}
				for k, v := range left {
					merged[k] = v
				}
				for k, v := range src {
					// 同名列以左表为准，保持 `select *` 可预期
					if _, exists := merged[k]; !exists {
						merged[k] = v
					}
					merged[rightAlias+"."+k] = v
				}
				for _, c := range rightCols {
					if _, ok := merged[rightAlias+"."+c]; !ok {
						merged[rightAlias+"."+c] = nil
					}
				}
				ok, err := evalBool(j.on, merged)
				if err != nil {
					return nil, err
				}
				if ok {
					next = append(next, merged)
					matched = true
				}
			}
			if !matched && j.kind == "left" {
				for _, c := range rightCols {
					left[rightAlias+"."+c] = nil
				}
				next = append(next, left)
			}
		}
		rows = next
	}
	return rows, nil
}

func columnNames(t *Table) []string {
	out := make([]string, 0, len(t.Columns))
	for _, c := range t.Columns {
		out = append(out, c.Name)
	}
	return out
}

func projectRows(q *sqlQuery, rows []sqlRow, base *Table) ([]string, []map[string]any, error) {
	var cols []string
	type proj struct {
		name string
		expr *sqlExpr
	}
	var items []proj
	for _, it := range q.items {
		if it.star {
			for _, c := range columnNames(base) {
				items = append(items, proj{name: c, expr: &sqlExpr{kind: "col", col: c}})
				cols = append(cols, c)
			}
			continue
		}
		name := it.alias
		if name == "" {
			name = exprName(it.expr)
		}
		items = append(items, proj{name: name, expr: it.expr})
		cols = append(cols, name)
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		rec := make(map[string]any, len(items))
		for _, item := range items {
			v, err := eval(item.expr, row)
			if err != nil {
				return nil, nil, err
			}
			rec[item.name] = v
		}
		out = append(out, rec)
	}
	return cols, out, nil
}

// starColumns 在没有行时也要能给出列名，所以从 expression 里反推不可靠，
// 这里按第一行推断；空结果时退化为空列（调用方另有 schema 兜底）。
func starColumns(rows []sqlRow) []string {
	if len(rows) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var cols []string
	for k := range rows[0] {
		if strings.Contains(k, ".") || k == "_" {
			continue
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		cols = append(cols, k)
	}
	sort.Strings(cols)
	return cols
}

func aggregateRows(q *sqlQuery, rows []sqlRow) ([]string, []map[string]any, error) {
	type group struct {
		key  string
		vals []any
		rows []sqlRow
	}
	groups := map[string]*group{}
	var order []string
	for _, row := range rows {
		keyVals := make([]any, 0, len(q.groupBy))
		for _, g := range q.groupBy {
			v, err := eval(g, row)
			if err != nil {
				return nil, nil, err
			}
			keyVals = append(keyVals, v)
		}
		raw, _ := json.Marshal(keyVals)
		key := string(raw)
		g, ok := groups[key]
		if !ok {
			g = &group{key: key, vals: keyVals}
			groups[key] = g
			order = append(order, key)
		}
		g.rows = append(g.rows, row)
	}
	if len(q.groupBy) == 0 {
		groups[""] = &group{key: ""}
		order = []string{""}
		for _, row := range rows {
			groups[""].rows = append(groups[""].rows, row)
		}
	}

	var cols []string
	type proj struct {
		name string
		expr *sqlExpr
		gIdx int // >=0 表示取分组键
	}
	var items []proj
	for _, it := range q.items {
		if it.star {
			for i, g := range q.groupBy {
				name := exprName(g)
				items = append(items, proj{name: name, gIdx: i})
				cols = append(cols, name)
			}
			continue
		}
		name := it.alias
		if name == "" {
			name = exprName(it.expr)
		}
		gIdx := -1
		for i, g := range q.groupBy {
			if exprName(g) == exprName(it.expr) {
				gIdx = i
				break
			}
		}
		items = append(items, proj{name: name, expr: it.expr, gIdx: gIdx})
		cols = append(cols, name)
	}

	out := make([]map[string]any, 0, len(order))
	for _, key := range order {
		g := groups[key]
		rec := make(map[string]any, len(items))
		for _, item := range items {
			if item.gIdx >= 0 && item.gIdx < len(g.vals) {
				rec[item.name] = g.vals[item.gIdx]
				continue
			}
			if item.expr != nil && containsAgg(item.expr) {
				v, err := evalAggExpr(item.expr, g.rows)
				if err != nil {
					return nil, nil, err
				}
				rec[item.name] = v
				continue
			}
			if len(g.rows) == 0 {
				rec[item.name] = nil
				continue
			}
			v, err := eval(item.expr, g.rows[0])
			if err != nil {
				return nil, nil, err
			}
			rec[item.name] = v
		}
		out = append(out, rec)
	}
	return cols, out, nil
}

func evalAggExpr(e *sqlExpr, rows []sqlRow) (any, error) {
	if e.kind == "agg" {
		return evalAgg(e, rows)
	}
	// 只支持单层聚合（sum(x) 这类），表达式套聚合直接拒绝
	return nil, fmt.Errorf("不支持在表达式里嵌套聚合，请把聚合放进 select 列表")
}

func evalAgg(e *sqlExpr, rows []sqlRow) (any, error) {
	if len(e.args) == 0 {
		return nil, fmt.Errorf("%s 缺少参数", e.fn)
	}
	arg := e.args[0]
	if e.fn == "count" && arg.kind == "lit" && arg.lit == "*" {
		return int64(len(rows)), nil
	}
	var vals []any
	for _, row := range rows {
		v, err := eval(arg, row)
		if err != nil {
			return nil, err
		}
		if v != nil {
			vals = append(vals, v)
		}
	}
	switch e.fn {
	case "count":
		return int64(len(vals)), nil
	case "sum", "avg":
		var sum float64
		for _, v := range vals {
			n, ok := toNumber(v)
			if !ok {
				return nil, fmt.Errorf("%s 需要数值列", e.fn)
			}
			sum += n
		}
		if e.fn == "avg" {
			if len(vals) == 0 {
				return nil, nil
			}
			return round2(sum / float64(len(vals))), nil
		}
		return round2(sum), nil
	case "min", "max":
		if len(vals) == 0 {
			return nil, nil
		}
		best := vals[0]
		for _, v := range vals[1:] {
			c := compareValues(v, best)
			if (e.fn == "min" && c < 0) || (e.fn == "max" && c > 0) {
				best = v
			}
		}
		return best, nil
	}
	return nil, fmt.Errorf("不支持的聚合函数 %s", e.fn)
}

func containsAgg(e *sqlExpr) bool {
	if e == nil {
		return false
	}
	if e.kind == "agg" {
		return true
	}
	for _, a := range e.args {
		if containsAgg(a) {
			return true
		}
	}
	return false
}

func exprName(e *sqlExpr) string {
	switch e.kind {
	case "col":
		if i := strings.LastIndex(e.col, "."); i >= 0 {
			return e.col[i+1:]
		}
		return e.col
	case "agg":
		arg := "*"
		if len(e.args) > 0 {
			arg = exprName(e.args[0])
			if e.args[0].kind == "lit" && e.args[0].lit == "*" {
				arg = "*"
			}
		}
		return e.fn + "(" + arg + ")"
	case "lit":
		return fmt.Sprint(e.lit)
	}
	return "expr"
}

func orderValue(e *sqlExpr, row map[string]any) any {
	if e.kind == "col" {
		name := e.col
		if i := strings.LastIndex(name, "."); i >= 0 {
			name = name[i+1:]
		}
		if v, ok := row[name]; ok {
			return v
		}
		return row[e.col]
	}
	return nil
}

func dedupeRows(rows []map[string]any) []map[string]any {
	seen := map[string]bool{}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		raw, _ := json.Marshal(row)
		key := string(raw)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, row)
	}
	return out
}

func eval(e *sqlExpr, row sqlRow) (any, error) {
	switch e.kind {
	case "lit":
		if e.lit == "*" {
			return nil, fmt.Errorf("`*` 只能出现在 count(*) 里")
		}
		return e.lit, nil
	case "col":
		if v, ok := row[e.col]; ok {
			return v, nil
		}
		if i := strings.LastIndex(e.col, "."); i >= 0 {
			if v, ok := row[e.col[i+1:]]; ok {
				return v, nil
			}
		}
		return nil, fmt.Errorf("Unknown column '%s'", e.col)
	case "and":
		a, err := evalBool(e.args[0], row)
		if err != nil || !a {
			return false, err
		}
		return evalBool(e.args[1], row)
	case "or":
		a, err := evalBool(e.args[0], row)
		if err != nil {
			return nil, err
		}
		if a {
			return true, nil
		}
		return evalBool(e.args[1], row)
	case "not":
		v, err := evalBool(e.args[0], row)
		if err != nil {
			return nil, err
		}
		return !v, nil
	}
	return nil, fmt.Errorf("无法求值表达式")
}

func evalBool(e *sqlExpr, row sqlRow) (bool, error) {
	if e == nil {
		return true, nil
	}
	switch e.kind {
	case "and", "or", "not":
		v, err := eval(e, row)
		if err != nil {
			return false, err
		}
		b, _ := v.(bool)
		return b, nil
	case "cmp":
		left, err := eval(e.args[0], row)
		if err != nil {
			return false, err
		}
		right, err := eval(e.args[1], row)
		if err != nil {
			return false, err
		}
		if left == nil || right == nil {
			return false, nil
		}
		c := compareValues(left, right)
		switch e.sym {
		case "=", "==":
			return c == 0, nil
		case "!=", "<>":
			return c != 0, nil
		case ">":
			return c > 0, nil
		case ">=":
			return c >= 0, nil
		case "<":
			return c < 0, nil
		case "<=":
			return c <= 0, nil
		}
	case "in":
		left, err := eval(e.args[0], row)
		if err != nil {
			return false, err
		}
		vals, _ := e.lit.([]*sqlExpr)
		found := false
		for _, v := range vals {
			rv, err := eval(v, row)
			if err != nil {
				return false, err
			}
			if left != nil && rv != nil && compareValues(left, rv) == 0 {
				found = true
				break
			}
		}
		if e.neg {
			return !found, nil
		}
		return found, nil
	case "like":
		left, err := eval(e.args[0], row)
		if err != nil {
			return false, err
		}
		pat, err := eval(e.args[1], row)
		if err != nil {
			return false, err
		}
		matched := likeMatch(fmt.Sprint(left), fmt.Sprint(pat))
		if e.neg {
			return !matched, nil
		}
		return matched, nil
	case "between":
		v, err := eval(e.args[0], row)
		if err != nil {
			return false, err
		}
		lo, err := eval(e.args[1], row)
		if err != nil {
			return false, err
		}
		hi, err := eval(e.args[2], row)
		if err != nil {
			return false, err
		}
		if v == nil || lo == nil || hi == nil {
			return false, nil
		}
		ok := compareValues(v, lo) >= 0 && compareValues(v, hi) <= 0
		if e.neg {
			return !ok, nil
		}
		return ok, nil
	case "isnull":
		v, err := eval(e.args[0], row)
		if err != nil {
			return false, err
		}
		if e.neg {
			return v != nil, nil
		}
		return v == nil, nil
	case "col":
		v, err := eval(e, row)
		if err != nil {
			return false, err
		}
		return v != nil && v != false, nil
	}
	return false, fmt.Errorf("条件表达式无法求值")
}

func likeMatch(value, pattern string) bool {
	var b strings.Builder
	b.WriteString("(?i)^")
	for _, r := range pattern {
		switch r {
		case '%':
			b.WriteString(".*")
		case '_':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return false
	}
	return re.MatchString(value)
}

func compareValues(a, b any) int {
	na, aok := toNumber(a)
	nb, bok := toNumber(b)
	if aok && bok {
		switch {
		case na < nb:
			return -1
		case na > nb:
			return 1
		default:
			return 0
		}
	}
	sa, sb := fmt.Sprint(a), fmt.Sprint(b)
	return strings.Compare(sa, sb)
}

func toNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	}
	return 0, false
}

func round2(f float64) float64 {
	return math.Round(f*100) / 100
}

// validateExpr 在 0 行结果时也能报出 Unknown column：分析智能体最常犯的错
// 就是字段名拼错，静默返回空表会让人以为是「没数据」。
func validateExpr(e *sqlExpr, base *Table, alias string) error {
	if e == nil {
		return nil
	}
	if e.kind == "col" {
		name := e.col
		if i := strings.LastIndex(name, "."); i >= 0 {
			prefix, col := name[:i], name[i+1:]
			if prefix == alias || prefix == base.Name {
				if !hasColumn(base, col) {
					return fmt.Errorf("Unknown column '%s'", name)
				}
				return nil
			}
			return nil // 其他别名交给 join 阶段
		}
		if !hasColumn(base, name) {
			return fmt.Errorf("Unknown column '%s'", name)
		}
	}
	for _, a := range e.args {
		if err := validateExpr(a, base, alias); err != nil {
			return err
		}
	}
	return nil
}

func hasColumn(t *Table, name string) bool {
	for _, c := range t.Columns {
		if strings.EqualFold(c.Name, name) {
			return true
		}
	}
	return false
}
