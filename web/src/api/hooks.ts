import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import type { QueryClient } from '@tanstack/react-query';

import { api, deleteAgentIcon, uploadAgentIcon, uploadSpaceFiles } from './client';
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
  MCPAppKeyAdminResponse,
  MCPAppKeyResponse,
  MCPAppsResponse,
  PluginBaselineStatus,
  PluginCatalogResponse,
  PluginConfigView,
  PluginDetailResponse,
  PluginDryRunResult,
  PluginKind,
  PluginsResponse,
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
  agents: (spaceId: string) => ['agents', spaceId] as const,
  messages: (sessionId: string) => ['messages', sessionId] as const,
  file: (spaceId: string, path: string) => ['file', spaceId, path] as const,
  adminSettings: ['admin', 'settings'] as const,
  mcpApps: ['admin', 'mcp-apps'] as const,
  plugins: (spaceId: string) => ['plugins', spaceId] as const,
  pluginInstalled: (spaceId: string) => ['plugin', 'installed', spaceId] as const,
  pluginConfig: (spaceId: string, slug: string) => ['plugin', 'config', spaceId, slug] as const,
  pluginCatalog: ['plugin', 'catalog'] as const,
};

/**
 * Agent 写过空间文件后要作废的缓存。
 *
 * 目录树有两份数据：`files` 只是可预览的文件，`dirs` 才是目录本身——新建的
 * `plugins/<slug>/` 只出现在 `dirs` 里。只刷 files 就会出现「新插件目录要刷新
 * 页面才看得到」，所以这里把这几类键绑在一起，避免各处只刷一半。
 */
export function invalidateAfterWrite(qc: QueryClient) {
  const keys = [queryKeys.files, queryKeys.dirs, ['file'], ['plugins'], ['plugin']] as const;
  for (const key of keys) void qc.invalidateQueries({ queryKey: key });
}

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

/**
 * 让「新建会话」弹窗里的智能体清单失效。
 *
 * 启停 / 安装 / 卸载智能体都会改变这份清单，不清理的话弹窗要刷新整个页面才看得到。
 * 用前缀匹配：系统空间里的启停对所有空间的清单都有效。
 */
function invalidateAgentLists(qc: QueryClient) {
  qc.invalidateQueries({ queryKey: ['agents'] });
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

/** 用户可见空间下的全部文件（平台保留的隐藏项除外），供左侧树展示；previewable 决定能否读正文 */
export function useSpaceFiles() {
  return useQuery({
    queryKey: queryKeys.files,
    queryFn: () => api<FilesResponse>('/spaces/files').then((r) => r.files),
    staleTime: 15_000,
  });
}

/**
 * 单个文件正文：选中文件后才请求，避免拉起大文件。
 * 非文本类型（图片、音视频、压缩包等）不请求：正文端点只认文本，请求只会回 400。
 */
export function useFileContent(
  spaceId: string | undefined,
  path: string | undefined,
  readable = true,
) {
  return useQuery({
    queryKey: queryKeys.file(spaceId ?? '', path ?? ''),
    queryFn: () =>
      api<FileContentResponse>(
        `/spaces/${spaceId}/file?path=${encodeURIComponent(path ?? '')}`,
      ).then((r) => r.file),
    enabled: readable && Boolean(spaceId && path),
    staleTime: 10_000,
  });
}

/**
 * 上传文件到空间：成功后刷新文件树，并把落盘结果返回给调用方。
 * dir 为空表示空间根；同名冲突由后端改名，前端以返回值为准。
 */
export function useUploadFiles() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { spaceId: string; files: File[]; dir?: string }) =>
      uploadSpaceFiles(input.spaceId, input.files, input.dir),
    onSuccess: () => invalidateAfterWrite(qc),
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
    onSuccess: () => invalidateAfterWrite(qc),
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
      invalidateAfterWrite(qc);
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
      invalidateAfterWrite(qc);
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
      // 目录改名会同步会话的 dir，树上的会话位置会变
      qc.invalidateQueries({ queryKey: queryKeys.sessions });
      // 目录下的文件路径整体变了，文件树不刷新会留在旧路径下
      invalidateAfterWrite(qc);
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
      // 目录内会话会移回空间根
      qc.invalidateQueries({ queryKey: queryKeys.sessions });
      // 目录里的文件是被磁盘递归删掉的，文件树必须一起失效，
      // 否则 15s 内左侧树还会显示已经不存在的文件。
      invalidateAfterWrite(qc);
      invalidateSkillLists(qc);
    },
  });
}

/**
 * 删除单个文件。已安装插件的子树与系统空间内置目录由后端拒绝，
 * 前端在这些位置不渲染入口。
 */
export function useDeleteFile() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { spaceId: string; path: string }) =>
      api<{ ok: boolean }>(`/spaces/${input.spaceId}/file?path=${encodeURIComponent(input.path)}`, {
        method: 'DELETE',
      }),
    onSuccess: () => {
      invalidateAfterWrite(qc);
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

/**
 * 改智能体头像。头像属于会话展示信息，改完要连会话列表一起作废：
 * 树里的图标、选择器里的头像都从这两处来。
 */
export function useSetAgentIcon() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { spaceId: string; slug: string; blob: Blob }) =>
      uploadAgentIcon(input.spaceId, input.slug, input.blob),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['agents'] });
      void qc.invalidateQueries({ queryKey: queryKeys.sessions });
      void qc.invalidateQueries({ queryKey: ['plugins'] });
      void qc.invalidateQueries({ queryKey: ['plugin'] });
    },
  });
}

