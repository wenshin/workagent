import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { api } from './client';
import type {
  AgentInfo,
  AppendMessageResponse,
  ChatSession,
  DirsResponse,
  FileContentResponse,
  FilesResponse,
  MeResponse,
  MessagesResponse,
  ProviderResponse,
  SessionsResponse,
  SkillsResponse,
  Space,
  SpaceDir,
  SpacesResponse,
} from './types';

export const queryKeys = {
  me: ['me'] as const,
  provider: ['provider'] as const,
  spaces: ['spaces'] as const,
  sessions: ['sessions'] as const,
  dirs: ['dirs'] as const,
  files: ['files'] as const,
  skills: (spaceId: string) => ['skills', spaceId] as const,
  agent: ['agent'] as const,
  messages: (sessionId: string) => ['messages', sessionId] as const,
  file: (spaceId: string, path: string) => ['file', spaceId, path] as const,
};

/** 当前 IdP 信息：切换钉钉/企微/飞书只改后端配置，前端文案自动跟随 */
export function useProviderInfo() {
  return useQuery({
    queryKey: queryKeys.provider,
    queryFn: () => api<ProviderResponse>('/auth/provider'),
    retry: false,
    staleTime: 5 * 60_000,
  });
}

/** 当前模型服务是否可用，用于对话页提示 */
export function useAgentInfo() {
  return useQuery({
    queryKey: queryKeys.agent,
    queryFn: () => api<AgentInfo>('/agent/info'),
    staleTime: 60_000,
    retry: false,
  });
}

export function useMe() {
  return useQuery({
    queryKey: queryKeys.me,
    queryFn: () => api<MeResponse>('/auth/me'),
    retry: false,
    staleTime: 60_000,
  });
}

export function useSpaces() {
  return useQuery({
    queryKey: queryKeys.spaces,
    queryFn: () => api<SpacesResponse>('/spaces').then((r) => r.spaces),
    staleTime: 15_000,
  });
}

/** 当前用户可见的全部会话（左侧树用，一次取全） */
export function useSessions() {
  return useQuery({
    queryKey: queryKeys.sessions,
    queryFn: () => api<SessionsResponse>('/sessions').then((r) => r.sessions),
    staleTime: 5_000,
  });
}

export function useMessages(sessionId: string | undefined) {
  return useQuery({
    queryKey: queryKeys.messages(sessionId ?? ''),
    queryFn: () => api<MessagesResponse>(`/sessions/${sessionId}/messages`).then((r) => r.messages),
    enabled: Boolean(sessionId),
    staleTime: 0,
  });
}

export function useCreateSpace() {
  const qc = useQueryClient();
  return useMutation({
    // 空间只有一级，parent_id 已废弃
    mutationFn: (input: { name: string }) =>
      api<{ space: Space }>('/spaces', { method: 'POST', body: input }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.spaces });
      qc.invalidateQueries({ queryKey: queryKeys.sessions });
    },
  });
}

export function useRenameSpace() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { id: string; name: string }) =>
      api<{ space: Space }>(`/spaces/${input.id}`, { method: 'PATCH', body: { name: input.name } }),
    onSuccess: () => qc.invalidateQueries({ queryKey: queryKeys.spaces }),
  });
}

export function useDeleteSpace() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { id: string; confirm_name: string }) =>
      api<{ ok: boolean }>(`/spaces/${input.id}`, {
        method: 'DELETE',
        body: { confirm_name: input.confirm_name },
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.spaces });
      qc.invalidateQueries({ queryKey: queryKeys.sessions });
    },
  });
}

/** 用户可见空间下的全部真实目录（左侧树一次取全） */
export function useSpaceDirs() {
  return useQuery({
    queryKey: queryKeys.dirs,
    queryFn: () => api<DirsResponse>('/spaces/dirs').then((r) => r.dirs),
    staleTime: 15_000,
  });
}

/** 用户可见空间下的可预览文件（md/json 等），供左侧树展示 */
export function useSpaceFiles() {
  return useQuery({
    queryKey: queryKeys.files,
    queryFn: () => api<FilesResponse>('/spaces/files').then((r) => r.files),
    staleTime: 15_000,
  });
}

