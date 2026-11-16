export type Perm = 'view' | 'read' | 'write' | 'manage';

export interface User {
  id: string;
  name: string;
  avatar: string;
  /** 企业通讯录邮箱，权限缺失时为空 */
  email?: string;
  /** 工号，权限缺失时为空 */
  job_number?: string;
  /** 企业名称，来自企业认证信息或 DINGTALK_CORP_NAME 兜底 */
  corp_name?: string;
  /** 通讯录同步失败原因（成功时为空），用于提示管理员开通权限 */
  profile_warning?: string;
  /** 身份来源：dingtalk / wecom / feishu */
  provider?: string;
}

/** 当前身份接入方信息（登录页据此渲染按钮与缺失提示） */
export interface ProviderResponse {
  provider: string;
  display_name: string;
  ready: boolean;
  missing: string[];
}

export interface Space {
  id: string;
  parent_id?: string;
  name: string;
  /** system：平台内置的「系统空间」，仅系统管理员可见 */
  type: 'personal' | 'shared' | 'system';
  path: string;
  depth: number;
  perm: Perm;
  is_system_reserved: boolean;
  locked: boolean;
  created_at: string;
}

/**
 * 空间内的真实文件目录：只存在于文件系统，不落 space 表。
 * 目录可多级（path 为相对空间 files/ 的路径），权限沿用所属空间。
 */
export interface SpaceDir {
  space_id: string;
  /** 相对空间 files/ 的路径，如 "设计/原型" */
  path: string;
  /** 展示名：skill 取自 .meta.json 的 name，其余等于末级目录名 */
  name: string;
  /** true 表示该目录是 skill（新插件或旧的 .meta.json 目录） */
  is_skill: boolean;
  /** 非空表示该目录是一个插件草稿（plugins/<slug>/），值即插件类型 */
  plugin_kind?: PluginKind;
  /** true 表示系统空间的内置目录（skills / tools / mcps），不可改名删除 */
  builtin: boolean;
  /** 安装副本：只读，只能启停/卸载 */
  installed?: boolean;
  /** 空间本地开发插件 */
  local?: boolean;
  /** 内容是否可编辑 */
  mutable?: boolean;
  /** 已安装或本地插件是否启用 */
  enabled?: boolean;
  description?: string;
}

/** skill 的 .meta.json 内容（前端只读展示，创建时由后端写入） */
export interface SkillMeta {
  name: string;
  description?: string;
  type: 'skill';
  created_at?: string;
}

export interface ChatSession {
  id: string;
  space_id: string;
  /** 会话所属目录路径，为空表示直接挂在空间根 */
  dir?: string;
  title: string;
  /** true 表示标题还允许被首条消息改写 */
  title_auto: boolean;
  /** 会话绑定的技能（SKILL.md 路径），空表示未绑定 */
  skill_path?: string;
  /** 会话绑定的智能体插件 slug，空表示未绑定 */
  agent_id?: string;
  /** 智能体所属空间；子任务会话指向发起方空间，空表示与会话同空间 */
  agent_space_id?: string;
  status: string;
  last_message_at?: string | null;
  created_at: string;
  updated_at: string;
}

export type MessageRole = 'user' | 'assistant' | 'system';

/** 工具调用结局：与后端 tool.InvokeStatus 一致 */
export type ToolStatus = 'running' | 'ok' | 'denied' | 'approval' | 'error';

/** 一次待确认的工具调用（approval.req） */
export interface ApprovalInfo {
  id: string;
  sessionId: string;
  kind: string;
  detail: string;
  reason: string;
  risk: number;
  timeoutS: number;
}

/** 服务端还没处理的确认（GET /sessions/{id}/approvals） */
export interface PendingApproval {
  approval_id: string;
  call_id: string;
  tool: string;
  kind: string;
  detail: string;
  reason: string;
  risk: number;
  timeout_s: number;
}

/** 一次工具调用的完整状态 */
export interface ToolCallRecord {
  id: string;
  name: string;
  args: string;
  result?: string;
  status: ToolStatus;
  duration_ms?: number;
  /** 命中审批规则时挂在卡片上，用户点按钮后由 tool.result 更新结局 */
  approval?: ApprovalInfo;
}

/** 消息里的结构化执行轨迹 */
export interface MessagePart {
  type: 'tool';
  id: string;
  name: string;
  args: string;
  result?: string;
  status: ToolStatus;
  duration_ms?: number;
}

