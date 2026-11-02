export type Perm = 'view' | 'read' | 'write' | 'manage';

export interface User {
  id: string;
  name: string;
  avatar: string;
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

/** 一次工具调用的完整状态 */
export interface ToolCallRecord {
  id: string;
  name: string;
  args: string;
  result?: string;
  status: 'running' | 'ok' | 'error';
  duration_ms?: number;
}

/** 消息里的结构化执行轨迹 */
export interface MessagePart {
  type: 'tool';
  id: string;
  name: string;
  args: string;
  result?: string;
  status: 'running' | 'ok' | 'error';
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
