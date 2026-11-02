import { IconPlus } from '@douyinfe/semi-icons';
import { Button, Card, Empty, Spin, Typography } from '@douyinfe/semi-ui';

import type { ChatSession, Space } from '../api/types';

const { Text } = Typography;

interface Props {
  space?: Space;
  sessions: ChatSession[];
  loading: boolean;
  onAddSession: () => void;
}

function formatTime(iso: string) {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return '';
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

export default function SessionPanel({ space, sessions, loading, onAddSession }: Props) {
  if (!space) {
    return (
      <div className="panel-empty">
        <Empty title="还没有空间" description="点击左上角 + 创建一个空间" />
      </div>
    );
  }

  const isSession = space.is_system_reserved;
  const writable = space.perm === 'write' || space.perm === 'manage';

  return (
    <div className="session-panel">
      <div className="panel-head">
        <div className="panel-heading">
          <h2 className="panel-title">{space.name}</h2>
          <Text type="tertiary" size="small">
            {isSession ? '会话空间 · 在这里发起和查看对话' : '空间根目录 · 选择左侧「会话」节点开始对话'}
          </Text>
        </div>
        {isSession && writable ? (
          <Button theme="solid" type="primary" icon={<IconPlus />} onClick={onAddSession}>
            添加会话
          </Button>
        ) : null}
      </div>

      {!isSession ? (
        <div className="panel-empty">
          <Empty
            title="这里是空间根目录"
            description="会话都在「会话」节点下，点击左侧带气泡图标的节点即可。"
          />
        </div>
      ) : loading ? (
        <div className="panel-empty">
          <Spin />
        </div>
      ) : sessions.length === 0 ? (
        <div className="panel-empty">
          <Empty title="还没有会话" description="点击右上角「添加会话」，开始第一段对话。" />
        </div>
      ) : (
        <div className="session-grid">
          {sessions.map((s) => (
            <Card key={s.id} className="session-card" bodyStyle={{ padding: 16 }}>
              <div className="session-card-title">{s.title}</div>
              <div className="session-card-time">{formatTime(s.created_at)}</div>
            </Card>
          ))}
        </div>
      )}
    </div>
  );
}