/**
 * 流式过程中的一段内容：思考、正文片段或一次工具调用。
 *
 * 用有序数组而不是几个并列字段，是为了保住「先想一段、再说一句、调工具、再想一段」
 * 的真实时间线——分组渲染会把工具卡全堆到正文上面、把思考永远钉在最顶部，
 * 看起来像顺序错了，也看不出当前进展到哪一步。
 * 只用于流式过程；落库后的历史消息没有这些片段的时间位置，仍按分组渲染。
 */
export type StreamItem =
  | { kind: 'reasoning'; text: string }
  | { kind: 'text'; text: string }
  | { kind: 'tool'; tool: ToolCallRecord };

export interface ChatMessage {
  id: string;
  session_id: string;
  role: MessageRole;
  content: string;
  /** 模型的思考过程，历史消息回放用 */
  reasoning?: string;
  parts?: MessagePart[];
  /** 产生这条助手消息的那一轮的 token 消耗与总耗时；用户消息为 0 */
  in_tokens?: number;
  out_tokens?: number;
  duration_ms?: number;
  /** 还在生成中：刷新后据此渲染骨架，而不是当成一条空消息 */
  streaming?: boolean;
  /** 这一轮被取消或中断，正文只到中断点 */
  interrupted?: boolean;
  /** user 用户输入 / task_result 任务回执 / task_brief 任务说明 / external 外部回信 */
  source?: string;
  created_at: string;
}

/** 一次派发任务（§15.12.4） */
export type TaskStatus =
  | 'pending'
  | 'running'
  | 'waiting_external'
  | 'succeeded'
  | 'failed'
  | 'cancelled';

export interface SessionTask {
  id: string;
  title: string;
  assignee: string;
  assignee_name?: string;
  status: TaskStatus;
  /** 开始等待的时间点；前端本地计时，不需要后端定时任务 */
  waiting_since?: string | null;
  dispatch_count: number;
  result?: string;
}

export interface TasksResponse {
  tasks: SessionTask[];
}

/** 排障只读链接（§15.12.3），token 只在创建时返回一次 */
export interface SessionShare {
  id: string;
  expires_at: string;
  revoked_at?: string | null;
  view_count: number;
  created_at: string;
  token?: string;
  url?: string;
}

export interface SharesResponse {
  shares: SessionShare[];
}

export interface ActiveRunResponse {
  active: boolean;
  run?: RunState;
}

export interface RunState {
  id: string;
  session_id: string;
  status: string;
  provider?: string;
  model?: string;
  error?: string;
  usage?: unknown;
  started_at: string;
  ended_at?: string | null;
}

export interface AgentInfo {
  ready: boolean;
  model: string;
  /** 单文件上传上限（MB），由服务端 UPLOAD_MAX_FILE_MB 决定 */
  upload_max_file_mb?: number;
}

export interface MeResponse {
  user: User;
  spaces: Space[];
}

export interface SpacesResponse {
  spaces: Space[];
}

export interface DirsResponse {
  dirs: SpaceDir[];
}

/** 空间目录下的可预览文件：md / json 等文本类型，展示在左侧树与右侧面板 */
export interface SpaceFile {
  space_id: string;
  /** 相对空间 files/ 的路径，如 skills/空间管理/SKILL.md */
  path: string;
  /** 所属目录路径，空串表示直接在空间根 */
  dir: string;
  name: string;
  size: number;
  mod_time?: string;
}

export interface FilesResponse {
  files: SpaceFile[];
}

/** 上传结果：服务端落盘后的文件（同名会被改名为 xxx-1.md，以返回值为准） */
export interface UploadFilesResponse {
  files: SpaceFile[];
}

/** 文件正文：超出后端上限时 truncated=true，content 为截断后的内容 */
export interface FileContent extends SpaceFile {
  content: string;
  truncated: boolean;
}

export interface FileContentResponse {
  file: FileContent;
}

export interface SessionsResponse {
  sessions: ChatSession[];
}

export interface MessagesResponse {
  messages: ChatMessage[];
}

/** 技能选择器里的一条：来自当前空间或系统空间 */
export interface SpaceSkill {
  name: string;
  description?: string;
  /** SKILL.md 相对空间 files/ 的路径 */
  path: string;
  /** current 当前空间 / system 系统空间 */
  source: 'current' | 'system';
}

