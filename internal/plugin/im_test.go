package plugin

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/workagent/auto-agent/internal/session"
)

// fakeIM 记录被投递了什么，并可按需返回错误。
type fakeIM struct {
	calls int32
	last  session.IMSender
	deliv session.IMDelivery
	err   error
}

func (f *fakeIM) HandleIM(_ context.Context, in session.IMSender, _ string) (session.IMDelivery, error) {
	atomic.AddInt32(&f.calls, 1)
	f.last = in
	return f.deliv, f.err
}

func imTestManager(inst *instance) *Manager {
	m := &Manager{
		o: Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
		inst: map[string]map[string]*instance{
			"space-1": {inst.Slug: inst},
		},
		imDedup: newInboxDedup(0, 0),
	}
	return m
}

func imInstance(caps ...Capability) *instance {
	return &instance{
		SpaceID: "space-1",
		Slug:    "dingtalk_bot",
		Manifest: Manifest{
			Kind:         KindTool,
			Slug:         "dingtalk_bot",
			Capabilities: caps,
			IM:           &IMSpec{Channel: "dingtalk", Label: "钉钉"},
		},
	}
}

// 没声明 im.reply 的插件绝对不能投递：能力清单是审批对象，
// 缺了它也能投递，等于「装了个普通工具就能驱动别人的执行」。
func TestSubmitInboxRequiresCapability(t *testing.T) {
	m := imTestManager(imInstance()) // 无任何能力
	sub := &fakeIM{}
	m.im = sub

	_, err := m.SubmitInbox(context.Background(), "space-1", "dingtalk_bot",
		[]IMInboxItem{{ChannelUID: "u1", Text: "你好"}})
	if err == nil || !strings.Contains(err.Error(), "im.reply") {
		t.Fatalf("没有 im.reply 时应拒绝，实际: %v", err)
	}
	if sub.calls != 0 {
		t.Fatal("被拒绝时不该发生任何投递")
	}
}

// 同一渠道消息重投两次只能投递一次：投递一次就是一轮模型调用。
func TestSubmitInboxDedupesByRef(t *testing.T) {
	m := imTestManager(imInstance(Capability{Method: CapIMReply}))
	sub := &fakeIM{deliv: session.IMDelivery{Reply: "好的"}}
	m.im = sub

	item := IMInboxItem{ChannelUID: "staff-1", Name: "张三", Text: "帮我查一下", Ref: "msg-1"}
	first, err := m.SubmitInbox(context.Background(), "space-1", "dingtalk_bot", []IMInboxItem{item})
	if err != nil {
		t.Fatalf("第一次投递: %v", err)
	}
	if !first[0].OK || first[0].Reply != "好的" {
		t.Fatalf("第一次应成功并带回答复: %+v", first[0])
	}

	second, err := m.SubmitInbox(context.Background(), "space-1", "dingtalk_bot", []IMInboxItem{item})
	if err != nil {
		t.Fatalf("重投: %v", err)
	}
	if second[0].Reply != "好的" || !second[0].OK {
		t.Fatalf("重投应返回第一次的结果: %+v", second[0])
	}
	if got := atomic.LoadInt32(&sub.calls); got != 1 {
		t.Fatalf("重投不该再调一次投递，实际调用 %d 次", got)
	}
	if sub.last.ProviderUID != "staff-1" || sub.last.Channel != "dingtalk" {
		t.Fatalf("投递身份不对: %+v", sub.last)
	}
}

// 用户没登录过平台是常见情况：必须给一句能直接回给钉钉的话，而不是 500。
func TestSubmitInboxUnknownUser(t *testing.T) {
	m := imTestManager(imInstance(Capability{Method: CapIMReply}))
	m.im = &fakeIM{err: session.ErrIMUnknownUser}

	res, err := m.SubmitInbox(context.Background(), "space-1", "dingtalk_bot",
		[]IMInboxItem{{ChannelUID: "staff-9", Name: "李四", Text: "在吗", Ref: "m2"}})
	if err != nil {
		t.Fatalf("不该整体失败: %v", err)
	}
	if res[0].OK || !res[0].NeedLogin || res[0].Reason == "" {
		t.Fatalf("应是「需要登录」的可读结果: %+v", res[0])
	}
}

