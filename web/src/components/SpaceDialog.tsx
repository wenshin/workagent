import { IconTick } from '@douyinfe/semi-icons';
import { Input, Modal, TextArea, Toast } from '@douyinfe/semi-ui';
import { useEffect, useRef, useState } from 'react';

import {
  useCreateDir,
  useCreateDraft,
  useCreateSpace,
  useDeleteDir,
  useDeleteSpace,
  useRenameDir,
  useRenameSpace,
} from '../api/hooks';
import type { PluginKind, Space, SpaceDir } from '../api/types';
import { KIND_LABEL, PluginIcon } from './PluginVisual';

export type DialogState =
  | { kind: 'none' }
  | { kind: 'create-root' }
  | { kind: 'create-dir'; space: Space; parent?: SpaceDir }
  | { kind: 'create-plugin'; space: Space }
  | { kind: 'rename'; target: Space }
  | { kind: 'delete'; target: Space }
  | { kind: 'rename-dir'; space: Space; dir: SpaceDir }
  | { kind: 'delete-dir'; space: Space; dir: SpaceDir };

// 可新建的插件类型，顺序与后端 DraftKinds 一致
const DRAFT_KINDS: PluginKind[] = ['skill', 'tool', 'mcp', 'component', 'agent', 'source'];

// 每类一句话：选类型是「我要做什么」的判断，标签太抽象，必须给出动作语义
const KIND_HINT: Record<PluginKind, string> = {
  skill: '一套工作方法或规范，让模型照着做',
  tool: '一段可执行逻辑，有入参与返回值',
  mcp: '接入外部 MCP Server 的工具',
  component: '在会话里渲染的声明式界面',
  agent: '提示词与依赖构成的行为设定',
  source: 'web_search 背后的检索源',
};

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
    case 'create-plugin':
      return {
        title: '新建插件',
        desc: `在「${state.space.name}」的插件区创建草稿：按类型生成 plugin.json 与入口文件，改完可在插件面板试跑。`,
        placeholder: '插件名称',
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
    case 'rename-dir': {
      const word = state.dir.plugin_kind ? '插件目录' : state.dir.is_skill ? '技能' : '目录';
      return {
        title: `重命名${word}`,
        desc: state.dir.plugin_kind
          ? '目录名是插件的草稿路径，改名会移动目录；plugin.json 里的 slug 不会自动改，建议在插件面板里维护。'
          : state.dir.is_skill
            ? '目录名就是真实路径，改名会移动目录并同步 .meta.json 里的名称，目录下的会话会一起迁移。'
            : '目录名就是文件系统里的真实路径，改名会移动目录，目录下的会话会一起迁移。',
        placeholder: '新名称',
        ok: '保存',
      };
    }
    case 'delete-dir': {
      const word = state.dir.plugin_kind ? '插件目录' : state.dir.is_skill ? '技能' : '目录';
      return {
        title: `删除${word}`,
        desc: `将删除「${state.dir.name}」及其中的全部文件（不进入回收站）；其中的会话不会丢失，会移回空间根。请输入名称确认：${state.dir.name}`,
        placeholder: `输入 ${state.dir.name} 以确认`,
        ok: '删除',
      };
    }
    default:
      return { title: '', desc: '', placeholder: '', ok: '确定' };
  }
}

export default function SpaceDialog({ state, onClose }: Props) {
  const open = state.kind !== 'none';
  const meta = describe(state);

  const [value, setValue] = useState('');
  const [description, setDescription] = useState('');
  const [pluginKind, setPluginKind] = useState<PluginKind>('skill');
  const [error, setError] = useState('');

  // 类型卡片按 radiogroup 的惯例做 roving tabindex：方向键切换，Tab 只停一次
  const kindRefs = useRef<Record<string, HTMLButtonElement | null>>({});

  const createSpace = useCreateSpace();
  const renameSpace = useRenameSpace();
  const deleteSpace = useDeleteSpace();
  const createDir = useCreateDir();
  const createDraft = useCreateDraft();
  const renameDir = useRenameDir();
  const deleteDir = useDeleteDir();

  const pending =
    createSpace.isPending ||
    renameSpace.isPending ||
    deleteSpace.isPending ||
    createDir.isPending ||
    createDraft.isPending ||
    renameDir.isPending ||
    deleteDir.isPending;

  useEffect(() => {
    if (!open) return;
    setError('');
    setDescription('');
    if (state.kind === 'rename') setValue(state.target.name);
    else if (state.kind === 'rename-dir') setValue(state.dir.name);
    else setValue('');
    if (state.kind === 'create-plugin') setPluginKind('skill');
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
        case 'create-plugin':
          if (!name) return setError('请输入插件名称');
          await createDraft.mutateAsync({
            spaceId: state.space.id,
            kind: pluginKind,
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
          : state.kind === 'create-plugin'
            ? `${KIND_LABEL[pluginKind]}已创建`
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
      width={state.kind === 'create-plugin' ? 560 : undefined}
    >
      {state.kind === 'create-plugin' ? (
        <>
          <p className="dialog-intro">{meta.desc}</p>

          {/* 类型不是「填一个值」，而是先决定要做什么：卡片一次摊开，配一句话语义 */}
          <div className="dialog-group">
            <span className="dialog-group-title">类型</span>
            <div className="plugin-kind-grid" role="radiogroup" aria-label="插件类型">
              {DRAFT_KINDS.map((k) => {
                const active = pluginKind === k;
                return (
                  <button
                    key={k}
                    ref={(el) => {
                      kindRefs.current[k] = el;
                    }}
                    type="button"
                    role="radio"
                    aria-checked={active}
                    tabIndex={active ? 0 : -1}
                    className={`plugin-kind-option${active ? ' is-active' : ''}`}
                    onClick={() => setPluginKind(k)}
                    onKeyDown={(e) => {
                      const step =
                        e.key === 'ArrowRight' || e.key === 'ArrowDown'
                          ? 1
                          : e.key === 'ArrowLeft' || e.key === 'ArrowUp'
                            ? -1
                            : 0;
                      if (step === 0) return;
                      e.preventDefault();
                      const next =
                        DRAFT_KINDS[
                          (DRAFT_KINDS.indexOf(k) + step + DRAFT_KINDS.length) % DRAFT_KINDS.length
                        ];
                      setPluginKind(next);
                      kindRefs.current[next]?.focus();
                    }}
                    disabled={pending}
                  >
                    <PluginIcon kind={k} size="sm" />
                    <span className="plugin-kind-body">
                      <span className="plugin-kind-name">{KIND_LABEL[k]}</span>
                      <span className="plugin-kind-hint">{KIND_HINT[k]}</span>
                    </span>
                    {active ? <IconTick className="plugin-kind-check" /> : null}
                  </button>
                );
              })}
            </div>
          </div>

          <div className="dialog-group">
            <span className="dialog-group-title">名称</span>
            <Input
              value={value}
              onChange={setValue}
              placeholder={meta.placeholder}
              maxLength={64}
              autoFocus
              onEnterPress={submit}
              disabled={pending}
            />
          </div>

          <div className="dialog-group">
            <span className="dialog-group-title">
              描述<span className="dialog-group-optional">选填</span>
            </span>
            <TextArea
              value={description}
              onChange={setDescription}
              placeholder="说明它解决什么问题、什么时候该用，模型靠这句话判断是否调用"
              maxCount={500}
              autosize={{ minRows: 2, maxRows: 4 }}
              disabled={pending}
            />
          </div>
        </>
      ) : (
        <>
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
        </>
      )}
      <div className="dialog-error">{error}</div>
    </Modal>
  );
}