export interface SkillsResponse {
  skills: SpaceSkill[];
}

export interface AppendMessageResponse {
  message: ChatMessage;
  session: ChatSession;
}

/** 平台引用的某条凭证：scope + name 用于寻址，label 是给管理员看的用途 */
export interface SecretRef {
  scope: string;
  name: string;
  label: string;
  /** store = 凭证存储（界面可管理）；env = 环境变量（由部署平台注入） */
  source: 'store' | 'env';
}

/** 已保存凭证的元信息：只有指纹，没有明文 */
export interface SecretItem {
  scope: string;
  name: string;
  fingerprint: string;
  updated_at: string;
  /** 是否被平台当前引用（未被引用的条目改了也不会生效） */
  in_use: boolean;
}

export interface AdminSettings {
  secrets_enabled: boolean;
  refs: SecretRef[];
  items: SecretItem[];
}

/* ---- 插件：一份清单 + 一个执行器 + 一套能力声明，四类 kind 共用生命周期 ---- */

export type PluginKind = 'skill' | 'tool' | 'mcp' | 'component' | 'agent' | 'source';

/** 空间内可见的插件：已安装的 + 空间里的草稿 */
export interface PluginItem {
  slug: string;
  kind: PluginKind;
  name: string;
  description: string;
  version: string;
  /** draft 未启用的本地插件 / enabled 已启用的本地插件 / installed / disabled */
  status: 'draft' | 'enabled' | 'installed' | 'disabled';
  origin: 'internal' | 'system' | 'external';
  risk: number;
  installed: boolean;
  local: boolean;
  enabled: boolean;
  mutable: boolean;
  published: boolean;
  has_draft: boolean;
  extensible: boolean;
  update_available: boolean;
  latest_version: string;
  baseline_version?: string;
  local_modified: boolean;
  tools?: string[];
  /** 清单声明的权限（人类可读），安装确认时展示 */
  permissions?: string[];
  /** 待升级版本的权限：升级确认时展示，避免权限静默变化 */
  latest_permissions?: string[];
  /** 清单声明的凭证与配置状态：缺凭证的插件装上也用不了 */
  secrets?: PluginSecret[];
  /** 声明的东西没齐（目前只有缺凭证），卡片按「未启用」呈现 */
  blocked?: boolean;
  /** 未启用原因（人类可读） */
  blocked_reason?: string;
}

/** 插件声明的一处凭证：scope/name 就是密钥配置里的地址 */
export interface PluginSecret {
  scope: string;
  name: string;
  configured: boolean;
}

export interface PluginsResponse {
  items: PluginItem[];
  /** 当前用户是否系统管理员：平台凭证只有他能录 */
  is_admin: boolean;
}

/** 插件市场左侧二级菜单的分类 */
export type PluginCategory = 'all' | 'internal' | 'pending' | 'system' | 'external';

/** 会话可选的智能体（kind=agent 插件；系统空间安装的全局可选） */
export interface AgentSummary {
  id: string;
  name: string;
  version: string;
  description: string;
  extends?: string;
}

/** 插件市场里的已发布插件（只读） */
export interface PluginCatalogItem {
  slug: string;
  kind: PluginKind;
  name: string;
  description: string;
  latest_version: string;
  visibility: 'public' | 'private';
  extensible: boolean;
  risk: number;
  origin: 'internal' | 'system' | 'external';
  category: 'internal' | 'system' | 'external';
  pending: boolean;
  installable: boolean;
  can_publish: boolean;
  source_space_id?: string;
  source_space?: string;
  author?: string;
  published: boolean;
  published_slug?: string;
  /** 清单声明的权限（人类可读），安装前给用户看 */
  permissions?: string[];
}

export interface PluginCatalogResponse {
  items: PluginCatalogItem[];
  is_admin: boolean;
}

/** 安装级配置：MCP 的服务地址与鉴权；凭证只回显名字 */
export interface PluginConfigField {
  key: string;
  label: string;
  type: 'text' | 'url' | 'list';
  required: boolean;
  placeholder?: string;
  help?: string;
}

export interface PluginConfigView {
  slug: string;
  kind: PluginKind;
  fields: PluginConfigField[];
  values: Record<string, string>;
}

