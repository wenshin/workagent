import { IconApps, IconKey, IconPlus } from '@douyinfe/semi-icons';
import { Button, Card, Empty, Select, Spin, Typography } from '@douyinfe/semi-ui';
import { useState } from 'react';

import { useAgents } from '../api/hooks';
import type { ChatSession, Space } from '../api/types';
import { SessionAgentIcon } from './AgentAvatar';
import PluginPanel from './PluginPanel';

const { Text } = Typography;

interface Props {
  space?: Space;
  sessions: ChatSession[];
  loading: boolean;
  onOpenSession: (id: string) => void;
  /** agentId 为空时由后端用默认智能体（base） */
  onAddSession: (agentId?: string) => void;
  /** 系统空间才有密钥配置入口（只影响平台能力，不涉及空间内容） */
  onOpenPlatform?: () => void;
  /** 系统空间才有应用密钥入口：应用名是全局的，不随空间复制 */
  onOpenMCPApps?: () => void;
}

function formatTime(iso: string) {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return '';
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

export default function SpacePanel({
  space,
  sessions,
  loading,
  onOpenSession,
  onAddSession,
  onOpenPlatform,
  onOpenMCPApps,
}: Props) {
  // 智能体来自系统空间的全局安装 + 本空间安装，新建会话时选一次并固化在会话上
  const { data: agentResp } = useAgents(space?.id);
  const agents = agentResp?.agents ?? [];
  const [agentId, setAgentId] = useState('');
  const chosenAgent =
    agentId || (agents.some((a) => a.id === 'base') ? 'base' : agents[0]?.id ?? '');

  if (!space) {
    return (
      <div className="panel-empty">
        <Empty title="还没有空间" description="点击左侧「工作空间」旁的 + 创建一个空间" />
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
            {space.type === 'personal'
              ? '个人空间 · '
              : space.type === 'system'
                ? '系统空间 · '
                : '共享空间 · '}
            选择下面的会话开始对话，或新建一个
          </Text>
        </div>
        <div style={{ display: 'flex', gap: 8 }}>
          {space.type === 'system' && onOpenMCPApps ? (
            <Button icon={<IconApps />} onClick={onOpenMCPApps}>
              MCP 应用
            </Button>
          ) : null}
          {space.type === 'system' && onOpenPlatform ? (
            <Button icon={<IconKey />} onClick={onOpenPlatform}>
              密钥配置
            </Button>
          ) : null}
          {writable && agents.length > 0 ? (
            <Select
              value={chosenAgent}
              onChange={(v) => setAgentId(String(v))}
              style={{ width: 170 }}
              placeholder="智能体"
              optionList={agents.map((a) => ({ value: a.id, label: a.name }))}
            />
          ) : null}
          {writable ? (
            <Button
              theme="solid"
              type="primary"
              icon={<IconPlus />}
              onClick={() => onAddSession(chosenAgent || undefined)}
            >
              新建会话
            </Button>
          ) : null}
        </div>
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
                <div className="session-card-head">
                  <SessionAgentIcon session={s} size={18} />
                  <div className="session-card-title">{s.title}</div>
                </div>
                <div className="session-card-time">{formatTime(s.last_message_at ?? s.created_at)}</div>
              </Card>
            </div>
          ))}
        </div>
      )}

      <PluginPanel space={space} />
    </div>
  );
}
