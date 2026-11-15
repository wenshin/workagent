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
	"time"
	"unicode/utf8"

	"github.com/workagent/auto-agent/internal/agent"
	"github.com/workagent/auto-agent/internal/agentspec"
	"github.com/workagent/auto-agent/internal/ctxmgr"
	"github.com/workagent/auto-agent/internal/llm"
	"github.com/workagent/auto-agent/internal/sandbox"
	"github.com/workagent/auto-agent/internal/search"
	"github.com/workagent/auto-agent/internal/secret"
	"github.com/workagent/auto-agent/internal/space"
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

	// defaultRunBudget 与配置项 LLM_MAX_WALL_CLOCK 的缺省值保持一致
	defaultRunBudget = 10 * time.Minute
	// runBudgetBackstopMargin 是 ctx 兜底比实现内部预算多留的余量：
	// 让实现先按自己的预算收尾，ctx 只兜住不自己计时的实现（如直答 direct）
	runBudgetBackstopMargin = 30 * time.Second

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
	// ReasonWallClockExceeded 本服务的墙钟上限到点，连接仍可用，会补发 run.finished
	ReasonWallClockExceeded = "wall_clock_exceeded"
	// ReasonBudgetExceeded 实现自己的预算（墙钟 / 工具次数 / 轮次）用尽
	ReasonBudgetExceeded = "budget_exceeded"
	// ReasonStalled 循环守卫判定「再做也不会产生新进展」而提前收尾。
	// 与 budget_exceeded 分开记：前者说明任务还有活没干完，后者说明方法本身没效。
	ReasonStalled = "stalled"
)

// RunBudgetExceededMessage 是墙钟上限到点时的用户可见文案。
const RunBudgetExceededMessage = "本次回答已达时长上限，已保留已生成的内容"