/** plugin.json 的内容：元数据的唯一定义点 */
export interface PluginManifest {
  protocol: string;
  kind: PluginKind;
  slug: string;
  version?: string;
  name: string;
  description: string;
  capability?: string;
  input_schema?: Record<string, unknown>;
  capabilities?: Array<{ method: string; hosts?: string[]; verbs?: string[]; name?: string }>;
  limits?: { timeout_ms?: number; max_rpc?: number; max_result_bytes?: number };
  extensible?: boolean;
  derived_from?: { plugin_id: string; version: string; sha256: string };
  runtime?: { entry?: string; timeout_ms?: number; max_rpc?: number };
  /** 文件范围：读默认整个空间，写必须显式声明子目录 */
  fs?: { read?: string[]; write?: string[]; read_only?: boolean };
  transport?: { type: string; url: string };
  tools?: { allow: string[]; deny?: string[] };
  ui?: { entry: string };
  skill?: { entry?: string; assets?: string[] };
  data_sources?: Array<{
    id: string;
    plugin: string;
    tool?: string;
    mode?: 'read' | 'action';
    params?: Record<string, unknown>;
  }>;
}

/** 组件 DSL：小程序式声明式 UI，由宿主原生渲染，不含可执行代码 */
export type UiDSL = {
  dsl: 'wa-ui/1';
  height?: number;
  state?: Record<string, unknown>;
  data?: Record<string, { params?: Record<string, unknown>; auto?: boolean }>;
  view: UiNode;
};

export type UiNode = {
  type: string;
  when?: UiCond;
  action?: UiAction;
  [key: string]: unknown;
};

export type UiCond = {
  path: string;
  op: 'eq' | 'ne' | 'gt' | 'lt' | 'in' | 'exists' | 'truthy';
  value?: unknown;
};

export type UiAction =
  | { type: 'reload'; target: string; params?: Record<string, unknown> }
  | { type: 'invoke'; source: string; params?: Record<string, unknown>; confirm?: string }
  | { type: 'message'; text: string };

export interface PluginComponentView {
  slug: string;
  version: string;
  height: number;
  manifest: PluginManifest;
  dsl: UiDSL;
}

export interface PluginDetailResponse {
  item: PluginItem;
  manifest: PluginManifest;
}

export interface PluginDryRunResult {
  kind: PluginKind;
  output?: string;
  detail?: string;
  duration: string;
  risk: number;
  /** 搜索源试跑的诊断：请求、状态码、响应顶层键、命中与样例 */
  source?: {
    method?: string;
    url?: string;
    status?: number;
    top_keys?: string[];
    items_path?: string;
    hit: number;
    dropped: number;
    samples?: Array<{ title: string; url: string; snippet?: string }>;
    warnings?: string[];
  };
}

export interface PluginBaselineStatus {
  baseline_version: string;
  latest_version: string;
  update_available: boolean;
  local_modified: boolean;
  dismissed: boolean;
}

/** 会话里「MCP 应用密钥」卡片的取数结果：key 只在创建者本人这一支上出现 */
export interface MCPAppKeyResponse {
  app: string;
  /** created 本次生成 / owned 自己的 / taken 别人的 / unowned 无归属 / disabled 没配 KEK */
  status: 'created' | 'owned' | 'taken' | 'unowned' | 'disabled';
  key?: string;
  owner_name?: string;
  scope?: string;
  algorithm?: string;
  header?: string;
  ttl_seconds?: number;
  claims?: string[];
}

/**
 * 管理员视角的一条 MCP 应用密钥（系统空间页入口）。
 *
 * 列表只给指纹：一次拉全部应用，明文一起下发就等于把生产密钥写进浏览器缓存、
 * 代理日志与 HAR。明文要点「查看密钥」单独取（MCPAppKeyAdminResponse）。
 */
export interface MCPAppItem {
  app: string;
  owner_id?: string;
  owner_name?: string;
  owner_space_id?: string;
  created_at: string;
  fingerprint?: string;
  updated_at?: string;
  /** 凭证存储里有没有这把密钥：没有就只能看归属，取不到明文 */
  has_key: boolean;
  /** 早于归属流程生成的历史密钥：没有创建人记录，只能管理员兜底 */
  legacy: boolean;
}

export interface MCPAppsResponse {
  /** 凭证存储没启用时列表只有归属、没有密钥 */
  secrets_enabled: boolean;
  items: MCPAppItem[];
}

/** 按应用取回明文：一次性响应，不进列表、不落缓存 */
export interface MCPAppKeyAdminResponse {
  app: string;
  key: string;
}
