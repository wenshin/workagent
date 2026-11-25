import { IconAlertCircle, IconExit, IconPlus } from '@douyinfe/semi-icons';
import { Avatar, Button, Layout, Modal, Spin, Toast, Tooltip } from '@douyinfe/semi-ui';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
  type KeyboardEvent as ReactKeyboardEvent,
  type MouseEvent as ReactMouseEvent,
} from 'react';
import { useLocation, useNavigate, useSearchParams } from 'react-router-dom';

import { api, firstOversize, getActiveRun, getResumable, listPendingApprovals } from '../api/client';
import {
  invalidateAfterWrite,
  queryKeys,
  useAgentInfo,
  useCreateSession,
  useDeleteSession,
  useMe,
  useMessages,
  usePluginCatalog,
  useSessions,
  useSpaceDirs,
  useSpaceFiles,
  useSpaces,
  useSpaceTreeAutoRefresh,
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
  toApprovalInfo,
} from '../api/liveRun';
import type { ChatSession, PluginCategory, Space, SpaceDir, SpaceFile } from '../api/types';
import { sessionWrittenPaths } from '../lib/changedFiles';
import ChangedFilesPanel from '../components/ChangedFilesPanel';
import ChatPanel, { splitReferences, type StreamView } from '../components/ChatPanel';
import FilePanel from '../components/FilePanel';
import MCPAppsDialog from '../components/MCPAppsDialog';
import NewSessionDialog, { type NewSessionState } from '../components/NewSessionDialog';
import PluginCatalogPanel, { PLUGIN_CATEGORY_LABEL } from '../components/PluginCatalogPanel';
import SecretSettingsDialog from '../components/SecretSettingsDialog';
import SpaceDialog, { type DialogState } from '../components/SpaceDialog';
import SpacePanel from '../components/SpacePanel';
import SpaceTree, { FILE_KEY_PREFIX, SESSION_KEY_PREFIX } from '../components/SpaceTree';

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
  // 定时刷新：树的内容是磁盘现状，别人（其他会话的 Agent、另一个标签页、shell）
  // 写出来的文件没有事件可订阅，只能靠版本号轮询发现
  useSpaceTreeAutoRefresh();
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
  const uploadFiles = useUploadFiles();
  const { data: agentInfo } = useAgentInfo();

  // 文件树的上传入口：先记下目标空间/目录，再弹系统文件选择框（全树复用一个 input）
  const treeUploadTarget = useRef<{ space: Space; dir?: string } | null>(null);
  const treeFileInput = useRef<HTMLInputElement>(null);

  const startTreeUpload = (space: Space, dir?: SpaceDir) => {
    treeUploadTarget.current = { space, dir: dir?.path };
    treeFileInput.current?.click();
  };

  const handleTreeUpload = async (input: HTMLInputElement) => {
    const picked = Array.from(input.files ?? []);
    // 清空才能再次选中同一个文件触发 change
    input.value = '';
    const target = treeUploadTarget.current;
    treeUploadTarget.current = null;
    if (!target || picked.length === 0) return;

    const tooBig = firstOversize(picked, agentInfo?.upload_max_file_mb);
    if (tooBig) {
      Toast.error(`「${tooBig.name}」超过 ${agentInfo?.upload_max_file_mb}MB 上限`);
      return;
    }
    try {
      const saved = await uploadFiles.mutateAsync({
        spaceId: target.space.id,
        files: picked,
        dir: target.dir,
      });
      Toast.success(
        target.dir
          ? `已上传 ${saved.length} 个文件到「${target.dir}」`
          : `已上传 ${saved.length} 个文件到「${target.space.name}」`,
      );
    } catch (e) {
      Toast.error(e instanceof Error ? e.message : '上传失败，请重试');
    }
  };

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
  // 左右分栏：左边对话，右边本会话修改过的文件。宽度可拖、双击复位
  const [splitOpen, setSplitOpen] = useState(false);
  const [splitPath, setSplitPath] = useState('');
  const [splitWidth, setSplitWidth] = useState(440);
  const splitRef = useRef<HTMLDivElement>(null);

  // 切会话时右侧退回清单：上一个会话写过的文件在新会话里通常一个都不在，
  // 停在预览上只会让人以为「新会话改了这个文件」
  const splitSessionId = selectedSession?.id ?? '';
  useEffect(() => {
    setSplitPath('');
  }, [splitSessionId]);

  // 分栏清单与每条消息底部的「本轮写入」同源：历史回执 + 当前轮实时工具调用。
  // 两段分开算：实时帧每来一次都会重算，历史消息不该跟着重新解析一遍参数
  const historyChanged = useMemo(
    () => (selectedSession ? sessionWrittenPaths(messages) : []),
    [selectedSession, messages],
  );
  const liveChanged = useMemo(() => sessionWrittenPaths([], stream?.items), [stream?.items]);
  const changedPaths = useMemo(() => {
    if (liveChanged.length === 0) return historyChanged;
    const out = [...historyChanged];
    for (const path of liveChanged) if (!out.includes(path)) out.push(path);
    return out;
  }, [historyChanged, liveChanged]);

  const toggleSplit = () => {
    setSplitOpen((open) => !open);
    setSplitPath('');
  };

  const startSplitResize = (e: ReactMouseEvent<HTMLDivElement>) => {
    const box = splitRef.current;
    if (!box) return;
    const startWidth = splitWidth;
    const startX = e.clientX;
    // 左侧至少留 360px：对话被压到几百像素以下就没法读了
    const clamp = (px: number) =>
      Math.min(Math.max(px, 320), Math.max(320, Math.round(box.getBoundingClientRect().width) - 360));
    const onMove = (ev: MouseEvent) => setSplitWidth(clamp(startWidth - (ev.clientX - startX)));
    const onUp = () => {
      window.removeEventListener('mousemove', onMove);
      window.removeEventListener('mouseup', onUp);
      document.body.style.userSelect = '';
      document.body.style.cursor = '';
    };
    window.addEventListener('mousemove', onMove);
    window.addEventListener('mouseup', onUp);
    document.body.style.userSelect = 'none';
    document.body.style.cursor = 'col-resize';
  };

  // 分隔条也支持键盘调整：只靠鼠标的布局控件键盘用户完全够不着
  const resizeSplitByKey = (e: ReactKeyboardEvent<HTMLDivElement>) => {
    const step = e.shiftKey ? 64 : 16;
    const boxWidth = splitRef.current?.getBoundingClientRect().width ?? 1200;
    if (e.key === 'ArrowLeft') {
      e.preventDefault();
      setSplitWidth((w) => Math.min(w + step, Math.max(320, Math.round(boxWidth) - 360)));
    } else if (e.key === 'ArrowRight') {
      e.preventDefault();
      setSplitWidth((w) => Math.max(w - step, 320));
    }
  };

  // 写入清单里的文件：分栏开着就在右侧预览，否则整页打开
  const openSessionFile = (path: string) => {
    if (!selectedSession) return;
    if (splitOpen) {
      setSplitPath(path);
      return;
    }
    setSearchParams({ space: selectedSession.space_id, file: path });
  };

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
        void queryClient.invalidateQueries({ queryKey: ['approvals', sessionId] });
        void queryClient.invalidateQueries({ queryKey: ['resumable', sessionId] });
        if (info.wroteFiles) invalidateAfterWrite(queryClient);
      },
    });
  };

  // 带上会话 id：服务端按会话定位这一轮，前端不必等 run.started 拿到 run_id
  const stopGenerating = () => stopLiveRun(selectedSession?.id);

  // 服务端仍挂着的待确认动作。审批即暂停，那一轮没有流可挂，卡片只能从这里来：
  // 不查的话，切页回来会只剩一句「已中断」，用户根本不知道在等什么。
  const approvalsKey = ['approvals', selectedSession?.id ?? ''] as const;
  const { data: pendingList = [] } = useQuery({
    queryKey: approvalsKey,
    queryFn: () => listPendingApprovals(selectedSession!.id),
    enabled: Boolean(selectedSession) && !stream,
    staleTime: 0,
    retry: false,
  });
  const approvals = useMemo(
    () => pendingList.map((a) => toApprovalInfo(a, selectedSession?.id ?? '')),
    [pendingList, selectedSession?.id],
  );

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
        void queryClient.invalidateQueries({ queryKey: ['approvals', sessionId] });
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
            void queryClient.invalidateQueries({ queryKey: ['approvals', session.id] });
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

  // 会话面板在整页与分栏两种布局里复用：分栏时它只是左半边
  const chatView = selectedSession ? (
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
      // 只按空间收窄：引用不限类型（非文本按附件进清单），跨空间路径会被后端跳过，
      // 所以这里不把其他空间的文件混进来
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
      // 有待办时只留审批卡：「继续」和它抢的是同一份断点，点了也只会再停一次
      resumable={Boolean(resumeInfo?.resumable) && !stream && approvals.length === 0}
      onResume={() => void resume()}
      // 审批即暂停：这一轮已经收尾，卡片要从服务端待办补，刷新后也在
      pendingApprovals={stream ? [] : approvals}
      splitOpen={splitOpen}
      onToggleSplit={toggleSplit}
      // 写入文件清单里的条目：分栏开着就在右侧预览，否则整页打开展示
      onOpenFile={openSessionFile}
    />
  ) : null;

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
          {/* 树里的「上传文件」共用这一个选择框：目标空间/目录由点击时记下的 ref 提供。
              不限类型：图片 / 音视频 / 压缩包都能传，右侧面板按 preview_kind 预览或给下载 */}
          <input
            ref={treeFileInput}
            type="file"
            multiple
            hidden
            onChange={(e) => void handleTreeUpload(e.currentTarget)}
          />
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
              onUploadFiles={startTreeUpload}
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
        ) : selectedSession && splitOpen ? (
          // 分栏：左边对话，右边本会话修改过的文件。宽度由分隔条拖出来，落在本地状态里
          <div className="chat-split" ref={splitRef}>
            <div className="chat-split-main">{chatView}</div>
            <div
              className="chat-split-divider"
              role="separator"
              aria-orientation="vertical"
              aria-label="拖动调整分栏宽度"
              aria-valuenow={splitWidth}
              aria-valuemin={320}
              tabIndex={0}
              title="拖动调整宽度，双击复位"
              onMouseDown={startSplitResize}
              onDoubleClick={() => setSplitWidth(440)}
              onKeyDown={resizeSplitByKey}
            />
            <aside className="chat-split-aside" style={{ width: splitWidth }}>
              <ChangedFilesPanel
                space={selectedSpace}
                files={files.filter((f) => f.space_id === selectedSession.space_id)}
                paths={changedPaths}
                selectedPath={splitPath}
                onSelect={setSplitPath}
              />
            </aside>
          </div>
        ) : selectedFile ? (
          // key 跟着文件走：切文件时媒体加载失败 / 加载中状态必须重置
          <FilePanel
            key={`${selectedFile.space_id}/${selectedFile.path}`}
            space={selectedSpace}
            file={selectedFile}
          />
        ) : selectedSession ? (
          <div className="chat-shell">{chatView}</div>
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
