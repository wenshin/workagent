package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// 老镜像与本地开发没有自述文件是正常情况：不能报错，只能「什么都不说」。
func TestLoadEnvFactsMissingFileIsEmptyNotError(t *testing.T) {
	f, err := LoadEnvFacts(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("缺文件不应报错: %v", err)
	}
	if !f.Empty() {
		t.Fatalf("缺文件应为空事实，实际: %+v", f)
	}
}

func TestLoadEnvFactsDropsBlankTools(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env.json")
	body := `{"os":" debian 12 ","tools":[{"name":"git","version":"2.39.5"},{"name":"  "},{"name":"jq"}]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	f, err := LoadEnvFacts(path)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if f.OS != "debian 12" {
		t.Errorf("OS 未去空白: %q", f.OS)
	}
	// 空名字会渲染成「可用工具：、jq」，必须在入口清掉
	if len(f.Tools) != 2 {
		t.Fatalf("应丢掉空名字工具，实际 %d 个: %+v", len(f.Tools), f.Tools)
	}
	if f.Tools[0].Name != "git" || f.Tools[1].Name != "jq" {
		t.Errorf("工具应按名字排序: %+v", f.Tools)
	}
}

func TestLoadEnvFactsBrokenJSONIsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadEnvFacts(path); err == nil {
		t.Fatal("坏 JSON 应报错：静默当成「没有环境」会让排障无从下手")
	}
}

// hash 用来判断「环境换了没」：同一份内容的顺序差异不该让它变一次。
func TestEnvFactsHashIgnoresToolOrder(t *testing.T) {
	a := EnvFacts{Tools: []ToolFact{{Name: "git", Version: "2.39"}, {Name: "jq", Version: "1.6"}}}
	b := EnvFacts{Tools: []ToolFact{{Name: "jq", Version: "1.6"}, {Name: "git", Version: "2.39"}}}

	if a.Hash() != b.Hash() {
		t.Fatalf("顺序不同不该影响 hash: %s vs %s", a.Hash(), b.Hash())
	}
	if a.Hash() == (EnvFacts{OS: "debian"}).Hash() {
		t.Fatal("内容不同应算出不同 hash")
	}
	if (EnvFacts{}).Hash() == "" {
		t.Fatal("空事实也应有稳定 hash，便于日志对账")
	}
}
