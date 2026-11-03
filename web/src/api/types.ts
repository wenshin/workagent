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
  type: 'personal' | 'shared';
  path: string;
  depth: number;
  perm: Perm;
  is_system_reserved: boolean;
  locked: boolean;
  created_at: string;
}

/**
 * 空间内的真实文件目录：只存在于文件系统，不落 space 表。
 * 没有默认会话、不能单独分享，权限沿用所属空间。
 */
export interface SpaceDir {
  space_id: string;
  name: string;
}

export interface ChatSession {
  id: string;
  space_id: string;
  title: string;
  /** true 表示标题还允许被首条消息改写 */
  title_auto: boolean;
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

export interface SessionsResponse {
  sessions: ChatSession[];
}

export interface MessagesResponse {
  messages: ChatMessage[];
}

export interface AppendMessageResponse {
  message: ChatMessage;
  session: ChatSession;
}
