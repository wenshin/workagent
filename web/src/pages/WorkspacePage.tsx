import { IconAlertCircle, IconExit, IconPlus } from '@douyinfe/semi-icons';
import { Avatar, Button, Layout, Modal, Spin, Toast, Tooltip } from '@douyinfe/semi-ui';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useCallback, useEffect, useMemo, useRef, useState, useSyncExternalStore } from 'react';
import { useLocation, useNavigate, useSearchParams } from 'react-router-dom';

import { api, getActiveRun, getResumable } from '../api/client';
import {
  invalidateAfterWrite,
  queryKeys,
  useAgentInfo,
  useCreateSession,
  useDeleteSession,
  useMe,
  useMessages,
  usePluginCatalog,
  useSetSessionSkill,
  useSessions,
  useSkills,
  useSpaceDirs,
  useSpaceFiles,
  useSpaces,
  useUploadFiles,
} from '../api/hooks';
import {
  attachLiveRun,
  getLiveRun,
  isLiveRunning,
  isStopping,
  resumeLiveRun,
  startLiveRun,
  stopLiveRun,
  subscribeLiveRun,
} from '../api/liveRun';
import type { ChatSession, PluginCategory, Space, SpaceDir, SpaceFile } from '../api/types';
import ChatPanel, { splitReferences, type StreamView } from '../components/ChatPanel';
import FilePanel from '../components/FilePanel';
import MCPAppsDialog from '../components/MCPAppsDialog';
import NewSessionDialog, { type NewSessionState } from '../components/NewSessionDialog';
import PluginCatalogPanel, { PLUGIN_CATEGORY_LABEL } from '../components/PluginCatalogPanel';
import SecretSettingsDialog from '../components/SecretSettingsDialog';
import SpaceDialog, { type DialogState } from '../components/SpaceDialog';
import SpacePanel from '../components/SpacePanel';
import SpaceTree, { FILE_KEY_PREFIX, SESSION_KEY_PREFIX } from '../components/SpaceTree';
import TaskStrip from '../components/TaskStrip';

