import { IconPlus } from '@douyinfe/semi-icons';
import { Avatar, Button, Layout, Spin } from '@douyinfe/semi-ui';
import { useEffect, useMemo, useState } from 'react';
import { useSearchParams } from 'react-router-dom';

import { api } from '../api/client';
import { useMe, useSessions, useSpaces } from '../api/hooks';
import type { Space } from '../api/types';
import SessionPanel from '../components/SessionPanel';
import SpaceDialog, { type DialogState } from '../components/SpaceDialog';
import SpaceTree from '../components/SpaceTree';

export default function WorkspacePage() {
  const { data: me } = useMe();
  const { data: spaces = [], isLoading: spacesLoading } = useSpaces();
  const [searchParams, setSearchParams] = useSearchParams();
  const [dialog, setDialog] = useState<DialogState>({ kind: 'none' });

  // 默认定位到个人空间的「会话」子空间
  const defaultId = useMemo(() => {
    const personal = spaces.find((s) => s.type === 'personal' && !s.parent_id);
    const session = personal
      ? spaces.find((s) => s.parent_id === personal.id && s.is_system_reserved)
      : undefined;
    return session?.id ?? personal?.id ?? spaces[0]?.id ?? '';
  }, [spaces]);

  const urlId = searchParams.get('space') ?? '';
  const selectedId = spaces.some((s) => s.id === urlId) ? urlId : defaultId;
  const selected = spaces.find((s) => s.id === selectedId);

  useEffect(() => {
    if (selectedId && selectedId !== urlId) {
      setSearchParams({ space: selectedId }, { replace: true });
    }
  }, [selectedId, urlId, setSearchParams]);

  const isSessionView = selected?.is_system_reserved ?? false;
  const { data: sessions = [], isLoading: sessionsLoading } = useSessions(selectedId, isSessionView);

  const logout = async () => {
    try {
      await api('/auth/logout', { method: 'POST' });
    } finally {
      const base = import.meta.env.BASE_URL.replace(/\/$/, '');
      window.location.href = `${base}/login`;
    }
  };

  const user = me?.user;
  const initial = user?.name?.trim().slice(0, 1) || '?';

  const selectSpace = (id: string) => setSearchParams({ space: id });

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
              value={selectedId}
              onSelect={selectSpace}
              onAddChild={(space: Space) => setDialog({ kind: 'create-child', parent: space })}
              onRename={(space: Space) => setDialog({ kind: 'rename', target: space })}
              onDelete={(space: Space) => setDialog({ kind: 'delete', target: space })}
              onAddSession={(space: Space) => setDialog({ kind: 'add-session', target: space })}
            />
          )}
        </Layout.Sider>

        <Layout.Content className="app-content">
          <SessionPanel
            space={selected}
            sessions={sessions}
            loading={sessionsLoading}
            onAddSession={() => selected && setDialog({ kind: 'add-session', target: selected })}
          />
        </Layout.Content>
      </Layout>

      <SpaceDialog state={dialog} onClose={() => setDialog({ kind: 'none' })} />
    </Layout>
  );
}