export function useClearAgentIcon() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { spaceId: string; slug: string }) =>
      deleteAgentIcon(input.spaceId, input.slug),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['agents'] });
      void qc.invalidateQueries({ queryKey: queryKeys.sessions });
      void qc.invalidateQueries({ queryKey: ['plugins'] });
      void qc.invalidateQueries({ queryKey: ['plugin'] });
    },
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
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.adminSettings });
      // 插件卡片上的「未启用（缺凭证）」要跟着消失，否则得手动刷新
      qc.invalidateQueries({ queryKey: ['plugins'] });
    },
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
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.adminSettings });
      qc.invalidateQueries({ queryKey: ['plugins'] });
    },
  });
}

/**
 * 取 MCP 应用密钥：一个应用名一把密钥，只有创建者能拿到明文。
 *
 * 走 POST 而不是 GET：应用名不存在时会顺手生成一把——这正是「第一次创建」
 * 的语义，也让卡片只需要一个按钮。明文只回到浏览器，不进对话与模型上下文。
 */
export function useMCPAppKey() {
  return useMutation({
    mutationFn: (input: { spaceId: string; app: string }) =>
      api<MCPAppKeyResponse>(
        `/spaces/${input.spaceId}/mcp-apps/${encodeURIComponent(input.app)}/key`,
        { method: 'POST' },
      ),
  });
}

/**
 * 全部 MCP 应用与密钥指纹（系统空间页入口）。
 *
 * 只有系统管理员能拿到，非管理员 403，所以用 enabled 控制是否发起请求。
 * 列表按需刷新即可：密钥只在「重新取回」时变，不需要轮询。
 */
export function useMCPApps(enabled: boolean) {
  return useQuery({
    queryKey: queryKeys.mcpApps,
    queryFn: () => api<MCPAppsResponse>('/admin/mcp-apps'),
    enabled,
    staleTime: 15_000,
  });
}

/**
 * 按应用取回签名密钥明文。
 *
 * 与 useMCPAppKey 的区别只在「谁能看」：那边按创建者收口（POST，缺密钥会顺手
 * 生成一把），这里只读不建——管理员点开列表不该改变任何应用的密钥。
 */
export function useRevealMCPAppKey() {
  return useMutation({
    mutationFn: (app: string) =>
      api<MCPAppKeyAdminResponse>(`/admin/mcp-apps/${encodeURIComponent(app)}/key`),
  });
}

/* ---- 插件：开发 → 发布 → 安装 → 运行 → 卸载 ---- */

/**
 * 空间内可见的插件（已安装 + 草稿），带基线升级提示。
 *
 * 返回整包而不是只取 items：卡片要用 is_admin 决定「配置密钥」还是「转给管理员」。
 */
export function usePlugins(spaceId?: string) {
  return useQuery({
    queryKey: queryKeys.plugins(spaceId ?? ''),
    queryFn: () => api<PluginsResponse>(`/spaces/${spaceId}/plugins`),
    enabled: Boolean(spaceId),
    staleTime: 10_000,
  });
}

/** 安装级配置：MCP 地址、鉴权 Header、凭证名，保存后整空间重载 */
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
    mutationFn: (input: {
      spaceId: string;
      slug: string;
      version?: string;
      notes?: string;
      /** MCP jwt 插件：应用名不属于发布者时必填，等于出示创建者的授权 */
      appKey?: string;
    }) =>
      api<{ version: string; sha256: string; risk: number }>(
        `/spaces/${input.spaceId}/plugins/${input.slug}/publish`,
        {
          method: 'POST',
          body: {
            version: input.version ?? '',
            notes: input.notes ?? '',
            app_key: input.appKey ?? '',
          },
        },
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
    onSuccess: () => {
      // 安装/卸载/启停都会动 plugins/<slug>/ 副本或树上的状态标记，树一起刷
      invalidateAfterWrite(qc);
      invalidateSkillLists(qc);
      invalidateAgentLists(qc);
    },
  });
}

export function useUninstallPlugin() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { spaceId: string; slug: string }) =>
      api<null>(`/spaces/${input.spaceId}/plugins/${input.slug}/uninstall`, { method: 'POST' }),
    onSuccess: () => {
      // 安装/卸载/启停都会动 plugins/<slug>/ 副本或树上的状态标记，树一起刷
      invalidateAfterWrite(qc);
      invalidateSkillLists(qc);
      invalidateAgentLists(qc);
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
    // 启停只是本地状态翻转，界面不该等一次往返：先改缓存，失败再回滚。
    // 服务端返回 204 后由 onSettled 重拉一次，确保与后端一致（权限、依赖校验可能拒绝）。
    onMutate: async (input) => {
      const key = queryKeys.plugins(input.spaceId);
      await qc.cancelQueries({ queryKey: key });
      const prev = qc.getQueryData<PluginsResponse>(key);
      if (prev) {
        qc.setQueryData<PluginsResponse>(key, {
          ...prev,
          items: prev.items.map((it) =>
            it.slug === input.slug ? { ...it, enabled: input.enabled } : it,
          ),
        });
      }
      return { prev };
    },
    onError: (_err, input, ctx) => {
      if (ctx?.prev) qc.setQueryData(queryKeys.plugins(input.spaceId), ctx.prev);
    },
    onSettled: () => {
      // 安装/卸载/启停都会动 plugins/<slug>/ 副本或树上的状态标记，树一起刷
      invalidateAfterWrite(qc);
      invalidateSkillLists(qc);
      invalidateAgentLists(qc);
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
    onSuccess: () => {
      // 覆盖会重写本地副本里的文件，树与文件正文都要跟着刷新
      invalidateAfterWrite(qc);
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
