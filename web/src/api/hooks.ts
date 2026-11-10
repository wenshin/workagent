import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import type { QueryClient } from '@tanstack/react-query';

import { api } from './client';
import type {
  AdminSettings,
  AgentInfo,
  AgentSummary,
  AppendMessageResponse,
  ChatSession,
  DirsResponse,
  FileContentResponse,
  FilesResponse,
  MeResponse,
  MessagesResponse,
  PluginBaselineStatus,
  PluginCatalogResponse,
  PluginConfigView,
  PluginDetailResponse,
  PluginDryRunResult,
  PluginKind,
  PluginsResponse,
  ProviderResponse,
  PublishedResource,
  ResourceDetailResponse,
  ResourcesResponse,
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
  agents: (spaceId: string) => ['agents', spaceId] as const,
  messages: (sessionId: string) => ['messages', sessionId] as const,
  file: (spaceId: string, path: string) => ['file', spaceId, path] as const,
  resources: ['resources'] as const,
  resource: (id: string) => ['resource', id] as const,
  adminSettings: ['admin', 'settings'] as const,
  plugins: (spaceId: string) => ['plugins', spaceId] as const,
  pluginInstalled: (spaceId: string) => ['plugin', 'installed', spaceId] as const,
  pluginConfig: (spaceId: string, slug: string) => ['plugin', 'config', spaceId, slug] as const,
  pluginCatalog: ['plugin', 'catalog'] as const,
};

/**
 * 让会话右上角的技能选择器失效。
 *
 * 列表由「当前空间启用的技能插件 + 系统空间安装」合并而成，任何改变这个集合的操作
 * （新建、删除、启停、安装、卸载）都要清理它，否则选择器会停在页面打开时的快照上。
 * 用前缀匹配：系统空间的变化对所有空间的选择器都有效。
 */
function invalidateSkillLists(qc: QueryClient) {
  qc.invalidateQueries({ queryKey: ['skills'] });
}

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
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.dirs });
      // 技能目录自带 SKILL.md，文件树里要立刻出现
      qc.invalidateQueries({ queryKey: queryKeys.files });
      invalidateSkillLists(qc);
    },
  });
}

/**
 * 新建插件草稿：六类插件共用一条接口，kind 决定后端生成的骨架文件。
 * 技能、工具、MCP、组件、智能体、搜索源。
 */
export function useCreateDraft() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { spaceId: string; kind: PluginKind; name: string; description?: string }) =>
      api<{ dir: SpaceDir }>(`/spaces/${input.spaceId}/drafts`, {
        method: 'POST',
        body: { kind: input.kind, name: input.name, description: input.description ?? '' },
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.dirs });
      // 草稿目录自带 plugin.json 与入口文件，文件树要立刻出现
      qc.invalidateQueries({ queryKey: queryKeys.files });
      invalidateSkillLists(qc);
    },
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
      // 目录下的文件路径整体变了，文件树不刷新会留在旧路径下
      qc.invalidateQueries({ queryKey: queryKeys.files });
      qc.invalidateQueries({ queryKey: ['file'] });
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
      // 目录里的文件是被磁盘递归删掉的，文件树必须一起失效，
      // 否则 15s 内左侧树还会显示已经不存在的文件。
      qc.invalidateQueries({ queryKey: queryKeys.files });
      qc.invalidateQueries({ queryKey: ['file'] });
      invalidateSkillLists(qc);
    },
  });
}

/** 空间可选的智能体：kind=agent 插件，系统空间安装的对所有空间可见 */
export function useAgents(spaceId?: string) {
  return useQuery({
    queryKey: queryKeys.agents(spaceId ?? ''),
    enabled: Boolean(spaceId),
    queryFn: () => api<{ agents: AgentSummary[] }>(`/spaces/${spaceId}/agents`),
  });
}

export function useCreateSession() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { spaceId: string; title?: string; dir?: string; agentId?: string }) =>
      api<{ session: ChatSession }>(`/spaces/${input.spaceId}/sessions`, {
        method: 'POST',
        body: {
          title: input.title ?? '',
          dir: input.dir ?? '',
          agent_id: input.agentId ?? '',
        },
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

/* ---- 资源管理：发布件与工作空间分离存储，只有用户范围（public / private） ---- */

export function useResources() {
  return useQuery({
    queryKey: queryKeys.resources,
    queryFn: () => api<ResourcesResponse>('/resources').then((r) => r.resources),
    staleTime: 10_000,
  });
}

/** 版本明细按需加载：列表不返回历史版本 */
export function useResourceDetail(id?: string) {
  return useQuery({
    queryKey: queryKeys.resource(id ?? ''),
    queryFn: () => api<ResourceDetailResponse>(`/resources/${id}`),
    enabled: Boolean(id),
  });
}

export function usePublishResource() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: {
      spaceId: string;
      path: string;
      version: string;
      notes?: string;
      visibility: 'public' | 'private';
    }) =>
      api<{ resource: PublishedResource }>('/resources', {
        method: 'POST',
        body: {
          space_id: input.spaceId,
          path: input.path,
          version: input.version,
          notes: input.notes ?? '',
          visibility: input.visibility,
        },
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: queryKeys.resources }),
  });
}

