package plugin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/workagent/auto-agent/internal/search"
)

// 预置搜索源是在启动时发布＋安装的，清单写错的表现是「服务起不来」或
// 「装上了但 web_search 一直报源不可用」，所以在测试里把两道校验都跑一遍：
// 插件清单本身（Parse/Validate），以及 search.json 的声明式描述。
func TestBuiltinSourceManifests(t *testing.T) {
	for _, src := range builtinSources() {
		spec := src.Source
		meta := Manifest{
			Protocol:    Protocol,
			Kind:        KindSource,
			Slug:        src.Slug,
			Name:        src.Name,
			Description: src.Description,
			Capability:  CapSearchQuery,
			Source:      &spec,
			Extensible:  boolPtr(false),
			Version:     "1.0.0",
		}
		raw, err := json.Marshal(meta)
		if err != nil {
			t.Fatalf("%s 清单序列化失败: %v", src.Slug, err)
		}
		parsed, err := Parse(raw, true)
		if err != nil {
			t.Fatalf("%s 清单校验失败: %v", src.Slug, err)
		}
		if parsed.Kind != KindSource || parsed.Capability != CapSearchQuery {
			t.Fatalf("%s 的类型或能力不对: kind=%s cap=%s", src.Slug, parsed.Kind, parsed.Capability)
		}
		if parsed.Source == nil || !strings.HasSuffix(parsed.Source.Entry, ".json") {
			t.Fatalf("%s 的 source.entry 应为 .json: %+v", src.Slug, parsed.Source)
		}
		// Guard 的白名单由 endpoint 的 host 推导，host 为空等于源永远连不上。
		if len(parsed.Hosts()) == 0 {
			t.Fatalf("%s 的 endpoint 没能推导出出网白名单", src.Slug)
		}
		// Derive 的第二个返回值是「为什么是这个风险」的说明，不是错误。
		if risk, _ := Derive(parsed); risk < 0 || risk > 3 {
			t.Fatalf("%s 风险值越界: %d", src.Slug, risk)
		}

		body, ok := src.Files[parsed.Source.Entry]
		if !ok {
			t.Fatalf("%s 缺少产物文件 %s", src.Slug, parsed.Source.Entry)
		}
		var sp search.Spec
		if err := json.Unmarshal([]byte(body), &sp); err != nil {
			t.Fatalf("%s 的 %s 解析失败: %v", src.Slug, parsed.Source.Entry, err)
		}
		if err := sp.Validate(); err != nil {
			t.Fatalf("%s 的 %s 校验失败: %v", src.Slug, parsed.Source.Entry, err)
		}
		if len(sp.Response.Snippet) < 2 {
			t.Fatalf("%s 的摘要字段应该配置回退路径，实际 %+v", src.Slug, sp.Response.Snippet)
		}
	}
}