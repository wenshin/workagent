# WorkAgent
面相企业级场景的 Agent 平台，提供企业级 Agent 的开发、部署、运维、监控等一站式服务。

# 文档与实现约定

项目的设计依据是以下文档。**实现必须遵循技术方案**，不要自创架构、分层或命名：

| 文档 | 内容 | 作用 |
|---|---|---|
| [docs/技术方案.md](docs/技术方案.md) | 分层架构、领域模型（五层模型 / 权限 / 资源）、模块设计、接口约定、安全与部署、演进路线 | **实现的事实标准**，代码与文档冲突时以文档为准 |
| [docs/前端设计规范.md](docs/前端设计规范.md) | 间距/圆角/字号标尺、颜色 token、组件模式（按钮/字段/弹窗/四态）、可访问性底线、反模式与评审清单 | **UI 实现的事实标准**，新增界面或改样式前必读 |
| [docs/MVP版本功能.md](docs/MVP版本功能.md) | MVP 范围、页面形态、功能优先级 | 范围裁剪与排期依据 |
| [docs/插件安全基线.md](docs/插件安全基线.md) | 发布扫描（CVE / 提示词 / 凭证，规则热更新）、运行时隔离（白名单挂载、断网与 socket 代理）、门禁口径 | **插件安全的事实标准**，改扫描或沙箱策略前必读 |

约定：

1. 开发新功能前，先读技术方案中对应章节，按其中的分层、领域模型与接口约定实现。
2. 需要偏离技术方案时，**先改文档并说明原因，再改代码**，不允许代码先行、文档长期滞后。
3. 阶段性简化（如首版只支持内置工具、单 Agent 不做编排）要在提交信息里注明与方案的差距及后续补齐计划。
4. 写前端样式或新组件前先读前端设计规范：间距、圆角、字号只从 `styles.css` 顶部的 `--wa-*` token 取，颜色只用 `--semi-color-*` 语义 token；规范没覆盖的新模式先补规范再落地。

# 架构
1. Agent Frontend Clients
    1. Web Client
    2. CLI Client
    3. Electron Client
    4. A2A Client
2. Agent Backend Services
    1. Login Service（SSO、OAuth2.0、OIDC）
    2. MCP Service
        1. Gateway Service
            1. inject jwt token
            2. service registry and jwt secret generation
            3. api proxy 到 web 端直接调用
        2. Management Service，MCP 注册、发现等功能
    3. Config Service
        1. 支持二次开发自定义实现
    4. Resource Service
    5. Tool Mangement
        1. 工具接口（Computer、CLI、Browser Use）
        2. 自定义工具
    6. UI Components Service
3. Agent Core SDK
    1. LLM Harness

# 权限管理
1. 实现组织架构管理
1. 实现 RBAC 权限管理，主要为资源管理使用
2. 一套发起审批流程的接口，默认实现为 AI 给对应用户发送审批信息，并回收回复完成审批

# 资源管理
1. 什么是资源？数据库、MCP服务、文件目录或者文件都可以作为一个资源
2. 每个资源可以配置对应的访问工具（工具参数）
3. 每个资源可以配置多个授权账号和角色

# 工具设计
1. 通用能力接口，是对通用能力抽象的接口，如 LLM wiki、Mysql、Redis、ES、Computer、FS、权限、资源 等
2. 工具是能力接口的具体实现，一个能力接口可以实现多个工具，一个工具可以配置多个资源，每个资源可以配置授权账号
3. 自定义工具，通过对话可以直接开发工具、安装工具、发布工具（其他同事可见，需要管理员审核）

# 关于 UI 客户端实现
1. 统一的 UI 设计规范
2. 通过对话可以直接开发 UI Components，可以直接让 AI 基于 MCP Gateway 实现一个组件，这个组件就是一段需求说明
3. 组件可以在 skills 中通过指令指定使用

# 本地运行

```bash
make pg-up   # 起 Postgres（docker）
make run     # 后端 :8321，启动时自动执行迁移
make web     # 前端 :5173
make mock-dc # mock 大数据平台 :8322（表搜索 / Spark SQL 异步任务，联调数仓类智能体）
```

# 系统空间（system）

平台内置一个全局唯一的「系统空间」，**只有系统管理员可见、可编辑**：

| 变量 | 说明 | 示例 |
|---|---|---|
| `ADMIN_JOB_NUMBERS` | 系统管理员工号，逗号分隔；MVP 用配置指定，后续接组织架构改角色表 | `16390,16391` |

- 服务启动时按工号查 `app_user` 初始化（管理员未登录过则跳过，首次登录时创建），重复执行幂等。
- 系统空间是 `space` 表中的一条 `type='system'` 记录（`is_system_reserved=true`），不可改名、不可删除，成员固定 `manage`。
- 插件化之后系统空间**只剩插件目录** `plugins/`（全局安装目录）。原 `config.json` / `net_whitelist.json` 已移除：搜索源改由 `kind=source` 插件声明，出网白名单跟着清单里的 `source.endpoint` 走，不再有第二处配置入口。
- 内置件以插件形式发布到 `published/{kind}/{plugin_id}/{version}/`，随后**安装到系统空间**：`系统空间/files/plugins/<slug>/` 就是全局安装目录，其他空间在 `ensureLoaded` 时把系统空间的安装一并加载，所以内置技能与内置工具默认对所有空间可用，市场「平台内置」分组展示但不提供安装按钮。
- 平台级文件**不参与跨空间回落**：`ReadSourceFile` 只允许回落到 `plugins/` 与 `published/`，否则普通空间可以用同名路径读到平台内容。平台自身读取时必须显式传系统空间 ID（`space.Service.SystemSpaceID`）。