export function useSetResourceStatus() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { id: string; status: 'enabled' | 'disabled' }) =>
      api<{ ok: boolean }>(`/resources/${input.id}`, { method: 'PATCH', body: { status: input.status } }),
    onSuccess: (_data, vars) => {
      qc.invalidateQueries({ queryKey: queryKeys.resources });
      qc.invalidateQueries({ queryKey: queryKeys.resource(vars.id) });
    },
  });
}

export function useDeleteResource() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api<{ ok: boolean }>(`/resources/${id}`, { method: 'DELETE' }),
    onSuccess: () => qc.invalidateQueries({ queryKey: queryKeys.resources }),
  });
}

export function useDeleteResourceVersion() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { id: string; version: string }) =>
      api<{ ok: boolean }>(`/resources/${input.id}/versions/${encodeURIComponent(input.version)}`, {
        method: 'DELETE',
      }),
    onSuccess: (_data, vars) => {
      qc.invalidateQueries({ queryKey: queryKeys.resources });
      qc.invalidateQueries({ queryKey: queryKeys.resource(vars.id) });
    },
  });
}

/** 平台配置：只有管理员能拿到；非管理员会 403，用 enabled 控制是否发起请求 */
export function useAdminSettings(enabled: boolean) {
  return useQuery({
    queryKey: queryKeys.adminSettings,
    queryFn: () => api<AdminSettings>('/admin/settings'),
    enabled,
    // 指纹变化只在保存后发生，避免反复拉取
    staleTime: 30_000,
  });
}

/** 写入凭证：只提交明文，服务端只回指纹 */
export function useSetSecret() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { scope: string; name: string; value: string }) =>
      api<{ scope: string; name: string; fingerprint: string }>(
        `/admin/secrets/${encodeURIComponent(input.scope)}/${encodeURIComponent(input.name)}`,
        { method: 'PUT', body: { value: input.value } },
      ),
    onSuccess: () => qc.invalidateQueries({ queryKey: queryKeys.adminSettings }),
  });
}

export function useDeleteSecret() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { scope: string; name: string }) =>
      api<{ ok: boolean }>(
        `/admin/secrets/${encodeURIComponent(input.scope)}/${encodeURIComponent(input.name)}`,
        { method: 'DELETE' },
      ),
    onSuccess: () => qc.invalidateQueries({ queryKey: queryKeys.adminSettings }),
  });
}

/* ---- 插件：开发 → 发布 → 安装 → 运行 → 卸载 ---- */

/** 空间内可见的插件（已安装 + 草稿），带基线升级提示 */
export function usePlugins(spaceId?: string) {
  return useQuery({
    queryKey: queryKeys.plugins(spaceId ?? ''),
    queryFn: () => api<PluginsResponse>(`/spaces/${spaceId}/plugins`).then((r) => r.items),
    enabled: Boolean(spaceId),
    staleTime: 10_000,
  });
}

/** 资源管理：全部已发布插件（只读目录），用于安装到空间 */
export function usePluginConfig(spaceId?: string, slug?: string, enabled = true) {
  return useQuery({
    queryKey: queryKeys.pluginConfig(spaceId ?? '', slug ?? ''),
    queryFn: () =>
      api<PluginConfigView>(`/spaces/${spaceId}/plugins/${slug}/config`),
    enabled: Boolean(spaceId) && Boolean(slug) && enabled,
  });
}

export function useUpdatePluginConfig() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: {
      spaceId: string;
      slug: string;
      values: Record<string, string>;
    }) =>
      api<PluginConfigView>(`/spaces/${input.spaceId}/plugins/${input.slug}/config`, {
        method: 'PUT',
        body: { values: input.values },
      }),
    onSuccess: (_d, vars) => {
      qc.invalidateQueries({ queryKey: queryKeys.pluginConfig(vars.spaceId, vars.slug) });
      qc.invalidateQueries({ queryKey: queryKeys.plugins(vars.spaceId) });
      qc.invalidateQueries({ queryKey: queryKeys.pluginInstalled(vars.spaceId) });
    },
  });
}

