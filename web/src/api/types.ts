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
  /** true 表示该目录是 skill（目录下有隐藏的 .meta.json） */
  is_skill: boolean;
  /** true 表示系统空间的内置目录（skills / tools / mcps），不可改名删除 */
  builtin: boolean;
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
  status: string;
  last_message_at?: string | null;
  created_at: string;
  updated_at: string;
}

export type MessageRole = 'user' | 'assistant' | 'system';

/** 工具调用结局：与后端 tool.InvokeStatus 一致 */
export type ToolStatus = 'running' | 'ok' | 'denied' | 'approval' | 'error';

/** 一次工具调用的完整状态 */
export interface ToolCallRecord {
  id: string;
  name: string;
  args: string;
  result?: string;
  status: ToolStatus;
  duration_ms?: number;
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
  created_at: string;
}

export interface AgentInfo {
  ready: boolean;
  model: string;
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

/** 已发布资源：资源管理板块只展示名称、版本与描述，不提供原始 MCP / tool / DB 配置 */
export interface PublishedResource {
  id: string;
  kind: 'skill' | 'tool' | 'mcp' | 'component' | 'db';
  name: string;
  description: string;
  /** public 全部可见 / private 仅自己可见 */
  visibility: 'public' | 'private';
  /** enabled 可被 Agent 检索使用 / disabled 已停用 */
  status: 'enabled' | 'disabled';
  owner_id: string;
  owner_name: string;
  source_space_id?: string;
  source_space_name?: string;
  latest_version: string;
  version_count: number;
  created_at: string;
  updated_at: string;
}

/** 一个不可变的发布版本；改内容必须发新版本 */
export interface ResourceVersionInfo {
  version: string;
  notes: string;
  file_count: number;
  total_bytes: number;
  created_by: string;
  created_at: string;
}

export interface ResourcesResponse {
  resources: PublishedResource[];
}

export interface ResourceDetailResponse {
  resource: PublishedResource;
  versions: ResourceVersionInfo[];
}