# 插件体系（`internal/plugin`）

六类 `kind`（`skill` / `tool` / `mcp` / `component` / `agent` / `source`）共用一份清单、一条生命周期，差异只在执行器：

- **单一定义点**：元数据只在 `plugin.json`。`SKILL.md` 不写 frontmatter，DB 快照与索引字段都是派生值，不允许第二处可编辑定义。
- **文件为内容事实、DB 为运行时态**：草稿在 `files/plugins/<slug>/`，发布后写 `plugin` + `plugin_version`，安装写 `plugin_install`；两边不互相镜像。
- **风险推导**：`Derive(manifest)` 由 `capabilities` 推导 `risk` 与 `side_effects`，清单里写 `risk` 直接解析失败；`secret.use` + 外部写 = L4，发布与安装都需要系统管理员。
- **发布权 / 安装权分离**：空间管理员只能启用本地插件（写 `plugin_candidate`，市场出现「待发布」元数据）；**系统管理员**才能发布（不可变快照）。安装由**空间管理员**执行，只在本空间生效，落 `plugins/<slug>/` 只读副本（所有 kind 都复制，写 `.install.json`）。
- **本地插件默认停用**：`plugin_candidate.status=enabled` 才进入检索/工具/MCP/组件；安装副本默认启用，可停用、卸载。同 slug 的本地目录与安装记录互斥：安装检查目录，本地启用检查安装记录。
- **平台内置 = 系统空间的安装**：`origin=system` 的插件只允许安装到系统空间，那份 `plugin_install` + `plugins/<slug>/` 副本就是「全局安装」；其他空间加载时并入快照，天然进检索池与工具列表。普通空间对内置件 `install` / `uninstall` / `enable|disable` 一律拒绝；系统空间里也由平台统一种子化，不作为普通插件启停。
- **智能体是插件**：`kind=agent`，`agent.entry`（默认 `AGENT.md`）是提示词正文，`agent.extends` 指父智能体（最多 4 层、禁止成环，可指向旧版 `base`），`agent.requires` 是随包依赖，安装时一并装进同一空间（内置依赖跳过）。智能体没有执行器，不允许写 `capabilities`；它不进 `resource_search`，只在新建会话的选择器里出现。装在系统空间即全局可选。
- **运行以空间副本为准**：已安装插件从发布件起 runtime，但空间 `plugins/<slug>/` 是只读展示副本；本地插件从 `plugins/<slug>/` 起 runtime。`extensible` 不再意味着安装后可编辑。
- **组件是声明式 DSL，不是代码**：`ui.json`（`wa-ui/1`）只描述节点、绑定与动作，由前端 `UiView` 按白名单原生渲染——没有 JS/HTML/CSS 注入点，因此不用 iframe、也不用 Shadow DOM。节点/字段/绑定/动作在 dry-run、发布、读取时各校验一次（`internal/plugin/dsl.go`）。
- **组件不直连接口**：消息里的 ` ```ui-block {"slug","version","props"} ` 由 `UiBlock` 取 `GET /spaces/{id}/plugins/{slug}/ui` 渲染；数据请求 `POST /spaces/{id}/plugins/{slug}/data` 经宿主转成依赖插件的工具调用，仍走 `Invoker`。只有已安装到当前空间的组件才能渲染。
- **DSL 边界**：绑定只有 `{{state.x}}` / `{{props.x}}` / `{{data.<id>.x}}`；动作只有 `reload` / `invoke`（仅 `mode=action` 源、需手势与 `confirm`）/ `message`（填进输入框，不自动发送）；`data.*` 只能引用清单里的 `data_sources[].id`。
- **脚本 ABI**：`sandbox.Start` 长驻进程 + NDJSON（`invoke` / `return` / `error` / `call` → `result`）。宿主 ABI 只剩两件事：`log`，以及 `secret.get`（按名字取凭证明文，名字必须逐条声明在 `capabilities` 的 `secret.use` 里；没声明的名字直接拒绝，不是返回空串）。宿主**不再代发任何 HTTP 请求**：`http.request` 只是清单里的出网声明，请求由插件在沙箱里经代理发出。
- **WebSocket**：`new WebSocket` 必须在清单里声明 `capabilities: [{"method":"ws.connect","hosts":["..."]}]`，否则升级请求被代理拒绝。做法是给沙箱注入 `HTTPS_PROXY=unix:<egress-sock>`（Deno 2.9 认这种写法），代理收到 absolute-form 的 `GET https://...` + `Upgrade: websocket`，自己连上游转发握手，收到 101 后双向透传。宿主只看得到域名与路径，看不到帧，也做不了体积限制——所以它是独立能力，不并入 `http.request`。
- **文件直连**：脚本用 `Deno.readTextFile(wa.path("data/x"))` 直接读写，路径范围由沙箱挂载决定，同一份 `sandbox.FS` 还翻译成 `--allow-read/--allow-write`（挂载决定路径存不存在，deno 权限决定允不允许读，缺一层都算没管）。`host.call("fs.*")` 已废弃，调用直接报错。
- **出网走代理**：`internal/plugin/egress_pool.go` 给每个插件实例建一条独立的 HTTP 代理，跑在 `/tmp/wa-egress-*.sock`（unix socket 路径上限 100 字节），`sandbox.Spec.Proxy` 单文件挂进沙箱，沙箱仍 `--unshare-net`。白名单来自 `Manifest.NetHosts()`，与 `search.Guard` 同源。插件侧由 `wa.fetch` 发 absolute-form 明文 HTTP/1.1，TLS 由宿主代理完成——**不能让插件自己建 TLS**：Deno 的 `startTls` 不接受 `UnixConn`，`createHttpClient` 的 `createConnection` 也已被移除。局限：只支持 HTTP/1.1 请求-响应，WebSocket / HTTP/2 不可用。
- **插件默认一次性**：不写 `runtime.resident` 的工具每次调用起一个新进程、跑完即退。只有声明 `runtime.resident: true` 的才按 `空间|slug|目录` 复用一个子进程（`internal/plugin/resident.go`），空转 `PLUGIN_PROC_IDLE_SECONDS` 回收、上限 `PLUGIN_PROC_MAX`；安装/升级/停用/卸载/空间重载/草稿变更都会立刻杀掉对应进程。试运行（`plugin_try`）与搜索脚本 `plan/map` 走一次性 `Run`。
- **文件权限**：默认只能**读**自己所在空间的工作区（`spaces/<id>/files`），**不能写**；空间之外不可见。写权限必须显式声明 `fs.write`，`fs.read` / `fs.write` 的每一项都必须是空间工作区下的**子目录**（`.`、绝对路径、`..` 一律拒绝），`fs.read_only` 覆盖 `fs.write`。文件权限只由 `fs` 段声明：`capabilities` 里写 `fs.read` / `fs.list` / `fs.write` 直接解析失败（宿主 RPC 不再有这三个 method），写文件由 `Derive` 抬到 L3。同一份 `sandbox.FS` 落到两个执行点——挂载决定「路径在沙箱里存不存在」，`--allow-read` / `--allow-write` 决定「deno 允不允许读」，缺一层就等于没管。`capabilities[].paths` 与 `runtime.timeout_ms` 已废弃（从未生效），写了直接解析失败。运行时装在非标准位置要用 `SANDBOX_READ_EXTRA`。
- **权限展示**：`Manifest.Permissions()` 是权限文案的唯一定义点（文件 / 网络 / 凭证 / 命令 / 远端工具），市场目录（`permissions`）与空间列表（`permissions` / `latest_permissions`）都读它；安装与升级都要先弹确认展示，新增权限类别只改这一个函数。
- **审计永久保留**：`plugin.publish` / `install` / `uninstall` / `enable` / `reset_baseline` 都写 `audit_log`，卸载不删审计。
- **启动迁移**：`plugin.Manager.Bootstrap` 发布内置技能、把旧 `skills/` `tools/` `mcps/` 备份到 `plugins/.history/legacy/{ts}/` 后一次性迁移，已存在的空间不追溯默认安装集（只打标记）。
- 配置项：`PLUGIN_DEFAULT_INSTALL`（逗号分隔 slug，新空间首次初始化时安装一次）；没有「是否允许安装」开关，安装权固定为空间 `manage`（L4 另需系统管理员）。
- 接口：`GET /spaces/{id}/plugins`、`GET|POST .../{slug}/{dry-run,publish,install,uninstall,enable,disable,baseline,baseline/reset,baseline/dismiss,data,ui}`、`GET /plugins`（市场目录，含待发布元数据与 `is_admin`）、`GET|PUT /spaces/{id}/plugins/{slug}/config`（安装级配置）。`publish` 现在只允许系统管理员；组件产物不再对外暴露静态路由。
- 前端：空间页 `PluginPanel`（本地/已安装、启停、卸载、配置、试运行；安装入口在市场）、插件市场 `PluginCatalogPanel`（组织内待发布/已发布、平台内置、外部内部可见；系统管理员可发布，空间管理员可安装；类型筛选含智能体）、配置入口 `PluginConfigDialog`、会话 `UiBlock` + `UiView`（DSL 解释器）、空间头部智能体选择器（`GET /spaces/{id}/agents`）。内置技能：`builtin_space` / `builtin_files` / `builtin_sessions` / `builtin_plugin_dev`（插件开发总纲 + 技能/工具/MCP/组件/智能体子手册，组件 DSL 全表在其 `references/component.md`）/ `builtin_coding`（编程实践：实现/调试/重构/审查四本子手册，正文以 `internal/space/coding_docs/` 下的 markdown 文件 embed 维护）。