/** 单个文件正文：选中文件后才请求，避免拉起大文件 */
export function useFileContent(spaceId: string | undefined, path: string | undefined) {
  return useQuery({
    queryKey: queryKeys.file(spaceId ?? '', path ?? ''),
    queryFn: () =>
      api<FileContentResponse>(
        `/spaces/${spaceId}/file?path=${encodeURIComponent(path ?? '')}`,
      ).then((r) => r.file),
    enabled: Boolean(spaceId && path),
    staleTime: 10_000,
  });
}

export function useCreateDir() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { spaceId: string; name: string; parent?: string }) =>
      api<{ dir: SpaceDir }>(`/spaces/${input.spaceId}/dirs`, {
        method: 'POST',
        body: { name: input.name, parent: input.parent ?? '' },
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: queryKeys.dirs }),
  });
}

/** 新建技能：目录 + 隐藏的 .meta.json，底层仍是一次目录创建 */
export function useCreateSkill() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { spaceId: string; name: string; description?: string }) =>
      api<{ dir: SpaceDir }>(`/spaces/${input.spaceId}/skills`, {
        method: 'POST',
        body: { name: input.name, description: input.description ?? '' },
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: queryKeys.dirs }),
  });
}

export function useRenameDir() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { spaceId: string; path: string; nextName: string }) =>
      api<{ dir: SpaceDir }>(`/spaces/${input.spaceId}/dirs`, {
        method: 'PATCH',
        body: { path: input.path, name: input.nextName },
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.dirs });
      // 目录改名会同步会话的 dir，树上的会话位置会变
      qc.invalidateQueries({ queryKey: queryKeys.sessions });
    },
  });
}

export function useDeleteDir() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { spaceId: string; path: string }) =>
      api<{ ok: boolean }>(`/spaces/${input.spaceId}/dirs?path=${encodeURIComponent(input.path)}`, {
        method: 'DELETE',
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.dirs });
      // 目录内会话会移回空间根
      qc.invalidateQueries({ queryKey: queryKeys.sessions });
    },
  });
}

export function useCreateSession() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { spaceId: string; title?: string; dir?: string }) =>
      api<{ session: ChatSession }>(`/spaces/${input.spaceId}/sessions`, {
        method: 'POST',
        body: { title: input.title ?? '', dir: input.dir ?? '' },
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: queryKeys.sessions }),
  });
}

export function useRenameSession() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { id: string; title: string }) =>
      api<{ session: ChatSession }>(`/sessions/${input.id}`, {
        method: 'PATCH',
        body: { title: input.title },
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: queryKeys.sessions }),
  });
}

/** 当前空间 + 系统空间合并后的技能列表（同名当前空间优先） */
export function useSkills(spaceId?: string) {
  return useQuery({
    queryKey: queryKeys.skills(spaceId ?? ''),
    queryFn: () => api<SkillsResponse>(`/spaces/${spaceId}/skills`).then((r) => r.skills),
    enabled: Boolean(spaceId),
    staleTime: 10_000,
  });
}

export function useSetSessionSkill() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { id: string; path: string }) =>
      api<{ session: ChatSession }>(`/sessions/${input.id}/skill`, {
        method: 'PUT',
        body: { path: input.path },
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: queryKeys.sessions }),
  });
}

export function useDeleteSession() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api<{ ok: boolean }>(`/sessions/${id}`, { method: 'DELETE' }),
    onSuccess: () => qc.invalidateQueries({ queryKey: queryKeys.sessions }),
  });
}

export function useSendMessage() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { sessionId: string; content: string }) =>
      api<AppendMessageResponse>(`/sessions/${input.sessionId}/messages`, {
        method: 'POST',
        body: { content: input.content },
      }),
    onSuccess: (_data, vars) => {
      qc.invalidateQueries({ queryKey: queryKeys.messages(vars.sessionId) });
      // 首条消息会改写标题，需要刷新树
      qc.invalidateQueries({ queryKey: queryKeys.sessions });
    },
  });
}
