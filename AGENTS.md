# WorkAgent
面相企业级场景的 Agent 平台，提供企业级 Agent 的开发、部署、运维、监控等一站式服务。

# 文档与实现约定

项目的设计依据是以下两份文档。**实现必须遵循技术方案**，不要自创架构、分层或命名：

| 文档 | 内容 | 作用 |
|---|---|---|
| [docs/技术方案.md](docs/技术方案.md) | 分层架构、领域模型（五层模型 / 权限 / 资源）、模块设计、接口约定、安全与部署、演进路线 | **实现的事实标准**，代码与文档冲突时以文档为准 |
| [docs/MVP版本功能.md](docs/MVP版本功能.md) | MVP 范围、页面形态、功能优先级 | 范围裁剪与排期依据 |

约定：

1. 开发新功能前，先读技术方案中对应章节，按其中的分层、领域模型与接口约定实现。
2. 需要偏离技术方案时，**先改文档并说明原因，再改代码**，不允许代码先行、文档长期滞后。
3. 阶段性简化（如首版只支持内置工具、单 Agent 不做编排）要在提交信息里注明与方案的差距及后续补齐计划。

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
```

# 系统空间（system）

平台内置一个全局唯一的「系统空间」，**只有系统管理员可见、可编辑**：

| 变量 | 说明 | 示例 |
|---|---|---|
| `ADMIN_JOB_NUMBERS` | 系统管理员工号，逗号分隔；MVP 用配置指定，后续接组织架构改角色表 | `16390,16391` |

- 服务启动时按工号查 `app_user` 初始化（管理员未登录过则跳过，首次登录时创建），重复执行幂等。
- 系统空间是 `space` 表中的一条 `type='system'` 记录（`is_system_reserved=true`），不可改名、不可删除，成员固定 `manage`。
- 固定内容：一个默认会话、四个**内置目录**与两个平台配置文件，目录本身不可改名/删除，内容可编辑：
  - `files/skills`：全局技能，每个技能含 `.meta.json` + `SKILL.md`
  - `files/tools`：内置工具定义，与 `tool.BuiltinManifests()` 同步，一个工具一个 JSON
  - `files/mcps`：MCP 服务快照，`README.md` + 每个已注册服务一个 JSON
  - `files/agents`：智能体设定，`index.json` + 每个智能体 `agent.json` + `prompt.md`
  - `files/config.json`：平台配置（搜索 provider / endpoint / 凭证引用名），**不含密钥**
  - `files/net_whitelist.json`：出网白名单（域名列表）
- `skills/` `tools/` 是 Agent 默认全局可见的能力搜索源，任何用户可读：Agent 用 `resource_search` 检索、`resource_read` 读文件（路径限定在 `skills/` `tools/` `mcps/` 下）。
- `skills/` 与 `tools/` 是平台托管内容，随代码版本覆盖（否则升级后文档与运行时工具对不上）；`mcps/` 与 `agents/` 只在缺失时写入，管理员改过就不覆盖。
- `agents/` `config.json` `net_whitelist.json` **不参与跨空间回落**：`ReadSourceFile` 只允许回落到 `skills/` `tools/` `mcps/`，否则普通空间可以用同名路径读到白名单与平台配置。平台自身读取时必须显式传系统空间 ID（`space.Service.SystemSpaceID`）。

# 能力索引与会话技能

- 每个空间文件根下有一个隐藏索引 `files/.meta.json`，汇总该空间的 skills / tools / mcps（名称、描述、相对路径）。写操作（建/改/删技能目录、写文件、seed）后重建；读取时缺失或损坏则按磁盘现状重建。
- Agent 的能力源 = **当前空间 + 系统空间**，同名（Name）以当前空间为准；系统空间未初始化时自动降级为只用当前空间。
- 资源总量不设上限，**不提供全量列举，也不常驻技能索引**：
  1. 系统提示词只写「何时必须检索」的硬规则：闲聊 / 翻译 / 改写 / 计算 / 总结已有材料直接做；提到「我们的规范 / 流程 / 模板 / 制度」、产出正式文档、执行明确业务动作必须先检索；检索不到要明说，不得用常识冒充公司规定。
  2. `resource_search`：`query` 可选，只给 `kind` 时按名称列出该类资源（按需拉目录）；默认 8 条、最多 20 条。`resource_read` 读取正文（先当前空间、再系统空间，支持 `offset` / `limit`）。
  3. 会话级技能：`session.skill_path` 绑定一个技能，每轮把 SKILL.md 正文注入系统提示词；`PUT /sessions/{id}/skill`，空 `path` 清除。
- 命中同名多来源资源时，必须让用户在候选中选择，不得自行假设。
- 上下文按 token 控制（不按字节）：单技能正文 4000 tokens（技能索引预算已删除），超出截断并提示继续读取；工具结果回填默认 4KB，读类工具可申请 24KB。
- 技能遵循 Agent Skills 规范：SKILL.md 带 YAML frontmatter（`name` ≤ 64 字符、`description` ≤ 1024 字符，写清“做什么 + 何时使用”），正文只写入口流程，长内容拆子文件且保持一层引用。
- 技能路径统一为相对 `files/` 的 `SKILL.md` 路径：普通空间是 `<技能名>/SKILL.md`，系统空间是 `skills/<技能名>/SKILL.md`。

# 智能体设定（agents）

- 一个智能体 = 系统空间 `agents/<id>/` 下的 `agent.json` + `prompt.md`；`index.json` 是选择器清单（不进模型上下文）。
- `agent.json`：`id` / `name` / `version`（x.y.z）/ `description` / `extends` / `prompt` / `enabled`。`extends` 只允许空或 `base`，**禁止链式继承**；未知字段直接加载失败，不静默忽略。
- **base 锁定（方案 A）**：平台基础规则写死在 `internal/agent/prompt.go`，智能体只能在其后追加，不能覆盖、不能删除；冲突时以基础规则为准。要改基础规则走平台发版。
- 智能体是行为设定，**不是权限**：可见工具仍由 `tool_binding` + 策略 + 审批决定，`agent.json` 里不出现任何 endpoint / 密钥 / 命令字段。
- 会话创建时绑定（`POST /spaces/{id}/sessions` 的 `agent_id`，空值用 `base`），并把 `agent_id` / `agent_version` / `agent_prompt` **固化**在 `session` 行；管理员事后改配置不影响已开会话。清单接口：`GET /spaces/{id}/agents`。

# 通用搜索与凭证

- `web_search`（能力 `search.query`）：provider 由平台配置决定，skill 与模型都无法指定 endpoint；参数只有 `query` / `provider` / `limit`，没有密钥字段。
- 出网唯一通道是 `internal/search.Guard`：只允许 https、域名边界白名单（`bocha.com.evil.com` 不命中）、dial 期拒绝私网 / 回环 / 链路本地 IP（含 `169.254.169.254`）、不跟随 30x、响应体上限 4MB。
- 配置优先级：系统空间 `config.json` / `net_whitelist.json` → 环境变量 `SEARCH_*`。启动时读一次，改完需重启；配置写错**启动失败**而不是静默用默认值。
- 凭证：`{STORAGE_ROOT}/secrets/{scope}.json`，KEK（`SECRET_KEK`）包裹每条凭证独立的 DEK，AES-256-GCM 信封加密；对外只暴露 8 位指纹。密钥只由平台内部取用，错误信息会精确抹除密钥原文。
- **密钥不进空间文件树**：系统空间对管理员是可下载的文件，明文密钥进树就等于进备份、发布快照和任何有 read 权限的会话。更新走管理员接口：`GET /admin/settings`（配置回显 + 指纹）、`PUT|DELETE /admin/secrets/{scope}/{name}`（只写不读，响应只含指纹），仅系统管理员可调用；界面入口是系统空间页的「平台配置」。`config.json` 里存的只是凭证引用名，不是密钥。
- 搜索密钥改完**立即生效**（每次调用都从凭证存储取）；`config.json` / `net_whitelist.json` 改完需重启。
- 新增 provider：实现 `search.Provider` 并在 `internal/app` 注册，工具与提示词都不用改。

# 资源发布与「资源管理」

- 生命周期：**空间草稿（可编辑）→ 发布（不可变版本快照）→ 资源管理板块（按需检索）**。发布范围只有用户范围：`public` 全部可见 / `private` 仅自己可见；没有「必须加载 / 可选加载」。
- 存储与工作空间分离：`{STORAGE_ROOT}/published/{kind}/{resource_id}/{version}/`，含快照文件与 `.meta.json`（kind / name / version / visibility / resource_id / source_space_id / published_at）。目录用 `resource_id` 是因为名称只在「发布者 + kind」内唯一。
- 数据表：`resource`（kind / name / description / visibility / status / owner_id / source_space_id / latest_version）+ `resource_version`（version / notes / storage_path / file_count / total_bytes），见 `0016_resource_publish.sql`。
- 接口：`GET/POST /resources`、`GET/PATCH/DELETE /resources/{id}`、`DELETE /resources/{id}/versions/{version}`；实现在 `internal/resource`，装配在 `internal/app/app.go`。
- 权限：发布需来源空间 `write`；`public` 发布需该空间 `manage` 或系统管理员；删除资源 / 删除版本 / 启停仅发布者本人或系统管理员。资源接口只返回名称、版本、描述与文件数/体积，**不返回发布目录与原始 MCP / tool / DB 配置**。
- 约束：版本号 `x.y.z`；同版本重复发布 409；只剩一个版本时禁止删版本；快照 ≤ 200 文件 / 5MB；发布技能必须有 SKILL.md 正文。
- Agent 侧：发布件是独立一层能力源（`source=published`），不做同名覆盖，命中同名多来源时 `resource_search` 返回 hint 要求先问用户；停用资源不进入检索；`resource_read` 支持 `published/{kind}/{id}/{version}/...` 路径并校验可见性。
- 前端：`web/src/components/ResourcePanel.tsx` + `PublishResourceDialog.tsx`，菜单与「工作空间」同级，路由 `/resources` 复用 `WorkspacePage` 外壳（用路径区分内容，不复制侧栏）。

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
| `LLM_MAX_ROUNDS` | 单轮对话内工具调用轮次上限 | `5` |
| `LLM_MAX_TOOL_CALLS` | 一次 run 内工具调用总次数上限 | `20` |
| `LLM_MAX_PARALLEL` | 同一轮内并行执行的工具数 | `4` |
| `LLM_MAX_WALL_CLOCK` | 一次 run 的墙钟预算 | `10m` |
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
- 已知差距：`Capabilities.Resume` 只声明未实现（§5.7.5 断点续跑未落地）；审批目前只推 `approval.req`，没有审批记录与恢复入口。

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
