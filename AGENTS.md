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
3. 阶段性简化（如首版工具只读、单 Agent 不做编排）要在提交信息里注明与方案的差距及后续补齐计划。

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
make run     # 后端 :8000，启动时自动执行迁移
make web     # 前端 :5173
```

# 身份接入（登录）

登录与用户资料统一走 `internal/identity.Provider` 抽象（技术方案 §5.1.1），切换 IdP 只改配置：

| 变量 | 说明 | 示例 |
|---|---|---|
| `IDENTITY_PROVIDER` | `dingtalk`（已实现）/ `wecom`、`feishu`（占位，登录返回 501，不影响启动） | `dingtalk` |
| `DINGTALK_CLIENT_ID` / `DINGTALK_CLIENT_SECRET` | 应用 appKey / appSecret；扫码登录与通讯录接口共用 | `dingxxx` |
| `DINGTALK_CORP_ID` | 企业 CorpID | `dingxxxx` |
| `DINGTALK_CORP_NAME` | 企业名称兜底（可选），未开通 `Contact.Org.Read` 时顶栏显示 | `某公司` |
| `DINGTALK_REDIRECT_URI` | 回调地址 | `http://127.0.0.1:8000/auth/callback` |

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
| `LLM_TIMEOUT` | 单次模型调用超时 | `120s` |
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

# 运行流与 checkpoint

- SSE 事件名与载荷按技术方案 §7.3：`run.started` → `message.delta` → `tool.call` / `tool.result` / `approval.req` → `message.done` → `run.finished`。
- 一次对话对应一条 `run`，每一步写一条 `run_step`（`tool.call` / `tool.result` / `approval.req` / `message`），用于恢复与审计；`tool_call.run_id` 与 `run` 关联。
- 修改事件名或载荷字段时必须同步改 `web/src/api/stream.ts`，两侧以本文档与技术方案为准。
