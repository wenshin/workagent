package plugin

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/workagent/auto-agent/internal/platform"
	"github.com/workagent/auto-agent/internal/session"
	"github.com/workagent/auto-agent/internal/store"
)

// IM 渠道桥：**渠道插件是宿主的下游，不是控制器**。
//
// 以前钉钉插件把收到的消息存进自己的内存数组（state.inbox），等模型在某个会话里
// 主动 drain_inbox 才取走。那条路有三个走不通的地方：一来没人拉就只有内存，
// 进程一重启消息全丢；二来投递目标是一个人类会话，渠道消息进不去自己的会话；
// 三来权限与审计都挂不上。
//
// 现在的方向反过来：插件发现「有消息要投递」就报给宿主，宿主负责把消息送进
// 那个人固定的渠道会话并跑完一轮，再把答复还给插件去回消息。
//
// 因此这里只需要两组接口：
//
//	POST /spaces/{id}/plugins/{slug}/inbox        插件报告「这些消息要投递哪个人」
//	POST /spaces/{id}/plugins/{slug}/inbox/reply  插件报告「这条消息我用这句话回」
//
// 为什么不让插件直接调 /sessions/{id}/chat：那条路认的是登录 Cookie，
// 而插件进程没有、也不该有任何人身份。插件唯一的身份是「某空间里的某个插件」，
// 拿到的只能是它被允许做的那两件事。这也让「能发消息的插件」与「能读别人会话的
// 插件」彻底分开——渠道插件即使被改坏，也读不到任何会话内容。
//
// 投递目标永远由宿主算：渠道 → 一批人员 → 每人自己的个人空间里那个固定会话。
// 插件不能指定空间或会话，否则一个被篡改的插件就能往任意会话里塞外部内容。

// PluginTokenHeader 是渠道插件回调宿主时携带的身份头。
//
// 为什么不是 Cookie：插件进程既没有、也不该有人的登录态——它代表的不是某个人，
// 而是「某个空间里的某个插件」。用人的凭证等于让渠道插件继承那个人的全部权限，
// 而它需要做的只有「把消息交给宿主」这一件事。
const PluginTokenHeader = "X-WA-Plugin-Token"

// PluginToken 派生某空间某插件的身份令牌。
//
// 令牌不落库、不进清单：由 (空间, slug) 用部署密钥做 HMAC 现算，宿主侧同一份
// 密钥复算即可校验。这样重启后令牌不变（常驻插件的配置不会因为重启失效），
// 也不需要多一张表或多一次写入；换部署密钥即整体失效，重装插件不换密钥也不变。
//
// 密钥缺失时返回空串：接口一律 401。宁可渠道暂时不通，也不开一个谁都能往
// 别人会话里塞消息的匿名写入口。
func (m *Manager) PluginToken(spaceID, slug string) string {
	if m.o.TokenSecret == "" || spaceID == "" || slug == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(m.o.TokenSecret))
	mac.Write([]byte("wa-plugin-inbox|" + spaceID + "|" + slug))
	return hex.EncodeToString(mac.Sum(nil))
}

// IMInboxItem 是插件上报的一条待投递消息。
type IMInboxItem struct {
	// ChannelUID 是发送者在渠道侧的稳定 id（钉钉 staffId）。宿主用它找平台用户。
	ChannelUID string `json:"channel_uid"`
	// Name 只用于「你不是平台用户」这类提示，不参与匹配。
	Name string `json:"name"`
	// Text 是要投给模型的消息正文。
	Text string `json:"text"`
	// Ref 是插件自己的消息 id，原样回传在结果里，便于它做去重与回执。
	Ref string `json:"ref"`
	// ReplyTo 是「回这条消息」需要的凭据（钉钉是 sessionWebhook）。
	// 宿主不解析它，只保证原样回给插件——那是渠道侧的格式，宿主不该知道。
	ReplyTo string `json:"reply_to"`
}

// IMInboxResult 是一条消息的处理结果。
type IMInboxResult struct {
	Ref string `json:"ref"`
	// ChannelUID 回传，插件据此决定回给谁。
	ChannelUID string `json:"channel_uid"`
	// OK 表示已投递并跑完一轮；false 时 Reason 给插件一句可直接回给用户的话。
	OK bool `json:"ok"`
	// Reason 是给**渠道用户**看的中文原因（不是给开发者的堆栈）。
	Reason string `json:"reason"`
	// Reply 是模型这一轮的答复，插件应回发给 ChannelUID。
	Reply     string `json:"reply"`
	SessionID string `json:"session_id"`
	// NeedLogin 为 true 表示这个人没登录过平台：插件可以据此回一条带引导的文案。
	NeedLogin bool `json:"need_login"`
}

