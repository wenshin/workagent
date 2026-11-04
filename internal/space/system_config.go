package space

import (
	"context"
	"path"
)

// builtinConfigFiles 是系统空间根下的平台配置初始内容。
//
// 这些文件只有管理员可见可写，且不会被普通空间读到（ReadSourceFile 不回落它们）。
// 内容里不放任何密钥：密钥在 {STORAGE_ROOT}/secrets/ 里加密存放，这里只放引用名。
var builtinConfigFiles = []struct {
	Path    string
	Content string
}{
	{
		Path: "config.json",
		Content: `{
  "search": {
    "provider": "bocha",
    "endpoint": "https://api.bochaai.com/v1/web-search",
    "spec": {
      "request": {
        "method": "POST",
        "headers": { "Content-Type": "application/json" },
        "auth": { "in": "header", "name": "Authorization", "prefix": "Bearer " },
        "body": { "query": "{{query}}", "count": "{{limit}}", "summary": true }
      },
      "response": {
        "items": "data.webPages.value",
        "title": "name",
        "url": "url",
        "snippet": "summary",
        "published_at": "datePublished",
        "error_code": "code",
        "error_ok": [0, 200],
        "error_message": "msg"
      }
    }
  }
}
`,
	},
	{
		Path: "net_whitelist.json",
		Content: `{
  "domains": ["api.bochaai.com"]
}
`,
	},
}

// seedConfigFiles 幂等写入平台配置，已存在时不覆盖管理员的修改。
func (s *Service) seedConfigFiles(ctx context.Context, spaceID string) error {
	root := s.filesKey(spaceID)
	for _, f := range builtinConfigFiles {
		if err := s.putIfAbsent(ctx, path.Join(root, f.Path), f.Content); err != nil {
			return err
		}
	}
	return nil
}
