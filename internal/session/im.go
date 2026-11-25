package session

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/workagent/auto-agent/internal/store"
)

// IM 投递的错误分类。投递方（渠道插件）据此决定是「回一句让用户去登录」
// 还是「重试」——把这两种混成一个 500，插件只能一律重试，用户永远收不到提示。
var (
	// ErrIMUnknownUser 表示渠道 id 在平台里没有对应账号。
	ErrIMUnknownUser = errors.New("该用户尚未登录过平台，无法投递")
	// ErrIMChannelDisabled 表示渠道被平台停用或未注册。
	ErrIMChannelDisabled = errors.New("该渠道未启用")
	// ErrIMBusy 表示上一轮还在跑：同一条会话不接受并发投递。
	ErrIMBusy = errors.New("该会话上一轮还在执行，请稍后再发")
)

// IMDelivery 是一次外部消息投递的落库结果，渠道插件据此回消息。
type IMDelivery struct {
	Session IMRef `json:"session"`
	// Created 报告这个渠道会话是不是本次新建的。
	Created bool `json:"created"`
	// Reply 是模型这一轮的最终答复，渠道插件直接回给用户。
	Reply string `json:"reply"`
	// RunID 供排查（一次投递 = 一次 run）。
	RunID string `json:"run_id"`
}

// IMRef 只暴露渠道插件需要的那几个会话字段。
type IMRef struct {
	ID      string `json:"id"`
	SpaceID string `json:"space_id"`
	Title   string `json:"title"`
}

// IMSender 是一条外部消息的发送者。
type IMSender struct {
	// Channel 是渠道标识（dingtalk / wecom ...），与 store.IMChannel.Channel 对应。
	Channel string
	// ProviderUID 是渠道侧的稳定用户 id（钉钉是 staffId / userid）。
	ProviderUID string
	// Name 是展示名，只进日志与「未登录」提示，不参与权限判定。
	Name string
}

// 说明：ProviderUID 这个名字沿用身份层的命名，但从 IM 回调拿到的是渠道自己的
// 员工 id（钉钉 senderStaffId），不是登录用的 unionId。两者的区别与坑见
// store.FindUserIDByStaff 的注释——弄反了会安静地一条也匹配不上。

// HandleIM 把一条外部 IM 消息投进该用户固定的渠道会话，同步等这一轮跑完。
//
// 设计要点（对应「钉钉绑一个会话、企微绑另一个」的需求）：
//
//  1. 绑定键是 (channel, user)，不是 (channel, 群)。同一个人在钉钉里从哪个群、
//     哪个私聊发来的消息都进同一个会话：上下文连续，工具权限也只按这个人算。
//  2. 会话不存在就建，建在**用户自己的个人空间**里。渠道消息绝不能落进共享空间：
//     run 的 actor 是会话所有者，共享空间里别人就能看到渠道内容。
//  3. 这一轮的权限仍是所有者本人的权限（ChatWithSource 的既有语义），
//     外部消息不会把「别人说的话」变成「所有者替别人执行」。
//
// 同步执行而不是排队：一次投递是一问一答，常驻的渠道进程本来就要等答案去回消息。
// 同一会话已有 run 时直接拒绝，不排队——排队会让用户以为消息丢了，
// 明确拒绝至少能收到一句话。
func (s *Service) HandleIM(ctx context.Context, in IMSender, content string) (IMDelivery, error) {
	out := IMDelivery{}

	channel := strings.TrimSpace(in.Channel)
	if channel == "" {
		return out, errors.New("缺少渠道标识")
	}
	if strings.TrimSpace(in.ProviderUID) == "" {
		return out, errors.New("缺少发送者在渠道侧的 id")
	}
	text, err := normalizeMessage(content)
	if err != nil {
		return out, err
	}

	ch, err := s.store.GetIMChannel(ctx, channel)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return out, fmt.Errorf("%w：%s", ErrIMChannelDisabled, channel)
		}
		return out, err
	}
	if !ch.Enabled {
		return out, fmt.Errorf("%w：%s", ErrIMChannelDisabled, channel)
	}
	label := strings.TrimSpace(ch.Label)
	if label == "" {
		label = channel
	}

	// 渠道 id → 平台用户。查的是登录时写入的 user_identity（员工 id 列），
	// 不猜工号或姓名：猜错就是把一个人的消息投进另一个人的会话，属于不可挽回的串号。
	userID, err := s.store.FindUserIDByStaff(ctx, channel, strings.TrimSpace(in.ProviderUID))
	if err != nil {
		return out, err
	}
	if userID == "" {
		return out, fmt.Errorf("%w（%s 用户 %s）", ErrIMUnknownUser, label, in.Name)
	}
	// 渠道会话建在个人空间：登录时已保证它存在，但被派发过任务的同事也可能有，
	// 这里统一走同一条幂等路径。
	spaceID, err := s.ensurePersonalSpace(ctx, userID)
	if err != nil {
		return out, err
	}

	sess, created, err := s.store.GetOrCreateChannelSession(ctx, channel, spaceID, userID, label+"消息",
		"", "", "", "")
	if err != nil {
		return out, err
	}
	out.Created = created
	out.Session = IMRef{ID: sess.ID, SpaceID: sess.SpaceID, Title: sess.Title}

	// 同一会话同一时刻只允许一轮：并发投递会让两轮抢同一份历史，
	// 后到的那轮看到的上下文缺前半段，回答会自相矛盾。
	if s.runs.active(sess.ID) != nil {
		return out, ErrIMBusy
	}

	deliverCtx := ctx
	if s.runBudget > 0 {
		var cancel context.CancelFunc
		deliverCtx, cancel = context.WithTimeout(ctx, s.runBudget)
		defer cancel()
	}

	outcome, err := s.ChatWithSource(deliverCtx, userID, sess.ID, text, "external", nil)
	out.RunID = outcome.RunID
	if err != nil {
		return out, err
	}
	out.Session.Title = outcome.Session.Title

	// 这一轮的最终答复要原样回给用户：取最后一条助手消息。
	// 取不到不算失败——那一轮可能是被人取消的，渠道侧收到空 reply 就不回消息。
	if reply, rerr := s.lastAssistant(sess.ID); rerr == nil {
		out.Reply = reply
	}
	s.log.Info("IM 消息已投递",
		"channel", channel, "user_id", userID, "session_id", sess.ID,
		"run_id", outcome.RunID, "created", created, "status", outcome.Status)
	return out, nil
}

// lastAssistant 取会话里最后一条助手消息正文，作为回给渠道用户的答复。
func (s *Service) lastAssistant(sessionID string) (string, error) {
	msgs, err := s.store.ListMessages(context.Background(), sessionID)
	if err != nil {
		return "", err
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" && strings.TrimSpace(msgs[i].Content) != "" {
			return msgs[i].Content, nil
		}
	}
	return "", nil
}
