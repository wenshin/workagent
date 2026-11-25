package plugin

import (
	"context"
	"testing"

	"github.com/workagent/auto-agent/internal/store"
)

// hostFactsManager 造一个只关心配置注入的 Manager。
//
// 嵌入 store.Store 接口而不是实现它：这些用例只走 effectiveConfig，
// 读安装记录那一步返回空即可，返回值 nil, nil 表示「这个空间没存过配置」。
type hostFactsStore struct {
	store.Store
}

func (s *hostFactsStore) ListPluginInstalls(context.Context, string) ([]store.PluginInstall, error) {
	return nil, nil
}

func (s *hostFactsStore) GetPluginInstall(context.Context, string, string) (store.PluginInstall, error) {
	return store.PluginInstall{}, store.ErrNotFound
}

// 草稿那一路也要兜住：storedConfig 在没装的情况下会继续读草稿。
func (s *hostFactsStore) GetPluginCandidate(context.Context, string, string) (store.PluginCandidate, error) {
	return store.PluginCandidate{}, store.ErrNotFound
}

func hostFactsManager(hostBase string) *Manager {
	return &Manager{
		o:    Options{Store: &hostFactsStore{}, HostBase: hostBase},
		inst: map[string]map[string]*instance{},
	}
}

// 宿主地址与空间 id 由平台注入，不是使用者填的配置项。
//
// 这两项抄错的唯一表现是投递 401，而排查方向会全跑到「插件没装好 / 空间不对」上，
// 所以必须钉住「无论清单怎么配，最终值都是部署事实」。
func TestInjectHostFactsOverridesUserConfig(t *testing.T) {
	m := hostFactsManager("http://workagent.default.svc:8321/")
	meta := Manifest{
		Capabilities: []Capability{{Method: CapIMReply}},
		Config: []ConfigItem{
			{Key: ConfigHostBase, Default: "http://127.0.0.1:8321"},
			{Key: ConfigHostSpace, Default: "手填的错空间"},
		},
	}
	cfg := m.effectiveConfig(context.Background(),
		"79398525-0527-4ecb-92f3-c855e6640e74", "dingtalk_bot", meta)

	if got := cfg[ConfigHostBase]; got != "http://workagent.default.svc:8321" {
		t.Fatalf("宿主地址应以部署值为准（去掉尾斜杠），实际 %q", got)
	}
	if got := cfg[ConfigHostSpace]; got != "79398525-0527-4ecb-92f3-c855e6640e74" {
		t.Fatalf("空间 id 应取当前空间，实际 %q", got)
	}
}

// 没有 config 段的渠道插件同样要拿到这两项：清单不声明可配置项，
// 不等于脚本不需要知道宿主在哪。
func TestInjectHostFactsWithoutConfigSection(t *testing.T) {
	m := hostFactsManager("http://127.0.0.1:8321")
	meta := Manifest{Capabilities: []Capability{{Method: CapIMReply}}}

	cfg := m.effectiveConfig(context.Background(), "space-1", "dingtalk_bot", meta)
	if cfg[ConfigHostBase] != "http://127.0.0.1:8321" {
		t.Fatalf("无 config 段也要注入宿主地址: %v", cfg)
	}
	if cfg[ConfigHostSpace] != "space-1" {
		t.Fatalf("无 config 段也要注入空间 id: %v", cfg)
	}
}

// 普通工具不该拿到宿主地址：注入面跟着能力走，多一个入口就多一份风险。
func TestInjectHostFactsSkippedWithoutIMCapability(t *testing.T) {
	m := hostFactsManager("http://127.0.0.1:8321")
	meta := Manifest{Capabilities: []Capability{{Method: CapHTTPRequest}}}

	cfg := m.effectiveConfig(context.Background(), "space-1", "some_tool", meta)
	if _, ok := cfg[ConfigHostBase]; ok {
		t.Fatal("没声明 im.reply 的插件不该拿到宿主地址")
	}
	if _, ok := cfg[ConfigHostSpace]; ok {
		t.Fatal("没声明 im.reply 的插件不该拿到空间 id")
	}
}

// 平台地址默认进白名单：插件不声明、也无从声明（清单 hosts 只收域名），
// 由平台按 APP_BASE_URL 算成精确 host:port 补进去。
func TestPlatformHostPortsFollowIMCapability(t *testing.T) {
	meta := Manifest{Capabilities: []Capability{{Method: CapIMReply}}}

	got := hostFactsManager("http://127.0.0.1:8321").platformHostPorts(meta)
	if len(got) != 1 || got[0] != "127.0.0.1:8321" {
		t.Fatalf("宿主地址应补成精确 host:port，实际 %v", got)
	}
	// 省端口的部署地址要按 scheme 补默认端口，否则和代理看到的真实端口对不上。
	got = hostFactsManager("https://wa.example.com").platformHostPorts(meta)
	if len(got) != 1 || got[0] != "wa.example.com:443" {
		t.Fatalf("应补默认 443，实际 %v", got)
	}
	// 注入面跟着能力走：普通工具不该多一条能调平台接口的通道。
	if got := hostFactsManager("http://127.0.0.1:8321").
		platformHostPorts(Manifest{Capabilities: []Capability{{Method: CapHTTPRequest}}}); len(got) != 0 {
		t.Fatalf("没声明 im.reply 不该注入宿主地址，实际 %v", got)
	}
	if got := hostFactsManager("").platformHostPorts(meta); len(got) != 0 {
		t.Fatalf("部署没配宿主地址时应为空，实际 %v", got)
	}
	if got := hostFactsManager("不是地址").platformHostPorts(meta); len(got) != 0 {
		t.Fatalf("解析不了应放弃注入而不是猜，实际 %v", got)
	}
}

// 渠道标识的两种清单写法都要能触发能力判定，否则「按新写法声明的被拒」。
func TestHasCapabilityBothForms(t *testing.T) {
	old := Manifest{Capability: CapIMReply}
	if !old.HasCapability(CapIMReply) {
		t.Fatal("老式单一能力写法应被识别")
	}
	cur := Manifest{Capabilities: []Capability{{Method: CapIMReply}}}
	if !cur.HasCapability(CapIMReply) {
		t.Fatal("capabilities 数组写法应被识别")
	}
	if cur.HasCapability(CapShellExec) {
		t.Fatal("未声明的方法不该被判为有")
	}
}

// 身份令牌随宿主事实一起注入，且只给声明了 im.reply 的插件。
//
// 少了这一项，插件照样连得上钉钉、照样收得到消息，但每条消息都投不出
// （401），表现是「用户发了没人回」——最难归因的形态，所以必须钉住。
func TestInjectHostFactsCarriesPluginToken(t *testing.T) {
	m := hostFactsManager("http://host.docker.internal:8321")
	m.o.TokenSecret = "deploy-secret"
	meta := Manifest{Capabilities: []Capability{{Method: CapIMReply}}}

	cfg := m.effectiveConfig(context.Background(), "space-1", "dingtalk_bot", meta)
	if got, want := cfg[ConfigHostToken], m.PluginToken("space-1", "dingtalk_bot"); got != want || got == "" {
		t.Fatalf("应注入平台签发的令牌，实际 %q，期望 %q", got, want)
	}
	// 普通工具拿不到：多一个能写会话的入口就是多一份风险。
	plain := Manifest{Capabilities: []Capability{{Method: CapHTTPRequest}}}
	if got := m.effectiveConfig(context.Background(), "space-1", "demo", plain)[ConfigHostToken]; got != "" {
		t.Fatalf("没声明 im.reply 不该注入令牌，实际 %q", got)
	}
}
