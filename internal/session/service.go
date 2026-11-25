package session

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/workagent/auto-agent/internal/agent"
	"github.com/workagent/auto-agent/internal/agentspec"
	"github.com/workagent/auto-agent/internal/cache"
	"github.com/workagent/auto-agent/internal/ctxmgr"
	"github.com/workagent/auto-agent/internal/llm"
	"github.com/workagent/auto-agent/internal/sandbox"
	"github.com/workagent/auto-agent/internal/search"
	"github.com/workagent/auto-agent/internal/secret"
	"github.com/workagent/auto-agent/internal/space"
	"github.com/workagent/auto-agent/internal/spacefs"
	"github.com/workagent/auto-agent/internal/spacelog"
	"github.com/workagent/auto-agent/internal/storage"
	"github.com/workagent/auto-agent/internal/store"
	"github.com/workagent/auto-agent/internal/tool"
)

var (
	ErrForbidden        = errors.New("没有该空间的权限")
	ErrTitleInvalid     = errors.New("会话标题需为 1-64 个字符")
	ErrMessageEmpty     = errors.New("消息内容不能为空")
	ErrMessageTooLong   = errors.New("单条消息不能超过 8000 字")
	ErrLastSession      = errors.New("每个空间至少要保留一个会话")
	ErrAgentUnavailable = errors.New("模型服务未配置，请联系管理员配置 LLM_BASE_URL、LLM_API_KEY、LLM_MODEL")
	ErrRunFinished      = errors.New("任务已经结束，无法继续操作")
)

const (
	defaultTitle    = "新会话"
	maxTitleRunes   = 64
	maxMessageRunes = 8000
	// 首条消息自动生成的标题长度（超出部分省略）
	autoTitleRunes = 20

	// defaultRunBudget 与配置项 LLM_MAX_WALL_CLOCK 的缺省值保持一致。
	// 它是「总上限」，只防「确实在动但永远跑不完」，不是长任务的成败判据。
	defaultRunBudget = 2 * time.Hour
	// defaultIdleTimeout 与配置项 RUN_IDLE_TIMEOUT 的缺省值保持一致：
	// 连续这么久没有任何事件才算卡住。
	defaultIdleTimeout = 10 * time.Minute
	// resumeTTL 是断点的保质期。超过这个时间的快照不再提供「继续」：
	// 文件树、插件、同事的回复都变了，接上去等于让模型基于过期世界行动。
	resumeTTL = 7 * 24 * time.Hour

	// spaceFilesDir 与 space 包的文件根一致（spaces/{id}/files）；用字面量避免为一行常量牵出循环依赖
	spaceFilesDir = "files"
	// maxReferences 是单条消息最多引用的文件数：够用即可，避免把上下文塞满路径清单
	maxReferences = 20
	// ReferenceMarker 是用户消息里「引用文件」块的起始标记。
	// 前端按它把正文与文件标签分开渲染，模型按它识别用户明确要求参考的材料。
	ReferenceMarker = "\n\n[引用文件]"
)

// 中断原因（§7.3 run.finished.reason）：一次被掐断的流要能区分「用户停止」与「服务端到点」。
const (
	// ReasonClientDisconnected 客户端断开或服务端关闭，事件已经写不出去。
	//
	// Deprecated: run 已不再随请求上下文结束，断开只解绑流（§15.12.1）。
	// 保留常量是为了让历史数据与旧前端仍能渲染。
	ReasonClientDisconnected = "client_disconnected"
	// ReasonUserCancelled 用户显式取消（POST /runs/{id}/cancel）或关掉任务
	ReasonUserCancelled = "user_cancelled"
	// ReasonWallClockExceeded 总时长上限到点（最后一道保险），连接仍可用，会补发 run.finished
	ReasonWallClockExceeded = "wall_clock_exceeded"
	// ReasonIdleTimeout 连续一段时间没有任何事件：模型流卡死、工具挂住或进程半死。
	// 这才是「卡住」的判据——跑得久不算问题，没有进展才算。
	ReasonIdleTimeout = "idle_timeout"
	// ReasonBudgetExceeded 实现自己的预算（墙钟 / 工具次数 / 轮次）用尽
	ReasonBudgetExceeded = "budget_exceeded"
	// ReasonStalled 循环守卫判定「再做也不会产生新进展」而提前收尾。
	// 与 budget_exceeded 分开记：前者说明任务还有活没干完，后者说明方法本身没效。
	ReasonStalled = "stalled"
	// ReasonAwaitingApproval 这一轮停在人工确认上：不是失败，也不是取消。
	//
	// 待办与断点都还在，用户点同意/拒绝后由决策驱动的续跑接着走；
	// 一直不点，会话就一直是「已暂停」——没有超时，也不会自己挑一个默认答案。
	ReasonAwaitingApproval = "awaiting_approval"
)

// RunBudgetExceededMessage 是总时长上限到点时的用户可见文案。
const RunBudgetExceededMessage = "本次回答已达总时长上限，已保留已生成的内容"

// RunIdleTimeoutMessage 是长时间没有进展时的用户可见文案。
//
// 与「时长上限」分开说：前者是这一轮真的卡住了，后者是跑得太久，
// 混成一句会让用户以为「是不是我任务太大」，而实际原因完全不同。
const RunIdleTimeoutMessage = "本轮长时间没有进展，已保留已生成的内容，可以继续"

// RunAbandonedMessage 是「库里还挂着 running、进程里已经没有执行体」时的收尾文案。
//
// 这条 run 不会再有事件，也不会有谁来写终态：进程被 kill、崩溃重启都会留下它。
// 措辞不提「继续」——断点不保证存在（中途未保存断点的那一轮就点不了继续）。
const RunAbandonedMessage = "这一轮执行已中断（服务端已不再持有它），已保留已生成的内容"

// cancelDetail 生成 run.error：预算是实现给出的具体说明（如「超过时间预算（10m0s）」），其余用原因常量。
func cancelDetail(reason string, runErr error) string {
	if reason == "" {
		return ""
	}
	// 落库的是给人看的原因：事后翻 run.error 时「idle_timeout」不如一句话直白
	switch reason {
	case ReasonIdleTimeout:
		return RunIdleTimeoutMessage
	case ReasonWallClockExceeded:
		return RunBudgetExceededMessage
	case ReasonUserCancelled:
		return "已按用户请求停止"
	case ReasonAwaitingApproval:
		return "等待人工确认，已暂停"
	}
	if errors.Is(runErr, agent.ErrBudgetExceeded) || errors.Is(runErr, agent.ErrStalled) {
		return runErr.Error()
	}
	return reason
}