// IMInboxReply 是插件报告「某条消息已经回过了」。
type IMInboxReply struct {
	Ref string `json:"ref"`
	// ReplyTo 回传原值，宿主只用于审计（哪个渠道目标被回过）。
	ReplyTo string `json:"reply_to"`
	// Note 是插件自己的说明（成功或失败原因），进日志与审计。
	Note string `json:"note"`
}

// IMSubmitter 是宿主提供的投递能力（*session.Service 实现）。
//
// 只暴露两个方法：渠道插件能做的事只有「投递」与「报告回执」。
// 用接口而不是直接依赖 *session.Service，是因为插件层不能反向依赖会话层
// 的完整能力——那会让「插件能不能读会话」的答案随一处小改动而变。
type IMSubmitter interface {
	HandleIM(ctx context.Context, in session.IMSender, content string) (session.IMDelivery, error)
}

// SetIM 注入投递能力。为 nil 时 inbox 接口返回「渠道能力未启用」。
func (m *Manager) SetIM(svc IMSubmitter) {
	m.mu.Lock()
	m.im = svc
	m.mu.Unlock()
}

// inboxDedup 是「消息 id → 处理结果」的短期缓存。
//
// 渠道侧重投很常见（网络抖动、进程重启后补发），而投递一次就是一轮模型调用。
// 不去重等于同一个问题被回答两遍、还写进两条消息。只留最近的一小段：
// 这是幂等保护，不是消息存档，存档在会话里。
type inboxDedup struct {
	mu    sync.Mutex
	limit int
	ttl   time.Duration
	items map[string]inboxDedupItem
	order []string
}

type inboxDedupItem struct {
	at     time.Time
	result IMInboxResult
}

func newInboxDedup(limit int, ttl time.Duration) *inboxDedup {
	if limit <= 0 {
		limit = 512
	}
	return &inboxDedup{limit: limit, ttl: ttl, items: map[string]inboxDedupItem{}}
}

func (d *inboxDedup) get(key string) (IMInboxResult, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	it, ok := d.items[key]
	if !ok {
		return IMInboxResult{}, false
	}
	if d.ttl > 0 && time.Since(it.at) > d.ttl {
		delete(d.items, key)
		return IMInboxResult{}, false
	}
	return it.result, true
}

func (d *inboxDedup) put(key string, res IMInboxResult) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, exists := d.items[key]; !exists {
		d.order = append(d.order, key)
	}
	d.items[key] = inboxDedupItem{at: time.Now(), result: res}
	for len(d.order) > d.limit {
		oldest := d.order[0]
		d.order = d.order[1:]
		delete(d.items, oldest)
	}
}

// imResultTTL 是去重记录的保留时长：够覆盖一次渠道重投的窗口即可。
const imResultTTL = 10 * time.Minute

// SubmitInbox 把渠道插件上报的一批消息逐条投进各人的渠道会话。
//
// 串行处理而不是并发：一次投递就是一轮模型调用，同一个插件的多条消息并发会
// 一起抢模型配额与彼此的历史；渠道侧本来就是排队来的消息，串行不影响时效。
func (m *Manager) SubmitInbox(ctx context.Context, spaceID, slug string, items []IMInboxItem) ([]IMInboxResult, error) {
	m.mu.RLock()
	sub := m.im
	inst := m.inst[spaceID][slug]
	m.mu.RUnlock()

	if sub == nil {
		return nil, errors.New("渠道投递能力未启用")
	}
	if inst == nil {
		return nil, store.ErrNotFound
	}
	// 只有声明了 im.reply 的插件能投递：清单里的能力是审批对象，
	// 没声明的插件即使改代码去调这个接口也必须在这里被挡住。
	if !inst.Manifest.HasCapability(CapIMReply) {
		return nil, fmt.Errorf("插件 %s 未声明 im.reply，不能投递消息", slug)
	}

	channel := m.imChannel(inst)
	out := make([]IMInboxResult, 0, len(items))
	for _, it := range items {
		out = append(out, m.deliverOne(ctx, sub, inst, channel, it))
	}
	return out, nil
}

// imChannel 取插件所属的渠道标识。
//
// 优先用清单里的 im.channel（一个插件可以只服务一个渠道），缺省回落到 slug：
// 渠道标识进数据库、也进会话的绑定键，必须稳定且唯一。
func (m *Manager) imChannel(inst *instance) string {
	if inst.Manifest.IM != nil {
		if c := strings.TrimSpace(inst.Manifest.IM.Channel); c != "" {
			return c
		}
	}
	return inst.Slug
}