# 能力索引与会话技能

- 每个空间文件根下有一个隐藏索引 `files/.meta.json`，只汇总 `plugins/` 下的四类插件。写操作（建/改/删插件目录、写文件、迁移、seed）后重建；读取时缺失或损坏则按磁盘现状重建。
- Agent 的能力源 = **已安装插件 + 当前空间草稿 + 系统空间草稿**，同名（Name）优先级：已安装 > 当前空间 > 系统空间。已安装件即使没有落副本（`extensible: false`）也会进检索池。
- 资源总量不设上限，**不提供全量列举，也不常驻技能索引**：
  1. 系统提示词只写「何时必须检索」的硬规则：闲聊 / 翻译 / 改写 / 计算 / 总结已有材料直接做；提到「我们的规范 / 流程 / 模板 / 制度」、产出正式文档、执行明确业务动作必须先检索；检索不到要明说，不得用常识冒充公司规定。
  2. `resource_search`：`query` 可选，只给 `kind` 时按名称列出该类资源；`kind` ∈ `skills|tools|mcps|components`；默认 8 条、最多 20 条。
  3. `resource_read` 读取正文：先当前空间，再系统空间；`published/{kind}/{slug}/{version}/...` 由插件层解析（已安装或 public 才可读），支持 `offset` / `limit`。
  4. 会话级技能：`session.skill_path` 绑定一个技能，每轮把 SKILL.md 正文注入系统提示词；`PUT /sessions/{id}/skill`，空 `path` 清除。路径是相对 `files/` 的 `plugins/<slug>/SKILL.md`，内置件是 `published/skills/<slug>/<version>/SKILL.md`。
