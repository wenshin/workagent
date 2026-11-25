package plugin

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/workagent/auto-agent/internal/store"
)

// warmStore 只实现启动预热会用到的那几个查询，其余方法由嵌入的接口兜底：
// 预热要验的是「有没有轮到每个空间」，不是存储实现本身。
type warmStore struct {
	store.Store
	systemID string
}

func (s *warmStore) ListPluginInstalls(ctx context.Context, spaceID string) ([]store.PluginInstall, error) {
	return nil, nil
}

func (s *warmStore) ListPluginCandidatesBySpace(ctx context.Context, spaceID string) ([]store.PluginCandidate, error) {
	return nil, nil
}

func (s *warmStore) ListPlugins(ctx context.Context) ([]store.Plugin, error) {
	return nil, nil
}

func (s *warmStore) GetSpaceByType(ctx context.Context, spaceType string) (store.Space, error) {
	if s.systemID == "" {
		return store.Space{}, errors.New("系统空间尚未初始化")
	}
	return store.Space{ID: s.systemID}, nil
}

func newWarmManager(st *warmStore) *Manager {
	return &Manager{
		o:     Options{Store: st, Logger: slog.Default()},
		inst:  map[string]map[string]*instance{},
		mcpCs: map[string]*mcpRuntime{},
		boot:  map[string]bool{},
	}
}

// 启动预热必须覆盖所有空间：钉钉这类长连接插件不能等人工打开面板才启动，
// 否则「服务在跑」与「通道在线」之间会有一个没人负责的窗口。
func TestWarmAllSpacesLoadsEverySpace(t *testing.T) {
	m := newWarmManager(&warmStore{})

	m.warmAllSpaces([]store.Space{{ID: "s1"}, {ID: "s2"}}, "")

	for _, id := range []string{"s1", "s2"} {
		if !m.loaded(id) {
			t.Fatalf("空间 %s 未被预热", id)
		}
	}
}

// 系统空间在 Bootstrap 里已经同步预热过（全局搜索源不能有窗口期），
// 异步那轮必须跳过它，否则会重复 buildInstance。
func TestWarmAllSpacesSkipsSystemSpace(t *testing.T) {
	m := newWarmManager(&warmStore{systemID: "sys"})

	m.warmAllSpaces([]store.Space{{ID: "sys"}, {ID: "s1"}}, "sys")

	if m.loaded("sys") {
		t.Fatal("系统空间不应被重复加载")
	}
	if !m.loaded("s1") {
		t.Fatal("普通空间未被预热")
	}
}

// 并发加载同一个空间只能有一份实例：启动预热与首个业务请求常常同时到达。
func TestEnsureLoadedIsIdempotent(t *testing.T) {
	m := newWarmManager(&warmStore{})
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			_ = m.ensureLoaded(context.Background(), "s1")
			done <- struct{}{}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	m.mu.RLock()
	n := len(m.inst)
	m.mu.RUnlock()
	if n != 1 {
		t.Fatalf("并发加载后实例表应只有一份，实际 %d 份", n)
	}
}
