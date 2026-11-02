import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { api } from './client';
import type { ChatSession, MeResponse, SessionsResponse, Space, SpacesResponse } from './types';

export const queryKeys = {
  me: ['me'] as const,
  spaces: ['spaces'] as const,
  sessions: (spaceId: string) => ['sessions', spaceId] as const,
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

export function useSessions(spaceId: string | undefined, enabled: boolean) {
  return useQuery({
    queryKey: queryKeys.sessions(spaceId ?? ''),
    queryFn: () => api<SessionsResponse>(`/spaces/${spaceId}/sessions`).then((r) => r.sessions),
    enabled: enabled && Boolean(spaceId),
    staleTime: 10_000,
  });
}

export function useCreateSpace() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { name: string; parent_id?: string }) =>
      api<{ space: Space }>('/spaces', { method: 'POST', body: input }),
    onSuccess: () => qc.invalidateQueries({ queryKey: queryKeys.spaces }),
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
    onSuccess: () => qc.invalidateQueries({ queryKey: queryKeys.spaces }),
  });
}

export function useCreateSession() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { spaceId: string; title: string }) =>
      api<{ session: ChatSession }>(`/spaces/${input.spaceId}/sessions`, {
        method: 'POST',
        body: { title: input.title },
      }),
    onSuccess: (_data, vars) => qc.invalidateQueries({ queryKey: queryKeys.sessions(vars.spaceId) }),
  });
}