- 命中同名多来源资源时，必须让用户在候选中选择，不得自行假设。
- 上下文按 token 控制（不按字节）：单技能正文 4000 tokens，超出截断并提示继续读取；工具结果回填默认 4KB，读类工具可申请 24KB。

# 智能体设定（agents）

- **搜索源也是插件**：`kind=source`，`source.protocol` 取 `spec`（`search.json` 声明式）或 `script`（`main.ts` plan/map 两段式）。它不是模型可调用的工具，而是 `web_search` 背后的实现：安装/启用即注册进 `search.Registry`，**不需要重启**；装在系统空间即全局可用，装在普通空间只对该空间生效。`source.endpoint` 决定 Guard 白名单，`allow_private` 放开 http 与私网（推导 L3），`secret` 只写凭证名。清单里不允许 `capabilities`——出网终点已由 endpoint 固定。`plugin_try` 工具用来试跑草稿（搜索源必须传 `query`），回执里的 `top_keys` 决定 `items` 路径怎么写。
- **唯一来源是 `kind=agent` 插件**：系统空间安装的智能体全局可选，普通空间安装的只在该空间可选。旧版系统空间 `agents/<id>/` 目录已废弃，启动时由 `plugin.Manager` 迁移成智能体插件（发布为 `origin=system` 并全局安装），随后目录被备份到 `plugins/.history/legacy/<ts>/agents` 并删除。
- 清单里 `agent` 段：`entry`（提示词正文，默认 `AGENT.md`）/ `extends`（父智能体 slug，可多层，最多 4 层、禁止成环）/ `requires`（依赖的技能、MCP、工具，安装本插件时一并装进同一空间）。
- 平台默认智能体是 slug `base` 的内置插件（正文即「通用助理」）：`extends: "base"` 表示继承它；平台基础规则仍写死在 `internal/agent/prompt.go`，只能追加。
- **base 锁定（方案 A）**：平台基础规则写死在 `internal/agent/prompt.go`，智能体只能在其后追加，不能覆盖、不能删除；冲突时以基础规则为准。要改基础规则走平台发版。
- **平台部署事实进提示词，不做成工具**：租户（企业）名称、IM 接入、当前用户与空间由 `session` 层填入 `RunInput.Platform`，`BuildSystemMessage` 渲染成 `## 平台（部署事实）`。理由：这些信息必须恒真，而模型对「公司用什么 IM」有强先验，做成工具会顺着猜而不调用；工具 schema 还会每轮常驻。新增同类事实（版本号、配额）优先走这里，只有「量大 / 需按权限裁剪 / 需审计」的才考虑工具。
- 智能体是行为设定，**不是权限**：可见工具仍由 `tool_binding` + 策略 + 审批决定，`agent.json` 里不出现任何 endpoint / 密钥 / 命令字段。
- **内置工具视为已绑定**：`tool_binding` 只用于给资源型工具（数据库 / MCP / 自定义工具）授权数据范围；内置件随发版走、没有按空间配置的入口，按已绑定处理（`internal/tool.applyBindings`）。豁免只放开「有没有绑定」：L2 放行，L3 仍需 `allow_write`（否则审批），L4 一律审批。`plugin_try` 是 L2，之前没有这条豁免对所有空间都判「需要管理员绑定」。
- 会话创建时绑定（`POST /spaces/{id}/sessions` 的 `agent_id`，空值用 `base`），并把 `agent_id` / `agent_version` / `agent_prompt` **固化**在 `session` 行；管理员事后改智能体不影响已开会话。清单接口：`GET /spaces/{id}/agents`。

# 通用搜索与凭证

- `web_search`（能力 `search.query`）：参数是 `query` / `limit` / `scope`。**schema 里不暴露 provider**——模型只表达「内网 / 公网 / 都要」，源由 `kind=source` 插件提供，换源不改工具契约。
- 多源路由：`scope=auto`（默认）先查 `internal` 再补 `external`，按 `priority` 排序、按 URL 去重；`scope=internal|external` 只查对应范围。每个源有自己的墙钟与 Guard，单源失败不打断整体，只有全部失败才报错。
- **搜索源只有插件一条路**：平台不再从 `config.json` / `net_whitelist.json` / 环境变量拼装内置源（原 `search.provider`、`search.spec`、`search.script`、`SEARCH_*` 与内置 `bocha` provider 已全部移除）。装一个源 = 发布并安装 `kind=source` 插件，注册名是**插件 slug**，出网白名单由清单里的 `source.endpoint` 自动推导。
- REST 搜索 API 没有行业标准（博查 `data.webPages.value[]`、Tavily `results[]`、Serper `organic[]`，字段名各不相同），但差异都能归结为「请求参数名 + 响应字段路径」。所以 `kind=source` 插件用 `search.json` 声明式描述，由 `internal/search.HTTPJSON` 解释，接新 API 不用写 Go、不用发版：

  ```json
  {
    "request": {
      "method": "POST",
      "auth": { "in": "header", "name": "Authorization", "prefix": "Bearer " },
      "body": { "query": "{{query}}", "max_results": "{{limit}}" }
    },
    "response": {
      "items": "results", "title": "title", "url": "url",
      "snippet": ["summary", "snippet"]
    }
  }
  ```

  占位符：`{{query}}` `{{limit}}` `{{freshness}}` `{{secret}}`（正好是占位符时保留类型，`{{limit}}` 仍是数字）。路径支持 `a.b[0].c`。`auth.in` ∈ `header|query|body`。`error_code` + `error_ok` 用于把业务错误码（如 `code != 0`）判成失败，而不是当成 0 条结果。
