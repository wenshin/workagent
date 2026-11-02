import { IconPlus } from '@douyinfe/semi-icons';
import { Avatar, Button, Layout, Spin, Toast } from '@douyinfe/semi-ui';
import { useCallback, useEffect, useMemo, useState } from 'react';
import { useSearchParams } from 'react-router-dom';

import { api } from '../api/client';
import { useCreateSession, useMe, useMessages, useSendMessage, useSessions, useSpaces } from '../api/hooks';
import type { Space } from '../api/types';
import ChatPanel from '../components/ChatPanel';
import SpaceDialog, { type DialogState } from '../components/SpaceDialog';
import SpacePanel from '../components/SpacePanel';
import SpaceTree, { SESSION_KEY_PREFIX } from '../components/SpaceTree';

export default function WorkspacePage() {
  const { data: me } = useMe();
  const { data: spaces = [], isLoading: spacesLoading } = useSpaces();
  const { data: sessions = [], isLoading: sessionsLoading } = useSessions();
  const [searchParams, setSearchParams] = useSearchParams();
  const [dialog, setDialog] = useState<DialogState>({ kind: 'none' });

  const createSession = useCreateSession();
  const sendMessage = useSendMessage();

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

  const spaceSessions = useMemo(
    () => sessions.filter((s) => s.space_id === selectedSpaceId),
    [sessions, selectedSpaceId],
  );

  const openSession = (id: string) => setSearchParams({ session: id });
  const openSpace = (id: string) => setSearchParams({ space: id });

  const addSession = useCallback(
    async (spaceId: string) => {
      try {
        const res = await createSession.mutateAsync({ spaceId });
        setSearchParams({ session: res.session.id });
      } catch (e) {
        Toast.error(e instanceof Error ? e.message : '新建会话失败');
      }
    },
    [createSession, setSearchParams],
  );

  const send = async (content: string) => {
    if (!selectedSession) return;
    await sendMessage.mutateAsync({ sessionId: selectedSession.id, content });
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
  const treeValue = selectedSession ? SESSION_KEY_PREFIX + selectedSession.id : selectedSpaceId;

  return (
    <Layout className="app-shell">
      <Layout.Header className="app-header">
        <div className="brand">
          <span className="brand-mark" />
          <span className="brand-name">AutoAgent</span>
        </div>
        <div className="header-right">
          <Avatar size="small" color="violet" src={user?.avatar || undefined}>
            {initial}
          </Avatar>
          <span className="header-name">{user?.name}</span>
          <Button theme="borderless" size="small" onClick={logout}>
            退出
          </Button>
        </div>
      </Layout.Header>

      <Layout className="app-body">
        <Layout.Sider className="app-sider" style={{ width: 280 }}>
          <div className="sider-head">
            <span className="sider-title">空间</span>
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
              value={treeValue}
              onSelectSpace={openSpace}
              onSelectSession={openSession}
              onAddChild={(space: Space) => setDialog({ kind: 'create-child', parent: space })}
              onAddSession={(space: Space) => void addSession(space.id)}
              onRename={(space: Space) => setDialog({ kind: 'rename', target: space })}
              onDelete={(space: Space) => setDialog({ kind: 'delete', target: space })}
            />
          )}
        </Layout.Sider>

        <Layout.Content className="app-content">
          {selectedSession ? (
            <ChatPanel
              session={selectedSession}
              spaceName={selectedSpace?.name}
              messages={messages}
              loading={messagesLoading}
              sending={sendMessage.isPending}
              onSend={send}
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
      </Layout>

      <SpaceDialog state={dialog} onClose={() => setDialog({ kind: 'none' })} />
    </Layout>
  );
}
