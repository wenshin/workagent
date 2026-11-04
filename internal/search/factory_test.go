package search

import (
	"context"
	"testing"
)

type fakeProvider struct{ name string }

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Search(context.Context, Request) ([]Result, error) {
	return []Result{{Title: "fake"}}, nil
}

// 新增搜索源只需要实现 Provider + init 注册，装配层不用改：
// 这条测试锁住「工厂表能解析出构造好的实例」这个契约。
func TestBuildRegisteredProvider(t *testing.T) {
	RegisterFactory("fake", func(string, KeyFunc, *Guard) Provider {
		return &fakeProvider{name: "fake"}
	})

	p, ok := Build("fake", "https://example.com/search", nil, nil)
	if !ok {
		t.Fatal("已注册的 provider 应该能构造出来")
	}
	if p.Name() != "fake" {
		t.Fatalf("构造出的 provider 名字不对: %s", p.Name())
	}

	if _, ok := Build("nope", "", nil, nil); ok {
		t.Fatal("未注册的名字不应构造成功")
	}
}

// 内置 provider 必须自注册，否则启动时装配会静默失败。
func TestBuiltinProvidersRegistered(t *testing.T) {
	names := ProviderNames()
	want := map[string]bool{"bocha": false}
	for _, n := range names {
		if _, ok := want[n]; ok {
			want[n] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Fatalf("内置 provider %q 没有注册，已注册: %v", name, names)
		}
	}
}
