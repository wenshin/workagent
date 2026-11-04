import { IconAlertCircle, IconExit, IconPlus } from '@douyinfe/semi-icons';
import { Avatar, Button, Layout, Modal, Spin, Toast, Tooltip } from '@douyinfe/semi-ui';
import { useQueryClient } from '@tanstack/react-query';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useLocation, useNavigate, useSearchParams } from 'react-router-dom';

import { api } from '../api/client';
import {
  queryKeys,
  useAgentInfo,
  useCreateSession,
  useDeleteSession,
  useMe,
  useMessages,
  useSetSessionSkill,
  useSessions,
  useSkills,
  useSpaceDirs,
  useSpaceFiles,
  useSpaces,
} from '../api/hooks';
import { streamChat } from '../api/stream';
import type { ChatSession, Space, SpaceDir, SpaceFile } from '../api/types';
import ChatPanel, { type StreamView } from '../components/ChatPanel';
import FilePanel from '../components/FilePanel';
import ResourcePanel from '../components/ResourcePanel';
import SpaceDialog, { type DialogState } from '../components/SpaceDialog';
import SpacePanel from '../components/SpacePanel';
import SpaceTree, { FILE_KEY_PREFIX, SESSION_KEY_PREFIX } from '../components/SpaceTree';

export default function WorkspacePage() {
  const location = useLocation();
  const navigate = useNavigate();
  // 「工作空间」与「资源管理」同级：同一套外壳，用路由区分内容
  const isResources = location.pathname.startsWith('/resources');
  const { data: me } = useMe();
  const { data: spaces = [], isLoading: spacesLoading } = useSpaces();
  const { data: sessions = [], isLoading: sessionsLoading } = useSessions();
  const { data: dirs = [] } = useSpaceDirs();
  const { data: files = [] } = useSpaceFiles();
  const [searchParams, setSearchParams] = useSearchParams();
  const [dialog, setDialog] = useState<DialogState>({ kind: 'none' });
  const [pendingDelete, setPendingDelete] = useState<ChatSession | null>(null);
  const [stream, setStream] = useState<StreamView | null>(null);
  const [pendingUser, setPendingUser] = useState<string | null>(null);

  const abortRef = useRef<AbortController | null>(null);
  // 本轮对话里 Agent 是否写过空间文件，决定结束后要不要刷新树与预览
  const wroteFiles = useRef(false);
  const queryClient = useQueryClient();

  const createSession = useCreateSession();
  const deleteSession = useDeleteSession();
  const setSessionSkill = useSetSessionSkill();
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
    if (urlSessionId || urlSpaceId || spaces.length === 0) return;
    if (defaultSessionId) setSearchParams({ session: defaultSessionId }, { replace: true });
  }, [urlSessionId, urlSpaceId, defaultSessionId, spaces.length, setSearchParams]);

  const { data: messages = [], isLoading: messagesLoading } = useMessages(selectedSession?.id);
  // 技能列表随会话所属空间变化：当前空间 + 系统空间合并，同名当前空间优先
  const { data: skills = [] } = useSkills(selectedSession?.space_id);

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
    async (spaceId: string, dir?: string) => {
      try {
        const res = await createSession.mutateAsync({ spaceId, dir });
        setSearchParams({ session: res.session.id });
      } catch (e) {
        Toast.error(e instanceof Error ? e.message : '新建会话失败');
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

  // 切换会话时中断正在进行的生成
  useEffect(() => {
    return () => {
      abortRef.current?.abort();
    };
  }, [selectedSession?.id]);

  const send = async (content: string) => {
    if (!selectedSession || abortRef.current) return;
    const sessionId = selectedSession.id;

    const controller = new AbortController();
    abortRef.current = controller;
    wroteFiles.current = false;
    setPendingUser(content);
    setStream({ reasoning: '', text: '', tools: [] });

    try {
      await streamChat(
        sessionId,
        content,
        {
          onReasoning: (delta) => setStream((s) => (s ? { ...s, reasoning: s.reasoning + delta } : s)),
          onText: (delta) => setStream((s) => (s ? { ...s, text: s.text + delta } : s)),
          onToolStart: (tool) => setStream((s) => (s ? { ...s, tools: [...s.tools, tool] } : s)),
          onToolResult: (tool) => {
            setStream((s) =>
              s ? { ...s, tools: s.tools.map((t) => (t.id === tool.id ? tool : t)) } : s,
            );
            // Agent 改过空间文件（含新建技能目录）：结束后让树与当前预览重新拉取
            if ((tool.name === 'file_write' || tool.name === 'skill_create') && tool.status === 'ok') {
              wroteFiles.current = true;
            }
          },
          onError: (message) => Toast.error(message),
        },
        controller.signal,
      );
    } catch (e) {
      // 用户主动停止不算错误
      const aborted = e instanceof DOMException && e.name === 'AbortError';
      if (!aborted) {
        Toast.error(e instanceof Error ? e.message : '发送失败，请重试');
      }
    } finally {
      abortRef.current = null;
      setStream(null);
      setPendingUser(null);
      await queryClient.invalidateQueries({ queryKey: queryKeys.messages(sessionId) });
      queryClient.invalidateQueries({ queryKey: queryKeys.sessions });
      if (wroteFiles.current) {
        queryClient.invalidateQueries({ queryKey: queryKeys.files });
        queryClient.invalidateQueries({ queryKey: ['file'] });
      }
    }
  };

  const stopGenerating = () => {
    abortRef.current?.abort();
  };

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

        {/* 一级菜单：工作空间 / 资源管理 */}
        <nav className="sider-nav">
          <button
            type="button"
            className={`sider-nav-item${isResources ? '' : ' active'}`}
            onClick={() => navigate('/')}
          >
            工作空间
          </button>
          <button
            type="button"
            className={`sider-nav-item${isResources ? ' active' : ''}`}
            onClick={() => navigate('/resources')}
          >
            资源管理
          </button>
        </nav>

        {/* 中部：工作空间树（资源管理下不展示树） */}
        <div className="sider-main" hidden={isResources}>
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
              value={treeValue}
              onSelectSpace={openSpace}
              onSelectSession={openSession}
              onSelectFile={openFile}
              onAddDir={(space: Space, parent?: SpaceDir) =>
                setDialog({ kind: 'create-dir', space, parent })
              }
              onAddSkill={(space: Space) => setDialog({ kind: 'create-skill', space })}
              onAddSession={(space: Space, dir?: SpaceDir) => void addSession(space.id, dir?.path)}
              onRename={(space: Space) => setDialog({ kind: 'rename', target: space })}
              onDelete={(space: Space) => setDialog({ kind: 'delete', target: space })}
              onRenameDir={(space: Space, dir: SpaceDir) => setDialog({ kind: 'rename-dir', space, dir })}
              onDeleteDir={(space: Space, dir: SpaceDir) => setDialog({ kind: 'delete-dir', space, dir })}
              onDeleteSession={setPendingDelete}
            />
          )}
        </div>

        {isResources ? (
          <div className="sider-main sider-note">
            资源管理展示已发布资源：版本快照不可修改，停用后 Agent 不会再检索到。发布前请先在空间里写好技能正文。
          </div>
        ) : null}

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
        {isResources ? (
          <ResourcePanel />
        ) : selectedFile ? (
          <FilePanel space={selectedSpace} file={selectedFile} />
        ) : selectedSession ? (
          <ChatPanel
            session={selectedSession}
            spaceName={selectedSpace?.name}
            messages={messages}
            loading={messagesLoading}
            sending={Boolean(stream)}
            stream={stream}
            pendingUser={pendingUser}
            agentReady={agentInfo?.ready ?? false}
            model={agentInfo?.model}
            skills={skills}
            onSelectSkill={(path) => {
              setSessionSkill.mutate(
                { id: selectedSession.id, path },
                { onError: (e) => Toast.error(e instanceof Error ? e.message : '技能切换失败') },
              );
            }}
            onSend={send}
            onStop={stopGenerating}
          />
        ) : (
          <SpacePanel
            space={selectedSpace}
            sessions={spaceSessions}
            loading={sessionsLoading}
            onOpenSession={openSession}
            onAddSession={() => selectedSpace && void addSession(selectedSpace.id)}
          />
        )}
      </Layout.Content>

      <SpaceDialog state={dialog} onClose={() => setDialog({ kind: 'none' })} />

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
