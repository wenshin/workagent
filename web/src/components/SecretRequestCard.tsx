import { Button, Input, Tag, Toast, Typography } from '@douyinfe/semi-ui';
import { useState } from 'react';

import { useAdminSettings, useSetSecret } from '../api/hooks';

const { Text } = Typography;

/** secret_request 工具回执里的描述，字段与后端 secretRequestSpec 一一对应 */
export interface SecretRequestData {
  scope: string;
  name: string;
  label: string;
  hint?: string;
  configured: boolean;
  fingerprint?: string;
  /** 后端给的结论：configured / missing / disabled；旧回执没有这个字段 */
  state?: string;
  /** 当前用户是否系统管理员：不是的话卡片只给「转给管理员」 */
  can_write: boolean;
  /** 凭证存储是否启用（没配 SECRET_KEK 时为 false） */
  available: boolean;
  /** 仅前端使用：回执还没到，先用调用参数占位 */
  pending?: boolean;
}

interface Props {
  data: SecretRequestData;
  /** 录入完成后让 Agent 继续；发的是状态，不是值 */
  onContinue?: (content: string) => void;
}

/**
 * 会话内的密钥录入卡片。
 *
 * 为什么不是一个普通输入框直接回给模型：明文一旦作为工具参数或消息内容回到后端，
 * 就会进消息表、进上下文、进 run_step 审计。这里值只从浏览器直接打到
 * PUT /admin/secrets/{scope}/{name}，Agent 只看到「已录入 + 指纹」。
 */
export default function SecretRequestCard({ data, onContinue }: Props) {
  const setSecret = useSetSecret();
  const [value, setValue] = useState('');
  // 本地记住刚保存的指纹：保存后不必等整条消息重新拉取就能给反馈
  const [saved, setSaved] = useState('');
  const [editing, setEditing] = useState(false);

  // 回执未到时不知道配没配：一律按「检查中」展示，避免让用户白填一遍。
  // 只认前端标注的 pending，不看 state：历史消息的回执里没有 state 字段。
  const pending = Boolean(data.pending);
  // 回执是「发起这次调用那一刻」的快照：用户完全可能在 agent 还在思考时就把密钥
  // 录完了，落库后重新渲染的卡片如果只认快照，就会把刚填的凭证当成没配、要人再填一遍。
  // 所以管理员额外读一次密钥配置列表（与弹窗共用缓存，写完一起刷新），拿实时状态；
  // 非管理员没有该接口的读权限，退回快照即可（他们本来就只看到「转给管理员」）。
  const { data: settings } = useAdminSettings(!pending && data.can_write && data.available);
  const live = settings?.items.find((it) => it.scope === data.scope && it.name === data.name);
  // 拿到实时列表就以它为准（可能已经被人删掉），否则退回快照；刚保存的这条永远算已配置。
  const configured = !pending && (Boolean(saved) || (settings ? Boolean(live) : data.configured));
  const fingerprint = saved || live?.fingerprint || data.fingerprint || '';
  const showInput = !pending && data.can_write && data.available && (!configured || editing);

  const save = async () => {
    const trimmed = value.trim();
    if (!trimmed) {
      Toast.warning('请填写密钥值');
      return;
    }
    try {
      const res = await setSecret.mutateAsync({ scope: data.scope, name: data.name, value: trimmed });
      setSaved(res.fingerprint);
      setEditing(false);
      // 明文用完即弃：即便保存失败也不留在组件状态里
      setValue('');
      Toast.success('已保存，下一次调用立即生效');
    } catch (err) {
      Toast.error(err instanceof Error ? err.message : '保存失败');
    }
  };

  return (
    <div className="msg-secret-req">
      <div className="msg-secret-req-head">
        <Text strong>{data.label}</Text>
        <Tag size="small" color={pending ? 'grey' : configured ? 'green' : 'orange'}>
          {pending ? '检查中' : configured ? '已配置' : '待录入'}
        </Tag>
      </div>

      {pending ? (
        <Text type="tertiary" size="small">
          正在确认这条凭证是否已经配置…
        </Text>
      ) : null}

      {data.hint ? (
        <Text type="tertiary" size="small" className="msg-secret-req-hint">
          {data.hint}
        </Text>
      ) : null}

      <Text type="tertiary" size="small" className="msg-secret-req-hint">
        凭证名 <Text code>{data.scope}/{data.name}</Text>
        ，清单里只写这个名字。
      </Text>

      {configured && !editing ? (
        <Text type="tertiary" size="small">
          指纹 {fingerprint || '—'} · 明文只在服务端加密保存，不会进入对话
        </Text>
      ) : null}

      {!pending && !data.available ? (
        <Text type="warning" size="small">
          凭证存储未启用（后端没读到 SECRET_KEK），先在部署侧配置后重启，不要把密钥贴到对话里。
        </Text>
      ) : null}

      {!pending && data.available && !data.can_write ? (
        <Text type="warning" size="small">
          只有系统管理员能录入平台凭证。把「scope={data.scope}、凭证名={data.name}」转给管理员，
          由他在「密钥配置」录入即可；不需要把密钥发给任何人。
        </Text>
      ) : null}

      {showInput ? (
        <div className="msg-secret-req-form">
          <Input
            mode="password"
            value={value}
            onChange={setValue}
            placeholder="粘贴密钥值（不会进入对话）"
            onEnterPress={() => void save()}
          />
          <Button theme="solid" type="primary" loading={setSecret.isPending} onClick={() => void save()}>
            保存
          </Button>
          {editing ? <Button onClick={() => setEditing(false)}>取消</Button> : null}
        </div>
      ) : null}

      {configured && !showInput ? (
        <div className="msg-secret-req-actions">
          <Button size="small" onClick={() => setEditing(true)}>
            更新密钥
          </Button>
          {onContinue ? (
            <Button
              size="small"
              theme="solid"
              type="primary"
              onClick={() => onContinue(`凭证 ${data.scope}/${data.name} 已录入，继续`)}
            >
              继续
            </Button>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}