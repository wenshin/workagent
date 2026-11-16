import { IconRefresh } from '@douyinfe/semi-icons';
import { Button, Modal, Tag, Toast, Typography } from '@douyinfe/semi-ui';
import { useEffect, useState } from 'react';

import { useMCPApps, useRevealMCPAppKey } from '../api/hooks';
import type { MCPAppItem } from '../api/types';

const { Text } = Typography;

interface Props {
  visible: boolean;
  onClose: () => void;
}

function formatTime(iso?: string) {
  if (!iso) return '';
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return '';
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

/**
 * MCP 应用密钥（仅系统空间成员）。
 *
 * 应用名是全局的：一个应用名一把 HS256 签名密钥，客户端拿它换网关的 JWT。
 * 列表只给指纹——一次拉全部应用时把明文一起下发，等于把生产密钥写进浏览器
 * 缓存、代理日志和 HAR 文件；明文只在点「查看密钥」时单独取一次，且每次取回
 * 都会写审计。这里只读不建：缺密钥说明该应用还没走过「重新取回」，不在这里生成。
 */
export default function MCPAppsDialog({ visible, onClose }: Props) {
  const { data, isLoading, isFetching, error, refetch } = useMCPApps(visible);
  const reveal = useRevealMCPAppKey();
  // 已经取回的明文按应用名缓存：切换查看不必重复请求
  const [keys, setKeys] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState('');

  useEffect(() => {
    if (!visible) {
      setKeys({});
      setBusy('');
    }
  }, [visible]);

  const items = data?.items ?? [];
  const enabled = data?.secrets_enabled ?? false;

  const toggle = async (item: MCPAppItem) => {
    if (keys[item.app]) {
      setKeys((prev) => {
        const next = { ...prev };
        delete next[item.app];
        return next;
      });
      return;
    }
    setBusy(item.app);
    try {
      const res = await reveal.mutateAsync(item.app);
      setKeys((prev) => ({ ...prev, [item.app]: res.key }));
    } catch (err) {
      Toast.error(err instanceof Error ? err.message : '取回密钥失败');
    } finally {
      setBusy('');
    }
  };

  const copy = async (key: string) => {
    try {
      await navigator.clipboard.writeText(key);
      Toast.success('已复制');
    } catch {
      Toast.error('复制失败，请手动选中');
    }
  };

  return (
    <Modal
      title="MCP 应用密钥"
      visible={visible}
      onCancel={onClose}
      footer={
        <div style={{ display: 'flex', justifyContent: 'flex-end' }}>
          <Button onClick={onClose}>关闭</Button>
        </div>
      }
      width={680}
      bodyStyle={{ paddingTop: 8 }}
    >
      {error ? (
        <Text type="danger">{error instanceof Error ? error.message : '读取应用列表失败'}</Text>
      ) : isLoading || !data ? (
        <Text type="tertiary">加载中…</Text>
      ) : (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
          <Text type="tertiary" size="small">
            应用名是全局的，一个应用一把 HS256 签名密钥：插件清单里写 <Text code>auth.app</Text>
            ，客户端拿密钥换网关的 JWT。列表只给指纹，点「查看密钥」才取回明文，每次取回都记审计。
          </Text>

          {!enabled ? (
            <div>
              <Text type="warning" size="small">
                凭证存储未启用：后端进程没读到环境变量 SECRET_KEK，应用密钥没法解密。
              </Text>
              <Text type="tertiary" size="small" style={{ display: 'block', marginTop: 6 }}>
                设置后重启后端再回来查看；这个值要长期留存，换掉它之前存过的密钥就解不开了。
              </Text>
            </div>
          ) : null}

          <section>
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
              <Text strong>全部应用（{items.length}）</Text>
              <Button
                size="small"
                icon={<IconRefresh />}
                loading={isFetching}
                onClick={() => void refetch()}
              >
                刷新
              </Button>
            </div>

            <div style={{ marginTop: 8 }}>
              {items.length === 0 ? (
                <Text type="tertiary" size="small">
                  还没有应用用过密钥：会话里让 AI 接管 MCP 应用（清单里带 auth.app）时会自动生成。
                </Text>
              ) : (
                items.map((item) => (
                  <div
                    key={item.app}
                    style={{
                      display: 'flex',
                      alignItems: 'flex-start',
                      gap: 8,
                      padding: '8px 0',
                      borderBottom: '1px solid var(--semi-color-border)',
                    }}
                  >
                    <div style={{ flex: 1, minWidth: 0 }}>
                      <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                        <Text strong>{item.app}</Text>
                        {item.legacy ? (
                          <Tag size="small" color="orange">
                            无归属
                          </Tag>
                        ) : null}
                        {item.has_key ? null : (
                          <Tag size="small" color="grey">
                            无密钥
                          </Tag>
                        )}
                      </div>
                      <Text type="tertiary" size="small">
                        {item.legacy
                          ? '历史数据，没有创建人记录'
                          : `创建人 ${item.owner_name || '未知'}`}
                        {item.fingerprint ? ` · 指纹 ${item.fingerprint}` : ''}
                        {formatTime(item.updated_at || item.created_at)
                          ? ` · ${formatTime(item.updated_at || item.created_at)}`
                          : ''}
                      </Text>
                      {keys[item.app] ? (
                        <Text code style={{ display: 'block', marginTop: 6, wordBreak: 'break-all' }}>
                          {keys[item.app]}
                        </Text>
                      ) : null}
                    </div>
                    {keys[item.app] ? (
                      <Button theme="borderless" size="small" onClick={() => void copy(keys[item.app])}>
                        复制
                      </Button>
                    ) : null}
                    <Button
                      theme="borderless"
                      size="small"
                      disabled={!item.has_key}
                      loading={busy === item.app}
                      onClick={() => void toggle(item)}
                    >
                      {keys[item.app] ? '隐藏' : '查看密钥'}
                    </Button>
                  </div>
                ))
              )}
            </div>
          </section>
        </div>
      )}
    </Modal>
  );
}
