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
  status: string;
  created_at: string;
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