// Service 承载聊天会话的业务规则。
type Service struct {
	store   store.Store
	spaces  *space.Service
	agent   agent.Agent
	storage storage.Storage
	memory  *ctxmgr.Manager
	// runBudget 是一轮回答的总时长上限（最后由 ctx 兜底），由 LLM_MAX_WALL_CLOCK 得到；
	// 0 表示不限——它只防「确实在动但永远跑不完」，不当长任务的成败判据。
	runBudget time.Duration
	// idleTimeout 是连续多久没有任何事件就判定卡住（RUN_IDLE_TIMEOUT）。
	// 长任务可以跑很久，但不能「没有动静地」跑很久，这才是要掐的情况。
	idleTimeout time.Duration
	// agentPlugins 是智能体来源（kind=agent 插件），为空时不注入追加提示词
	agentPlugins AgentPluginSource
	// search 是搜索 provider 注册表，为 nil 时 web_search 返回「能力未配置」
	search *search.Registry
	// shell 是命令执行沙箱，为 nil 时 shell_exec 返回「能力未配置」
	shell sandbox.Runner
	// pluginTry 是插件试跑（plugin_try），为 nil 时该工具不可用
	pluginTry tool.PluginTry
	// pluginRestart 是常驻进程重启（plugin_restart），为 nil 时该工具不可用
	pluginRestart tool.PluginRestarter
	// pluginLogs 是插件调用记录（plugin_logs），为 nil 时该工具报不可用
	pluginLogs tool.PluginLogs
	// spaceLog 是空间运行日志（files/.logs），为 nil 时不写
	spaceLog *spacelog.Logger
	// secrets 是凭证存储（secret_request 用它判断「配过没有」），为 nil 时该工具只报未启用
	secrets secret.Store
	// platform 是平台部署事实（租户 / IM 接入），每轮注入系统提示词
	platform agent.PlatformFacts
	// runs 是进程内正在执行的 run：既提供显式取消，也提供断流续传的缓冲。
	// 它不持久化——run 的权威状态在库里，这里只是「还活着」的那一份视图。
	runs *liveRuns
	// stream 是会话流的缓存（Redis）：登录态之外的另一半职责。
	// 为 nil 时退回纯内存（单实例、进程一死就丢正文）。
	stream cache.Client
	// autoBudget 限制「回执触发的 run」的密度，避免十条回执催出十轮模型调用。
	autoBudget *autoRunBudget
	// approvals 是审批通道：命中出网写 / 高危命令时把执行体挂起，等前端点按钮。
	approvals *approvalHub
	// resuming 记录「正在自动续跑」的会话：批准一个已经没人等的待办时，
	// 连点两次不该起两份执行。
	resuming sync.Map
	log      *slog.Logger
}

// NewService 构造会话服务；能力源（插件目录）由空间服务与插件层提供。
//
// wallClock 是一轮回答的总时长上限（LLM_MAX_WALL_CLOCK，<=0 表示不限），
// idleTimeout 是「多久没有事件算卡住」（RUN_IDLE_TIMEOUT）。两者管的是不同的事：
// 前者防无限跑，后者才防真卡死。
func NewService(st store.Store, spaces *space.Service, agents agent.Agent, fs storage.Storage, memory *ctxmgr.Manager, searcher *search.Registry, shell sandbox.Runner, stream cache.Client, wallClock, idleTimeout time.Duration, log *slog.Logger) *Service {
	if idleTimeout <= 0 {
		idleTimeout = defaultIdleTimeout
	}
	// 总时长不允许比 idle 还短：那不是「上限」，是把正常的长回答一起砍掉
	if wallClock > 0 && wallClock < idleTimeout {
		wallClock = idleTimeout
	}
	return &Service{
		store: st, spaces: spaces, agent: agents, storage: fs,
		memory: memory, search: searcher, shell: shell,
		runBudget: wallClock, idleTimeout: idleTimeout, runs: newLiveRuns(),
		stream:     stream,
		autoBudget: newAutoRunBudget(), approvals: newApprovalHub(st, log), log: log,
	}
}

// AgentPluginSource 提供插件化智能体（kind=agent）。
//
// 之所以定义接口而不是直接依赖插件包：会话层只需要「有哪些智能体」和「某个智能体的正文」，
// 不关心它来自发布件、空间安装还是系统空间的全局安装。
type AgentPluginSource interface {
	ListAgentPlugins(ctx context.Context, spaceID string) ([]agentspec.Summary, error)
	// AgentDisplayNames 返回空间内**全部**智能体的展示名（含已停用与草稿）。
	// 只用于会话列表渲染：停用的智能体不能新建会话，但历史会话还在。
	AgentDisplayNames(ctx context.Context, spaceID string) (map[string]string, error)
	ResolveAgentPlugin(ctx context.Context, spaceID, slug string) (agentspec.Resolved, error)
	// AgentDependencies 返回智能体及其 requires 闭包，用于跨空间借用时只加载它自己声明的能力。
	AgentDependencies(ctx context.Context, spaceID, slug string) ([]string, error)
	// AgentInternals 返回智能体自带的技能页（agent.skills）。只在绑定它的会话里注入，
	// 不进空间索引，所以别的智能体搜不到。
	AgentInternals(ctx context.Context, spaceID, slug string) ([]tool.PluginEntry, error)
}

// SetAgentPlugins 注入插件化智能体来源；不注入时行为与旧版完全一致。
func (s *Service) SetAgentPlugins(src AgentPluginSource) { s.agentPlugins = src }

// SetPluginTry 注入插件试跑能力；不注入时 plugin_try 返回「能力未配置」。
func (s *Service) SetPluginTry(src tool.PluginTry) { s.pluginTry = src }

// SetPluginRestart 注入常驻进程重启能力；不注入时 plugin_restart 返回「能力未配置」。
func (s *Service) SetPluginRestart(src tool.PluginRestarter) { s.pluginRestart = src }

// SetPluginLogs 注入插件调用记录；不注入时 plugin_logs 返回「能力未配置」。
func (s *Service) SetPluginLogs(src tool.PluginLogs) { s.pluginLogs = src }

// SetSpaceLog 注入空间运行日志；不注入时不写，执行链路完全不受影响。
func (s *Service) SetSpaceLog(l *spacelog.Logger) { s.spaceLog = l }

// SetSecrets 注入凭证存储。
//
// 只用于 secret_request 查询「这个名字配过没有」以及是否可写；明文始终留在
// 凭证存储里，工具链路上没有任何取得明文的通道。
func (s *Service) SetSecrets(st secret.Store) { s.secrets = st }

// SetPlatformFacts 注入平台部署事实（租户兜底名、IM 接入），每轮写进系统提示词。
// 不注入时只是少一段事实，模型会如实说不知道，而不是猜。
func (s *Service) SetPlatformFacts(f agent.PlatformFacts) { s.platform = f }

// AgentReady 返回模型服务是否可用。
func (s *Service) AgentReady() bool { return s.agent != nil && s.agent.Ready() }

