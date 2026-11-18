import {
  Empty,
  Input,
  Modal,
  Select,
  Spin,
  Switch,
  TextArea,
  Toast,
  Typography,
} from '@douyinfe/semi-ui';
import { useEffect, useState } from 'react';

import { usePluginConfig, useUpdatePluginConfig } from '../api/hooks';

const { Text } = Typography;

interface Props {
  visible: boolean;
  spaceId?: string;
  slug?: string;
  name?: string;
  onClose: () => void;
}

/**
 * 已安装插件的空间级配置。
 *
 * 字段定义来自后端：平台内建的那几项（MCP 地址、审批档位、shell 名单）
 * 加清单 config 段声明的通用项，前端只按 type 渲染，不写死字段名。
 * 只改「安装」这一层，发布快照不动；凭证只填引用名，密钥在「密钥配置」里维护。
 */
export default function PluginConfigDialog({ visible, spaceId, slug, name, onClose }: Props) {
  const { data, isLoading } = usePluginConfig(spaceId, slug, visible);
  const update = useUpdatePluginConfig();
  const [values, setValues] = useState<Record<string, string>>({});

  useEffect(() => {
    if (!visible) return;
    // 开关型字段没有「空」这个状态：补一个 false 再存，否则用户不动它
    // 直接保存，插件拿到的就是「这个键不存在」，与界面上看到的不一致。
    const next: Record<string, string> = { ...(data?.values ?? {}) };
    for (const f of data?.fields ?? []) {
      if (f.type === 'boolean' && next[f.key] === undefined) next[f.key] = 'false';
    }
    setValues(next);
  }, [visible, data]);

  const save = async () => {
    if (!spaceId || !slug) return;
    try {
      await update.mutateAsync({ spaceId, slug, values });
      Toast.success('配置已保存，已按新配置重载插件');
      onClose();
    } catch (e) {
      Toast.error(e instanceof Error ? e.message : '保存失败');
    }
  };

  return (
    <Modal
      title={`配置${name ? ` · ${name}` : ''}`}
      visible={visible}
      onCancel={onClose}
      onOk={() => void save()}
      okText="保存"
      cancelText="取消"
      okButtonProps={{ loading: update.isPending }}
      maskClosable={!update.isPending}
      width={520}
    >
      {isLoading ? (
        <div className="panel-empty">
          <Spin />
        </div>
      ) : !data || data.fields.length === 0 ? (
        <Empty
          title="该插件没有可配置项"
          description="只有脚本工具声明了 config 段、或 MCP 需要填地址时才有表单；其余参数属于发布内容，改行为请改清单后重新发布。"
        />
      ) : (
        <div className="publish-form">
          <Text type="tertiary" size="small">
            {data.scope === 'global'
              ? '平台内置件：这份配置对全部空间生效（内置件只装在系统空间一份）。'
              : '只影响当前空间，不修改发布快照；保存后立即重建运行时。'}
          </Text>
          {data.fields.map((field) => {
            // 选中项的说明随选择变化：档位的三个选项后果差别很大，
            // 只给一句固定的字段说明，用户得自己猜「这个档会怎样」。
            const selected = field.options?.find((o) => o.value === (values[field.key] ?? ''));
            return (
              <div key={field.key} className="publish-field">
                <label className="publish-label">
                  {field.label}
                  {field.required ? <span className="publish-required"> *</span> : null}
                </label>
                {field.type === 'boolean' ? (
                  <Switch
                    checked={values[field.key] === 'true'}
                    onChange={(v) =>
                      setValues((prev) => ({ ...prev, [field.key]: v ? 'true' : 'false' }))
                    }
                  />
                ) : field.type === 'select' ? (
                  <Select
                    value={values[field.key] || field.options?.[0]?.value}
                    onChange={(v) =>
                      setValues((prev) => ({ ...prev, [field.key]: v === undefined ? '' : String(v) }))
                    }
                    style={{ width: '100%' }}
                  >
                    {(field.options ?? []).map((op) => (
                      <Select.Option key={op.value} value={op.value} label={op.label} />
                    ))}
                  </Select>
                ) : field.type === 'list' || field.type === 'textarea' ? (
                  <TextArea
                    rows={4}
                    value={values[field.key] ?? ''}
                    placeholder={field.placeholder}
                    onChange={(v) => setValues((prev) => ({ ...prev, [field.key]: v }))}
                  />
                ) : (
                  <Input
                    value={values[field.key] ?? ''}
                    placeholder={
                      field.placeholder ??
                      (field.type === 'number'
                        ? '整数'
                        : field.type === 'secret_ref'
                          ? '密钥配置里的名称'
                          : undefined)
                    }
                    onChange={(v) => setValues((prev) => ({ ...prev, [field.key]: v }))}
                  />
                )}
                {selected?.help ? (
                  <Text type="tertiary" size="small">
                    {selected.help}
                  </Text>
                ) : null}
                {field.help ? (
                  <Text type="tertiary" size="small">
                    {field.help}
                  </Text>
                ) : null}
              </div>
            );
          })}
        </div>
      )}
    </Modal>
  );
}
