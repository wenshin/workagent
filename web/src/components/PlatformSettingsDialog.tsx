import { Button, Input, Modal, Tag, Toast, Typography } from '@douyinfe/semi-ui';
import { useEffect, useState } from 'react';

import { useAdminSettings, useDeleteSecret, useSetSecret } from '../api/hooks';

const { Text } = Typography;

interface Props {
  visible: boolean;
  onClose: () => void;
}

/** search.SecretScope 的取值；凭证按 scope 分文件存放 */
const SEARCH_SCOPE = 'search';

function formatTime(iso?: string) {
  if (!iso) return '未设置';
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return '未设置';
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

/**
 * 平台配置与凭证维护（仅系统管理员）。
 *
 * 密钥只写不读：页面只能看到指纹与更新时间，明文永远不会从后端返回。
 * provider / endpoint / 白名单来自系统空间的 config.json 与 net_whitelist.json，
 * 改完需要重启；密钥本身存的是加密文件，改完立即生效。
 */
export default function PlatformSettingsDialog({ visible, onClose }: Props) {
  const { data, isLoading, error } = useAdminSettings(visible);
  const setSecret = useSetSecret();
  const deleteSecret = useDeleteSecret();
  const [value, setValue] = useState('');

  useEffect(() => {
    if (!visible) setValue('');
  }, [visible]);

  const search = data?.search;
  const enabled = data?.secrets_enabled ?? false;

  const save = async () => {
    if (!search) return;
    const trimmed = value.trim();
    if (!trimmed) {
      Toast.warning('请先粘贴密钥');
      return;
    }
    try {
      await setSecret.mutateAsync({ scope: SEARCH_SCOPE, name: search.secret_name, value: trimmed });
      setValue('');
      Toast.success('密钥已更新，下一次搜索立即生效');
    } catch (err) {
      Toast.error(err instanceof Error ? err.message : '保存失败');
    }
  };

  const remove = async () => {
    if (!search) return;
    try {
      await deleteSecret.mutateAsync({ scope: SEARCH_SCOPE, name: search.secret_name });
      Toast.success('密钥已删除，web_search 将不可用');
    } catch (err) {
      Toast.error(err instanceof Error ? err.message : '删除失败');
    }
  };

  const footer = (
    <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8 }}>
      <Button onClick={onClose}>关闭</Button>
    </div>
  );

  return (
    <Modal
      title="平台配置"
      visible={visible}
      onCancel={onClose}
      footer={footer}
      width={620}
      bodyStyle={{ paddingTop: 8 }}
    >
      {error ? (
        <Text type="danger">{error instanceof Error ? error.message : '读取平台配置失败'}</Text>
      ) : isLoading || !search ? (
        <Text type="tertiary">加载中…</Text>
      ) : (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 20 }}>
          <section>
            <Text strong>搜索接入</Text>
            <div style={{ marginTop: 8, display: 'flex', flexDirection: 'column', gap: 6 }}>
              <Text type="tertiary" size="small">
                provider：{search.provider || '未配置'}
              </Text>
              <Text type="tertiary" size="small">
                endpoint：{search.endpoint || '未配置'}
              </Text>
              <div style={{ display: 'flex', alignItems: 'center', gap: 6, flexWrap: 'wrap' }}>
                <Text type="tertiary" size="small">
                  出网白名单：
                </Text>
                {search.allow.length === 0 ? (
                  <Text type="warning" size="small">
                    空（不允许任何外部请求）
                  </Text>
                ) : (
                  search.allow.map((domain) => (
                    <Tag key={domain} size="small">
                      {domain}
                    </Tag>
                  ))
                )}
              </div>
              <Text type="tertiary" size="small">
                以上三项来自系统空间 <Text code>files/config.json</Text> 与{' '}
                <Text code>files/net_whitelist.json</Text>，改完需要重启后端。
              </Text>
            </div>
          </section>

          <section>
            <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
              <Text strong>搜索密钥</Text>
              <Tag size="small" color={search.secret_present ? 'green' : 'red'}>
                {search.secret_present ? '已配置' : '未配置'}
              </Tag>
            </div>

            {!enabled ? (
              <Text type="warning" size="small" style={{ display: 'block', marginTop: 8 }}>
                凭证存储未启用：需要在后端环境里配置 SECRET_KEK（至少 16 个字符）并重启。
              </Text>
            ) : (
              <>
                <div style={{ marginTop: 8, display: 'flex', flexDirection: 'column', gap: 4 }}>
                  <Text type="tertiary" size="small">
                    凭证名：<Text code>{search.secret_name}</Text>
                  </Text>
                  <Text type="tertiary" size="small">
                    指纹：{search.fingerprint ? <Text code>{search.fingerprint}</Text> : '未设置'}
                  </Text>
                  <Text type="tertiary" size="small">
                    更新时间：{formatTime(search.updated_at)}
                  </Text>
                </div>

                <div style={{ marginTop: 12, display: 'flex', gap: 8 }}>
                  <Input
                    mode="password"
                    value={value}
                    onChange={setValue}
                    placeholder="粘贴新的密钥，保存后只显示指纹"
                    style={{ flex: 1 }}
                  />
                  <Button theme="solid" type="primary" loading={setSecret.isPending} onClick={save}>
                    保存
                  </Button>
                  <Button
                    type="danger"
                    disabled={!search.secret_present}
                    loading={deleteSecret.isPending}
                    onClick={remove}
                  >
                    删除
                  </Button>
                </div>
                <Text type="tertiary" size="small" style={{ display: 'block', marginTop: 8 }}>
                  密钥加密后存在后端存储目录，不会进入空间文件树，也不会进备份快照；保存后下一次搜索立即生效。
                </Text>
              </>
            )}
          </section>
        </div>
      )}
    </Modal>
  );
}
