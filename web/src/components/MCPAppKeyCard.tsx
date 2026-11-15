import { Button, Input, Tag, Toast, Typography } from '@douyinfe/semi-ui';
import { useEffect, useState } from 'react';

import { useMCPAppKey } from '../api/hooks';
import type { MCPAppKeyResponse } from '../api/types';

const { Text } = Typography;

/** mcp_app_key 工具回执里的描述，字段与后端 mcpAppSpec 一一对应 */
export interface MCPAppKeyData {
  app: string;
  /** needs_app / new / owned / taken / unowned / disabled；回执没到前是 checking */
  state: string;
  owner_name?: string;
  hint?: string;
  available?: boolean;
  /** 仅前端使用：回执还没到，先用调用参数占位 */
  pending?: boolean;
}

interface Props {
  data: MCPAppKeyData;
  spaceId?: string;
  /** 拿到密钥后让 Agent 继续；发的是状态，不是密钥 */
  onContinue?: (content: string) => void;
}

const TAG: Record<string, { text: string; color: 'green' | 'orange' | 'red' | 'grey' | 'blue' }> = {
  created: { text: '已生成', color: 'green' },
  owned: { text: '已取回', color: 'green' },
  taken: { text: '已被占用', color: 'orange' },
  unowned: { text: '需管理员', color: 'orange' },
  disabled: { text: '未启用', color: 'red' },
  new: { text: '待生成', color: 'blue' },
};

/**
 * 会话内的 MCP 应用密钥卡片。
 *
 * 一个应用名只有一把 JWT 签名密钥，所以这里的规则是「谁创建、谁能看」：
 * 名字不存在就生成并把创建者记下来，名字存在则只有创建者能取回明文。
 *
 * 明文只从浏览器直接打到后端（POST /spaces/{id}/mcp-apps/{app}/key），
 * 不经过工具回执，因此不会进消息表、上下文与 run_step 审计。
 */
