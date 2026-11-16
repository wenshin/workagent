package space

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/workagent/auto-agent/internal/platform"
	"github.com/workagent/auto-agent/internal/storage"
	"github.com/workagent/auto-agent/internal/store"
)

// EnsureSystemSpace 幂等地保证「系统空间」存在，并把当前管理员加入成员。
//
// 系统空间全局唯一，只有系统管理员（配置 ADMIN_JOB_NUMBERS 中的工号）可见可编辑；
// 其根下的 skills / tools / mcps 是 Agent 默认全局可见的能力搜索源，不可改名删除。
func (s *Service) EnsureSystemSpace(ctx context.Context, user store.User) error {
	if !s.IsAdmin(user) {
		return nil
	}

	sp, err := s.store.GetSpaceByType(ctx, SpaceTypeSystem)
	switch {
	case errors.Is(err, store.ErrNotFound):
		id := platform.NewUUID()
		sp, err = s.store.CreateSpace(ctx, store.SpaceInput{
			ID:               id,
			OwnerID:          user.ID,
			Name:             SystemSpaceName,
			Type:             SpaceTypeSystem,
			Path:             "/system/" + id,
			Depth:            0,
			IsSystemReserved: true,
		})
		if err != nil {
			return fmt.Errorf("创建系统空间失败: %w", err)
		}
		s.log.Info("系统空间已创建", "space_id", sp.ID, "owner", user.ID)
	case err != nil:
		return err
	}

	// 每个管理员都是 manage，但系统空间不提供共享入口
	if err := s.store.EnsureSpaceMember(ctx, sp.ID, user.ID, "manage"); err != nil {
		return err
	}
	return s.seedSystemSpace(ctx, sp.ID)
}

// BootstrapSystemSpace 在服务启动时按配置工号尝试初始化系统空间。
//
// 管理员可能还没有登录过（app_user 无记录），此时跳过，等其首次登录时再创建。
func (s *Service) BootstrapSystemSpace(ctx context.Context) {
	if len(s.admins) == 0 {
		s.log.Info("未配置系统管理员（ADMIN_JOB_NUMBERS），跳过系统空间初始化")
		return
	}

	ready := 0
	for job := range s.admins {
		user, err := s.store.FindUserByJobNumber(ctx, job)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				s.log.Warn("按工号查询管理员失败", "job_number", job, "err", err)
			}
			continue
		}
		if err := s.EnsureSystemSpace(ctx, user); err != nil {
			s.log.Warn("系统空间初始化失败", "job_number", job, "err", err)
			continue
		}
		ready++
	}
	if ready == 0 {
		s.log.Warn("系统管理员尚未登录过，系统空间将在首次登录时创建", "admins", len(s.admins))
	}
}

// seedSystemSpace 幂等准备系统空间的文件区。
//
// 插件化之后系统空间只剩插件目录：内置技能、工具、智能体全部由插件管理器发布后
// **安装到系统空间**，系统空间的 `plugins/` 就是全局安装目录，其他空间在加载时
// 把这份安装一并算作可用。平台不再往这里塞 config.json / net_whitelist.json：
// 搜索源改由 kind=source 的插件声明，白名单跟着清单里的 endpoint 走。
func (s *Service) seedSystemSpace(ctx context.Context, spaceID string) error {
	if err := s.storage.EnsureDir(ctx, s.filesKey(spaceID)); err != nil {
		return err
	}
	// 内置内容变了，索引要跟着重建（files/.meta.json）
	s.refreshIndex(ctx, spaceID)
	return nil
}

