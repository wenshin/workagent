package plugin

// builtinSource 是随平台发布、安装到系统空间的搜索源。
//
// 它和「内置工具」不是一回事：搜索源不给模型调用，只实现 web_search 背后的
// search.query 能力，所以不需要 capability 之外的任何风险声明。
type builtinSource struct {
	Slug        string
	Name        string
	Description string
	Source      SourceSpec
	// Files 是随包产物，键是插件目录内的相对路径。
	Files map[string]string
}

// builtinSources 返回平台预置搜索源。
//
// 预置一个通用网页搜索，让新部署在录入密钥后立刻能用；default 源之外的效果
// （内网、私有 API、签名特殊）由用户自己发布 source 插件，安装后按 priority
// 参与路由——用户装的源默认 priority=0，会排在预置源之前接管。
func builtinSources() []builtinSource {
	return []builtinSource{{
		Slug:        "bocha",
		Name:        "博查搜索",
		Description: "通用网页搜索，需要博查 API Key；平台预置，可在市场停用",
		Source: SourceSpec{
			Protocol: SourceProtocolSpec,
			Entry:    SourceSpecEntry,
			Endpoint: "https://api.bochaai.com/v1/web-search",
			Secret:   "bocha",
			Scope:    SourceScopeExternal,
			// 排在用户自建源之后：自己装的源更贴合场景，应该先被尝试。
			Priority:       100,
			TimeoutSeconds: 8,
		},
		Files: map[string]string{SourceSpecEntry: bochaSearchSpec},
	}}
}

// bochaSearchSpec 是博查 Web Search API 的声明式描述。
//
// 摘要用数组路径回退：请求里开了 summary，博查会把正文放 summary，snippet
// 常常为空，只取一个字段会静默产生空摘要。不写 freshness 是因为博查默认
// 不限制时间范围，写死一个值反而挡住了以后加时间过滤。
const bochaSearchSpec = `{
  "request": {
    "method": "POST",
    "headers": { "Content-Type": "application/json" },
    "auth": { "in": "header", "name": "Authorization", "prefix": "Bearer " },
    "body": {
      "query": "{{query}}",
      "summary": true,
      "count": "{{limit}}"
    }
  },
  "response": {
    "items": "data.webPages.value",
    "title": "name",
    "url": "url",
    "snippet": ["summary", "snippet"],
    "published_at": "datePublished"
  }
}
`