export default function MCPAppKeyCard({ data, spaceId, onContinue }: Props) {
  const request = useMCPAppKey();
  const [app, setApp] = useState(data.app);
  const [result, setResult] = useState<MCPAppKeyResponse | null>(null);
  const [visible, setVisible] = useState(true);
  const [autoLoaded, setAutoLoaded] = useState(false);

  const pending = Boolean(data.pending);
  // 回执到了就以服务端结论为准（用户可能在等回执时已经改过应用名）
  const state = result?.status ?? (pending ? 'checking' : data.state || 'needs_app');
  const confirmed = state === 'created' || state === 'owned';

  const load = async (name: string) => {
    const trimmed = name.trim();
    if (!trimmed) {
      Toast.warning('请先填写应用名');
      return;
    }
    if (!spaceId) {
      Toast.error('缺少空间信息，刷新页面重试');
      return;
    }
    try {
      const res = await request.mutateAsync({ spaceId, app: trimmed });
      setResult(res);
      setVisible(true);
    } catch (err) {
      Toast.error(err instanceof Error ? err.message : '获取密钥失败');
    }
  };

  // 查询自己的密钥：模型已经把应用名带过来了，进页面直接取回，不用再点一次。
  // 只在 owned 时自动跑：new 必须由用户点一下才生成，避免模型猜的名字被落库。
  useEffect(() => {
    if (pending || autoLoaded) return;
    if (data.state !== 'owned' || !data.app) return;
    setAutoLoaded(true);
    void load(data.app);
    // load 只依赖 spaceId 与入参，这里按「回执一次性」触发即可
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pending, data.state, data.app]);

  const copy = async (text: string, label: string) => {
    try {
      await navigator.clipboard.writeText(text);
      Toast.success(`已复制${label}`);
    } catch {
      Toast.error('复制失败，请手动选中');
    }
  };

  const tag = TAG[state] ?? { text: '检查中', color: 'grey' as const };
  const masked = result?.key ? '•'.repeat(Math.min(result.key.length, 40)) : '';

  return (
    <div className="msg-secret-req">
      <div className="msg-secret-req-head">
        <Text strong>MCP 应用密钥</Text>
        <Tag size="small" color={pending ? 'grey' : tag.color}>
          {pending ? '检查中' : tag.text}
        </Tag>
      </div>

      {data.hint ? (
        <Text type="tertiary" size="small" className="msg-secret-req-hint">
          {data.hint}
        </Text>
      ) : null}

      <Text type="tertiary" size="small" className="msg-secret-req-hint">
        一个应用名对应一把密钥，清单里写 <Text code>auth.mode=jwt</Text> 与{' '}
        <Text code>auth.app=&lt;应用名&gt;</Text>。密钥只给创建者，发布前后都是同一把。
      </Text>

      {pending ? (
        <Text type="tertiary" size="small">
          正在确认这个应用名是否已经存在…
        </Text>
      ) : (
        <div className="msg-secret-req-form">
          <Input
            value={app}
            onChange={setApp}
            disabled={request.isPending}
            placeholder="应用名，如 crm_mcp"
            onEnterPress={() => void load(app)}
          />
          <Button
            theme="solid"
            type="primary"
            loading={request.isPending}
            onClick={() => void load(app)}
          >
            {confirmed ? '重新取回' : '生成密钥'}
          </Button>
        </div>
      )}

      {!pending && data.available === false ? (
        <Text type="warning" size="small">
          凭证存储未启用（后端没读到 SECRET_KEK），现在生成不了密钥。先在部署侧配置后重启。
        </Text>
      ) : null}

      {state === 'taken' ? (
        <Text type="warning" size="small">
          应用名 {result?.app || app} 已被 {result?.owner_name || data.owner_name || '其他成员'} 创建。
          密钥只能由创建者取回——联系他，或换一个属于你自己的应用名。
        </Text>
      ) : null}

      {state === 'unowned' ? (
        <Text type="warning" size="small">
          应用名 {result?.app || app} 已经在用，但没有创建人记录（早于这套流程生成的）。
          找系统管理员确认归属后再用，不要在这里直接换一把新密钥。
        </Text>
      ) : null}

      {state === 'disabled' ? (
        <Text type="warning" size="small">
          凭证存储未启用，生成不了签名密钥。不要把密钥贴到对话里，先让部署侧配置 SECRET_KEK。
        </Text>
      ) : null}

      {result?.key ? (
        <>
          <Text type="tertiary" size="small">
            这是给 MCP Server 验签用的密钥，配到服务端的密钥配置里；不要提交到仓库、不要贴进对话。
          </Text>
          <div className="mcp-key-box">
            <code className="mcp-key-value">{visible ? result.key : masked}</code>
            <Button size="small" onClick={() => setVisible((v) => !v)}>
              {visible ? '隐藏' : '显示'}
            </Button>
            <Button size="small" onClick={() => void copy(result.key ?? '', '密钥')}>
              复制
            </Button>
          </div>
          <div className="mcp-key-box">
            <code className="mcp-key-value">
              {visible ? `MCP_JWT_SECRET=${result.key}` : 'MCP_JWT_SECRET=••••••'}
            </code>
            <Button
              size="small"
              onClick={() => void copy(`MCP_JWT_SECRET=${result.key}`, '环境变量')}
            >
              复制 .env 行
            </Button>
          </div>
          <Text type="tertiary" size="small">
            HS256 · Header {result.header || 'Authorization'} · aud={result.app} · 用户令牌{' '}
            {result.ttl_seconds ?? 300}s
          </Text>
        </>
      ) : null}

      {confirmed && result?.key && onContinue ? (
        <div className="msg-secret-req-actions">
          <Button
            size="small"
            theme="solid"
            type="primary"
            onClick={() => onContinue(`应用 ${result.app} 的密钥已获取，继续`)}
          >
            继续
          </Button>
        </div>
      ) : null}
    </div>
  );
}