- **字段路径可以写成数组**：按顺序取第一个非空值（`"snippet": ["summary", "snippet"]`）。不同 API 把正文放哪个字段并不稳定（博查开了 `summary` 就把正文放 `summary`，`snippet` 常为空），只取一个字段会静默产生空摘要。`items` 也支持数组，用于应对「正常返回在 A、降级返回在 B」。
- 声明式覆盖不到的情况（游标分页、非 JSON 响应、请求签名）用脚本源：同一份清单里 `source.protocol=script`，正文是 `.ts`（见下）。
- 凭证由清单里的 `source.secret` 声明（如 `"secret": "bocha"`），scope 固定 `search`，每次搜索都从凭证存储读，轮换后立即生效。**没有统一 `default` 名、没有 `${ENV}` 写法、没有启动播种**：密钥从哪来只由插件声明 + 凭证存储决定。
- 出网唯一通道是 `internal/search.Guard`：只允许 https、域名边界白名单（`bocha.com.evil.com` 不命中）、dial 期拒绝私网 / 回环 / 链路本地 IP（含 `169.254.169.254`）、不跟随 30x、响应体上限 4MB。
- 凭证：`{STORAGE_ROOT}/secrets/{scope}.json`，KEK（`SECRET_KEK`）包裹每条凭证独立的 DEK，AES-256-GCM 信封加密；对外只暴露 8 位指纹。密钥只由平台内部取用，错误信息会精确抹除密钥原文。
- **密钥不进空间文件树**：系统空间对管理员是可下载的文件，明文密钥进树就等于进备份、发布快照和任何有 read 权限的会话。更新走管理员接口：`GET /admin/settings`（指纹）、`PUT|DELETE /admin/secrets/{scope}/{name}`（只写不读，响应只含指纹），仅系统管理员可调用。插件清单里存的只是凭证名，不是密钥。
- 界面入口是系统空间页的「**密钥配置**」：可录入**名称 + 密钥值**，列表展示指纹与更新时间。`refs` 传空——搜索凭证的引用关系由 `kind=source` 插件清单声明，属于运行期状态（随安装/卸载变化），启动时无法静态列出。
- **会话内录入**（`secret_request`，能力 `system.secret.request`，L1）：模型只提供 `scope` / `name` / `label` / `hint`，前端在消息里渲染录入卡片（`SecretRequestCard`，不折叠），明文由浏览器直接 `PUT /admin/secrets/{scope}/{name}`，**不进工具参数、不进模型上下文、不进 `run_step` 审计**；模型回读到的只有「是否已配置 + 指纹」，`can_write` 由 `space.IsAdmin` 判定，非管理员只拿到「转给管理员」的指引。密钥存储没启用（无 `SECRET_KEK`）时工具直接说明，不引导填写，避免变相索要明文。平台基础规则与 `builtin_plugin_dev`（总纲 + `references/source.md` + `references/mcp.md`）都写明：需要凭证时用它，禁止向用户索要明文。
- 搜索密钥改完**立即生效**（每次调用都从凭证存储取）。插件安装/启用即注册进 `search.Registry`，停用/卸载立即移除，不需要重启。
- 新增搜索源：写一个 `kind=source` 插件（`plugin.json` 的 `source.endpoint` / `scope` / `priority` / `secret` + `search.json` 或 `main.ts`），发布、安装。**没有「改 Go 代码注册 provider」这条路了**，`internal/search` 不再持有任何厂商实现。

- **脚本搜索源**（`source.protocol=script`）：签名、游标分页、非 JSON 响应这类声明式表达不了的，写成 `.ts` 脚本交给 `deno` 执行。脚本**没有网络权限**，两段式：`plan` 输出 `{request:{method,url,headers,body}}`，`map` 收 `{status,body}` 输出 `{results:[...]}`；出网仍由 `Guard` 完成。密钥只走 stdin，不进 argv（`ps` 可见）也不进环境变量（`/proc/<pid>/environ` 可见）。脚本写坏了只让这个源加载失败，其余源与内置能力照常可用。

# 命令执行沙箱（`internal/sandbox`）

