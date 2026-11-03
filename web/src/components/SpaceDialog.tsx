import { Input, Modal, TextArea, Toast } from '@douyinfe/semi-ui';
import { useEffect, useState } from 'react';

import {
  useCreateDir,
  useCreateSkill,
  useCreateSpace,
  useDeleteDir,
  useDeleteSpace,
  useRenameDir,
  useRenameSpace,
} from '../api/hooks';
import type { Space, SpaceDir } from '../api/types';

export type DialogState =
  | { kind: 'none' }
  | { kind: 'create-root' }
  | { kind: 'create-dir'; space: Space; parent?: SpaceDir }
  | { kind: 'create-skill'; space: Space }
  | { kind: 'rename'; target: Space }
  | { kind: 'delete'; target: Space }
  | { kind: 'rename-dir'; space: Space; dir: SpaceDir }
  | { kind: 'delete-dir'; space: Space; dir: SpaceDir };

interface Props {
  state: DialogState;
  onClose: () => void;
}

function describe(state: DialogState) {
  switch (state.kind) {
    case 'create-root':
      return {
        title: '新建空间',
        desc: '创建一个共享空间，用于组织会话、目录与技能。空间只有一级，更细的分类用目录表达。',
        placeholder: '空间名称',
        ok: '创建',
      };
    case 'create-dir':
      return {
        title: '新建目录',
        desc: state.parent
          ? `在「${state.parent.name}」下创建目录：文件系统里的真实目录，可以继续建子目录或新建会话。`
          : `在「${state.space.name}」下创建目录：文件系统里的真实目录，可以继续建子目录或新建会话。`,
        placeholder: '目录名称',
        ok: '创建',
      };
    case 'create-skill':
      return {
        title: '新建技能',
        desc: `在「${state.space.name}」下创建技能：本质是一个带 .meta.json 的特殊目录，可在其中继续建目录和会话。`,
        placeholder: '技能名称',
        ok: '创建',
      };
    case 'rename':
      return {
        title: '重命名空间',
        desc: '仅修改显示名称，物理路径（space_id）与内容不变。',
        placeholder: '空间名称',
        ok: '保存',
      };
    case 'delete':
      return {
        title: '删除空间',
        desc: `将删除「${state.target.name}」及其所有目录与会话（进入回收站）。请输入空间名确认：${state.target.name}`,
        placeholder: `输入 ${state.target.name} 以确认`,
        ok: '删除',
      };
    case 'rename-dir':
      return {
        title: state.dir.is_skill ? '重命名技能' : '重命名目录',
        desc: state.dir.is_skill
          ? '目录名就是真实路径，改名会移动目录并同步 .meta.json 里的名称，目录下的会话会一起迁移。'
          : '目录名就是文件系统里的真实路径，改名会移动目录，目录下的会话会一起迁移。',
        placeholder: '新名称',
        ok: '保存',
      };
    case 'delete-dir':
      return {
        title: state.dir.is_skill ? '删除技能' : '删除目录',
        desc: `将删除「${state.dir.name}」及其中的全部文件（不进入回收站）；其中的会话不会丢失，会移回空间根。请输入名称确认：${state.dir.name}`,
        placeholder: `输入 ${state.dir.name} 以确认`,
        ok: '删除',
      };
    default:
      return { title: '', desc: '', placeholder: '', ok: '确定' };
  }
}

export default function SpaceDialog({ state, onClose }: Props) {
  const open = state.kind !== 'none';
  const meta = describe(state);

  const [value, setValue] = useState('');
  const [description, setDescription] = useState('');
  const [error, setError] = useState('');

  const createSpace = useCreateSpace();
  const renameSpace = useRenameSpace();
  const deleteSpace = useDeleteSpace();
  const createDir = useCreateDir();
  const createSkill = useCreateSkill();
  const renameDir = useRenameDir();
  const deleteDir = useDeleteDir();

  const pending =
    createSpace.isPending ||
    renameSpace.isPending ||
    deleteSpace.isPending ||
    createDir.isPending ||
    createSkill.isPending ||
    renameDir.isPending ||
    deleteDir.isPending;

  useEffect(() => {
    if (!open) return;
    setError('');
    setDescription('');
    if (state.kind === 'rename') setValue(state.target.name);
    else if (state.kind === 'rename-dir') setValue(state.dir.name);
    else setValue('');
  }, [open, state]);

  const submit = async () => {
    setError('');
    const name = value.trim();

    try {
      switch (state.kind) {
        case 'create-root':
          if (!name) return setError('请输入空间名称');
          await createSpace.mutateAsync({ name });
          break;
        case 'create-dir':
          if (!name) return setError('请输入目录名称');
          await createDir.mutateAsync({ spaceId: state.space.id, name, parent: state.parent?.path });
          break;
        case 'create-skill':
          if (!name) return setError('请输入技能名称');
          await createSkill.mutateAsync({
            spaceId: state.space.id,
            name,
            description: description.trim(),
          });
          break;
        case 'rename':
          if (!name) return setError('请输入空间名称');
          await renameSpace.mutateAsync({ id: state.target.id, name });
          break;
        case 'delete':
          if (name !== state.target.name) return setError('输入的空间名与当前空间不一致');
          await deleteSpace.mutateAsync({ id: state.target.id, confirm_name: name });
          break;
        case 'rename-dir':
          if (!name) return setError('请输入名称');
          await renameDir.mutateAsync({ spaceId: state.space.id, path: state.dir.path, nextName: name });
          break;
        case 'delete-dir':
          if (name !== state.dir.name) return setError('输入的名称与当前目录不一致');
          await deleteDir.mutateAsync({ spaceId: state.space.id, path: state.dir.path });
          break;
        default:
          return;
      }
      Toast.success(
        state.kind === 'delete' || state.kind === 'delete-dir'
          ? '已删除'
          : state.kind === 'create-skill'
            ? '技能已创建'
            : '已保存',
      );
      onClose();
    } catch (e) {
      setError(e instanceof Error ? e.message : '操作失败，请重试');
    }
  };

  const danger = state.kind === 'delete' || state.kind === 'delete-dir';

  return (
    <Modal
      title={meta.title}
      visible={open}
      onCancel={onClose}
      onOk={submit}
      okText={meta.ok}
      cancelText="取消"
      okButtonProps={{ loading: pending, type: danger ? 'danger' : 'primary' }}
      maskClosable={!pending}
      closeOnEsc={!pending}
    >
      <p className="dialog-desc">{meta.desc}</p>
      <Input
        value={value}
        onChange={setValue}
        placeholder={meta.placeholder}
        maxLength={64}
        autoFocus
        onEnterPress={submit}
        disabled={pending}
      />
      {state.kind === 'create-skill' ? (
        <TextArea
          className="dialog-desc-input"
          value={description}
          onChange={setDescription}
          placeholder="技能描述（可选），用于说明这个技能解决什么问题"
          maxLength={500}
          autosize={{ minRows: 2, maxRows: 4 }}
          disabled={pending}
        />
      ) : null}
      <div className="dialog-error">{error}</div>
    </Modal>
  );
}