export function usePluginCatalog() {
  return useQuery({
    queryKey: queryKeys.pluginCatalog,
    queryFn: () => api<PluginCatalogResponse>('/plugins'),
    staleTime: 10_000,
  });
}

export function usePluginDetail(spaceId?: string, slug?: string) {
  return useQuery({
    queryKey: ['plugin', spaceId ?? '', slug ?? ''],
    queryFn: () => api<PluginDetailResponse>(`/spaces/${spaceId}/plugins/${slug}`),
    enabled: Boolean(spaceId && slug),
    staleTime: 5_000,
  });
}

/** 草稿试运行：不产生副作用，只回放样例输入 */
export function useDryRunPlugin() {
  return useMutation({
    mutationFn: (input: { spaceId: string; slug: string; input?: unknown }) =>
      api<PluginDryRunResult>(`/spaces/${input.spaceId}/plugins/${input.slug}/dry-run`, {
        method: 'POST',
        body: { input: input.input ?? {} },
      }),
  });
}

/** 发布：草稿 → 不可变版本快照 */
export function usePublishPlugin() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { spaceId: string; slug: string; version?: string; notes?: string }) =>
      api<{ version: string; sha256: string; risk: number }>(
        `/spaces/${input.spaceId}/plugins/${input.slug}/publish`,
        { method: 'POST', body: { version: input.version ?? '', notes: input.notes ?? '' } },
      ),
    onSuccess: (_d, vars) => {
      qc.invalidateQueries({ queryKey: queryKeys.plugins(vars.spaceId) });
      qc.invalidateQueries({ queryKey: queryKeys.pluginCatalog });
    },
  });
}

/** 安装：只接受已发布版本，安装即授权能力清单 */
export function useInstallPlugin() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { spaceId: string; slug: string; version: string }) =>
      api<{ version: string }>(`/spaces/${input.spaceId}/plugins/${input.slug}/install`, {
        method: 'POST',
        body: { version: input.version },
      }),
    onSuccess: (_d, vars) => {
      qc.invalidateQueries({ queryKey: queryKeys.plugins(vars.spaceId) });
      qc.invalidateQueries({ queryKey: queryKeys.pluginCatalog });
      invalidateSkillLists(qc);
    },
  });
}

export function useUninstallPlugin() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { spaceId: string; slug: string }) =>
      api<null>(`/spaces/${input.spaceId}/plugins/${input.slug}/uninstall`, { method: 'POST' }),
    onSuccess: (_d, vars) => {
      qc.invalidateQueries({ queryKey: queryKeys.plugins(vars.spaceId) });
      qc.invalidateQueries({ queryKey: queryKeys.pluginCatalog });
      invalidateSkillLists(qc);
    },
  });
}

export function useTogglePlugin() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { spaceId: string; slug: string; enabled: boolean }) =>
      api<null>(
        `/spaces/${input.spaceId}/plugins/${input.slug}/${input.enabled ? 'enable' : 'disable'}`,
        { method: 'POST' },
      ),
    onSuccess: (_d, vars) => {
      qc.invalidateQueries({ queryKey: queryKeys.plugins(vars.spaceId) });
      qc.invalidateQueries({ queryKey: queryKeys.pluginCatalog });
      invalidateSkillLists(qc);
    },
  });
}

export function usePluginBaseline(spaceId?: string, slug?: string, enabled = true) {
  return useQuery({
    queryKey: ['plugin', 'baseline', spaceId ?? '', slug ?? ''],
    queryFn: () => api<PluginBaselineStatus>(`/spaces/${spaceId}/plugins/${slug}/baseline`),
    enabled: Boolean(enabled && spaceId && slug),
    staleTime: 10_000,
  });
}

/** 用上游最新版覆盖本地副本；覆盖前平台会自动备份 */
export function useResetPluginBaseline() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { spaceId: string; slug: string }) =>
      api<null>(`/spaces/${input.spaceId}/plugins/${input.slug}/baseline/reset`, { method: 'POST' }),
    onSuccess: (_d, vars) => {
      qc.invalidateQueries({ queryKey: queryKeys.plugins(vars.spaceId) });
      qc.invalidateQueries({ queryKey: queryKeys.dirs });
    },
  });
}

export function useDismissPluginUpdate() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { spaceId: string; slug: string; version: string }) =>
      api<null>(`/spaces/${input.spaceId}/plugins/${input.slug}/baseline/dismiss`, {
        method: 'POST',
        body: { version: input.version },
      }),
    onSuccess: (_d, vars) => {
      qc.invalidateQueries({ queryKey: queryKeys.plugins(vars.spaceId) });
      qc.invalidateQueries({ queryKey: queryKeys.pluginCatalog });
    },
  });
}
