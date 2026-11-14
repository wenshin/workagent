package space

import (
	"context"
	"errors"
	"testing"
)

// published/... 只能由插件层解析：路径里是 slug 与版本，真实发布目录以 plugin id 落盘。
// 会话层曾经自己用资源服务解析，把 slug 当 uuid 查库，内置技能一读就 500。
func TestReadSourceFileRoutesPublishedToPluginLayer(t *testing.T) {
	var gotSpace, gotRel string
	s := &Service{}
	s.SetPublishedReader(func(ctx context.Context, spaceID, rel string) (string, error) {
		gotSpace, gotRel = spaceID, rel
		return "BODY", nil
	})

	const rel = "published/skills/builtin_plugin_dev/1.0.9135/SKILL.md"
	out, err := s.ReadSourceFile(context.Background(), "sp1", rel, 1024)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if out != "BODY" {
		t.Fatalf("返回值未透传: %q", out)
	}
	if gotSpace != "sp1" || gotRel != rel {
		t.Fatalf("转发参数不对: space=%q rel=%q", gotSpace, gotRel)
	}
}

func TestReadSourceFilePublishedWithoutReader(t *testing.T) {
	s := &Service{}
	_, err := s.ReadSourceFile(context.Background(), "sp1", "published/skills/x/1.0.0/SKILL.md", 1024)
	if !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("未注入读取器时应返回文件不存在，得到 %v", err)
	}
}