export default function WorkspacePage() {
  const location = useLocation();
  const navigate = useNavigate();
  // 一级入口：工作空间 / 插件市场。已装插件属于空间，入口在空间页里。
  const isPlugins = location.pathname.startsWith('/plugins');
  // 市场分类由路径决定，左侧二级菜单只是同一份状态的入口
  const pluginCategory: PluginCategory = useMemo(() => {
    const seg = location.pathname.split('/')[3];
    return seg === 'internal' || seg === 'pending' || seg === 'system' || seg === 'external'
      ? seg
      : 'all';
  }, [location.pathname]);
  const { data: catalog } = usePluginCatalog();
  const { data: me } = useMe();
  const { data: spaces = [], isLoading: spacesLoading, isFetched: spacesFetched } = useSpaces();
  const { data: sessions = [], isLoading: sessionsLoading, isFetched: sessionsFetched } = useSessions();
  const { data: dirs = [], isFetched: dirsFetched } = useSpaceDirs();
  const { data: files = [], isFetched: filesFetched } = useSpaceFiles();
  // 树是四份数据拼出来的：齐了再定默认展开，否则后到的目录会被当成新增节点全部展开
  const treeReady = spacesFetched && sessionsFetched && dirsFetched && filesFetched;
  const [searchParams, setSearchParams] = useSearchParams();
  const [dialog, setDialog] = useState<DialogState>({ kind: 'none' });
  const [pendingDelete, setPendingDelete] = useState<ChatSession | null>(null);
  const [platformOpen, setPlatformOpen] = useState(false);
  const [mcpAppsOpen, setMcpAppsOpen] = useState(false);

  const queryClient = useQueryClient();
  // 流式状态放在模块级单例里：路由切换时本组件会被卸载，
  // 状态若跟着组件走，「正在生成」的这段内容就一起没了
  const liveRun = useSyncExternalStore(subscribeLiveRun, getLiveRun);

  const createSession = useCreateSession();
  // 新建会话统一走弹窗：树右键与空间面板都要能选智能体
  const [newSession, setNewSession] = useState<NewSessionState | null>(null);
  const deleteSession = useDeleteSession();
  const setSessionSkill = useSetSessionSkill();
  const uploadFiles = useUploadFiles();
  const { data: agentInfo } = useAgentInfo();

  const personalId = useMemo(
    () => spaces.find((s) => s.type === 'personal' && !s.parent_id)?.id ?? '',
    [spaces],
  );

  const urlSessionId = searchParams.get('session') ?? '';
  const urlSpaceId = searchParams.get('space') ?? '';

  const selectedSession = sessions.find((s) => s.id === urlSessionId);
  const selectedSpaceId = selectedSession
    ? selectedSession.space_id
    : spaces.some((s) => s.id === urlSpaceId)
      ? urlSpaceId
      : personalId;
  const selectedSpace = spaces.find((s) => s.id === selectedSpaceId);

  // 文件选中用 ?file=<path> 表达（与 session 互斥），右侧据此切换到内容面板
  const urlFilePath = searchParams.get('file') ?? '';
  const selectedFile = useMemo(
    () =>
      urlFilePath
        ? files.find((f) => f.space_id === selectedSpaceId && f.path === urlFilePath)
        : undefined,
    [files, selectedSpaceId, urlFilePath],
  );

  

  // 进入工作台默认打开个人空间里最近使用的会话
  const defaultSessionId = useMemo(() => {
    const personalSessions = sessions.filter((s) => s.space_id === personalId);
    return (personalSessions[0] ?? sessions[0])?.id ?? '';
  }, [sessions, personalId]);

  useEffect(() => {
    if (isPlugins) return;
    if (urlSessionId || urlSpaceId || spaces.length === 0) return;
    if (defaultSessionId) setSearchParams({ session: defaultSessionId }, { replace: true });
  }, [isPlugins, urlSessionId, urlSpaceId, defaultSessionId, spaces.length, setSearchParams]);

  // 文件或它所在的目录被删掉后，URL 里还留着 ?file=<path>，内容面板会一直停在旧路径。
  // 等文件列表至少成功拉取过一次再判断，避免首屏还没加载完就误清。
  useEffect(() => {
    if (!filesFetched || !urlFilePath) return;
    if (files.some((f) => f.path === urlFilePath)) return;
    const next = new URLSearchParams(searchParams);
    next.delete('file');
    if (!next.get('space') && selectedSpaceId) next.set('space', selectedSpaceId);
    setSearchParams(next, { replace: true });
  }, [filesFetched, files, urlFilePath, searchParams, selectedSpaceId, setSearchParams]);

  const { data: messages = [], isLoading: messagesLoading } = useMessages(selectedSession?.id);

  // 只认当前会话的轮次：别的会话在后台跑时，这里应当显示历史消息而不是它的草稿
  const stream: StreamView | null =
    liveRun && liveRun.sessionId === selectedSession?.id ? liveRun : null;
  // 用户消息在 run 开始时就已落库：列表里已有这条时不再渲染草稿气泡，
  // 否则「切走再回来」会看到同一句话出现两遍
  const lastMessage = messages[messages.length - 1];
  // 落库后的用户消息在正文后追加了引用清单，比较前先切掉，否则占位气泡会重复出现
  const pendingUser =
    liveRun &&
    stream &&
    !(lastMessage?.role === 'user' && splitReferences(lastMessage.content).text === liveRun.pendingUser)
      ? liveRun.pendingUser
      : null;
  // 技能列表随会话所属空间变化：当前空间 + 系统空间合并，同名当前空间优先
  const { data: skills = [], refetch: refetchSkills } = useSkills(selectedSession?.space_id);

  const spaceSessions = useMemo(
    () => sessions.filter((s) => s.space_id === selectedSpaceId),
    [sessions, selectedSpaceId],
  );

  const openSession = (id: string) => setSearchParams({ session: id });
  const openSpace = (id: string) => setSearchParams({ space: id });
  const openFile = useCallback(
    (space: Space, file: SpaceFile) => setSearchParams({ space: space.id, file: file.path }),
    [setSearchParams],
  );

  const addSession = useCallback(
    async (spaceId: string, dir?: string, agentId?: string, title?: string) => {
      try {
        const res = await createSession.mutateAsync({ spaceId, dir, agentId, title });
        setSearchParams({ session: res.session.id });
        return true;
      } catch (e) {
        Toast.error(e instanceof Error ? e.message : '新建会话失败');
        return false;
      }
    },
    [createSession, setSearchParams],
  );

  const confirmDeleteSession = async () => {
    if (!pendingDelete) return;
    const target = pendingDelete;
    try {
      await deleteSession.mutateAsync(target.id);
      Toast.success('会话已删除');
      setPendingDelete(null);
      // 删掉的是当前会话时，切到同空间下一个，没有则回到空间视图
      if (urlSessionId === target.id) {
        const rest = sessions.filter((s) => s.id !== target.id && s.space_id === target.space_id);
        if (rest[0]) setSearchParams({ session: rest[0].id });
        else setSearchParams({ space: target.space_id });
      }
    } catch (e) {
      Toast.error(e instanceof Error ? e.message : '删除失败，请重试');
    }
  };

  // 切会话或离开页面都不主动中断：run 由 liveRun 单例持有，工具调用会在后台跑完并落库。
  // 只有两种中断：用户点停止，或在另一个会话里发消息（此时才接管）。
  const send = async (content: string, references: string[] = []) => {
    if (!selectedSession) return;
    const sessionId = selectedSession.id;
    // 同一会话还在生成：拦下来，别把这次输入吞掉
    if (isLiveRunning(sessionId)) throw new Error('这轮回答还在生成，请先停止或等它结束');
    // 别的会话在跑：startLiveRun 会接管（停掉旧的），提醒一句免得用户以为回答凭空消失
    if (isLiveRunning()) Toast.info('已停止另一个会话的生成');
    await startLiveRun({
      sessionId,
      content,
      references,
      onError: (message) => Toast.error(message),
      // 回调可能在本组件卸载后才跑，所以只用全局 queryClient，不碰组件状态
      onSettled: (info) => {
        void queryClient.invalidateQueries({ queryKey: queryKeys.messages(sessionId) });
        void queryClient.invalidateQueries({ queryKey: queryKeys.sessions });
        void queryClient.invalidateQueries({ queryKey: ['resumable', sessionId] });
        if (info.wroteFiles) invalidateAfterWrite(queryClient);
      },
    });
  };

  // 带上会话 id：服务端按会话定位这一轮，前端不必等 run.started 拿到 run_id
  const stopGenerating = () => stopLiveRun(selectedSession?.id);

  // 断点续跑入口：只在没有轮次在跑时才探测，一轮结束后重新问一次
  // （断点可能刚被这一轮覆盖或被用户停掉清掉，缓存留着就是错的）
  const resumeKey = ['resumable', selectedSession?.id ?? ''] as const;
  const { data: resumeInfo } = useQuery({
    queryKey: resumeKey,
    queryFn: () => getResumable(selectedSession!.id),
    enabled: Boolean(selectedSession) && !stream,
    staleTime: 0,
    retry: false,
  });

  // 探测「有没有断点」时，服务端会顺手结算中断轮残留的占位消息（把 run_step 里的
  // 工具轨迹写回消息）。所以探到断点后再拉一次消息，用户切页回来就能看到中断前
  // 做过什么，而不是只剩一条问题、等下一次别的刷新才补上。
  const resumeSessionId = selectedSession?.id ?? '';
  useEffect(() => {
    if (!resumeInfo?.resumable || !resumeSessionId) return;
    void queryClient.invalidateQueries({ queryKey: queryKeys.messages(resumeSessionId) });
  }, [resumeInfo?.resumable, resumeSessionId, queryClient]);

  const resume = async () => {
    if (!selectedSession) return;
    const sessionId = selectedSession.id;
    if (isLiveRunning(sessionId)) throw new Error('这轮回答还在生成，请先停止或等它结束');
    if (isLiveRunning()) Toast.info('已停止另一个会话的生成');
    await resumeLiveRun({
      sessionId,
      onError: (message) => Toast.error(message),
      onSettled: (info) => {
        void queryClient.invalidateQueries({ queryKey: queryKeys.messages(sessionId) });
        void queryClient.invalidateQueries({ queryKey: queryKeys.sessions });
        void queryClient.invalidateQueries({ queryKey: ['resumable', sessionId] });
        if (info.wroteFiles) invalidateAfterWrite(queryClient);
      },
    });
  };

  // 刷新/切页回来：先问服务端「这个会话还有没有在跑」，有就接着往下看。
  // run 已经和请求解耦（§15.12.1），所以断开只是解绑流，内容仍在内存缓冲里。
  const attachedRef = useRef<Set<string>>(new Set());
  useEffect(() => {
    const session = selectedSession;
    if (!session) return;
    // isStopping：用户刚点过停止，服务端还在收尾。这时候 active-run 依然回答「在跑」，
    // 挂上去只会把刚停下的气泡又拉回来（得点第二次停止）
    if (isLiveRunning(session.id) || isStopping(session.id) || attachedRef.current.has(session.id)) {
      return;
    }

    let cancelled = false;
    // 只有真的发起续流才登记：StrictMode 会把 effect 跑两遍（挂载 → 清理 → 再挂载），
    // 提前登记会让第二遍被 attachedRef 挡住，这一轮就再也续不上了。
    let attached = false;
    void (async () => {
      try {
        const res = await getActiveRun(session.id);
        if (cancelled || !res.active || !res.run) return;
        // 请求在飞的时候用户点了停止：这一轮的「在跑」结论已经过期，不能再挂
        if (isLiveRunning(session.id) || isStopping(session.id)) return;
        attached = true;
        attachedRef.current.add(session.id);
        await attachLiveRun({
          sessionId: session.id,
          runId: res.run.id,
          // 耗时以这一轮真正的开始时间为准：切页回来不重新数
          startedAt: res.run.started_at,
          onError: (message) => Toast.error(message),
          // 续流同样要刷树：这一轮可能在页面刷新期间写过文件（本轮事件是从 offset 0 重放的）
          onSettled: (info) => {
            void queryClient.invalidateQueries({ queryKey: queryKeys.messages(session.id) });
            void queryClient.invalidateQueries({ queryKey: queryKeys.sessions });
            if (info.wroteFiles) invalidateAfterWrite(queryClient);
          },
        });
        // 挂流已经结束（这一轮收尾，或者缓存里没内容直接 detached）：放开登记，
        // 下次切页/切回会话还能再试一次。不释放的话，一次没挂上的失败会在这份
        // 组件实例里变成永久空白——后面每次进来都被这行挡住。
        attachedRef.current.delete(session.id);
      } catch {
        // 拉不到当次状态就当没在跑：用户可以重新发一条
      }
    })();

    return () => {
      cancelled = true;
      if (!attached) attachedRef.current.delete(session.id);
    };
  }, [selectedSession, queryClient]);

  const logout = async () => {
    try {
      await api('/auth/logout', { method: 'POST' });
    } finally {
      window.location.href = '/login';
    }
  };

  const user = me?.user;
  const initial = user?.name?.trim().slice(0, 1) || '?';
  // 工号/邮箱来自企业通讯录，权限缺失时为空
  const userMeta = [user?.job_number ? `工号 ${user.job_number}` : '', user?.email ?? '']
    .filter(Boolean)
    .join(' · ');
  const treeValue = selectedFile
    ? `${FILE_KEY_PREFIX}${selectedFile.space_id}/${selectedFile.path}`
    : selectedSession
      ? SESSION_KEY_PREFIX + selectedSession.id
      : selectedSpaceId;

  // 市场分类计数与当前类型筛选无关：侧栏先给总量，明细在右侧筛
  const catalogItems = catalog?.items ?? [];
  const categoryCount = (c: PluginCategory) =>
    c === 'all'
      ? catalogItems.length
      : catalogItems.filter((item) => (item.pending ? 'pending' : item.category) === c).length;
  const openCategory = (c: PluginCategory) =>
    navigate(c === 'all' ? '/plugins/market' : `/plugins/market/${c}`);

  return (
    <Layout className="app-shell">
      <Layout.Sider className="app-sider" style={{ width: 280 }}>
        {/* 顶部：Logo 与组织 */}
        <div className="sider-brand">
          <span className="brand-mark" />
          <div className="sider-brand-text">
            <span className="brand-name">WorkAgent</span>
            {user?.corp_name ? (
              <span className="sider-corp" title={user.corp_name}>
                {user.corp_name}
              </span>
            ) : null}
          </div>
        </div>

        {/* 一级菜单：工作空间 / 插件 */}
        <nav className="sider-nav">
          <button
            type="button"
            className={`sider-nav-item${isPlugins ? '' : ' active'}`}
            onClick={() => navigate('/')}
          >
            工作空间
          </button>
          <button
            type="button"
            className={`sider-nav-item${isPlugins ? ' active' : ''}`}
            onClick={() => navigate('/plugins/market')}
          >
            插件市场
          </button>
        </nav>

        {/* 中部：插件市场的分类二级菜单 */}
        {isPlugins ? (
          <div className="sider-main">
            <div className="sider-head">
              <span className="sider-title">插件市场</span>
            </div>
            <div className="sider-subnav">
              {(['all', 'internal', 'pending', 'system', 'external'] as PluginCategory[]).map((c) => (
                <button
                  key={c}
                  type="button"
                  className={`sider-sub-item${pluginCategory === c ? ' active' : ''}`}
                  onClick={() => openCategory(c)}
                >
                  <span>{PLUGIN_CATEGORY_LABEL[c]}</span>
                  <span className="sider-sub-count">{categoryCount(c)}</span>
                </button>
              ))}
            </div>
          </div>
        ) : null}

        {/* 中部：工作空间树（插件市场下不展示树） */}
        <div className="sider-main" hidden={isPlugins}>
          <div className="sider-head">
            <span className="sider-title">工作空间</span>
            <Button
              theme="borderless"
              size="small"
              icon={<IconPlus />}
              aria-label="新建空间"
              onClick={() => setDialog({ kind: 'create-root' })}
            />
          </div>
          {spacesLoading ? (
            <div className="sider-loading">
              <Spin />
            </div>
          ) : (
            <SpaceTree
              spaces={spaces}
              sessions={sessions}
              dirs={dirs}
              files={files}
              ready={treeReady}
              value={treeValue}
              onSelectSpace={openSpace}
              onSelectSession={openSession}
              onSelectFile={openFile}
              onAddDir={(space: Space, parent?: SpaceDir) =>
                setDialog({ kind: 'create-dir', space, parent })
              }
              onAddSkill={(space: Space) => setDialog({ kind: 'create-plugin', space })}
              onAddSession={(space: Space, dir?: SpaceDir) =>
                setNewSession({ space, dir: dir?.path })
              }
              onRename={(space: Space) => setDialog({ kind: 'rename', target: space })}
              onDelete={(space: Space) => setDialog({ kind: 'delete', target: space })}
              onRenameDir={(space: Space, dir: SpaceDir) => setDialog({ kind: 'rename-dir', space, dir })}
              onDeleteDir={(space: Space, dir: SpaceDir) => setDialog({ kind: 'delete-dir', space, dir })}
              onDeleteFile={(space: Space, file: SpaceFile) =>
                setDialog({ kind: 'delete-file', space, file })
              }
              onDeleteSession={setPendingDelete}
            />
          )}
        </div>

        {/* 底部：用户与退出 */}
        <div className="sider-user">
          <Avatar size="small" color="violet" src={user?.avatar || undefined}>
            {initial}
          </Avatar>
          <div className="sider-user-text">
            <span className="sider-user-name" title={userMeta || undefined}>
              {user?.name}
            </span>
            {userMeta ? (
              <span className="sider-user-meta" title={userMeta}>
                {userMeta}
              </span>
            ) : null}
          </div>
          {user?.profile_warning ? (
            <Tooltip content={user.profile_warning} position="topRight">
              <IconAlertCircle className="header-warn" />
            </Tooltip>
          ) : null}
          <Tooltip content="退出登录" position="topRight">
            <Button
              theme="borderless"
              size="small"
              icon={<IconExit />}
              aria-label="退出登录"
              onClick={logout}
            />
          </Tooltip>
        </div>
      </Layout.Sider>

      <Layout.Content className="app-content">
        {isPlugins ? (
          <div className="plugin-market">
            <PluginCatalogPanel defaultSpaceId={selectedSpaceId} category={pluginCategory} />
          </div>
        ) : selectedFile ? (
          <FilePanel space={selectedSpace} file={selectedFile} />
        ) : selectedSession ? (
          <div className="chat-shell">
            <TaskStrip sessionId={selectedSession.id} />
            <ChatPanel
            session={selectedSession}
            spaceName={selectedSpace?.name}
            messages={messages}
            loading={messagesLoading}
            sending={Boolean(stream)}
            stream={stream}
            pendingUser={pendingUser}
            pendingRefs={stream ? liveRun?.pendingRefs : undefined}
            agentReady={agentInfo?.ready ?? false}
            model={agentInfo?.model}
            uploadMaxMb={agentInfo?.upload_max_file_mb}
            skills={skills}
            onRefreshSkills={() => {
              void refetchSkills();
            }}
            onSelectSkill={(path) => {
              setSessionSkill.mutate(
                { id: selectedSession.id, path },
                { onError: (e) => Toast.error(e instanceof Error ? e.message : '技能切换失败') },
              );
            }}
            // 引用文件只列当前空间：跨空间的路径会被后端跳过，列出来只会误导
            spaceFiles={files.filter((f) => f.space_id === selectedSession.space_id)}
            onUploadFiles={(list) =>
              uploadFiles.mutateAsync({
                spaceId: selectedSession.space_id,
                files: list,
                // 会话挂在某个目录下时，材料默认放进这个目录
                dir: selectedSession.dir,
              })
            }
              onSend={send}
              onStop={stopGenerating}
              resumable={Boolean(resumeInfo?.resumable) && !stream}
              onResume={() => void resume()}
              // 写入文件清单里的条目：切到右侧文件预览，路径落在会话所属空间
              onOpenFile={(path) =>
                setSearchParams({ space: selectedSession.space_id, file: path })
              }
            />
          </div>
        ) : (
          <SpacePanel
            space={selectedSpace}
            sessions={spaceSessions}
            loading={sessionsLoading}
            onOpenSession={openSession}
            onAddSession={(agentId?: string) =>
              selectedSpace && setNewSession({ space: selectedSpace, agentId })
            }
            onOpenPlatform={() => setPlatformOpen(true)}
            onOpenMCPApps={() => setMcpAppsOpen(true)}
          />
        )}
      </Layout.Content>

      <SpaceDialog
        state={dialog}
        onClose={() => setDialog({ kind: 'none' })}
        onOpenSession={openSession}
      />
      <NewSessionDialog
        state={newSession}
        onClose={() => setNewSession(null)}
        onCreate={(input) =>
          addSession(input.spaceId, input.dir, input.agentId, input.title)
        }
      />
      <SecretSettingsDialog visible={platformOpen} onClose={() => setPlatformOpen(false)} />
      <MCPAppsDialog visible={mcpAppsOpen} onClose={() => setMcpAppsOpen(false)} />

      <Modal
        title="删除会话"
        visible={Boolean(pendingDelete)}
        onCancel={() => setPendingDelete(null)}
        onOk={confirmDeleteSession}
        okText="删除"
        cancelText="取消"
        okButtonProps={{ type: 'danger', loading: deleteSession.isPending }}
        maskClosable={!deleteSession.isPending}
        closeOnEsc={!deleteSession.isPending}
      >
        <p className="dialog-desc">
          将删除「{pendingDelete?.title}」及其全部消息，删除后不可恢复。
        </p>
      </Modal>
    </Layout>
  );
}
