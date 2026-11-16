import { Input, Modal, Select, Typography } from '@douyinfe/semi-ui';
import { useEffect, useState } from 'react';

import { useAgents } from '../api/hooks';
import type { Space } from '../api/types';

const { Text } = Typography;

export interface NewSessionState {
  space: Space;
  /** 从目录右键进入时带上，会话直接挂在该目录下 */
  dir?: string;
  /** 空间面板里已经选过的智能体，弹窗打开时预选 */
  agentId?: string;
}

interface Props {
  state: NewSessionState | null;
  onClose: () => void;
  /** 返回 true 表示创建成功；失败时保持弹窗打开，让用户改一下再试 */
  onCreate: (input: {
    spaceId: string;
    dir?: string;
    agentId?: string;
    title?: string;
  }) => Promise<boolean>;
}

/**
 * 新建会话的唯一入口：左侧树右键、空间面板按钮都走这里。
 *
 * 以前树入口直接建会话、面板入口才带智能体下拉，两条路径行为不一致，
 * 从树里建出来的会话永远拿不到选择的机会。选智能体只有在这一步才有意义：
 * 会话创建后提示词与工具集就固化了。
 */
export default function NewSessionDialog({ state, onClose, onCreate }: Props) {
  const { data } = useAgents(state?.space.id);
  const agents = data?.agents ?? [];

  const [agentId, setAgentId] = useState('');
  const [title, setTitle] = useState('');
  const [pending, setPending] = useState(false);

  // 每次打开重置：上一轮选的智能体不该悄悄带到另一个空间
  useEffect(() => {
    if (!state) return;
    setTitle('');
    setAgentId(state.agentId ?? '');
    setPending(false);
  }, [state]);

  // 清单是异步来的，拿到后补默认选中（base 优先）
  useEffect(() => {
    if (!state || agentId || agents.length === 0) return;
    const base = agents.find((a) => a.id === 'base');
    setAgentId((base ?? agents[0]).id);
  }, [state, agentId, agents]);

  if (!state) {
    return null;
  }

  const submit = async () => {
    setPending(true);
    const ok = await onCreate({
      spaceId: state.space.id,
      dir: state.dir,
      agentId: agentId || undefined,
      title: title.trim() || undefined,
    });
    setPending(false);
    if (ok) {
      onClose();
    }
  };

  return (
    <Modal
      title="新建会话"
      visible
      onCancel={onClose}
      onOk={submit}
      okText="创建"
      cancelText="取消"
      okButtonProps={{ loading: pending, type: 'primary' }}
      maskClosable={!pending}
      closeOnEsc={!pending}
    >
      <p className="dialog-intro">
        {state.dir ? `会话会挂在「${state.dir}」目录下。` : null}
        选择智能体：会话的提示词与可用工具由它决定，创建时固化，之后改配置不影响这个会话。
      </p>

      <div className="dialog-group">
        <span className="dialog-group-title">智能体</span>
        <Select
          value={agentId}
          onChange={(v) => setAgentId(String(v))}
          optionList={agents.map((a) => ({ value: a.id, label: a.name }))}
          style={{ width: '100%' }}
          placeholder={agents.length === 0 ? '暂无可用智能体' : '选择智能体'}
          disabled={pending}
        />
        {data && agents.length === 0 ? (
          <Text type="tertiary" size="small">
            当前空间没有可用智能体，将由平台默认的通用助理接手。
          </Text>
        ) : null}
      </div>

      <div className="dialog-group">
        <span className="dialog-group-title">会话名称（可选）</span>
        <Input
          value={title}
          onChange={setTitle}
          placeholder="留空则由首条消息自动生成"
          disabled={pending}
          onEnterPress={submit}
        />
      </div>
    </Modal>
  );
}