- 入口是 `sandbox.Runner`（`Name()` + `Run(ctx, Spec)`），三个后端：`bwrap`（Linux）、`sandbox-exec`（macOS Seatbelt）、`local`（无隔离，仅开发/降级）。`Detect()` 按强度降级，调用方**必须检查 `Name()`**，把 `local` 当沙箱用是错误用法。
- 三档策略 `Mode`：`read-only`（全盘只读）/ `workspace-write`（`Spec.Dir` + 私有临时目录可写）/ `full`（不套沙箱，审计记 `none`）。网络默认关闭，要开得显式 `Network: true`。
- 每次调用固定做四件事：`MkdirTemp` 私有 `TMPDIR`/`HOME`（结束即删）、清空父进程环境（只保留 `PATH`/`LANG`/`TZ` 等无害项，防止密钥被 `env`/`cat` 带走）、墙钟超时、`stdout`/`stderr` 各 64KB 上限（超限即 kill）。
- 进程组语义：`Setpgid` + `Kill(-pgid)`，超时或输出超限时**整棵进程树**一起回收，不留孤儿。非零退出码是数据（`Result.ExitCode`），只有启动失败 / 超时 / 输出超限才返回 error。
- 路径要先 `EvalSymlinks`：macOS 上 `/var` → `/private/var`，不解析会出现「策略放开了写权限却报 Operation not permitted」。
- `shell_exec`（能力 `shell.exec`，risk=write，副作用限空间工作区）把命令跑在当前空间 `files/` 下，固定 `ModeWorkspaceWrite` + 无网、默认 15s / 最多 25s（Invoker 墙钟是 30s）。非零退出码、超时、输出超限都作为结果回给模型，只有「沙箱不可用 / 存储不是本地路径」才返回 error。后端降级到 `local` 时启动日志会显式告警。
- **`SANDBOX_REQUIRE=true`（生产必须打开）**：启动阶段走 `sandbox.Ensure`，只有 `local` 就**启动失败**（`ErrNotIsolated`），把「以为有沙箱、其实在裸跑」挡在上线前。开发机默认 false，允许降级但一定留告警。后端名用常量 `sandbox.NameBwrap` / `NameSeatbelt` / `NameLocal` 比较，不要写字面量。

# 资源发布、插件市场与发布记录

- **一级菜单只有两个**：工作空间 / **插件市场**。市场对全员可见，分类是**左侧二级菜单**（路由 `/plugins/market/{category}`）：组织内已发布、组织内待发布（全员见元数据，内容仅系统管理员）、平台内置、外部（内部可见）。没有独立「已装插件」页；已装插件属于空间，入口在空间页。
- **待发布 -> 发布 -> 安装**：空间管理员启用本地插件后进入市场「待发布」；系统管理员发布后其他空间才可安装；安装只对目标空间生效，写只读副本。
- **安装级配置**：MCP 的服务地址、鉴权 Header、凭证名可以按空间覆盖，存 `plugin_install.config`（`0020_plugin_install_config.sql`），发布快照不动；`GET|PUT /spaces/{id}/plugins/{slug}/config`，写需要 `manage`，保存后整空间重载并记 `plugin.config` 审计。只存凭证名不存密钥；地址只允许 https（本机 http 放行）。技能/工具/组件没有安装级配置——参数属于清单与内容，改行为要重新发布。
- 「资源快照」不是并列能力，降级为空间页头部的「发布记录」次级页（`/resources/snapshots`），因为它本质是空间文件的发布结果。
- 生命周期：**空间草稿（可编辑）→ 发布（不可变版本快照）→ 发布记录页 / Agent 按需检索**。发布范围只有用户范围：`public` 全部可见 / `private` 仅自己可见；没有「必须加载 / 可选加载」。
- 存储与工作空间分离：`{STORAGE_ROOT}/published/{kind}/{resource_id}/{version}/`，含快照文件与 `.meta.json`（kind / name / version / visibility / resource_id / source_space_id / published_at）。目录用 `resource_id` 是因为名称只在「发布者 + kind」内唯一。
- 数据表：`resource`（kind / name / description / visibility / status / owner_id / source_space_id / latest_version）+ `resource_version`（version / notes / storage_path / file_count / total_bytes），见 `0016_resource_publish.sql`。
- 接口：`GET/POST /resources`、`GET/PATCH/DELETE /resources/{id}`、`DELETE /resources/{id}/versions/{version}`；实现在 `internal/resource`，装配在 `internal/app/app.go`。
- 权限：发布需来源空间 `write`；`public` 发布需该空间 `manage` 或系统管理员；删除资源 / 删除版本 / 启停仅发布者本人或系统管理员。资源接口只返回名称、版本、描述与文件数/体积，**不返回发布目录与原始 MCP / tool / DB 配置**。
- 约束：版本号 `x.y.z`；同版本重复发布 409；只剩一个版本时禁止删版本；快照 ≤ 200 文件 / 5MB；发布技能必须有 SKILL.md 正文。
- Agent 侧：发布件是独立一层能力源（`source=published`），不做同名覆盖，命中同名多来源时 `resource_search` 返回 hint 要求先问用户；停用资源不进入检索；`resource_read` 支持 `published/{kind}/{id}/{version}/...` 路径并校验可见性。
- 前端：`/plugins/market` 与 `/plugins/market/:category` 渲染插件市场（分类在左侧二级菜单，右侧只留类型筛选）；`/plugins/installed` 兼容跳转到市场；`/resources` 跳转到插件市场；`/resources/snapshots` 渲染 `ResourcePanel`（发布记录），入口在 `SpacePanel` 头部。所有路由复用 `WorkspacePage` 外壳。插件类型统一用 `PluginVisual` 的 `PluginIcon`；左侧空间树按 `Dir.PluginKind` 挂图标，安装副本额外显示锁，未启用本地插件置灰。

# 身份接入（登录）

登录与用户资料统一走 `internal/identity.Provider` 抽象（技术方案 §5.1.1），切换 IdP 只改配置：

| 变量 | 说明 | 示例 |
|---|---|---|
| `IDENTITY_PROVIDER` | `dingtalk`（已实现）/ `wecom`、`feishu`（占位，登录返回 501，不影响启动） | `dingtalk` |
| `DINGTALK_CLIENT_ID` / `DINGTALK_CLIENT_SECRET` | 应用 appKey / appSecret；扫码登录与通讯录接口共用 | `dingxxx` |
| `DINGTALK_CORP_ID` | 企业 CorpID | `dingxxxx` |
| `DINGTALK_CORP_NAME` | 企业名称兜底（可选），未开通 `Contact.Org.Read` 时顶栏显示 | `某公司` |
| `DINGTALK_REDIRECT_URI` | 回调地址 | `http://127.0.0.1:8321/auth/callback` |

