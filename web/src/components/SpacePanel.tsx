import { IconPlus } from '@douyinfe/semi-icons';
import { Button, Card, Empty, Spin, Typography } from '@douyinfe/semi-ui';

import type { ChatSession, Space } from '../api/types';

const { Text } = Typography;

interface Props {
  space?: Space;
  sessions: ChatSession[];
  loading: boolean;
  onOpenSession: (id: string) => void;
  onAddSession: () => void;
}

function formatTime(iso: string) {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return '';
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

export default function SpacePanel({ space, sessions, loading, onOpenSession, onAddSession }: Props) {
  if (!space) {
    return (
      <div className="panel-empty">
        <Empty title="还没有空间" description="点击左上角 + 创建一个空间" />
      </div>
    );
  }

  const writable = space.perm === 'write' || space.perm === 'manage';

  return (
    <div className="session-panel">
      <div className="panel-head">
        <div className="panel-heading">
          <h2 className="panel-title">{space.name}</h2>
          <Text type="tertiary" size="small">
            {space.type === 'personal' ? '个人空间 · ' : '共享空间 · '}
            选择下面的会话开始对话，或新建一个
          </Text>
        </div>
        {writable ? (
          <Button theme="solid" type="primary" icon={<IconPlus />} onClick={onAddSession}>
            新建会话
          </Button>
        ) : null}
      </div>

      {loading ? (
        <div className="panel-empty">
          <Spin />
        </div>
      ) : sessions.length === 0 ? (
        <div className="panel-empty">
          <Empty title="还没有会话" description="点击右上角「新建会话」，开始第一段对话。" />
        </div>
      ) : (
        <div className="session-grid">
          {sessions.map((s) => (
            <div key={s.id} onClick={() => onOpenSession(s.id)}>
              <Card className="session-card" bodyStyle={{ padding: 16 }}>
                <div className="session-card-title">{s.title}</div>
                <div className="session-card-time">{formatTime(s.last_message_at ?? s.created_at)}</div>
              </Card>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
