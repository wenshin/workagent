import { IconPlus } from '@douyinfe/semi-icons';
import { Button, Input, Modal, Popconfirm, Tag, Toast, Typography } from '@douyinfe/semi-ui';
import { useEffect, useState } from 'react';

import { useAdminSettings, useDeleteSecret, useSetSecret } from '../api/hooks';
import type { SecretItem } from '../api/types';

const { Text } = Typography;

interface Props {
  visible: boolean;
  onClose: () => void;
  /** 从插件卡片进来时带上「缺的那条凭证」：直接展开录入表单，不用自己找名字 */
  preset?: { scope: string; name: string } | null;
}

// 与后端 secret.validateName 一致，先在前端拦一次，避免白跑一趟
const NAME_RE = /^[a-zA-Z0-9_-]{1,64}$/;

function formatTime(iso?: string) {
  if (!iso) return '';
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return '';
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

/**
 * 密钥配置（仅系统管理员）。
 *
 * 密钥只写不读：列表里只有名称、指纹与更新时间，明文永远不会从后端返回。
 * 名称对应 source 插件清单里的 source.secret，值改完立即生效；
 * 新增其他名称只是「待用条目」，只有被引用的那一条会真正生效。
 */
export default function SecretSettingsDialog({ visible, onClose, preset }: Props) {
  const { data, isLoading, error } = useAdminSettings(visible);
  const setSecret = useSetSecret();
  const deleteSecret = useDeleteSecret();

  // 表单：editing 为正在更新的名称，null 表示新增
  const [formOpen, setFormOpen] = useState(false);
  const [editing, setEditing] = useState<string | null>(null);
  // scope 必须跟着条目走：搜索凭证在 search，插件 secret.use 在 plugin，
  // 统一按 refs[0] 写会把更新落到另一个 scope 上，等于凭空多出一条。
  const [formScope, setFormScope] = useState('search');
  const [name, setName] = useState('');
  const [value, setValue] = useState('');

  const refs = data?.refs ?? [];
  const items = data?.items ?? [];
  const enabled = data?.secrets_enabled ?? false;
  // 环境变量提供的引用不在界面上管理：那里没有可录入的条目
  const envRefs = refs.filter((r) => r.source === 'env');

  useEffect(() => {
    if (!visible) {
      setFormOpen(false);
      setEditing(null);
      setFormScope('search');
      setName('');
      setValue('');
    }
  }, [visible]);

  // 从插件卡片「配置密钥」进来：名称由清单固定（改了就引用不上），直接展开录入，
  // 用户只需要粘贴值。名称框仍可改，但预填对了就不用动。
  useEffect(() => {
    if (!visible || !preset) return;
    setFormScope(preset.scope);
    setName(preset.name);
    setValue('');
    setEditing(null);
    setFormOpen(true);
  }, [visible, preset?.scope, preset?.name]);

  const openAdd = () => {
    // 平台引用的那条还没配过时直接带上名称（含 ${} 引用里的名字），
    // 这样即便当前靠环境变量在跑，新增一条同名凭证就能接管。
    const referenced = refs[0]?.name ?? '';
    const exists = items.some((it) => it.name === referenced);
    setFormScope(refs[0]?.scope ?? 'search');
    setName(!referenced || exists ? '' : referenced);
    setValue('');
    setEditing(null);
    setFormOpen(true);
  };

  const openEdit = (item: SecretItem) => {
    setFormScope(item.scope);
    setName(item.name);
    setValue('');
    setEditing(item.name);
    setFormOpen(true);
  };

  const closeForm = () => {
    setFormOpen(false);
    setEditing(null);
    setName('');
    setValue('');
  };

  const save = async () => {
    const target = editing ?? name.trim();
    const trimmedValue = value.trim();
    if (!NAME_RE.test(target)) {
      Toast.warning('名称只允许字母、数字、下划线、短横线，且不超过 64 字符');
      return;
    }
    if (!trimmedValue) {
      Toast.warning('请填写密钥值');
      return;
    }
    try {
      await setSecret.mutateAsync({ scope: formScope, name: target, value: trimmedValue });
      // 「会不会真正生效」以列表里的标记为准：引用关系由插件清单在运行期声明，
      // 前端拿不到（refs 为空是常态），这里不猜。
      Toast.success(editing ? '密钥已更新，下一次调用立即生效' : '密钥已添加，下一次调用立即生效');
      closeForm();
    } catch (err) {
      Toast.error(err instanceof Error ? err.message : '保存失败');
    }
  };

  const remove = async (item: SecretItem) => {
    try {
      await deleteSecret.mutateAsync({ scope: item.scope, name: item.name });
      Toast.success(item.in_use ? '已删除，引用它的能力立即不可用' : '已删除');
    } catch (err) {
      Toast.error(err instanceof Error ? err.message : '删除失败');
    }
  };

  return (
    <Modal
      title="密钥配置"
      visible={visible}
      onCancel={onClose}
      footer={
        <div style={{ display: 'flex', justifyContent: 'flex-end' }}>
          <Button onClick={onClose}>关闭</Button>
        </div>
      }
      width={620}
      bodyStyle={{ paddingTop: 8 }}
    >
      {error ? (
        <Text type="danger">{error instanceof Error ? error.message : '读取密钥状态失败'}</Text>
      ) : isLoading || !data ? (
        <Text type="tertiary">加载中…</Text>
      ) : (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
          <Text type="tertiary" size="small">
            密钥加密保存在服务端存储目录，不进入空间文件树，也不进备份快照；接口只返回指纹。
          </Text>

          {envRefs.length > 0 ? (
            <div>
              <Text size="small">
                平台当前用的是后端环境变量（凭证存储里没有同名条目）：
              </Text>
              <Text code style={{ display: 'block', marginTop: 6 }}>
                {envRefs.map((r) => r.name).join(', ')}
              </Text>
              <Text type="tertiary" size="small" style={{ display: 'block', marginTop: 6 }}>
                在这里新增一条同名密钥就会覆盖环境变量，且改完立即生效，不用重启。
              </Text>
            </div>
          ) : null}

          {!enabled && envRefs.length === 0 ? (
            <div>
              <Text type="warning" size="small">
                凭证存储未启用：后端进程没读到环境变量 SECRET_KEK，密钥没法加密落盘。
              </Text>
              <Text type="tertiary" size="small" style={{ display: 'block', marginTop: 6 }}>
                在启动后端的那个终端 / 容器里设置一次（至少 16 个字符），然后重启后端：
              </Text>
              <Text code style={{ display: 'block', marginTop: 6 }}>
                SECRET_KEK=$(openssl rand -base64 32) make run
              </Text>
              <Text type="tertiary" size="small" style={{ display: 'block', marginTop: 6 }}>
                这个值要长期留存：换掉它，之前存过的密钥就解不开了，需要重新录入。
              </Text>
            </div>
          ) : null}

          {enabled ? (
            <>
              <section>
                <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
                  <Text strong>已保存的密钥</Text>
                  {!formOpen ? (
                    <Button size="small" icon={<IconPlus />} onClick={openAdd}>
                      新增
                    </Button>
                  ) : null}
                </div>
                {refs.length > 0 ? (
                  <Text type="tertiary" size="small" style={{ display: 'block', marginTop: 4 }}>
                    平台当前使用：{refs.map((r) => `${r.label} → ${r.name}`).join('、')}
                    ；其余条目只是备用，不会生效。
                  </Text>
                ) : null}
                <div style={{ marginTop: 8 }}>
                  {items.length === 0 ? (
                    <Text type="tertiary" size="small">
                      还没有保存过密钥，点右上角「新增」录入。
                    </Text>
                  ) : (
                    items.map((item) => (
                      <div
                        key={`${item.scope}/${item.name}`}
                        style={{
                          display: 'flex',
                          alignItems: 'center',
                          gap: 8,
                          padding: '8px 0',
                          borderBottom: '1px solid var(--semi-color-border)',
                        }}
                      >
                        <div style={{ flex: 1, minWidth: 0 }}>
                          <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                            <Text strong>{item.name}</Text>
                            <Tag size="small" color="blue">
                              {item.scope}
                            </Tag>
                            <Tag size="small" color={item.in_use ? 'green' : 'grey'}>
                              {item.in_use ? '使用中' : '未引用'}
                            </Tag>
                          </div>
                          <Text type="tertiary" size="small">
                            指纹 {item.fingerprint} · {formatTime(item.updated_at)}
                          </Text>
                        </div>
                        <Button
                          theme="borderless"
                          size="small"
                          disabled={formOpen && editing === item.name && formScope === item.scope}
                          onClick={() => openEdit(item)}
                        >
                          更新
                        </Button>
                        <Popconfirm
                          title="删除这条密钥？"
                          content={item.in_use ? '删除后引用它的能力会立即不可用' : undefined}
                          onConfirm={() => void remove(item)}
                        >
                          <Button type="danger" theme="borderless" size="small">
                            删除
                          </Button>
                        </Popconfirm>
                      </div>
                    ))
                  )}
                </div>
              </section>

              {formOpen ? (
                <section>
                  <Text strong>
                    {editing ? `更新 ${formScope}/${editing}` : '新增密钥'}
                  </Text>
                  <div style={{ display: 'flex', gap: 8, marginTop: 8 }}>
                    <Input
                      value={name}
                      onChange={setName}
                      disabled={editing !== null}
                      placeholder="名称，如 bocha"
                      style={{ width: 200 }}
                    />
                    <Input
                      mode="password"
                      value={value}
                      onChange={setValue}
                      placeholder="密钥值"
                      style={{ flex: 1 }}
                    />
                    <Button theme="solid" type="primary" loading={setSecret.isPending} onClick={save}>
                      保存
                    </Button>
                    <Button onClick={closeForm}>取消</Button>
                  </div>
                  <Text type="tertiary" size="small" style={{ display: 'block', marginTop: 8 }}>
                    名称必须与插件清单声明的一致：搜索源看 <Text code>source.secret</Text>
                    （如 <Text code>bocha</Text>），插件看 <Text code>secret.use</Text> 的 name。
                    名称保存后不可改，换个名字等于新增一条。
                  </Text>
                </section>
              ) : null}
            </>
          ) : null}
        </div>
      )}
    </Modal>
  );
}
