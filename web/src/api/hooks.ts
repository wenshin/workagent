import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { api } from './client';
import type {
  AppendMessageResponse,
  ChatSession,
  MeResponse,
  MessagesResponse,
  SessionsResponse,
  Space,
  SpacesResponse,
} from './types';

export const queryKeys = {
  me: ['me'] as const,
  spaces: ['spaces'] as const,
  sessions: ['sessions'] as const,
  messages: (sessionId: string) => ['messages', sessionId] as const,
};

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
    mutationFn: (input: { name: string; parent_id?: string }) =>
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

export function useCreateSession() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { spaceId: string; title?: string }) =>
      api<{ session: ChatSession }>(`/spaces/${input.spaceId}/sessions`, {
        method: 'POST',
        body: { title: input.title ?? '' },
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
