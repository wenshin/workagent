package plugin

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/workagent/auto-agent/internal/storage/localfs"
	"github.com/workagent/auto-agent/internal/store"
)

// draftConfigStore 只覆盖草稿配置这条线用到的方法。
//
// 内嵌 store.Store 是为了不用把几十个方法都实现一遍；没覆盖的方法一旦被调用
// 就会 nil panic——所以这些用例只能碰「读安装记录 / 读草稿」两个入口。
type draftConfigStore struct {
	store.Store
	installs []store.PluginInstall
	cand     store.PluginCandidate
	candErr  error
	saved    json.RawMessage
}

func (s *draftConfigStore) ListPluginInstalls(context.Context, string) ([]store.PluginInstall, error) {
	return s.installs, nil
}

func (s *draftConfigStore) GetPluginInstall(context.Context, string, string) (store.PluginInstall, error) {
	for _, in := range s.installs {
		return in, nil
	}
	return store.PluginInstall{}, store.ErrNotFound
}

func (s *draftConfigStore) GetPluginCandidate(context.Context, string, string) (store.PluginCandidate, error) {
	if s.candErr != nil {
		return store.PluginCandidate{}, s.candErr
	}
	return s.cand, nil
}

func (s *draftConfigStore) UpdatePluginCandidateConfig(_ context.Context, _, _ string, cfg json.RawMessage) error {
	s.saved = cfg
	return nil
}

// draftConfigRuntime 造一个「空间里只有草稿、没有安装记录」的管理器：
// 草稿目录里放一份声明了 client_id 的清单，草稿状态存在 plugin_candidate。
func draftConfigRuntime(t *testing.T, candConfig string) (*Manager, *draftConfigStore) {
	t.Helper()
	root := t.TempDir()
	fs, err := localfs.New(root)
	if err != nil {
		t.Fatalf("初始化存储: %v", err)
	}
	const (
		spaceID = "11111111-1111-1111-1111-111111111111"
		slug    = "dingtalk_bot"
	)
	dir := filepath.Join(root, "spaces", spaceID, "files", "plugins", slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建草稿目录: %v", err)
	}
	manifest := `{
	  "protocol": "wa-plugin/1",
	  "kind": "tool",
	  "slug": "dingtalk_bot",
	  "name": "钉钉机器人",
	  "description": "往钉钉群发消息",
	  "capability": "plugin.invoke",
	  "input_schema": {"type": "object", "properties": {}},
	  "runtime": {"entry": "index.ts"},
	  "capabilities": [],
	  "config": [{"key": "client_id", "label": "AppKey", "type": "text"}]
	}`
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("写清单: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.ts"), []byte("export default {};\n"), 0o644); err != nil {
		t.Fatalf("写入口: %v", err)
	}

	st := &draftConfigStore{cand: store.PluginCandidate{
		SpaceID: spaceID, Slug: slug, Kind: "tool", Status: store.PluginCandidateEnabled,
		Config: json.RawMessage(candConfig),
	}}
	m := &Manager{
		o: Options{Store: st, Storage: fs, Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))},
	}
	return m, st
}

// 启用的草稿没有安装记录，配置必须从 plugin_candidate 读出来。
//
// 回归的是现场症状：草稿态 wa.config() 只有清单默认值 → client_id 为空 →
// 插件自己报「未就绪」，作者必须先发布安装才能填参数。
func TestConfigViewReadsDraftConfig(t *testing.T) {
	m, _ := draftConfigRuntime(t, `{"client_id":"ding_draft"}`)
	const spaceID = "11111111-1111-1111-1111-111111111111"

	view, err := m.ConfigView(context.Background(), spaceID, "dingtalk_bot")
	if err != nil {
		t.Fatalf("读取草稿配置: %v", err)
	}
	if got := view.Values["client_id"]; got != "ding_draft" {
		t.Fatalf("client_id = %q，期望读到草稿里存的值", got)
	}
	if len(view.Fields) == 0 {
		t.Fatal("草稿的清单声明了 config 段，表单字段不能为空")
	}
}

// 运行时取值同样要兜底到草稿：工具进程读的是 effectiveConfig，不是 ConfigView。
func TestEffectiveConfigFallsBackToDraft(t *testing.T) {
	m, _ := draftConfigRuntime(t, `{"client_id":"ding_runtime"}`)
	meta, err := m.readDraft(context.Background(), "11111111-1111-1111-1111-111111111111", "dingtalk_bot")
	if err != nil {
		t.Fatalf("读草稿清单: %v", err)
	}
	cfg := m.effectiveConfig(context.Background(), "11111111-1111-1111-1111-111111111111", "dingtalk_bot", meta)
	if got := cfg["client_id"]; got != "ding_runtime" {
		t.Fatalf("生效配置 client_id = %q，期望读到草稿里存的值", got)
	}
}

// 装了之后仍然以安装记录为准：草稿与安装副本互斥，但读取顺序不能反过来。
func TestEffectiveConfigPrefersInstall(t *testing.T) {
	m, st := draftConfigRuntime(t, `{"client_id":"from_draft"}`)
	st.installs = []store.PluginInstall{{
		SpaceID: "11111111-1111-1111-1111-111111111111", Slug: "dingtalk_bot",
		Status: store.PluginInstallEnabled, Config: json.RawMessage(`{"client_id":"from_install"}`),
	}}
	meta, err := m.readDraft(context.Background(), "11111111-1111-1111-1111-111111111111", "dingtalk_bot")
	if err != nil {
		t.Fatalf("读草稿清单: %v", err)
	}
	cfg := m.effectiveConfig(context.Background(), "11111111-1111-1111-1111-111111111111", "dingtalk_bot", meta)
	if got := cfg["client_id"]; got != "from_install" {
		t.Fatalf("生效配置 client_id = %q，期望安装记录优先", got)
	}
}

// 既没安装也没草稿时保持原来的 404 语义，不能变成「空配置」。
func TestConfigViewMissingPlugin(t *testing.T) {
	m, st := draftConfigRuntime(t, `{}`)
	st.candErr = store.ErrNotFound
	if _, err := m.ConfigView(context.Background(), "11111111-1111-1111-1111-111111111111", "nope"); err == nil {
		t.Fatal("既没安装也没草稿时应返回 NotFound")
	}
}
