import { Input, Modal, Toast } from '@douyinfe/semi-ui';
import { useEffect, useState } from 'react';

import { useCreateSpace, useDeleteSpace, useRenameSpace } from '../api/hooks';
import type { Space } from '../api/types';

export type DialogState =
  | { kind: 'none' }
  | { kind: 'create-root' }
  | { kind: 'create-child'; parent: Space }
  | { kind: 'rename'; target: Space }
  | { kind: 'delete'; target: Space };

interface Props {
  state: DialogState;
  onClose: () => void;
}

function describe(state: DialogState) {
  switch (state.kind) {
    case 'create-root':
      return { title: '新建空间', desc: '创建一个共享空间，用于组织会话与子目录。', placeholder: '空间名称', ok: '创建' };
    case 'create-child':
      return {
        title: '新建目录',
        desc: `在「${state.parent.name}」下创建子目录。`,
        placeholder: '目录名称',
        ok: '创建',
      };
    case 'rename':
      return {
        title: '重命名目录',
        desc: '仅修改显示名称，存储路径与内容不变。',
        placeholder: '目录名称',
        ok: '保存',
      };
    case 'delete':
      return {
        title: '删除目录',
        desc: `将删除「${state.target.name}」及其所有子目录与会话（进入回收站）。请输入目录名确认：${state.target.name}`,
        placeholder: `输入 ${state.target.name} 以确认`,
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

  const pending = createSpace.isPending || renameSpace.isPending || deleteSpace.isPending;

  useEffect(() => {
    if (!open) return;
    setError('');
    setValue(state.kind === 'rename' ? state.target.name : '');
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
        case 'create-child':
          if (!name) return setError('请输入目录名称');
          await createSpace.mutateAsync({ name, parent_id: state.parent.id });
          break;
        case 'rename':
          if (!name) return setError('请输入目录名称');
          await renameSpace.mutateAsync({ id: state.target.id, name });
          break;
        case 'delete':
          if (name !== state.target.name) return setError('输入的目录名与当前目录不一致');
          await deleteSpace.mutateAsync({ id: state.target.id, confirm_name: name });
          break;
        default:
          return;
      }
      Toast.success(state.kind === 'delete' ? '已删除' : '已保存');
      onClose();
    } catch (e) {
      setError(e instanceof Error ? e.message : '操作失败，请重试');
    }
  };

  return (
    <Modal
      title={meta.title}
      visible={open}
      onCancel={onClose}
      onOk={submit}
      okText={meta.ok}
      cancelText="取消"
      okButtonProps={{ loading: pending, type: state.kind === 'delete' ? 'danger' : 'primary' }}
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