// AgentModel 返回当前模型名。
func (s *Service) AgentModel() string {
	if s.agent == nil {
		return ""
	}
	return s.agent.Model()
}

// AgentImpl 返回当前生效的 Agent 实现名（§5.7.7 实现切换）。
func (s *Service) AgentImpl() string {
	if s.agent == nil {
		return ""
	}
	return s.agent.Name()
}

// AgentCapabilities 返回当前实现的能力声明，供接口层向客户端说明。
func (s *Service) AgentCapabilities() agent.Capabilities {
	if s.agent == nil {
		return agent.Capabilities{}
	}
	return s.agent.Capabilities()
}

// List 返回空间下的会话。
func (s *Service) List(ctx context.Context, userID, spaceID string) ([]store.Session, error) {
	if _, err := s.spaces.Get(ctx, userID, spaceID); err != nil {
		return nil, err
	}
	all, err := s.store.ListSessions(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	// 列表也必须过滤：只藏详情、不藏列表等于没藏（§15.12.3）
	out := make([]store.Session, 0, len(all))
	for _, sess := range all {
		if sess.CreatedBy == "" || sess.CreatedBy == userID {
			out = append(out, sess)
		}
	}
	return out, nil
}

// ListMine 返回当前用户可见的全部会话（左侧树一次取数）。
func (s *Service) ListMine(ctx context.Context, userID string) ([]store.Session, error) {
	return s.store.ListUserSessions(ctx, userID)
}

// Create 在空间（或空间内某个目录）下新建会话，标题默认为「新会话」。
//
// agentID 是 agents/ 目录里的智能体 id，空表示用默认的 base；
// 智能体只影响追加提示词，不影响权限。
func (s *Service) Create(ctx context.Context, userID, spaceID, rawTitle, rawDir, agentID string) (store.Session, error) {
	sp, err := s.spaces.Get(ctx, userID, spaceID)
	if err != nil {
		return store.Session{}, err
	}
	if !canWrite(sp.Perm) {
		return store.Session{}, ErrForbidden
	}

	title, err := normalizeTitle(rawTitle)
	if err != nil {
		return store.Session{}, err
	}

	// 会话可以挂在目录下：目录必须真实存在，避免树里出现悬空节点
	dir, err := normalizeDirPath(rawDir)
	if err != nil {
		return store.Session{}, err
	}
	if err := s.spaces.CheckDir(ctx, spaceID, dir); err != nil {
		return store.Session{}, err
	}

	agent, err := s.resolveAgent(ctx, spaceID, agentID)
	if err != nil {
		return store.Session{}, err
	}

	// 内置智能体不固化正文与版本：它是运营对象，卡片上改了要立刻生效。
	// 自建/安装的智能体继续固化，改版不会无声地改变已有会话的行为。
	version, prompt := agent.Version, agent.Prompt
	if agent.Builtin {
		version, prompt = "", ""
	}
	sess, err := s.store.CreateSession(ctx, spaceID, userID, title, dir, agent.ID, version, prompt)
	if err != nil {
		return store.Session{}, err
	}
	s.log.Info("会话已创建", "session_id", sess.ID, "space_id", spaceID, "dir", dir, "agent_id", agent.ID, "by", userID)
	return sess, nil
}

// resolveAgent 取会话要绑定的智能体。
//
// 请求为空时用默认的 base 插件；base 插件缺失（系统空间尚未种子化）时退化为
// 「无追加设定」，不阻塞会话创建。显式指定了不存在的智能体则报错，避免用户以为选上了。
func (s *Service) resolveAgent(ctx context.Context, spaceID, rawID string) (agentspec.Resolved, error) {
	id := strings.TrimSpace(rawID)
	useDefault := id == ""
	if useDefault {
		id = agentspec.BaseID
	}
	if s.agentPlugins == nil {
		return agentspec.Resolved{ID: id}, nil
	}

	r, err := s.agentPlugins.ResolveAgentPlugin(ctx, spaceID, id)
	if err != nil {
		if useDefault && errors.Is(err, store.ErrNotFound) {
			return agentspec.Resolved{}, nil
		}
		return agentspec.Resolved{}, err
	}
	return r, nil
}

// Agents 返回可选的智能体清单，供会话创建时的选择器使用。
//
// 唯一来源是 kind=agent 插件：系统空间安装的对所有空间可选，普通空间的只在本空间可选。
func (s *Service) Agents(ctx context.Context, userID, spaceID string) ([]agentspec.Summary, error) {
	if _, err := s.spaces.Get(ctx, userID, spaceID); err != nil {
		return nil, err
	}
	if s.agentPlugins == nil {
		return nil, nil
	}
	plugins, err := s.agentPlugins.ListAgentPlugins(ctx, spaceID)
	if err != nil {
		s.log.Warn("读取智能体插件失败", "space", spaceID, "err", err)
		return nil, nil
	}
	sort.Slice(plugins, func(i, j int) bool { return plugins[i].ID < plugins[j].ID })
	return plugins, nil
}

// AgentBadges 批量解析会话绑定的智能体，key 为「来源空间 + \x00 + slug」。
//
// 会话行里只固化了 slug；列表要显示名称与头像，就得回到插件快照去取。
// 按空间去重后每个空间只查一次，避免 N 个会话查 N 次。
func (s *Service) AgentBadges(ctx context.Context, list []store.Session) map[string]agentspec.Summary {
	out := make(map[string]agentspec.Summary, len(list))
	if s.agentPlugins == nil {
		return out
	}
	wanted := map[string]map[string]bool{}
	for _, sess := range list {
		if sess.AgentID == "" {
			continue
		}
		spaceID := sess.SpaceID
		if sess.AgentSpaceID != "" {
			spaceID = sess.AgentSpaceID
		}
		if wanted[spaceID] == nil {
			wanted[spaceID] = map[string]bool{}
		}
		wanted[spaceID][sess.AgentID] = true
	}
	for spaceID, slugs := range wanted {
		missing := make(map[string]bool, len(slugs))
		for slug := range slugs {
			missing[slug] = true
		}
		items, err := s.agentPlugins.ListAgentPlugins(ctx, spaceID)
		if err != nil {
			s.log.Warn("读取智能体插件失败", "space", spaceID, "err", err)
		}
		for _, item := range items {
			if slugs[item.ID] {
				out[spaceID+"\x00"+item.ID] = item
				delete(missing, item.ID)
			}
		}
		if len(missing) == 0 {
			continue
		}
		// 快照只含「已启用」的插件，而插件列表里草稿与停用件都在。会话图标要
		// 显示它的名字，否则会退化成英文 slug——同一张卡片中文、会话里英文。
		names, nerr := s.agentPlugins.AgentDisplayNames(ctx, spaceID)
		if nerr != nil {
			s.log.Warn("读取智能体展示名失败", "space", spaceID, "err", nerr)
			continue
		}
		for slug := range missing {
			if name, ok := names[slug]; ok {
				out[spaceID+"\x00"+slug] = agentspec.Summary{ID: slug, Name: name}
			}
		}
	}
	return out
}

// wikiSpaceID 返回企业 wiki 的宿主空间（系统空间）。
//
// 取不到就返回空串而不是报错：wiki 是增强能力，系统空间没建好时
// 对话本身不该受影响，由工具在调用点给出一条明确的「不可用」提示。
func (s *Service) wikiSpaceID(ctx context.Context) string {
	if s.spaces == nil {
		return ""
	}
	id, err := s.spaces.SystemSpaceID(ctx)
	if err != nil {
		return ""
	}
	return id
}

// wikiAgentID 是 wiki 分区用的智能体标识。
//
// 知识页跟着智能体走，所以这条链路上「空」不能是一个合法值：老会话（本次改动前
// 创建的、没有固化 agent_id 的）与 base 插件缺失时统一落到 base，保证同一个
// 会话的知识目录在重启前后不会跳变。
func wikiAgentID(raw string) string {
	if id := strings.TrimSpace(raw); id != "" {
		return id
	}
	return agentspec.BaseID
}

// canWriteSecrets 判断当前用户能否写平台凭证。
//
// 与 /admin/secrets 用同一套判定（系统管理员），否则会出现「卡片给了输入框、
// 保存却 403」这种更糟的体验。
func (s *Service) canWriteSecrets(user store.User) bool {
	if user.ID == "" {
		return false
	}
	return s.spaces.IsAdmin(user)
}

// platformFacts 汇总本轮注入系统提示词的平台事实。
//
// 租户名称以登录用户的通讯录资料为准（多租户部署下比配置里的兜底值准确），
// 取不到时沿用装配层注入的兜底值；IM 接入只来自装配层。查询失败不影响对话。
func (s *Service) platformFacts(user store.User, sp store.Space) agent.PlatformFacts {
	f := s.platform
	if sp.Name != "" {
		f.SpaceName = sp.Name
	}
	if user.ID == "" {
		return f
	}
	if tenant := strings.TrimSpace(user.CorpName); tenant != "" {
		f.Tenant = tenant
	}
	f.UserName = strings.TrimSpace(user.Name)
	f.JobNumber = strings.TrimSpace(user.JobNumber)
	return f
}

// pluginKindLabels 是插件类型的中文名，只用于提示词，不参与校验。
var pluginKindLabels = map[string]string{
	"skill":     "技能插件",
	"tool":      "工具插件",
	"mcp":       "MCP 插件",
	"component": "组件插件",
	"agent":     "智能体插件",
	"source":    "搜索源插件",
}

// dirPluginContext 判断会话目录是否属于某个插件（plugins/<slug>[/...]），
// 返回插件画像供系统提示词使用。
//
// 这里不做完整清单校验：用户可能正写到一半，清单暂时不合规也要给得出上下文，
// 所以只宽松解析展示字段；读不到也返回带 slug 的画像，至少让模型知道
// 「这是一个插件目录」，而不是当成普通目录。
func (s *Service) dirPluginContext(ctx context.Context, spaceID, dir string) *agent.DirPlugin {
	rest, ok := strings.CutPrefix(dir, "plugins/")
	if !ok {
		return nil
	}
	slug, _, _ := strings.Cut(rest, "/")
	if slug == "" {
		return nil
	}

	info := &agent.DirPlugin{Slug: slug}
	key := path.Join(s.storage.SpaceKey(spaceID), "files", "plugins", slug, "plugin.json")
	rc, err := s.storage.Get(ctx, key)
	if err != nil {
		return info
	}
	defer func() { _ = rc.Close() }()

	var raw struct {
		Kind        string `json:"kind"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(io.LimitReader(rc, 64<<10)).Decode(&raw); err != nil {
		s.log.Debug("插件清单读取失败，按目录名降级", "space", spaceID, "dir", dir, "err", err)
		return info
	}
	info.Name = strings.TrimSpace(raw.Name)
	info.Kind = pluginKindLabels[strings.TrimSpace(raw.Kind)]
	info.Description = strings.TrimSpace(raw.Description)
	return info
}

// normalizeDirPath 统一目录路径格式：去掉首尾斜杠与空白，空串表示空间根。
func normalizeDirPath(raw string) (string, error) {
	dir := strings.Trim(strings.TrimSpace(raw), "/")
	if dir == "" {
		return "", nil
	}
	if strings.Contains(dir, "..") || strings.Contains(dir, "\\") {
		return "", space.ErrNameInvalid
	}
	return dir, nil
}

// Rename 手动重命名会话；命名后不再被首条消息自动改写。
func (s *Service) Rename(ctx context.Context, userID, sessionID, rawTitle string) (store.Session, error) {
	sess, sp, err := s.load(ctx, userID, sessionID)
	if err != nil {
		return store.Session{}, err
	}
	if !canWrite(sp.Perm) {
		return store.Session{}, ErrForbidden
	}

	title, err := normalizeTitle(rawTitle)
	if err != nil {
		return store.Session{}, err
	}
	if err := s.store.RenameSession(ctx, sessionID, title, false); err != nil {
		return store.Session{}, err
	}
	sess.Title = title
	sess.TitleAuto = false
	return sess, nil
}

// Delete 删除会话，需要空间写权限；消息记录保留但不再展示。
func (s *Service) Delete(ctx context.Context, userID, sessionID string) error {
	sess, sp, err := s.load(ctx, userID, sessionID)
	if err != nil {
		return err
	}
	if !canWrite(sp.Perm) {
		return ErrForbidden
	}

	// 空间至少要保留一个会话，否则进入空间后没有对话入口
	if list, err := s.store.ListSessions(ctx, sess.SpaceID); err != nil {
		return err
	} else if len(list) <= 1 {
		return ErrLastSession
	}

	if err := s.store.DeleteSession(ctx, sessionID); err != nil {
		return err
	}
	s.log.Info("会话已删除", "session_id", sess.ID, "space_id", sess.SpaceID, "by", userID)
	return nil
}

// ValidateChat 校验一次对话请求，供 SSE 开始前返回常规错误码。
func (s *Service) ValidateChat(ctx context.Context, userID, sessionID, content string) (store.Session, store.Space, error) {
	sess, sp, err := s.load(ctx, userID, sessionID)
	if err != nil {
		return store.Session{}, store.Space{}, err
	}
	if !canWrite(sp.Perm) {
		return store.Session{}, store.Space{}, ErrForbidden
	}
	if _, err := normalizeMessage(content); err != nil {
		return store.Session{}, store.Space{}, err
	}
	if !s.AgentReady() {
		return store.Session{}, store.Space{}, ErrAgentUnavailable
	}
	return sess, sp, nil
}

// ResolveReferences 校验用户勾选的引用文件，返回去重后的相对路径（相对空间 files/）。
//
// 只做存在性校验，不做内容读取：正文由 Agent 在需要时用 file_read 自己取，
// 否则一条消息引用了大文件就会把上下文直接撑爆。文件在这期间被删掉时跳过，
// 不让一条已不存在的路径阻塞整条消息。
func (s *Service) ResolveReferences(ctx context.Context, userID, sessionID string, refs []string) ([]string, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	sess, _, err := s.load(ctx, userID, sessionID)
	if err != nil {
		return nil, err
	}
	base := path.Join(s.storage.SpaceKey(sess.SpaceID), spaceFilesDir)

	seen := make(map[string]bool, len(refs))
	out := make([]string, 0, len(refs))
	for _, raw := range refs {
		rel := strings.Trim(strings.TrimSpace(strings.ReplaceAll(raw, "\\", "/")), "/")
		if rel == "" || strings.Contains(rel, "..") {
			continue
		}
		// 平台保留的隐藏项不进引用（用户自己的 .env 等隐藏文件正常引用）
		blocked := false
		for _, seg := range strings.Split(rel, "/") {
			if seg == "" || seg == "." || seg == ".." || spacefs.Reserved(seg) {
				blocked = true
				break
			}
		}
		if blocked || seen[rel] {
			continue
		}
		entry, err := s.storage.Stat(ctx, path.Join(base, rel))
		if err != nil || entry.IsDir {
			continue
		}
		seen[rel] = true
		out = append(out, rel)
		if len(out) >= maxReferences {
			break
		}
	}
	return out, nil
}

// referenceFree 去掉消息末尾的引用清单：会话标题、日志摘要只该来自用户写的话。
func referenceFree(content string) string {
	if idx := strings.Index(content, ReferenceMarker); idx >= 0 {
		return content[:idx]
	}
	return content
}

// ComposeContent 把引用文件清单拼到用户消息末尾。
//
// 用一个稳定的标记块而不是各写各的自然语言：前端据此把清单渲染成文件标签，
// 模型据此知道这些文件是用户明确要求参考的材料。引用不限类型，但它读不到二进制：
// 清单行保持纯路径（前端按 "- " 行切标签），可读性说明放到清单之后——
// 文本才要求 file_read，图片 / 音视频 / 压缩包按附件对待，明确让它不要读。
func ComposeContent(text string, refs []string) string {
	if len(refs) == 0 {
		return text
	}
	hasReadable, hasAttachment := false, false
	for _, ref := range refs {
		if space.Readable(ref) {
			hasReadable = true
		} else {
			hasAttachment = true
		}
	}

	var b strings.Builder
	b.WriteString(text)
	b.WriteString(ReferenceMarker)
	b.WriteString("（相对当前空间文件根目录）\n")
	for _, ref := range refs {
		b.WriteString("- ")
		b.WriteString(ref)
		b.WriteString("\n")
	}
	if hasReadable {
		b.WriteString("回答前先逐个 file_read 读取上面的文本文件，以文件内容为准；读不到时如实说明。")
	}
	if hasAttachment {
		if hasReadable {
			b.WriteString("\n")
		}
		b.WriteString("清单里其余的是非文本附件（图片 / 音视频 / PDF / 压缩包等），平台不提供正文读取：不要对它们调用 file_read，也不要假设其内容；用户问起时说明只能预览或下载。")
	}
	return b.String()
}

// ChatOutcome 是一轮对话的落库结果与运行统计，供 SSE 收尾事件使用。
type ChatOutcome struct {
	Message store.Message
	Session store.Session
	RunID   string
	Usage   llm.Usage
	Status  string
	// Reason 是中断原因（仅 cancelled 有值），供 run.finished 说明发生了什么
	Reason string
}

// Chat 执行一次完整对话：保存用户消息 → 建 run → 调用 Agent（含工具循环）→ 保存助手消息。
//
// 每个执行步骤都写 run_step 做 checkpoint（§5.7.5）；流式增量通过 emit 推给调用方，
// 即使客户端中途断开，已生成的内容与 checkpoint 也会落库。
func (s *Service) Chat(
	ctx context.Context,
	userID, sessionID, content string,
	clock ClientClock,
	emit func(agent.Event, int) error,
) (ChatOutcome, error) {
	return s.chat(ctx, userID, sessionID, content, chatOptions{source: "user", clock: clock}, emit)
}

// ChatWithSource 与 Chat 相同，但标记这条用户消息的来源（task_result / external）。
//
// 来源只影响「怎么渲染」与「进模型时如何包装」，不改变权限：run 的 actor 永远是
// 会话所有者，外部回信不会让别人的内容驱动所有者的工具权限（§15.10）。
func (s *Service) ChatWithSource(
	ctx context.Context,
	userID, sessionID, content, source string,
	emit func(agent.Event, int) error,
) (ChatOutcome, error) {
	return s.chat(ctx, userID, sessionID, content, chatOptions{source: source}, emit)
}

// chatOptions 是内部执行开关，不对外暴露：来源影响渲染与提示词包装，不影响权限。
type chatOptions struct {
	source string
	// clock 是用户端上报的时间与时区；零值表示没有上报，注入时回落服务器时间。
	// 只影响系统提示词里的「当前时间」，不参与权限与业务判定。
	clock ClientClock
	// skipUser 表示用户消息已经落库（回执是「先落消息、再起 run」），这一轮不要再写一次
	skipUser bool
}

func (s *Service) chat(
	ctx context.Context,
	userID, sessionID, content string,
	opt chatOptions,
	emit func(agent.Event, int) error,
) (ChatOutcome, error) {
	out := ChatOutcome{}

	sess, sp, err := s.ValidateChat(ctx, userID, sessionID, content)
	if err != nil {
		return out, err
	}
	text := strings.TrimSpace(content)

	// 用户资料一次查够：管理员判定（canWriteSecrets）与平台事实都要用；
	// 查失败只降级（少几句事实、不给写凭证），不阻断对话
	user, err := s.store.GetUser(ctx, userID)
	if err != nil {
		s.log.Warn("读取用户资料失败", "user_id", userID, "err", err)
	}

	// 先读历史，再写入本条用户消息，避免同一条消息重复进入上下文
	history, err := s.store.ListMessages(ctx, sessionID)
	if err != nil {
		return out, err
	}

	source := opt.source
	if source == "" {
		source = "user"
	}
	var userMsg store.Message
	if opt.skipUser {
		// 回执消息已经落库，这一轮只是把它交给模型
		userMsg = store.Message{SessionID: sessionID, Role: "user", Content: text, Source: source}
	} else {
		userMsg, err = s.store.CreateMessageFrom(ctx, sessionID, "user", text, source)
		if err != nil {
			return out, err
		}
	}
	if err := s.store.TouchSession(ctx, sessionID); err != nil {
		s.log.Warn("更新会话活跃时间失败", "session_id", sessionID, "err", err)
	}

	// 首条消息决定会话名：标题只取用户写的话，引用清单不进标题。
	// 系统触发的轮次（回执回填等）不能改名：那不是用户说的话。
	if sess.TitleAuto && !opt.skipUser {
		if title := summarize(referenceFree(text)); title != "" {
			if err := s.store.RenameSession(ctx, sessionID, title, false); err != nil {
				s.log.Warn("会话标题自动生成失败", "session_id", sessionID, "err", err)
			} else {
				sess.Title = title
				sess.TitleAuto = false
			}
		}
	}

	history = append(history, store.Message{Role: "user", Content: text})

	// 上下文三段式组装：摘要 + 按需检索 + 滑动窗口（§5.7.2 / §5.7.4）。
	// 摘要只读快照，落后时由 Build 丢给后台折叠——这一句不会被摘要模型挡住。
	contextMessages, err := s.memory.Build(ctx, ctxmgr.Input{
		SessionID: sessionID,
		History:   history,
		Query:     text,
	})
	if err != nil {
		return out, err
	}

	// 上一轮若停在审批上，这一句就是「翻篇」：待办与它的断点一起作废，
	// 免得几天前的半截执行被一张旧卡片续起来，还和新一轮抢同一个会话
	s.expireSessionApprovals(sessionID)

	// 一次 run 为根：预算、步骤与断点都落库，进程重启后能看出走到哪、能接着走（§5.7.5）
	run, err := s.createRun(ctx, sessionID)
	if err != nil {
		return out, err
	}
	out.RunID = run.ID

	return s.execute(ctx, sess, sp, user, userMsg, run, runSeed{
		History:     contextMessages,
		Query:       text,
		AgentPrompt: sess.AgentPrompt,
		ClientClock: opt.clock,
	}, out, emit)
}

// RunState 返回一次执行的状态。刷新页面后前端靠它判断「还在不在跑」，
// 再决定是续流、还是直接取最终消息。
func (s *Service) RunState(ctx context.Context, userID, runID string) (store.Run, error) {
	return s.loadRun(ctx, userID, runID)
}

// ActiveRun 返回会话当前活跃的 run：刷新后的第一件事就是问它。
//
// 判活要过两道：库里的记录（心跳新鲜）与本进程的执行体。只看库会答错——进程被杀、
// 崩溃重启之后，那条 run 可能还挂着 running 且心跳在窗口内（迁移给历史行补默认心跳
// 就会造出这种假活），前端据此每次刷新都挂一条不会再来的流：先显示「思考中」，
// 收到 detached 收尾再消失。所以这里查不到执行体就当场收尾，再回答「没在跑」。
func (s *Service) ActiveRun(ctx context.Context, userID, sessionID string) (store.Run, bool, error) {
	if _, _, err := s.load(ctx, userID, sessionID); err != nil {
		return store.Run{}, false, err
	}
	// 心跳停了的 run 不算「在跑」：它属于上一个进程，回答「在跑」会让前端
	// 一直等一条不会再来的流，用户连「继续」都不会看到
	run, found, err := s.store.GetActiveRun(ctx, sessionID, s.idleTimeout)
	if err != nil {
		return store.Run{}, false, err
	}
	if !found {
		// 心跳过期不等于记录已终态：那条 run 还挂着 running / waiting_approval，
		// 占位消息也还 streaming。不顺手收尾的话，用户回来看到的是永远的「生成中」，
		// 正文与工具轨迹都不会被结算（staleAfter=0 表示不看心跳，只找非终态记录）。
		if stale, ok, err := s.store.GetActiveRun(ctx, sessionID, 0); err == nil && ok {
			s.abandonRun(ctx, stale)
		}
		return store.Run{}, false, nil
	}
	if s.runs.get(run.ID) != nil {
		return run, true, nil
	}
	// 本进程没有执行体，但这一轮可能在别的副本上跑：缓存里的快照还标着未结束、
	// 且刚更新过，就说明它活着。这里答「没在跑」的代价很大——前端连流都不会挂，
	// 正在写的内容就永久看不到（多副本部署与本地同时起了两个进程时，表现就是
	// 「切页回来会话没了」）。缓存里已经没有进展的才按无人执行收尾。
	if s.stream != nil {
		if st, ok, err := s.stream.LoadRun(ctx, run.ID); err == nil && ok && !st.Done &&
			time.Since(st.UpdatedAt) < s.idleTimeout {
			return run, true, nil
		}
	}
	s.abandonRun(ctx, run)
	return store.Run{}, false, nil
}

// abandonRun 把「没人跑」的 run 落成终态，并结算它留下的流式占位消息。
//
// 这是自愈动作，不能因为它失败就让 active-run 变成 500：写库抖动时记日志走人，
// 前端答案照旧是「没在跑」——挂不上流反正也拿不到内容。
func (s *Service) abandonRun(ctx context.Context, run store.Run) {
	// 请求可能已经被掐断（刷新、关页面），收尾必须落库，所以脱掉取消信号
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	// 心跳还在跳就不收：本进程拿不到流，不代表执行在别处没活。留两道心跳的余量，
	// 假活（进程已死）半分钟内会被真正收掉，界面当次就已经答「没在跑」了。
	grace := 2 * heartbeatInterval
	recovered := s.recoverContent(run.ID)
	if err := s.store.AbandonRun(ctx, run.ID, run.SessionID, RunAbandonedMessage, grace, recovered); err != nil {
		s.log.Warn("收尾无人执行的 run 失败", "run_id", run.ID, "session_id", run.SessionID, "err", err)
		return
	}
	// 落库成功才清缓存：失败时正文还要靠它，下一次清扫能再取一次
	if err := s.stream.DropRun(ctx, run.ID); err != nil {
		s.log.Warn("清理流缓存失败", "run_id", run.ID, "err", err)
	}
	s.log.Info("收尾无人执行的 run",
		"run_id", run.ID, "session_id", run.SessionID, "started_at", run.StartedAt,
		"recovered_chars", len(recovered.Content))
}

// recoverContent 从中断执行的缓存快照里取回正文。
//
// 取不到（没配缓存、TTL 已过、另一个实例还没写）就返回零值：
// 收尾仍要做，只是那条消息只剩工具轨迹。
func (s *Service) recoverContent(runID string) store.RunContent {
	if s.stream == nil || runID == "" {
		return store.RunContent{}
	}
	// 收尾路径的 ctx 可能已经被取消（用户关了页面），这里不跟着它一起死
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	st, ok, err := s.stream.LoadRun(ctx, runID)
	if err != nil {
		s.log.Warn("读取流缓存失败", "run_id", runID, "err", err)
		return store.RunContent{}
	}
	if !ok {
		return store.RunContent{}
	}
	return store.RunContent{Content: st.Content, Reasoning: st.Reasoning}
}

// CancelRun 是唯一能停掉一轮执行的入口。
//
// 客户端断开不再等于停止（§15.12.1），所以必须有一个显式动作，
// 否则用户没有任何办法中止一个跑偏的任务。
func (s *Service) CancelRun(ctx context.Context, userID, runID string) error {
	run, err := s.loadRun(ctx, userID, runID)
	if err != nil {
		return err
	}
	if run.Status != store.RunRunning && run.Status != store.RunWaitingApproval {
		return ErrRunFinished
	}
	live := s.runs.get(runID)
	if live == nil || !live.markCancelled(ReasonUserCancelled) {
		// 不在本进程（换了副本）或已经收尾：库里的状态才是权威
		return ErrRunFinished
	}
	s.log.Info("任务被用户取消", "run_id", runID, "by", userID)
	return nil
}

// CancelSessionRun 按会话停止当前这一轮，给「停止」按钮一个不用先知道 run_id 的入口。
//
// 前端只知道会话 id：思考阶段 run.started 还没到达，用 run_id 取消就得多一步「先问
// 一次 active-run 再取消」，中间那一步正是「点了停止没反应、要点两次」的来源。
// 这里由服务端定位这一轮，并且**在返回前就把 run 置为终态**——客户端拿到的 200
// 就是「已经停了」，不必再轮询确认，也不会在空窗期被断流自愈挂回来。
//
// 幂等：这一轮已经结束（或压根没在跑）时返回 nil，重复点击不是错误。
func (s *Service) CancelSessionRun(ctx context.Context, userID, sessionID string) error {
	run, found, err := s.ActiveRun(ctx, userID, sessionID)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	live := s.runs.get(run.ID)
	if live == nil || !live.markCancelled(ReasonUserCancelled) {
		// 不在本进程（换了副本）或刚好收尾：库里的状态由跑它的节点负责写，这里不越权
		return nil
	}
	// 立刻落终态：active-run 从此回答「没在跑」，前端刷新/续流的判断不会再有空窗
	run.Status = store.RunCancelled
	run.Error = ReasonUserCancelled
	if err := s.store.FinishRun(context.WithoutCancel(ctx), run); err != nil {
		// 写失败不影响取消本身：agent 收尾时还会再写一次
		s.log.Warn("取消时写入 run 状态失败", "run_id", run.ID, "err", err)
	}
	s.log.Info("任务被用户取消", "run_id", run.ID, "session_id", sessionID, "by", userID)
	return nil
}

// SubscribeRun 订阅一次执行的事件流，afterOffset 是客户端已收到的最大偏移。
// Live 为 false 表示 run 已不在本进程，调用方应改走「拉 run_step + 最终消息」。
func (s *Service) SubscribeRun(ctx context.Context, userID, runID string, afterOffset int) (*runFeed, error) {
	run, err := s.loadRun(ctx, userID, runID)
	if err != nil {
		return nil, err
	}
	if live := s.runs.get(runID); live != nil {
		return live.subscribe(afterOffset), nil
	}
	// 本进程没有执行体：要么这一轮在别的副本上跑，要么进程刚重启。
	// 缓存里还有帧就从缓存回放并继续跟随；实在没有才让调用方去库里取最终消息。
	if s.stream != nil {
		if feed := s.subscribeFromCache(ctx, run.ID, afterOffset); feed != nil {
			return feed, nil
		}
	}
	return &runFeed{Done: closedCh, Live: false}, nil
}

// cachePollInterval 是「跟随远端副本」的轮询间隔。
//
// 用轮询而不是 Redis 订阅：一份实现同时覆盖「回放」与「追新」，
// 且断线重连不需要额外状态；500ms 对一个人看回答的体感足够。
const cachePollInterval = 500 * time.Millisecond

// subscribeFromCache 从缓存回放 afterOffset 之后的帧，并继续读后续增量。
//
// 返回 nil 表示缓存里没有这一轮（已结束 / TTL 到期 / 没配 Redis）。
func (s *Service) subscribeFromCache(ctx context.Context, runID string, afterOffset int) *runFeed {
	frames, err := s.stream.ReplayFrames(ctx, runID, afterOffset)
	if err != nil {
		return nil
	}
	backlog := decodeFrames(frames)
	// 帧列表在缓存里按条数上限只保留最新的那批，一段很长的思考会产生上万帧，
	// 开头就被裁掉了。客户端从 after_offset=0 重连时，「只看到后半段」甚至
	// 「什么都没有」都是这个原因。正文与思考的完整文本其实还在快照里，
	// 按 offset 切出缺口补在最前面即可。
	if st, ok, err := s.stream.LoadRun(ctx, runID); err == nil && ok {
		backlog = append(recoverPrefix(st, afterOffset, frameGap(backlog, st.Offset)), backlog...)
	}
	if len(backlog) == 0 {
		return nil
	}
	last := afterOffset
	for _, f := range backlog {
		if f.Offset > last {
			last = f.Offset
		}
	}

	// 只在这里关闭 done：handler 收到它就会结束这次订阅
	done := make(chan struct{})
	framesCh := make(chan RunFrame, 64)
	pollCtx, cancel := context.WithCancel(ctx)

	go func() {
		defer close(done)
		ticker := time.NewTicker(cachePollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-pollCtx.Done():
				return
			case <-ticker.C:
			}
			more, err := s.stream.ReplayFrames(pollCtx, runID, last)
			if err != nil {
				// 缓存暂时读不到：这一轮可能还在跑，继续等而不是把流断掉
				continue
			}
			for _, raw := range more {
				if raw.Offset <= last {
					continue
				}
				f, ok := decodeFrame(raw)
				if !ok {
					continue
				}
				last = f.Offset
				select {
				case framesCh <- f:
				case <-pollCtx.Done():
					return
				}
			}
			// 生产者写完 done，或键已过期：交给调用方去库里拿最终消息
			st, ok, err := s.stream.LoadRun(pollCtx, runID)
			if err == nil && (!ok || st.Done) {
				return
			}
		}
	}()

	return &runFeed{Backlog: backlog, Frames: framesCh, Done: done, Live: true, stop: cancel}
}

// frameGap 返回「第一批可用帧之前已经产出多少字符」。
//
// offset 是思考与正文的累计字符数，落在「这条事件处理完之后」，所以首帧的起点
// 要减掉它自己的长度；非增量帧（工具、审批）不改变 offset，直接拿它当边界。
// 没有帧时以快照的 offset 为界：整轮内容都靠快照补。
func frameGap(backlog []RunFrame, snapshotOffset int) int {
	if len(backlog) == 0 {
		return snapshotOffset
	}
	first := backlog[0]
	gap := first.Offset
	if first.Event.Type != agent.EventMessageDelta {
		return gap
	}
	if n := utf8.RuneCountInString(first.Event.Delta); first.Offset >= n {
		gap = first.Offset - n
	}
	return gap
}

// recoverPrefix 用缓存快照补回 (after, gap) 这段缺口。
//
// 缺口只可能落在「思考或正文」里：offset 不累计工具事件，所以前 gap 个字符
// 就是 reasoning 的前缀加上 content 的前缀。思考总在前、正文在后，切分位置
// 因此是确定的，不需要额外的分段记录。
//
// 快照是秒级写入的，可能比帧旧：缺口上界取快照已有字符数，宁可少补，
// 不要凭长度切出空的或越界的内容。
func recoverPrefix(st cache.RunState, after, gap int) []RunFrame {
	if gap <= after || gap <= 0 {
		return nil
	}
	reasoning := []rune(st.Reasoning)
	content := []rune(st.Content)
	upto := gap
	if total := len(reasoning) + len(content); upto > total {
		upto = total
	}
	if upto <= after {
		return nil
	}
	var out []RunFrame
	if after < len(reasoning) {
		end := upto
		if end > len(reasoning) {
			end = len(reasoning)
		}
		out = append(out, RunFrame{
			Offset: end,
			Event: agent.Event{
				Type: agent.EventMessageDelta, Role: "assistant",
				Delta: string(reasoning[after:end]), Reasoning: true,
			},
		})
	}
	if upto > len(reasoning) {
		start := after - len(reasoning)
		if start < 0 {
			start = 0
		}
		out = append(out, RunFrame{
			Offset: upto,
			Event: agent.Event{
				Type: agent.EventMessageDelta, Role: "assistant",
				Delta: string(content[start : upto-len(reasoning)]),
			},
		})
	}
	return out
}

// decodeFrames 把缓存里的帧解成事件；坏帧直接跳过，不打断整条流。
func decodeFrames(raw []cache.Frame) []RunFrame {
	out := make([]RunFrame, 0, len(raw))
	for _, f := range raw {
		if frame, ok := decodeFrame(f); ok {
			out = append(out, frame)
		}
	}
	return out
}

func decodeFrame(raw cache.Frame) (RunFrame, bool) {
	var ev agent.Event
	if err := json.Unmarshal(raw.Payload, &ev); err != nil {
		return RunFrame{}, false
	}
	return RunFrame{Offset: raw.Offset, Event: ev}, true
}

// loadRun 读 run 并校验归属：run 的可见性跟随会话，只对会话创建者开放。
func (s *Service) loadRun(ctx context.Context, userID, runID string) (store.Run, error) {
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return store.Run{}, err
	}
	sess, err := s.store.GetSession(ctx, run.SessionID)
	if err != nil {
		return store.Run{}, err
	}
	if sess.CreatedBy != userID {
		return store.Run{}, ErrForbidden
	}
	// 会话还在，但人可能已经被移出空间：两道校验都要过
	if _, err := s.spaces.Get(ctx, userID, sess.SpaceID); err != nil {
		if errors.Is(err, space.ErrForbidden) {
			return store.Run{}, ErrForbidden
		}
		return store.Run{}, err
	}
	return run, nil
}

// Messages 返回会话消息。
func (s *Service) Messages(ctx context.Context, userID, sessionID string) ([]store.Message, error) {
	if _, _, err := s.load(ctx, userID, sessionID); err != nil {
		return nil, err
	}
	return s.store.ListMessages(ctx, sessionID)
}

// AppendMessage 追加一条用户消息；若标题仍是自动状态，用首条消息生成标题。
func (s *Service) AppendMessage(ctx context.Context, userID, sessionID, content string) (store.Message, store.Session, error) {
	sess, sp, err := s.load(ctx, userID, sessionID)
	if err != nil {
		return store.Message{}, store.Session{}, err
	}
	if !canWrite(sp.Perm) {
		return store.Message{}, store.Session{}, ErrForbidden
	}

	text, err := normalizeMessage(content)
	if err != nil {
		return store.Message{}, store.Session{}, err
	}

	msg, err := s.store.CreateMessage(ctx, sessionID, "user", text)
	if err != nil {
		return store.Message{}, store.Session{}, err
	}
	if err := s.store.TouchSession(ctx, sessionID); err != nil {
		s.log.Warn("更新会话活跃时间失败", "session_id", sessionID, "err", err)
	}

	now := time.Now()
	sess.LastMessageAt = &now

	// 首条消息决定会话名：命名完成后 title_auto 置为 false，后续消息不再覆盖
	if sess.TitleAuto {
		if title := summarize(text); title != "" {
			if err := s.store.RenameSession(ctx, sessionID, title, false); err != nil {
				s.log.Warn("会话标题自动生成失败", "session_id", sessionID, "err", err)
			} else {
				sess.Title = title
				sess.TitleAuto = false
			}
		}
	}

	// 回读一次，保证返回的 updated_at / last_message_at 与实际一致
	if latest, err := s.store.GetSession(ctx, sessionID); err == nil {
		sess = latest
	}
	return msg, sess, nil
}

// load 读取会话并校验当前用户对所属空间的可见性。
func (s *Service) load(ctx context.Context, userID, sessionID string) (store.Session, store.Space, error) {
	sess, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return store.Session{}, store.Space{}, err
	}

	// 会话只对创建者可见（§15.12.3）：同空间的其他成员也看不到。
	// 跨用户的内容流动只允许走「显式派发」——代建的子会话归属被咨询人，
	// 所以两边各自看到的都是自己的会话，不需要会话级 ACL。
	if sess.CreatedBy != "" && sess.CreatedBy != userID {
		return store.Session{}, store.Space{}, ErrForbidden
	}

	sp, err := s.spaces.Get(ctx, userID, sess.SpaceID)
	if err != nil {
		if errors.Is(err, space.ErrForbidden) {
			return store.Session{}, store.Space{}, ErrForbidden
		}
		return store.Session{}, store.Space{}, err
	}
	return sess, sp, nil
}

func normalizeMessage(raw string) (string, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return "", ErrMessageEmpty
	}
	if utf8.RuneCountInString(text) > maxMessageRunes {
		return "", ErrMessageTooLong
	}
	return text, nil
}

func normalizeTitle(raw string) (string, error) {
	title := strings.TrimSpace(raw)
	if title == "" {
		return defaultTitle, nil
	}
	if utf8.RuneCountInString(title) > maxTitleRunes || strings.ContainsAny(title, "\r\n\t") {
		return "", ErrTitleInvalid
	}
	return title, nil
}

// summarize 把首条消息压成标题：合并空白，截断到固定字数。
func summarize(text string) string {
	flat := strings.Join(strings.Fields(text), " ")
	runes := []rune(flat)
	if len(runes) <= autoTitleRunes {
		return flat
	}
	return string(runes[:autoTitleRunes]) + "…"
}

func canWrite(perm string) bool {
	return perm == "write" || perm == "manage"
}