// cancelDetail 生成 run.error：预算是实现给出的具体说明（如「超过时间预算（10m0s）」），其余用原因常量。
func cancelDetail(reason string, runErr error) string {
	if reason == "" {
		return ""
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
	// runBudget 是单次回答的墙钟上限（ctx 兜底），由 LLM_MAX_WALL_CLOCK 加余量得到
	runBudget time.Duration
	// agentPlugins 是智能体来源（kind=agent 插件），为空时不注入追加提示词
	agentPlugins AgentPluginSource
	// search 是搜索 provider 注册表，为 nil 时 web_search 返回「能力未配置」
	search *search.Registry
	// shell 是命令执行沙箱，为 nil 时 shell_exec 返回「能力未配置」
	shell sandbox.Runner
	// pluginTry 是插件试跑（plugin_try），为 nil 时该工具不可用
	pluginTry tool.PluginTry
	// secrets 是凭证存储（secret_request 用它判断「配过没有」），为 nil 时该工具只报未启用
	secrets secret.Store
	// platform 是平台部署事实（租户 / IM 接入），每轮注入系统提示词
	platform agent.PlatformFacts
	// runs 是进程内正在执行的 run：既提供显式取消，也提供断流续传的缓冲。
	// 它不持久化——run 的权威状态在库里，这里只是「还活着」的那一份视图。
	runs *liveRuns
	// autoBudget 限制「回执触发的 run」的密度，避免十条回执催出十轮模型调用。
	autoBudget *autoRunBudget
	log        *slog.Logger
}

// NewService 构造会话服务；能力源（插件目录）由空间服务与插件层提供，
// wallClock 是单次回答的墙钟预算（LLM_MAX_WALL_CLOCK）。
func NewService(st store.Store, spaces *space.Service, agents agent.Agent, fs storage.Storage, memory *ctxmgr.Manager, searcher *search.Registry, shell sandbox.Runner, wallClock time.Duration, log *slog.Logger) *Service {
	if wallClock <= 0 {
		wallClock = defaultRunBudget
	}
	return &Service{
		store: st, spaces: spaces, agent: agents, storage: fs,
		memory: memory, search: searcher, shell: shell,
		runBudget: wallClock + runBudgetBackstopMargin, runs: newLiveRuns(),
		autoBudget: newAutoRunBudget(), log: log,
	}
}

// AgentPluginSource 提供插件化智能体（kind=agent）。
//
// 之所以定义接口而不是直接依赖插件包：会话层只需要「有哪些智能体」和「某个智能体的正文」，
// 不关心它来自发布件、空间安装还是系统空间的全局安装。
type AgentPluginSource interface {
	ListAgentPlugins(ctx context.Context, spaceID string) ([]agentspec.Summary, error)
	ResolveAgentPlugin(ctx context.Context, spaceID, slug string) (agentspec.Resolved, error)
}

// SetAgentPlugins 注入插件化智能体来源；不注入时行为与旧版完全一致。
func (s *Service) SetAgentPlugins(src AgentPluginSource) { s.agentPlugins = src }

// SetPluginTry 注入插件试跑能力；不注入时 plugin_try 返回「能力未配置」。
func (s *Service) SetPluginTry(src tool.PluginTry) { s.pluginTry = src }

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

	sess, err := s.store.CreateSession(ctx, spaceID, userID, title, dir, agent.ID, agent.Version, agent.Prompt)
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
		hidden := false
		for _, seg := range strings.Split(rel, "/") {
			if seg == "" || seg == "." || strings.HasPrefix(seg, ".") {
				hidden = true
				break
			}
		}
		if hidden || seen[rel] {
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
// 模型据此知道这些文件是用户明确要求参考的、需要先 file_read 再作答。
func ComposeContent(text string, refs []string) string {
	if len(refs) == 0 {
		return text
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
	b.WriteString("回答前先逐个 file_read 读取这些文件，以文件内容为准；读不到时如实说明。")
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
	emit func(agent.Event, int) error,
) (ChatOutcome, error) {
	return s.chat(ctx, userID, sessionID, content, chatOptions{source: "user"}, emit)
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

	// 上下文三段式组装：摘要 + 按需检索 + 滑动窗口（§5.7.2 / §5.7.4）
	contextMessages, err := s.memory.Build(ctx, ctxmgr.Input{
		SessionID: sessionID,
		History:   history,
		Query:     text,
	})
	if err != nil {
		return out, err
	}

	// 一次 run 为根，预算与步骤都落库，进程重启后可据此恢复（§5.7.5）
	budgetMap := s.agent.Budget()
	if budgetMap == nil {
		budgetMap = map[string]any{}
	}
	// 记录实际生效的实现，便于按实现定位问题（AGENT_IMPL 可切换）
	budgetMap["impl"] = s.agent.Name()
	budget, marshalErr := json.Marshal(budgetMap)
	if marshalErr != nil {
		budget = []byte(`{}`)
	}
	run, err := s.store.CreateRun(ctx, store.Run{SessionID: sessionID, Budget: budget})
	if err != nil {
		return out, err
	}
	out.RunID = run.ID

	// checkpoint 与事件推送共用一条链路，保证「推给前端的」与「落库的」一致
	seq := 0
	appendStep := func(typ string, payload any) {
		data, err := json.Marshal(payload)
		if err != nil {
			return
		}
		seq++
		stepCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		_, err = s.store.AppendRunStep(stepCtx, store.RunStep{
			RunID: run.ID, Seq: seq, Type: typ, Payload: data,
		})
		cancel()
		if err != nil {
			s.log.Warn("写入 run checkpoint 失败", "run_id", run.ID, "seq", seq, "err", err)
		}
	}

	// runCtx 剥离了请求的取消信号：客户端刷新、切页、断网都只解绑流，
	// 任务照旧跑完（§15.12.1）。只有显式取消或墙钟能停它。
	//
	// 前置准备（读历史、落用户消息、建 run）仍然跟着请求走——客户端已经
	// 断开时没必要再为一个没人收的结果起任务，那才是真的浪费。
	runCtx, cancelRun := context.WithTimeout(context.WithoutCancel(ctx), s.runBudget)
	defer cancelRun()
	live := s.runs.add(run.ID, userID, cancelRun)
	defer func() {
		live.finish()
		s.runs.remove(run.ID, live)
	}()

	// 事件先落内存缓冲再推客户端：客户端断了写入会失败，但缓冲还在，
	// 重连后按 offset 补齐（§15.12.2）。因此这里的错误一律吞掉——
	// 让「写不出去」去中断 Agent，正是刷新丢任务的老问题。
	// 首个正文增量到达时占位一条 streaming 消息：刷新后列表能看出「这一轮真的在跑」，
	// 而不是空白。正文仍然在结束时一次性覆盖，不逐 token 写库（§15.12.2）。
	var placeholderID string
	ensurePlaceholder := func() {
		if placeholderID != "" {
			return
		}
		placeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		m, err := s.store.CreateStreamingMessage(placeCtx, sessionID)
		cancel()
		if err != nil {
			s.log.Warn("占位消息写入失败", "session_id", sessionID, "err", err)
			return
		}
		placeholderID = m.ID
	}

	emitEvent := func(ev agent.Event) error {
		switch ev.Type {
		case agent.EventToolCall, agent.EventToolResult:
			appendStep(ev.Type, ev.Tool.Payload())
		case agent.EventApprovalReq:
			appendStep(ev.Type, ev.Approval)
		case agent.EventMessageDelta:
			// 思考不占位：只有真的开始写正文了，才值得在列表里留一条记录
			if !ev.Reasoning {
				ensurePlaceholder()
			}
		}
		offset := live.broadcast(ev)
		if emit != nil {
			_ = emit(ev, offset)
		}
		return nil
	}

	// run.started 由会话层补发：此时 run 已落库，前端可以据此做断线重连与取消
	emitEvent(agent.Event{Type: agent.EventRunStarted, RunID: run.ID})

	// 技能：不再常驻索引，只有会话绑定的技能正文直接注入
	var activeSkill *agent.ActiveSkill
	if sess.SkillPath != "" {
		skill, err := s.loadSkill(ctx, sess.SpaceID, sess.SkillPath)
		if err != nil {
			s.log.Warn("技能正文加载失败", "session_id", sessionID, "path", sess.SkillPath, "err", err)
		} else {
			activeSkill = skill
		}
	}

	// 智能体：优先用建会话时固化的提示词，管理员事后改智能体不影响已开会话。
	// 老会话（智能体还写在 agents/ 的年代）没有快照，这里按当前插件补一次。
	agentPrompt := sess.AgentPrompt
	if agentPrompt == "" && sess.AgentID != "" && s.agentPlugins != nil {
		if r, err := s.agentPlugins.ResolveAgentPlugin(ctx, sess.SpaceID, sess.AgentID); err == nil {
			agentPrompt = r.Prompt
		} else {
			s.log.Warn("智能体设定加载失败", "session_id", sessionID, "agent_id", sess.AgentID, "err", err)
		}
	}

	// 整轮墙钟：从进入实现到回答落库，含所有工具轮次与思考时间。
	// 用应用侧时钟而不是 run.started_at，避免与应用、数据库的时钟差导致耗时失真。
	startedAt := time.Now()

	res, runErr := s.agent.Run(runCtx, agent.RunInput{
		Env: tool.Env{
			UserID:    userID,
			SpaceID:   sess.SpaceID,
			SessionID: sessionID,
			RunID:     run.ID,
			SpacePerm: sp.Perm,
			// 企业 wiki 的宿主：固定系统空间，与当前会话所在空间无关。
			// 未初始化时为空，wiki 工具会报「不可用」而不是写进当前空间。
			WikiSpaceID: s.wikiSpaceID(ctx),
			Store:       s.store,
			Storage:     s.storage,
			Log:         s.log,
			// 插件目录：当前空间 + 系统空间 + 已安装插件（未安装的发布件按需检索，不做同名覆盖）。
			// published/... 路径也由空间服务转给插件层解析。
			Plugins: pluginLoader{
				spaces:  s.spaces,
				spaceID: sess.SpaceID,
			},
			// 搜索：provider 与密钥都在装配层，工具只拿到注册表
			Search: s.search,
			// 命令执行：沙箱后端在装配层选定（bwrap / sandbox-exec / local），
			// 工具只拿到 Runner，无法自行降级到无隔离执行。
			Shell: s.shell,
			// 插件试跑：写完草稿直接在会话里验，不走「发布-安装-发现错」的长链路
			PluginTry: s.pluginTry,
			// 任务派发：主 Agent 把需要他人确认的事丢出去，自己继续干活（§15.12.4）
			Tasks: s,
			// 凭证：只给「配过没有 + 指纹」，工具拿不到明文
			Secrets:         s.secrets,
			CanWriteSecrets: s.canWriteSecrets(user),
		},
		History: contextMessages,
		Query:   text,
		// 顶层会话允许派生子 Agent；子 Agent 的深度与预算由实现自己管
		AllowDispatch: true,
		ActiveSkill:   activeSkill,
		Dir:           sess.Dir,
		DirPlugin:     s.dirPluginContext(ctx, sess.SpaceID, sess.Dir),
		AgentPrompt:   agentPrompt,
		// 平台事实进系统提示词，不做成工具：租户、IM 这类信息必须恒真
		Platform: s.platformFacts(user, sp),
	}, emitEvent)

	// 落库不跟着请求走：run 已经跑完，客户端在不在都要把结果写进去
	saveCtx, cancelSave := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancelSave()

	// 中断原因：只有「用户显式取消」与「本服务的墙钟到点」两种。
	// 少了这一步，被掐断的 run 会被记成 succeeded，事后完全看不出是截断的。
	reason := live.cancelledReason()
	switch {
	case reason != "":
		// 已经在 live 上记过，不再覆盖
	case errors.Is(runErr, agent.ErrStalled):
		// 循环守卫判定停滞：已经强制收尾并给出结论，重试同样不会更好
		reason = ReasonStalled
	case errors.Is(runErr, agent.ErrBudgetExceeded):
		// 实现自己声明的预算用尽（builtin 的墙钟与工具次数上限）
		reason = ReasonBudgetExceeded
	case runCtx.Err() != nil:
		reason = ReasonWallClockExceeded
	}

	status := store.RunSucceeded
	switch {
	case reason != "":
		status = store.RunCancelled
	case runErr != nil:
		status = store.RunFailed
	}
	if reason != "" {
		s.log.Warn("对话被中断",
			"session_id", sessionID, "run_id", run.ID, "reason", reason,
			"chars", utf8.RuneCountInString(res.Content), "tools", len(res.Steps))
	}
	out.Reason = reason

	finish := func(errMsg string) {
		usage, err := json.Marshal(map[string]int{
			"in":  res.Usage.InputTokens,
			"out": res.Usage.OutputTokens,
		})
		if err != nil {
			usage = []byte(`{}`)
		}
		if err := s.store.FinishRun(saveCtx, store.Run{
			ID: run.ID, Status: status, Usage: usage,
			Provider: res.Provider, Model: res.Model, Error: errMsg,
		}); err != nil {
			s.log.Warn("更新 run 状态失败", "run_id", run.ID, "err", err)
		}
	}

	if res.Content == "" && len(res.Steps) == 0 {
		// 没有内容可存：取消不改判为失败，原因如实写进 run.error
		if reason == "" {
			status = store.RunFailed
		}
		// 状态要跟着带出去：思考阶段就被叫停时这里是唯一出口，不写的话 handler 会按
		// 「失败」广播 run.finished（reason 为空），前端弹一个本不该有的错误
		out.Status = status
		err := runErr
		if err == nil {
			err = errors.New("模型没有返回内容，请重试")
		}
		if reason != "" {
			finish(cancelDetail(reason, runErr))
		} else {
			finish(err.Error())
		}
		return out, err
	}

	// res.Steps 为空时 json.Marshal 会给出 null，而 parts 的契约是数组，这里显式落 []
	parts := []byte("[]")
	if len(res.Steps) > 0 {
		data, marshalErr := json.Marshal(res.Steps)
		if marshalErr != nil {
			s.log.Warn("序列化执行轨迹失败", "session_id", sessionID, "err", marshalErr)
		} else {
			parts = data
		}
	}

	// 这一轮的用量与耗时随消息落库：前端刷新后仍能看到 token 与总耗时
	stats := store.MessageStats{
		InTokens:   res.Usage.InputTokens,
		OutTokens:  res.Usage.OutputTokens,
		DurationMS: int(time.Since(startedAt).Milliseconds()),
	}
	var msg store.Message
	if placeholderID != "" {
		// 占位消息已经存在：整段覆盖，并把 streaming 置回 false
		msg, err = s.store.FinishMessage(saveCtx, placeholderID,
			res.Content, res.Reasoning, parts, stats, reason != "")
	} else {
		msg, err = s.store.CreateAssistantMessage(saveCtx, sessionID, res.Content, res.Reasoning, parts, stats)
	}
	if err != nil {
		status = store.RunFailed
		finish(err.Error())
		return out, err
	}
	appendStep("message", map[string]any{
		"message_id":      msg.ID,
		"chars":           utf8.RuneCountInString(res.Content),
		"reasoning_chars": utf8.RuneCountInString(res.Reasoning),
		"tools":           len(res.Steps),
		"need_approval":   res.NeedApproval,
		// 循环是怎么收尾的：done / budget / stalled。
		// 与 reason 分开记——「答案被逼出来但可用」和「运行被打断」是两件事。
		"stop_reason": res.StopReason,
	})
	// cancelled 也写原因，便于事后区分「用户停止」与「服务端到点」
	finish(cancelDetail(reason, runErr))

	if err := s.store.TouchSession(saveCtx, sessionID); err != nil {
		s.log.Warn("更新会话活跃时间失败", "session_id", sessionID, "err", err)
	}

	sess.LastMessageAt = &msg.CreatedAt
	if latest, err := s.store.GetSession(saveCtx, sessionID); err == nil {
		sess = latest
	}

	// 向量索引异步执行，失败不影响本次对话
	if s.memory.EmbeddingEnabled() {
		go func() {
			indexCtx, indexCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer indexCancel()
			batch := []store.Message{msg}
			if userMsg.ID != "" {
				batch = append([]store.Message{userMsg}, msg)
			}
			if err := s.memory.Index(indexCtx, sessionID, batch); err != nil {
				s.log.Warn("写入消息向量失败", "session_id", sessionID, "err", err)
			}
		}()
	}

	// 这个会话如果是一个派发任务的子会话，把结果回填给父会话（§15.12.4）
	s.completeParentTask(saveCtx, sessionID, res.Content)

	s.log.Info("对话完成",
		"session_id", sessionID, "space_id", sess.SpaceID, "by", userID, "run_id", run.ID,
		"status", status, "provider", res.Provider, "model", res.Model,
		"chars", utf8.RuneCountInString(res.Content), "tools", len(res.Steps))

	out.Message = msg
	out.Session = sess
	out.Usage = res.Usage
	out.Status = status
	return out, runErr
}

// RunState 返回一次执行的状态。刷新页面后前端靠它判断「还在不在跑」，
// 再决定是续流、还是直接取最终消息。
func (s *Service) RunState(ctx context.Context, userID, runID string) (store.Run, error) {
	return s.loadRun(ctx, userID, runID)
}

// ActiveRun 返回会话当前活跃的 run：刷新后的第一件事就是问它。
func (s *Service) ActiveRun(ctx context.Context, userID, sessionID string) (store.Run, bool, error) {
	if _, _, err := s.load(ctx, userID, sessionID); err != nil {
		return store.Run{}, false, err
	}
	return s.store.GetActiveRun(ctx, sessionID)
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
	if _, err := s.loadRun(ctx, userID, runID); err != nil {
		return nil, err
	}
	live := s.runs.get(runID)
	if live == nil {
		return &runFeed{Done: closedCh, Live: false}, nil
	}
	return live.subscribe(afterOffset), nil
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
