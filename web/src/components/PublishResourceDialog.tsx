import { Input, Modal, Radio, Select, TextArea, Toast, Typography } from '@douyinfe/semi-ui';
import { useEffect, useMemo, useState } from 'react';

import { usePublishResource, useSkills, useSpaces } from '../api/hooks';

const { Text } = Typography;

interface Props {
  visible: boolean;
  onClose: () => void;
}

const VERSION_RE = /^\d+\.\d+\.\d+$/;

/** 发布草稿：版本快照不可变，再次发布同名技能 = 追加版本 */
export default function PublishResourceDialog({ visible, onClose }: Props) {
  const { data: spaces } = useSpaces();
  const publish = usePublishResource();

  const [spaceId, setSpaceId] = useState('');
  const [path, setPath] = useState('');
  const [version, setVersion] = useState('1.0.0');
  const [notes, setNotes] = useState('');
  const [visibility, setVisibility] = useState<'public' | 'private'>('private');

  const { data: skills } = useSkills(spaceId || undefined);
  // 只有本空间自己的草稿可以发布；系统空间内置技能属于平台托管内容，不可直接发布
  const options = useMemo(
    () =>
      (skills ?? [])
        .filter((s) => s.source === 'current' && s.path.endsWith('SKILL.md'))
        .map((s) => ({ value: s.path.replace(/\/SKILL\.md$/, ''), label: s.name })),
    [skills],
  );

  useEffect(() => {
    if (!visible) return;
    setPath('');
    setVersion('1.0.0');
    setNotes('');
    setVisibility('private');
    const personal = (spaces ?? []).find((s) => s.type === 'personal');
    setSpaceId((prev) => prev || personal?.id || spaces?.[0]?.id || '');
  }, [visible, spaces]);

  const submit = async () => {
    if (!spaceId) {
      Toast.error('请选择空间');
      return;
    }
    if (!path) {
      Toast.error('请选择要发布的技能');
      return;
    }
    if (!VERSION_RE.test(version.trim())) {
      Toast.error('版本号需要形如 1.0.0');
      return;
    }
    try {
      const res = await publish.mutateAsync({
        spaceId,
        path,
        version: version.trim(),
        notes: notes.trim(),
        visibility,
      });
      Toast.success(`已发布「${res.resource.name}」v${res.resource.latest_version}`);
      onClose();
    } catch (err) {
      Toast.error(err instanceof Error ? err.message : '发布失败');
    }
  };

  return (
    <Modal
      title="发布资源"
      visible={visible}
      onCancel={onClose}
      onOk={() => void submit()}
      okText="发布"
      cancelText="取消"
      confirmLoading={publish.isPending}
      maskClosable={!publish.isPending}
      closeOnEsc={!publish.isPending}
    >
      <div className="publish-form">
        <div className="publish-field">
          <Text type="tertiary" size="small">
            来源空间
          </Text>
          <Select
            value={spaceId || undefined}
            placeholder="选择空间"
            onChange={(v) => {
              setSpaceId(String(v ?? ''));
              setPath('');
            }}
            optionList={(spaces ?? []).map((s) => ({
              value: s.id,
              label: s.type === 'system' ? `${s.name}（系统空间）` : s.name,
            }))}
          />
        </div>

        <div className="publish-field">
          <Text type="tertiary" size="small">
            技能草稿
          </Text>
          <Select
            value={path || undefined}
            placeholder={options.length ? '选择技能' : '该空间下没有带正文的技能'}
            onChange={(v) => setPath(String(v ?? ''))}
            optionList={options}
            emptyContent="该空间下没有可发布的技能"
          />
        </div>

        <div className="publish-field">
          <Text type="tertiary" size="small">
            版本号
          </Text>
          <Input value={version} onChange={setVersion} placeholder="1.0.0" />
        </div>

        <div className="publish-field">
          <Text type="tertiary" size="small">
            用户范围
          </Text>
          <Radio.Group type="button" value={visibility} onChange={(e) => setVisibility(e.target.value as 'public' | 'private')}>
            <Radio value="private">仅自己可见</Radio>
            <Radio value="public">全部可见</Radio>
          </Radio.Group>
        </div>

        <div className="publish-field">
          <Text type="tertiary" size="small">
            发布说明
          </Text>
          <TextArea value={notes} onChange={setNotes} rows={2} maxCount={500} placeholder="这次发布改了什么" />
        </div>

        <Text type="tertiary" size="small">
          发布件与工作空间分开存储，版本一旦发布不可修改；要改内容请发布新版本。
        </Text>
      </div>
    </Modal>
  );
}
