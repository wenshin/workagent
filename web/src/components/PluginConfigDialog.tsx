import { Empty, Input, Modal, Spin, Toast, Typography } from '@douyinfe/semi-ui';
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
 * 只改「安装」这一层（MCP 的服务地址与鉴权），发布快照不动；
 * 凭证只填引用名，密钥本身在「密钥配置」里维护，不在这里存明文。
 */
export default function PluginConfigDialog({ visible, spaceId, slug, name, onClose }: Props) {
  const { data, isLoading } = usePluginConfig(spaceId, slug, visible);
  const update = useUpdatePluginConfig();
  const [values, setValues] = useState<Record<string, string>>({});

  useEffect(() => {
    if (!visible) return;
    setValues(data?.values ?? {});
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
          description="技能、工具与组件的参数由插件清单固定；需要改行为请修改内容后重新发布。"
        />
      ) : (
        <div className="publish-form">
          <Text type="tertiary" size="small">
            只影响当前空间，不修改发布快照；保存后立即重建运行时。
          </Text>
          {data.fields.map((field) => (
            <div key={field.key} className="publish-field">
              <label className="publish-label">
                {field.label}
                {field.required ? <span className="publish-required"> *</span> : null}
              </label>
              <Input
                value={values[field.key] ?? ''}
                placeholder={field.placeholder}
                onChange={(v) => setValues((prev) => ({ ...prev, [field.key]: v }))}
              />
              {field.help ? (
                <Text type="tertiary" size="small">
                  {field.help}
                </Text>
              ) : null}
            </div>
          ))}
        </div>
      )}
    </Modal>
  );
}