// putIfAbsent 仅在文件不存在时写入，避免覆盖管理员编辑过的内置内容。
func (s *Service) putIfAbsent(ctx context.Context, key, content string) error {
	if _, err := s.storage.Stat(ctx, key); err == nil {
		return nil
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	_, err := s.storage.Put(ctx, key, strings.NewReader(content), int64(len(content)))
	return err
}

// BuiltinSkill 是平台预置技能。slug 全局唯一，name/description 供展示。
type BuiltinSkill struct {
	Slug        string
	DirName     string
	Name        string
	Description string
	Doc         string
	// Files 是随包文件（子手册、模板），路径相对插件目录。
	// 模型可以用 plugin_read 读 published/skills/<slug>/<version>/<path>。
	Files map[string]string
}

// builtinSkills 是平台预置技能（发布为独立插件，不落系统空间文件树）。
//
// 内容与平台当前能力保持一致：Agent 目前只读，写操作需用户在界面上完成。
//
// 内容按「Agent 操作手册」写：讲清能用哪些工具、参数怎么校验、遇到限制怎么办，
// 而不是界面点击路径——这些文档会被注入或按需读取，直接指导模型的下一步动作。
var builtinSkills = []BuiltinSkill{
	{
		Slug:        "builtin_space",
		DirName:     "空间管理",
		Name:        "空间管理",
		Description: "空间与成员权限的基本规则，以及 Agent 能查询哪些空间信息",
		Doc: joinDoc(
			"# 空间管理",
			"",
			"## 用途",
			"空间是权限边界，只有一级；空间内的分类用目录表达。Agent 只能看到自己是成员的空间。",
			"",
			"## 规则",
			"- 空间类型：`personal`（个人空间，登录时或被派发任务时自动创建）、`shared`、`system`（平台内置，仅管理员可见）。",
			"- 成员权限：`view` < `read` < `write` < `manage`；目录不单独授权，随所属空间。",
			"- 只有 `write` / `manage` 才能写文件或建技能；权限不足时如实告知，不要换路径重试。",
			"- 派发任务不要求对方是当前空间的成员：任务建在对方的个人空间，平台不写入成员关系、也不扩大他在本空间的权限。",
			"- 系统空间是平台默认能力源，任何用户都可以用 `plugin_search` 检索、`plugin_read` 只读访问。",
			"",
			"## 工具",
			"- `space_list`：列出当前用户可见的空间（名称、类型、权限、路径）。",
			"",
			"## 回答要点",
			"- 用户问「我有哪些空间」时调用 `space_list`，不要依赖历史记忆。",
			"- 创建、改名、删除空间尚未开放给 Agent，需要用户在界面操作，直接说明即可。",
			"- 被问到「对方还没加入空间怎么派任务」时说明：不需要加入，任务会落在他自己的个人空间。",
		),
	},
	{
		Slug:        "builtin_files",
		DirName:     "文件与目录操作",
		Name:        "文件与目录操作",
		Description: "在当前空间读写文件与建目录的操作手册",
		Doc: joinDoc(
			"# 文件与目录操作",
			"",
			"## 用途",
			"空间内的文件区（`files/`）是 Agent 唯一可读写的位置，所有路径都相对它。",
			"",
			"## 规则",
			"- 目录最多 5 层，名称不能含 `/`、`\\` 与 `..`；隐藏文件（`.meta.json`）不暴露、不可读。",
			"- 可读写类型：md / json / txt / yaml；读上限 24KB，写上限 256KB。",
			"- 修改已有文件前先 `file_read`，避免覆盖用户内容。",
			"- 技能是插件目录 `plugins/<slug>/`：元数据只在 `plugin.json`，正文在 `SKILL.md`（不写 frontmatter）。",
			"",
			"## 工具",
			"- `file_list(path)`：列目录与文件，返回 name / path / dir / size。",
			"- `file_read(path, offset, limit)`：读正文，长文件分段读。",
			"- `file_write(path, content, mode)`：`overwrite` / `append` / `create`，父目录不存在时自动创建。",
			"- 建技能 / 插件没有专用工具：按 `builtin_plugin_dev` 的 `references/skill.md` 用 `file_write` 写 `plugins/<slug>/plugin.json` 与 `SKILL.md`。",
			"",
			"## 回答要点",
			"- 写入后告诉用户具体路径与字节数。",
			"- 不要尝试访问空间之外的路径（`../`），会被直接拒绝。",
		),
	},
	{
		Slug:        "builtin_sessions",
		DirName:     "会话管理",
		Name:        "会话管理",
		Description: "会话与目录的关系、会话级技能，以及 Agent 能查询的会话信息",
		Doc: joinDoc(
			"# 会话管理",
			"",
			"## 用途",
			"每个会话是一段与 Agent 的对话，归属某个空间，可挂在空间内的某个目录下。",
			"",
			"## 规则",
			"- 会话可挂在空间根或任意目录；目录改名时会话路径同步迁移，目录删除时会话移回空间根。",
			"- 每个空间至少保留一个会话。",
			"- 会话可以绑定一个技能（会话级技能），绑定后每轮对话都会按该技能的步骤执行。",
			"",
			"## 任务派发",
			"把一件事交给别人处理时用 `task_create`，不要试图在本会话里等对方回答。",
			"- 任务落在**对方的个人空间**，对方不需要是当前空间的成员；没用过平台的人会被自动开通个人空间。",
			"- 对方那边由平台默认的通用助理接手，不是你当前绑定的智能体：任务描述要写成对方能独立看懂的话，不要引用只有你知道的上下文。",
			"- 对方看不到你空间里的文件，需要引用的内容直接写进 `brief`，不要只给路径。",
			"- `title` 会成为对方会话列表里的会话名，写成「对象 + 事项」；少于 4 个字会被拒绝，空泛的名字会让人分不清。",
			"- 派发后当前轮正常结束；对方**确认结论后**回执才回到本会话，不需要轮询、不要反复问用户「回了吗」。",
			"- 同一件事再问一次用 `task_remind`（先用 `task_list` 取 id）：重新 `task_create` 会在对方空间里多出一个会话。",
			"- 已完成的任务不能催办，隔天再问属于新任务；同一个人 + 同一个标题 + 未完成的任务重复派发会被拒绝。",
			"- 等待期间状态是 `waiting_external`：对方只是在反问、还没确认结论时不会回传，这是正常的。",
			"",
			"## 工具",
			"- `session_list`：列出当前空间的会话，最近活跃优先。",
			"- `task_create`：派发任务（标题、完整说明、被派发人）。",
			"- `task_list`：列出本会话派出过的任务与当前状态。",
			"- `task_remind`：催办一个还没回的任务，不新建会话。",
			"- `task_cancel`：取消一个还没完成的任务。",
			"- `task_reply`：子会话侧的回执工具，只有被派发人确认结论后才由对方那边的助理调用；发起方不需要用。",
			"",
			"## 回答要点",
			"- 用户问「最近的会话」时调用 `session_list`。",
			"- 用户问「事情办得怎么样」时调用 `task_list`，按状态和已等待时长回答，不要凭印象。",
			"- 新建、删除会话需要用户在界面操作，直接说明即可。",
			"- 派发前先确认收件人是谁、要对方交付什么；缺这些信息时问清楚再派。",
		),
	},
	pluginDevSkill(),
	codingSkill(),
}

func joinDoc(lines ...string) string {
	return strings.Join(lines, "\n")
}

// pluginDevSkill 是「插件开发」总纲，子手册按类型拆分。
//
// 写这份文档的目的：模型自己就能把一个插件从零做出来，不需要人再讲一遍规则；
// 子手册按需要读，避免总纲膨胀成几百行的规范合集。
func pluginDevSkill() BuiltinSkill {
	return BuiltinSkill{
		Slug:        "builtin_plugin_dev",
		DirName:     "插件开发",
		Name:        "插件开发",
		Description: "从零开发插件：选类型、写清单、试运行、发布与安装；含技能/工具/MCP/组件/智能体子手册",
		Doc: joinDoc(
			"# 插件开发",
			"",
			"用户要新增或改造一个插件时，先读本页，再按类型读对应子手册。",
			"",
			"## 先选类型",
			"| 用户想要 | kind | 子手册 |",
			"|---|---|---|",
			"| 一套工作方法/规范/模板，模型照着做 | `skill` | `references/skill.md` |",
			"| 一段能执行并返回结果的逻辑 | `tool` | `references/tool.md` |",
			"| 接入外部 MCP Server 的工具 | `mcp` | `references/mcp.md` |",
			"| 在会话里渲染的界面 | `component` | `references/component.md` |",
			"| 带提示词与依赖的智能体 | `agent` | `references/agent.md` |",
			"| 新增/替换 `web_search` 背后的搜索源 | `source` | `references/source.md` |",
			"",
			"## 目录形状",
			"~~~",
			"plugins/<slug>/",
			"  plugin.json   清单（元数据唯一定义点）",
			"  SKILL.md      技能正文",
			"  index.ts      工具入口（kind=tool）",
			"  ui.json       组件 DSL（kind=component）",
			"  AGENT.md      智能体提示词（kind=agent）",
			"  search.json   声明式搜索源（kind=source, protocol=spec）",
			"  main.ts       脚本搜索源（kind=source, protocol=script）",
			"~~~",
			"除 `plugin.json` 之外的文件名由清单里的 `entry` 决定，但默认值就是上面这些。",
			"",
			"## 硬约束（写错会直接解析失败）",
			"- **不写 `version`**：发布时生成；草稿带版本会被拒绝。",
			"- **不写 `risk`**：风险由 `capabilities` 推导，作者不能自称。",
			"- 未知字段直接失败，不会被静默忽略——字段名写错要自己发现。",
			"- `slug`：小写字母开头、`[a-z0-9_]`、2-32 位，同空间唯一。",
			"- `name` ≤ 64 字，`description` ≤ 500 字（模型靠它判断何时使用，要写清「什么时候用」）。",
			"- `capability` 只能取平台枚举：`skill.use` / `search.query` / `http.request` / `fs.read` / `fs.write` / `sql.query` / `shell.exec` / `ui.render` / `plugin.invoke` / `agent.use`；省略时按类型给默认值。它只是分类标签，**真正的权限声明只有 `capabilities` 数组与 `fs` 段**。",
			"- `limits.timeout_ms` ≤ 25000，`max_rpc` ≤ 50，`max_result_bytes` ≤ 24KB——这三项都是**单次调用**的上限，不管进程寿命。",
			"- `runtime` 只接受 `entry`；写 `runtime.timeout_ms` 会被拒绝（上限只有一个入口：`limits`）。",
			"- 文件权限只有顶层 `fs` 一个入口：默认整个空间**只读**，写必须在 `fs.write` 里显式声明；`fs.read` / `fs.write` 的每一项都必须是空间工作区的**子目录**（`.`、绝对路径、`..` 都会被拒绝）。`capabilities[].paths` 已废弃，写了直接失败。",
			"- 清单声明的权限会出现在「安装确认」里给用户看：写宽了没人装，别把整个空间或整个域名段塞进去。",
			"- `secret.use` + 外部写 = L4，发布与安装都需要系统管理员。",
			"",
			"## 需要凭证时（不要向用户索要明文）",
			"- 清单、正文、脚本里**只写凭证名**，值由平台加密保管，不写进任何文件。写法只有两种：工具插件用 `capabilities: [{\"method\":\"secret.use\",\"name\":\"erp_token\"}]`（一个凭证一条），搜索源用 `source.secret: \"intranet\"`。",
			"- 需要凭证时**先**用 `secret_request` 查状态：你只传 `scope` / `name` / `label` / `hint`，",
			"  用户在会话界面上自己填。明文不进对话、不进你的上下文，你只会拿到「是否已配置 + 指纹」。",
			"- 回执 `state=configured` 表示凭证已存在：直接往下做，**不要提示用户录入**，也不要把上游 401/403",
			"  当成缺凭证（先检查 endpoint、请求参数，或确认这把 key 是否真能访问那个上游）；`state=missing` 时卡片会自动",
			"  出现在消息里，等用户填完再继续；`state=disabled` 说明部署没配 `SECRET_KEK`，如实说明即可。",
			"- **绝不主动索要** API Key / Token / 密码。用户主动贴了明文时：先提醒这条已经泄露、建议作废重签，",
			"  然后用 `secret_request` 走正规通道；不要复述、不要写进文件或清单、不要再传下去。",
			"- `scope` 约定：搜索源固定 `search`，MCP 鉴权用 `mcp`，其他按插件能力自取；凭证名与清单里的引用保持一致。",
			"- MCP 用 `auth.mode=jwt` 时密钥由**平台生成**，不是用户提供的凭证：先调 `mcp_app_key`，",
			"  用户在卡片里定应用名并拿到密钥，清单里只写 `auth.app`；应用名已被别人创建时让用户联系创建者，不要换名重试。",
			"- 用户不是系统管理员时，`secret_request` 只会提示「转给管理员」：把 scope 与凭证名告诉用户即可，不要绕道要明文。",
			"",
			"## 执行器形态",
			"- 只有 `kind=tool` 与 `kind=source`（`protocol=script`）有执行器；技能 / 组件 / MCP / 智能体都是纯声明，没有进程。",
			"- 工具插件默认**一次性**：每次调用起一个进程、跑完即退，模块级变量不跨调用保留。",
			"  需要长连接或缓存时在清单写 `runtime.resident: true`，才会复用进程并占用常驻池名额；",
			"  常驻时不要把用户数据放进模块级变量，那会跨调用泄漏。",
			"- `host` 与 `wa` 由平台挂到全局，不要 `import \"./host_sdk.ts\"`（那个文件在运行时目录，插件目录里没有）。",
			"- 出网用 `wa.fetch`（走宿主代理，域名受 `capabilities.hosts`、方法受 `verbs` 限制）；凭证用 `host.secret(\"名称\")` 取明文，名称必须在 `capabilities` 里逐条声明。",
			"- WebSocket 必须先声明 `ws.connect` 才能用原生 `new WebSocket`（用 wss://；宿主看得到域名，看不到帧）。",
			"- 文件直接 `Deno.readTextFile(wa.path(\"...\"))` 读写；`host.call(\"fs.*\")` 已从宿主 ABI 移除，调用会收到替代写法。",
			"- 插件升级 / 停用 / 卸载 / 空间重载会立刻重启进程，不用自己处理热更新。",
			"- `plugin_try` 试运行总走一次性进程，即使声明了 resident 也一样。",
			"",
			"## 流程",
			"1. 先问清三件事：输入是什么、输出给谁看、有没有写操作或外部副作用；缺关键信息一次问清。",
			"2. 用 `file_write` 在 `plugins/<slug>/` 下写 `plugin.json` 与内容文件。",
			"3. 用 `plugin_try` 自己试跑：清单、DSL、脚本、搜索源都会在这一步校验并给出具体报错；",
			"   搜索源必须传真实检索词，返回里的 `top_keys` 决定 `items` 路径怎么写。",
			"4. 写 `evals.json` 自测用例（`references/evals.md`）：发布时会执行，没过就发不出去；至少给一条有断言的用例。",
			"5. 用户启用后进入插件市场「组织内待发布」，系统管理员发布，空间管理员再安装。",
			"6. 安装后：技能进检索池（会话可绑定）、工具进模型工具列表、MCP 同步远端工具、组件可用 `ui-block` 引用、智能体出现在新建会话的选择器里。",
			"",
			"## 改已有插件",
			"- 先 `plugin_search` / `file_read` 看现有 `plugin.json` 与内容，不要在没看过清单的情况下重写。",
			"- 已安装的插件目录是只读副本：改行为要在空间里新建同 slug 的草稿（先卸载或改名）并重新发布。",
			"",
			"## 子手册",
			"- `references/skill.md`：技能正文写法、触发条件、与检索的关系",
			"- `references/tool.md`：沙箱、一次性与常驻、宿主 ABI、能力声明、测试要点",
			"- `references/mcp.md`：MCP 连接、工具白名单、按空间覆盖配置",
			"- `references/component.md`：组件 DSL 全表：节点、绑定、动作、生成流程与边界",
			"- `references/agent.md`：智能体提示词、父智能体继承、依赖一并安装",
			"- `references/source.md`：搜索源的 spec / script 两种写法、试跑诊断、密钥与内网边界",
			"- `references/evals.md`：发布前自测（`evals.json`）的两种用例与断言字段；发布时会执行，没过就发不出去",
		),
		Files: map[string]string{
			"references/skill.md":     pluginDevSkillDoc,
			"references/tool.md":      pluginDevToolDoc,
			"references/mcp.md":       pluginDevMCPDoc,
			"references/component.md": pluginDevComponentDoc,
			"references/agent.md":     pluginDevAgentDoc,
			"references/source.md":    pluginDevSourceDoc,
			"references/evals.md":     pluginDevEvalsDoc,
		},
	}
}

// BuiltinSkills 返回平台预置技能，供插件装配时发布为内置插件（extensible=false）。
func BuiltinSkills() []BuiltinSkill { return builtinSkills }
