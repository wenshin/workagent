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

/** 审批档位是平台内建字段，键名与后端 tool.ApprovalPolicyKey 一致。 */
const APPROVAL_POLICY_KEY = 'approval_policy';
/** 最松的一档：选中它等于不再有人工关卡，视觉上要和另一档区分开。 */
const APPROVAL_OFF = 'off';
/** 界面第一档：按清单声明拦有风险的调用，「需要用户审批」就是它。 */
const APPROVAL_DECLARED = 'declared';
/** 已从界面移除的旧档位：每次都问。旧配置仍按它执行，只会更严。 */
const APPROVAL_ALWAYS = 'always';

/**
 * 把存储值映射到界面上的档位。
 *
 * 只影响高亮，不改提交值：always 若被改写成 declared，用户只改了个 AppKey
 * 点保存，插件就从「每次都问」静默放宽成「只拦有风险的调用」。
 */
function approvalOptionOf(value: string) {
  return value === APPROVAL_ALWAYS ? APPROVAL_DECLARED : value;
}

interface Props {
  visible: boolean;
  spaceId?: string;
  slug?: string;
  name?: string;
  onClose: () => void;
}

/**
 * 插件在当前空间的配置：已安装件写安装记录，未发布的草稿写草稿记录。
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
          {/* 这句话管整个弹窗（作用范围与生效时机），字段的说明管单个开关：
              用更淡的层级，避免两句小字看起来是同一类信息 */}
          <Text type="quaternary" size="small">
            {data.scope === 'global'
              ? '平台内置件：这份配置对全部空间生效。'
              : '只对当前空间生效，保存后立即生效。'}
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
                {/* 说明与事实都放在选项前面：先知道「这个开关管什么、它能碰什么」，
                    再看选哪一档。放在控件下面会被读成脚注，没人往下看。 */}
                {field.help ? (
                  <Text type="tertiary" size="small">
                    {field.help}
                  </Text>
                ) : null}
                {field.note ? <div className="publish-note">{field.note}</div> : null}
                {field.type === 'boolean' ? (
                  <Switch
                    checked={values[field.key] === 'true'}
                    onChange={(v) =>
                      setValues((prev) => ({ ...prev, [field.key]: v ? 'true' : 'false' }))
                    }
                  />
                ) : field.type === 'select' && field.key === APPROVAL_POLICY_KEY ? (
                  // 只留卡片：上面再放一个下拉框就是同一件事两个控件，用户会不确定
                  // 「到底点哪个才算保存」，而且下拉框只能看到当前选中那一句说明。
                  <div className="approval-levels" role="radiogroup" aria-label={field.label}>
                    {(field.options ?? []).map((op) => {
                      const active = approvalOptionOf(values[field.key] ?? '') === op.value;
                      return (
                        <button
                          key={op.value}
                          type="button"
                          role="radio"
                          aria-checked={active}
                          className={`approval-level${active ? ' is-active' : ''}${
                            op.value === APPROVAL_OFF ? ' is-danger' : ''
                          }`}
                          onClick={() => setValues((prev) => ({ ...prev, [field.key]: op.value }))}
                        >
                          <span className="approval-level-head">
                            {op.label}
                            {active ? <span className="approval-level-now">当前</span> : null}
                          </span>
                          {op.help ? <span className="approval-level-help">{op.help}</span> : null}
                        </button>
                      );
                    })}
                  </div>
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
                {field.key !== APPROVAL_POLICY_KEY && selected?.help ? (
                  <Text type="tertiary" size="small">
                    {selected.help}
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