// 上一轮还在跑时不排队，明确告诉用户稍后重试。
func TestSubmitInboxBusy(t *testing.T) {
	m := imTestManager(imInstance(Capability{Method: CapIMReply}))
	m.im = &fakeIM{err: session.ErrIMBusy}

	res, _ := m.SubmitInbox(context.Background(), "space-1", "dingtalk_bot",
		[]IMInboxItem{{ChannelUID: "staff-1", Text: "再来一个", Ref: "m3"}})
	if res[0].OK || !strings.Contains(res[0].Reason, "稍后") {
		t.Fatalf("忙时应提示稍后重试: %+v", res[0])
	}
}

// 渠道标识来自清单：它进库、进会话绑定键，必须稳定。
func TestIMChannelFromManifest(t *testing.T) {
	inst := imInstance(Capability{Method: CapIMReply})
	m := imTestManager(inst)
	if got := m.imChannel(inst); got != "dingtalk" {
		t.Fatalf("应取清单里的 channel，实际 %q", got)
	}
	inst.Manifest.IM = nil
	if got := m.imChannel(inst); got != "dingtalk_bot" {
		t.Fatalf("缺省应回落到 slug，实际 %q", got)
	}
}

// 投递能力未注入时明确报未启用，不要静默丢消息。
func TestSubmitInboxWithoutSubmitter(t *testing.T) {
	m := imTestManager(imInstance(Capability{Method: CapIMReply}))
	_, err := m.SubmitInbox(context.Background(), "space-1", "dingtalk_bot",
		[]IMInboxItem{{ChannelUID: "u", Text: "x"}})
	if err == nil || !strings.Contains(err.Error(), "未启用") {
		t.Fatalf("应报渠道能力未启用: %v", err)
	}
}

// 插件身份令牌必须绑定「空间 + slug」，且只由部署密钥决定：
// 换空间、换插件、换密钥都要得到不同令牌，否则一个空间能冒用另一个空间的身份。
func TestPluginTokenBindsSpaceAndSlug(t *testing.T) {
	m := &Manager{o: Options{TokenSecret: "deploy-secret"}}
	tok := m.PluginToken("space-1", "dingtalk_bot")
	if len(tok) != 64 {
		t.Fatalf("令牌应是 32 字节的十六进制，实际 %q", tok)
	}
	if got := m.PluginToken("space-1", "dingtalk_bot"); got != tok {
		t.Fatalf("同一空间同一插件应复算出同一个令牌，%q != %q", got, tok)
	}
	if m.PluginToken("space-2", "dingtalk_bot") == tok {
		t.Fatal("换空间必须换令牌")
	}
	if m.PluginToken("space-1", "other_bot") == tok {
		t.Fatal("换插件必须换令牌")
	}
	if (&Manager{o: Options{TokenSecret: "another"}}).PluginToken("space-1", "dingtalk_bot") == tok {
		t.Fatal("换部署密钥必须让旧令牌失效")
	}
}

// 没配密钥时不签发：接口因此一律 401，而不是留一个匿名写入口。
func TestPluginTokenRequiresSecret(t *testing.T) {
	m := &Manager{}
	if got := m.PluginToken("space-1", "dingtalk_bot"); got != "" {
		t.Fatalf("没有密钥不应签发令牌，实际 %q", got)
	}
	if got := (&Manager{o: Options{TokenSecret: "x"}}).PluginToken("", "dingtalk_bot"); got != "" {
		t.Fatalf("缺空间不应签发令牌，实际 %q", got)
	}
}