func (m *Manager) deliverOne(ctx context.Context, sub IMSubmitter, inst *instance, channel string, it IMInboxItem) IMInboxResult {
	res := IMInboxResult{Ref: it.Ref, ChannelUID: it.ChannelUID}

	text := strings.TrimSpace(it.Text)
	uid := strings.TrimSpace(it.ChannelUID)
	if uid == "" {
		res.Reason = "消息缺少发送者 id"
		return res
	}
	if text == "" {
		res.Reason = "消息正文为空"
		return res
	}

	key := channel + "|" + inst.Slug + "|" + firstNonEmpty(it.Ref, uid+"|"+text)
	if prev, ok := m.imDedup.get(key); ok {
		return prev
	}

	delivery, err := sub.HandleIM(ctx, session.IMSender{
		Channel:     channel,
		ProviderUID: uid,
		Name:        it.Name,
	}, text)

	switch {
	case err == nil:
		res.OK = true
		res.Reply = delivery.Reply
		res.SessionID = delivery.Session.ID
	case errors.Is(err, session.ErrIMUnknownUser):
		// 这是渠道侧的常见情况：同事在钉钉里问了一句，但他没上过平台。
		// 必须回一句人话，而不是 500——他要的是「去登录一下」，不是重试。
		res.NeedLogin = true
		res.Reason = "你还没有登录过平台，请先在工作台登录一次再发消息"
	case errors.Is(err, session.ErrIMBusy):
		res.Reason = "上一轮还在执行，请稍后再发"
	default:
		res.Reason = "处理失败：" + err.Error()
		m.o.Logger.Warn("IM 消息投递失败",
			"channel", channel, "slug", inst.Slug, "ref", it.Ref, "err", err)
	}

	m.imDedup.put(key, res)
	return res
}

// RecordInboxReply 记录插件对某条消息的回执。只进日志与审计：
// 回复动作本身发生在插件里（各渠道的发送方式差别太大，宿主不该掺和），
// 这里只保证「回过哪条消息」在平台侧留痕。
func (m *Manager) RecordInboxReply(ctx context.Context, spaceID, slug string, in IMInboxReply) error {
	m.mu.RLock()
	inst := m.inst[spaceID][slug]
	m.mu.RUnlock()
	if inst == nil {
		return store.ErrNotFound
	}
	if !inst.Manifest.HasCapability(CapIMReply) {
		return fmt.Errorf("插件 %s 未声明 im.reply", slug)
	}
	m.o.Logger.Info("IM 消息已回执",
		"channel", m.imChannel(inst), "slug", slug, "ref", in.Ref, "note", in.Note)
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// ---------------------------------------------------------------- HTTP 接口

// pluginCaller 校验「调用方是这个空间里的这个插件」。
//
// 契约与登录态完全无关：空间与 slug 来自路径，令牌来自请求头，宿主用部署密钥
// 复算一遍比对。没有可伪造的字段——插件选不了别的空间，也选不了别人的会话；
// 能做的只有上报消息，投给谁由宿主按渠道 uid 去算。
func (h *Handler) pluginCaller(w http.ResponseWriter, r *http.Request) bool {
	spaceID, slug := r.PathValue("id"), r.PathValue("slug")
	want := h.mgr.PluginToken(spaceID, slug)
	got := strings.TrimSpace(r.Header.Get(PluginTokenHeader))
	// 空令牌恒不等：密钥没配时不签发，也就没人能通过。
	if want == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		platform.WriteError(w, r, http.StatusUnauthorized, "unauthorized",
			"插件身份校验失败：请通过 wa.config() 读取平台注入的 host_token")
		return false
	}
	return true
}

func (h *Handler) imInbox(w http.ResponseWriter, r *http.Request) {
	if !h.pluginCaller(w, r) {
		return
	}
	var body struct {
		Items []IMInboxItem `json:"items"`
	}
	if err := platform.DecodeJSON(r, &body); err != nil {
		platform.WriteError(w, r, http.StatusBadRequest, "bad_request", "请求体格式错误")
		return
	}
	if len(body.Items) == 0 {
		platform.WriteJSON(w, http.StatusOK, map[string]any{"results": []IMInboxResult{}})
		return
	}
	res, err := h.mgr.SubmitInbox(r.Context(), r.PathValue("id"), r.PathValue("slug"), body.Items)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"results": res})
}

func (h *Handler) imInboxReply(w http.ResponseWriter, r *http.Request) {
	if !h.pluginCaller(w, r) {
		return
	}
	var body IMInboxReply
	if err := platform.DecodeJSON(r, &body); err != nil {
		platform.WriteError(w, r, http.StatusBadRequest, "bad_request", "请求体格式错误")
		return
	}
	if err := h.mgr.RecordInboxReply(r.Context(), r.PathValue("id"), r.PathValue("slug"), body); err != nil {
		h.fail(w, r, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}
