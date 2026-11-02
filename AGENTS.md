# WorkAgent
面相企业级场景的 Agent 平台，提供企业级 Agent 的开发、部署、运维、监控等一站式服务。

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