- 端点与 IdP 无关：`GET /auth/login/url`、`GET /auth/login`、`GET|POST /auth/callback`、`GET /auth/provider`；历史 `/auth/dingtalk/*` 保留为别名。
- 钉钉资料链路：用户令牌 `GET /v1.0/contact/users/me` → 企业令牌 `POST /v1.0/oauth2/accessToken` → `POST /topapi/user/getbyunionid` → `POST /topapi/v2/user/get`，后者提供**工号 `job_number` 与 `email`**；企业名称取 `GET /v1.0/contact/organizations/authInfos?targetCorpId=` 的 `orgName`。
- **权限点**：`qyapi_get_member`（通讯录成员信息读，工号）、`Contact.Org.Read`（企业认证信息读，企业名称）、「企业员工手机号信息 / 邮箱等个人信息」（敏感字段，email/mobile）。前两者缺失返回 `errcode=88 / sub_code=60011`；敏感字段未勾选时接口**整段不返回** `email`/`mobile`，工号仍正常。
- 通讯录权限缺失或接口失败时只告警降级，工号/邮箱/企业名留空，**不阻断登录**；已存字段只做「有值才覆盖」；同步告警（缺权限、邮箱未返回）写 `user_identity.contact_error`，由 `/auth/me` 的 `profile_warning` 暴露给前端提示。
- 账号唯一键 `(provider, provider_uid)`，落表 `user_identity`；`app_user` 存 name / avatar / mobile / email / job_number / corp_name。

# 模型配置

模型访问统一走 Provider 适配层（技术方案 §5.7.1），由 ModelRouter 按路由（`chat` / `summary` / `embedding`）选择主模型并在可重试错误上降级到备模型：

| 变量 | 说明 | 示例 |
|---|---|---|
| `LLM_PROVIDER` | 协议族：`openai`（OpenAI 兼容，含 DeepSeek/vLLM/Ollama）或 `anthropic` | `openai` |
| `LLM_BASE_URL` | 端点（不含 `/chat/completions`） | `https://api.deepseek.com/v1` |
| `LLM_API_KEY` | 密钥；本地 Ollama / vLLM 可留空 | `sk-xxx` |
| `LLM_MODEL` | 模型名 | `deepseek-chat` |
| `LLM_TIMEOUT` | 单次模型调用超时（整请求，含流式读取），**必须 ≥ `LLM_MAX_WALL_CLOCK`** | `10m` |
| `LLM_MAX_TOKENS` | 单次响应 token 上限，0 表示不传 | `4096` |
| `LLM_MAX_ROUNDS` | 单轮对话内工具调用轮次上限（一轮 = 一次模型调用） | `8` |
| `LLM_MAX_TOOL_CALLS` | 一次 run 内工具调用总次数上限 | `20` |
| `LLM_MAX_PARALLEL` | 同一轮内并行执行的工具数 | `4` |
| `LLM_MAX_WALL_CLOCK` | 一次 run 的墙钟预算 | `10m` |
| `LLM_MAX_TOTAL_TOKENS` | 一次 run 的累计 token 上限（与单次响应的 `LLM_MAX_TOKENS` 不是一回事），0 表示不限 | `80000` |
| `AGENT_DISPATCH` | 是否把 `agent_dispatch`（派生子 Agent）暴露给模型 | `true` |
| `AGENT_MAX_DEPTH` | 派发深度上限，1 = 只能主 → 子 | `2` |
| `AGENT_STALL_ROUNDS` | 连续多少轮没有任何新信息就收回工具、强制收尾 | `2` |
| `LLM_SUMMARY_MODEL` | 历史摘要用小模型，为空复用主模型 | `deepseek-chat` |

备模型与向量模型均为可选，不配则只跑主链路：

| 变量 | 说明 |
|---|---|
| `LLM_FALLBACK_PROVIDER` / `LLM_FALLBACK_BASE_URL` / `LLM_FALLBACK_API_KEY` / `LLM_FALLBACK_MODEL` | 主模型失败（0/429/5xx）时的降级目标；仅同协议族可降级，已产出流后不再切换 |
| `LLM_EMBED_BASE_URL` / `LLM_EMBED_API_KEY` / `LLM_EMBED_MODEL` | 向量模型（`/embeddings`），配置后开启历史消息语义检索；未配置则检索退化，只保留摘要 + 滑动窗口 |

未配置密钥时，启动日志会告警，对话接口返回 `503`，前端输入框禁用并提示。

# Agent 运行时与实现切换

Agent 的「编排与循环」是唯一可替换的一层，按技术方案 §5.7.7 / §5.7.8 隔离：

