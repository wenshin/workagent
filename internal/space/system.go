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

// seedSystemSpace 幂等写入系统空间的平台配置。
//
// 插件化之后系统空间只剩两类内容：平台配置文件（config.json / net_whitelist.json）
// 与插件目录。内置技能、工具、智能体全部由插件管理器发布后**安装到系统空间**：
// 系统空间的 `plugins/` 就是全局安装目录，其他空间在加载时把这份安装一并算作可用。
func (s *Service) seedSystemSpace(ctx context.Context, spaceID string) error {
	if err := s.storage.EnsureDir(ctx, s.filesKey(spaceID)); err != nil {
		return err
	}
	if err := s.seedConfigFiles(ctx, spaceID); err != nil {
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
	// 模型可以用 resource_read 读 published/skills/<slug>/<version>/<path>。
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
			"- 空间类型：`personal`（个人空间，登录时自动创建）、`shared`、`system`（平台内置，仅管理员可见）。",
			"- 成员权限：`view` < `read` < `write` < `manage`；目录不单独授权，随所属空间。",
			"- 只有 `write` / `manage` 才能写文件或建技能；权限不足时如实告知，不要换路径重试。",
			"- 系统空间是平台默认能力源，任何用户都可以用 `resource_search` 检索、`resource_read` 只读访问。",
			"",
			"## 工具",
			"- `resource_list`：列出当前用户可见的空间（名称、类型、权限、路径）。",
			"",
			"## 回答要点",
			"- 用户问「我有哪些空间」时调用 `resource_list`，不要依赖历史记忆。",
			"- 创建、改名、删除空间尚未开放给 Agent，需要用户在界面操作，直接说明即可。",
		),
	},
	{
		Slug:        "builtin_files",
		DirName:     "文件与目录操作",
		Name:        "文件与目录操作",
		Description: "在当前空间读写文件、建目录与创建技能的操作手册",
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
			"- `skill_create(name, description, content)`：建 `plugins/<slug>/` 草稿，写 `plugin.json` 与 SKILL.md。",
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
			"## 工具",
			"- `session_list`：列出当前空间的会话，最近活跃优先。",
			"",
			"## 回答要点",
			"- 用户问「最近的会话」时调用 `session_list`。",
			"- 新建、删除会话需要用户在界面操作，直接说明即可。",
		),
	},
	{
		Slug:        "builtin_ui",
		DirName:     "前端组件",
		Name:        "前端组件",
		Description: "用声明式 DSL 生成会话内可渲染的组件（表格、指标、表单），并完成试运行与发布",
		Doc: joinDoc(
			"# 前端组件（wa-ui/1 DSL）",
			"",
			"## 组件是什么",
			"- 组件是 `kind=component` 的插件：`plugins/<slug>/plugin.json` + `plugins/<slug>/ui.json`。",
			"- `ui.json` 是**声明式 DSL**，不是代码：平台前端按节点原生渲染，组件拿不到 window / cookie / 网络，也注入不了 CSS。",
			"- 组件不能自己写接口地址或工具名，只能用 `plugin.json` 的 `data_sources` 里声明的数据源；取数由宿主带着当前用户身份走权限、审批与审计。",
			"- 只在会话里通过 ```ui-block 渲染；用户没安装组件时不会渲染。",
			"",
			"## 文件结构",
			"```",
			"plugins/sales_board/",
			"  plugin.json   # 元数据唯一入口",
			"  ui.json       # 渲染 DSL，ui.entry 默认就是它",
			"```",
			"",
			"## plugin.json",
			"```json",
			"{",
			"  \"protocol\": \"wa-plugin/1\",",
			"  \"kind\": \"component\",",
			"  \"slug\": \"sales_board\",",
			"  \"name\": \"销售看板\",",
			"  \"description\": \"按周期展示销售明细，需要看本月/本季销售时使用\",",
			"  \"data_sources\": [",
			"    {\"id\": \"orders\", \"plugin\": \"mysql\", \"tool\": \"sql_query\", \"mode\": \"read\", \"params\": {\"limit\": 20}},",
			"    {\"id\": \"submit\", \"plugin\": \"crm\", \"tool\": \"lead_create\", \"mode\": \"action\"}",
			"  ]",
			"}",
			"```",
			"- `slug`：小写字母开头、只含 `[a-z0-9_]`，2-32 位；同空间唯一。",
			"- `data_sources[].id`：DSL 里引用的名字；`plugin` 必须是**当前空间已安装**的插件 slug。",
			"- `mode`：`read` 会在渲染时自动取数；`action` 只能由按钮触发，且建议配 `confirm`。",
			"- 不要写 `version`（发布时生成）、不要写 `risk`（由能力推导）、不要写 `trust`/`events`。",
			"",
			"## ui.json 骨架",
			"```json",
			"{",
			"  \"dsl\": \"wa-ui/1\",",
			"  \"height\": 360,",
			"  \"state\": {\"period\": \"month\"},",
			"  \"data\": {\"orders\": {\"params\": {\"period\": \"{{state.period}}\"}}},",
			"  \"view\": {\"type\": \"Stack\", \"gap\": \"md\", \"children\": []}",
			"}",
			"```",
			"- `height`：120-2000，组件最小高度。",
			"- `state`：初始状态，键必须是 `a-z` 开头的 `[a-z0-9_]`。",
			"- `data`：键必须是 `data_sources` 里的只读数据源 id；`auto: false` 可关掉首屏取数；`params` 支持 `{{state.x}}`。",
			"",
			"## 可用节点",
			"| type | 必需字段 | 说明 |",
			"|---|---|---|",
			"| `Stack` | `children` | 纵向布局，`gap`: sm/md/lg |",
			"| `Row` | `children` | 横向布局，`align`: start/center/end/between |",
			"| `Grid` | `columns`(1-4)、`children` | 网格 |",
			"| `Card` | `children` | 卡片，`title` 可选 |",
			"| `Heading` | `text` | 标题，`level`: 1-4（默认 3） |",
			"| `Text` | `text` | 正文，`size`: sm/md/lg，`tone`: default/muted/success/warning/danger/info |",
			"| `Badge` | `text` | 徽标，`tone` 同上 |",
			"| `Alert` | `text` | 提示块，`title` 可选，`tone` 默认 info |",
			"| `Stat` | `label`、`value` | 指标卡，`delta` 可选 |",
			"| `KeyValue` | `items` | `[{\"label\":\"客户\",\"value\":\"{{data.orders.name}}\"}]` |",
			"| `List` | `source` | `source` 绑定数组，元素用 title/description/badge/tone 字段 |",
			"| `Table` | `columns`、`source` | `columns`: `[{\"key\":\"name\",\"label\":\"客户\",\"align\":\"left\"}]`；`source` 绑定对象数组 |",
			"| `Progress` | `value` | 进度条，`max` 默认 100，`label` 可选 |",
			"| `Button` | `text`、`action` | 触发动作 |",
			"| `Input` | `name` | 文本输入，写入 `state.<name>` |",
			"| `Select` | `name`、`options` | 下拉，`options`: `[{\"label\":\"本月\",\"value\":\"month\"}]` |",
			"| `Tabs` | `name`、`options` | 页签，选中值写入 `state.<name>` |",
			"| `Divider` | — | 分割线 |",
			"",
			"所有节点可选 `when`：`{\"path\":\"state.period\",\"op\":\"eq\",\"value\":\"month\"}`，`op` ∈ eq/ne/gt/lt/in/exists/truthy。",
			"字段写错会直接校验失败（不会静默忽略），节点最多 300 个、嵌套最多 8 层。",
			"",
			"## 绑定",
			"- 只支持 `{{state.x}}`、`{{props.x}}`、`{{data.<数据源id>.x}}` 三种路径，不能写表达式或函数。",
			"- 整个字符串就是一个绑定时保留原始类型：`\"source\": \"{{data.orders.items}}\"` 得到数组。",
			"- 文本里可以拼接：`\"共 {{data.orders.total}} 条\"`。",
			"- 字符串里出现无法配对的 `}}` 会被拒绝；纯文本不需要转义。",
			"",
			"## 动作",
			"| 写法 | 用途 |",
			"|---|---|",
			"| `{\"type\":\"reload\",\"target\":\"orders\",\"params\":{…}}` | 重新取只读数据 |",
			"| `{\"type\":\"invoke\",\"source\":\"submit\",\"params\":{…},\"confirm\":\"确认提交？\"}` | 调写数据源；必须用户点击，成功后自动刷新只读数据 |",
			"| `{\"type\":\"message\",\"text\":\"帮我把 {{state.period}} 的销售明细导出\"}` | 把一句话填进输入框，由用户决定是否发送 |",
			"",
			"## 生成流程",
			"1. 先问清：展示什么、数据来自哪个**已安装**插件的哪个工具、有没有写操作。",
			"2. `file_write(\"plugins/<slug>/plugin.json\", …)`，再 `file_write(\"plugins/<slug>/ui.json\", …)`。",
			"3. 让用户点插件的「试运行」：DSL 节点/字段/绑定/数据源写错会直接报错并指出位置，按提示改文件即可。",
			"4. 告诉用户在空间插件面板发布并安装；会话里用 `ui-block {\"slug\":\"<slug>\",\"props\":{…}}` 围栏引用（版本可省略，默认用已安装版本；不要自己编版本号）。",
			"",
			"## 边界",
			"- 组件不能发网络请求、不能读本地文件、不能执行脚本：需要新数据就新增数据源并让用户安装对应插件。",
			"- 不要伪造版本号；版本由发布生成，历史消息固定渲染当时版本。",
		),
	},
	pluginDevSkill(),
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
			"- `capability` 只能取平台枚举：`skill.use` / `search.query` / `http.request` / `fs.read` / `fs.write` / `sql.query` / `shell.exec` / `ui.render` / `plugin.invoke` / `agent.use`；省略时按类型给默认值。",
			"- `limits.timeout_ms` ≤ 25000，`max_rpc` ≤ 50，`max_result_bytes` ≤ 24KB。",
			"- `secret.use` + 外部写 = L4，发布与安装都需要系统管理员。",
			"",
			"## 需要凭证时（不要向用户索要明文）",
			"- 清单、正文、脚本里**只写凭证名**（如 `\"secret\": \"bocha\"`），值由平台加密保管，不写进任何文件。",
			"- 需要凭证时**先**用 `secret_request` 查状态：你只传 `scope` / `name` / `label` / `hint`，",
			"  用户在会话界面上自己填。明文不进对话、不进你的上下文，你只会拿到「是否已配置 + 指纹」。",
			"- 回执 `state=configured` 表示凭证已存在：直接往下做，**不要提示用户录入**，也不要把上游 401/403",
			"  当成缺凭证（先检查 endpoint、请求参数，或确认这把 key 是否真能访问那个上游）；`state=missing` 时卡片会自动",
			"  出现在消息里，等用户填完再继续；`state=disabled` 说明部署没配 `SECRET_KEK`，如实说明即可。",
			"- **绝不主动索要** API Key / Token / 密码。用户主动贴了明文时：先提醒这条已经泄露、建议作废重签，",
			"  然后用 `secret_request` 走正规通道；不要复述、不要写进文件或清单、不要再传下去。",
			"- `scope` 约定：搜索源固定 `search`，MCP 鉴权用 `mcp`，其他按插件能力自取；凭证名与清单里的引用保持一致。",
			"- 用户不是系统管理员时，`secret_request` 只会提示「转给管理员」：把 scope 与凭证名告诉用户即可，不要绕道要明文。",
			"",
			"## 流程",
			"1. 先问清三件事：输入是什么、输出给谁看、有没有写操作或外部副作用；缺关键信息一次问清。",
			"2. 用 `file_write` 在 `plugins/<slug>/` 下写 `plugin.json` 与内容文件。",
			"3. 用 `plugin_try` 自己试跑：清单、DSL、脚本、搜索源都会在这一步校验并给出具体报错；",
			"   搜索源必须传真实检索词，返回里的 `top_keys` 决定 `items` 路径怎么写。",
			"4. 用户启用后进入插件市场「组织内待发布」，系统管理员发布，空间管理员再安装。",
			"5. 安装后：技能进检索池（会话可绑定）、工具进模型工具列表、MCP 同步远端工具、组件可用 `ui-block` 引用、智能体出现在新建会话的选择器里。",
			"",
			"## 改已有插件",
			"- 先 `resource_search` / `file_read` 看现有 `plugin.json` 与内容，不要在没看过清单的情况下重写。",
			"- 已安装的插件目录是只读副本：改行为要在空间里新建同 slug 的草稿（先卸载或改名）并重新发布。",
			"",
			"## 子手册",
			"- `references/skill.md`：技能正文写法、触发条件、与检索的关系",
			"- `references/tool.md`：Deno 运行时、宿主 ABI、能力声明、测试要点",
			"- `references/mcp.md`：MCP 连接、工具白名单、按空间覆盖配置",
			"- `references/component.md`：组件 DSL 要点（完整节点表见 `builtin_ui`）",
			"- `references/agent.md`：智能体提示词、父智能体继承、依赖一并安装",
			"- `references/source.md`：搜索源的 spec / script 两种写法、试跑诊断、密钥与内网边界",
		),
		Files: map[string]string{
			"references/skill.md":     pluginDevSkillDoc,
			"references/tool.md":      pluginDevToolDoc,
			"references/mcp.md":       pluginDevMCPDoc,
			"references/component.md": pluginDevComponentDoc,
			"references/agent.md":     pluginDevAgentDoc,
			"references/source.md":    pluginDevSourceDoc,
		},
	}
}

// BuiltinSkills 返回平台预置技能，供插件装配时发布为内置插件（extensible=false）。
func BuiltinSkills() []BuiltinSkill { return builtinSkills }
