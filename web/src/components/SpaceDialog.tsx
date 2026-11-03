import { Input, Modal, Toast } from '@douyinfe/semi-ui';
import { useEffect, useState } from 'react';

import { useCreateDir, useCreateSpace, useDeleteDir, useDeleteSpace, useRenameDir, useRenameSpace } from '../api/hooks';
import type { Space, SpaceDir } from '../api/types';

export type DialogState =
  | { kind: 'none' }
  | { kind: 'create-root' }
  | { kind: 'create-subspace'; parent: Space }
  | { kind: 'create-dir'; parent: Space }
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
        desc: '创建一个共享空间，用于组织会话、子空间与目录。',
        placeholder: '空间名称',
        ok: '创建',
      };
    case 'create-subspace':
      return {
        title: '新建子空间',
        desc: `在「${state.parent.name}」下创建子空间：落库到空间树、自带一个默认会话、可单独授权（仅共享空间下的子空间可分享）。`,
        placeholder: '子空间名称',
        ok: '创建',
      };
    case 'create-dir':
      return {
        title: '新建目录',
        desc: `在「${state.parent.name}」下创建文件目录：它是文件系统里的真实目录（spaces/{space_id}/files/ 下），只放文件，不产生会话、不能单独分享。`,
        placeholder: '目录名称',
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
        desc: `将删除「${state.target.name}」及其所有子空间、目录与会话（进入回收站）。请输入空间名确认：${state.target.name}`,
        placeholder: `输入 ${state.target.name} 以确认`,
        ok: '删除',
      };
    case 'rename-dir':
      return {
        title: '重命名目录',
        desc: '目录名就是文件系统里的真实路径，改名会同步移动目录。',
        placeholder: '新目录名称',
        ok: '保存',
      };
    case 'delete-dir':
      return {
        title: '删除目录',
        desc: `将删除目录「${state.dir.name}」及其中的全部文件，直接从文件系统移除、不进入回收站。请输入目录名确认：${state.dir.name}`,
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
  const [error, setError] = useState('');

  const createSpace = useCreateSpace();
  const renameSpace = useRenameSpace();
  const deleteSpace = useDeleteSpace();
  const createDir = useCreateDir();
  const renameDir = useRenameDir();
  const deleteDir = useDeleteDir();

  const pending =
    createSpace.isPending ||
    renameSpace.isPending ||
    deleteSpace.isPending ||
    createDir.isPending ||
    renameDir.isPending ||
    deleteDir.isPending;

  useEffect(() => {
    if (!open) return;
    setError('');
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
          await createSpace.mutateAsync({ name, parent_id: '' });
          break;
        case 'create-subspace':
          if (!name) return setError('请输入子空间名称');
          await createSpace.mutateAsync({ name, parent_id: state.parent.id });
          break;
        case 'create-dir':
          if (!name) return setError('请输入目录名称');
          await createDir.mutateAsync({ spaceId: state.parent.id, name });
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
          if (!name) return setError('请输入目录名称');
          await renameDir.mutateAsync({ spaceId: state.space.id, name: state.dir.name, nextName: name });
          break;
        case 'delete-dir':
          if (name !== state.dir.name) return setError('输入的目录名与当前目录不一致');
          await deleteDir.mutateAsync({ spaceId: state.space.id, name: state.dir.name });
          break;
        default:
          return;
      }
      Toast.success(state.kind === 'delete' || state.kind === 'delete-dir' ? '已删除' : '已保存');
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
      <div className="dialog-error">{error}</div>
    </Modal>
  );
}