- 契约 `internal/agent`：`Agent` 接口（`Name` / `Capabilities` / `Ready` / `Model` / `Budget` / `Run`）、§7.3 事件协议、`RunInput` / `Result`、系统提示词组装 `BuildSystemMessage`。**不出现任何框架类型**。
- 实现：`internal/agent/builtin`（内置 ReAct，默认）、`internal/agent/direct`（无工具单轮直答）。实现包在 `init()` 里 `agent.Register(Name, New)`；接入 Eino 时新增 `internal/agent/eino`，**只有实现子包可以 import 框架**。
- 切换：`AGENT_IMPL`（`builtin` / `direct`），装配在 `internal/app/app.go`；未注册的名字启动告警并回退 `builtin`。当前实现与能力见 `GET /agent/info`，每次运行写入 `run.budget.impl`。
- 平台侧不因实现而变，实现里不要重做：审批判定与挂起（`internal/tool/invoker` + `internal/policy`）、checkpoint（`run` / `run_step`，会话层按事件落库）、上下文压缩（`internal/ctxmgr`）、预算上限（实现用 `Budget()` 声明，循环外层兜底）、SSE 事件契约。
- 实现只负责：每轮上下文如何拼（可复用 `BuildSystemMessage`）、循环与轮次控制、工具选择与并行调度、把工具执行结果翻译成 §7.3 事件。
- **循环守卫（`builtin/guard.go`）**：每轮结束后判断「下一步还有没有价值」——同一工具 + 相同参数（参数按 JSON 规范化后取指纹）第 3 次起拦截、不执行；连续 `AGENT_STALL_ROUNDS` 轮没有任何新信息（空结果 / 报错 / 与之前完全相同）就**收回工具**，逼一次不带工具的结论，而不是继续烧预算。轮次用尽同样走这条收尾路径，因此不会再出现「轮次耗尽 → 空回答」。
- **收尾语义**：拿到结论 → `run` 成功，`stop_reason` 记 `budget` / `stalled` 供归因；拿不到结论 → 返回 `ErrBudgetExceeded` / `ErrStalled`，会话层标 `cancelled` 并保留已生成内容。
- **派发（`builtin/dispatch.go`）**：`agent_dispatch` 暴露为一个普通工具，子 Agent 跑同一套 `Invoker`（PDP / 审批 / 审计不变），上下文与轮次预算隔离，结论按 4KB 回填。规则：子 Agent 只能拿到父 Agent 已有的工具；深度受 `AGENT_MAX_DEPTH` 约束；**子 Agent 看不到父会话历史**，任务描述必须自包含。派发本身是 L2、无副作用，真正的权限判定发生在子 Agent 的每次工具调用上。
- **黄金用例（`internal/golden`）**：harness 行为用脚本化模型跑成数据（`evals.json`），断言停止原因、工具序列、轮次与拦截次数；`internal/golden/testdata/harness.json` 是平台回归闸门。插件目录里的 `evals.json` 在**发布时**执行（`kind=tool` 走 `DryRun` 的确定性断言，`kind=agent` 用同一套脚本化执行），失败即 `409` 拒绝发布，审计记 `plugin.publish.eval_failed`。
- 已知差距：`Capabilities.Resume` 只声明未实现（§5.7.5 断点续跑未落地）；审批目前只推 `approval.req`，没有审批记录与恢复入口；长任务仍受「run 绑定请求」限制，跨 run 的任务队列未落地。

# 运行流与 checkpoint

- SSE 事件名与载荷按技术方案 §7.3：`run.started` → `message.delta` → `tool.call` / `tool.result` / `approval.req` → `message.done` → `run.finished`。
- 一次对话对应一条 `run`，每一步写一条 `run_step`（`tool.call` / `tool.result` / `approval.req` / `message`），用于恢复与审计；`tool_call.run_id` 与 `run` 关联。
- 修改事件名或载荷字段时必须同步改 `web/src/api/stream.ts`，两侧以本文档与技术方案为准。
- 助手消息落库时同时记录这一轮的用量与耗时（`message.in_tokens` / `out_tokens` / `duration_ms`，见迁移 `0017_message_stats.sql`），`/sessions/{id}/messages` 随之返回，前端在消息卡片上展示「token 消耗 + 总耗时」。用户消息这三列为 0；未产生回答的被中断轮次不落消息。

## SSE 时限（业内取值，别自己发明）

| 项 | 取值 | 说明 |
| --- | --- | --- |
| 服务端 `WriteTimeout` | **0** | Go 的这个值是整条响应的绝对写截止，且无法按路由豁免；设了就会把流按秒掐断 |
| 单次写阻塞容忍 | 30s | `platform.Stream` 每次写前 `ResponseController.SetWriteDeadline` 刷新，超时熔断 |
| 心跳注释行 | 15s | 必须小于链路上最短空闲超时（nginx / 云 LB 默认 60s），取 1/4 ~ 1/3 |
| 首字节 | 开流立刻发 `: open` | 穿越 CDN 的首字节超时（Cloudflare 约 100s 报 524） |
| 流的生命周期上限 | `LLM_MAX_WALL_CLOCK`（缺省 10m）+ 30s ctx 兜底 | 由应用自己定，不依赖中间件超时；到点以 `cancelled` 收尾 |

- 实现位置：`internal/platform/sse.go`（推送器，唯一允许写 SSE 的地方）、`cmd/server/main.go`（不设 `WriteTimeout`）。
- 中断可观测：`run.finished` 带 `reason`（`client_disconnected` / `wall_clock_exceeded` / `budget_exceeded`），同时写 `run.error` 与会话层 Warn 日志；客户端已断开时事件写不出去，只能靠日志。
- 预算用尽由实现返回 `agent.ErrBudgetExceeded`，外层据此判定为 `cancelled` 而不是 `failed`——「重试就好」对预算用尽是误导。
- 心跳只能打败「空闲超时」；「整响应总超时」类计时器只能改配置（GCP 外部 ALB 默认 30s、Envoy route 默认 15s、API Gateway REST 集成超时 29s）。
- nginx 部署时：流式 location 配 `proxy_read_timeout 3600s; proxy_buffering off; gzip off;`（代码已发 `X-Accel-Buffering: no`），且只对流式路由放宽，别全局调大。
