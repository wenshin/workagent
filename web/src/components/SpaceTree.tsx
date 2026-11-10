import {
  IconBook,
  IconComment,
  IconDelete,
  IconFile,
  IconFolder,
  IconFolderStroked,
  IconLock,
  IconMore,
  IconPlus,
} from '@douyinfe/semi-icons';
import { Button, Dropdown, Tree } from '@douyinfe/semi-ui';
import type { TreeNodeData as SemiTreeNodeData } from '@douyinfe/semi-ui/lib/es/tree/interface';
import { useEffect, useMemo, useRef, useState } from 'react';
import type { ReactNode } from 'react';

import type { ChatSession, Space, SpaceDir, SpaceFile } from '../api/types';
import { PluginIcon } from './PluginVisual';

export const SESSION_KEY_PREFIX = 'session:';
// 目录不是空间，单独用一套 key 前缀，选中时不做导航
const DIR_KEY_PREFIX = 'dir:';
// 文件节点的 key：选中后右侧切换到文件内容面板
export const FILE_KEY_PREFIX = 'file:';

interface TreeNodeData {
  key: string;
  label?: ReactNode;
  isLeaf?: boolean;
  icon?: ReactNode;
  kind: 'space' | 'session' | 'dir' | 'file';
  refId: string;
  children?: TreeNodeData[];
}

interface Props {
  spaces: Space[];
  sessions: ChatSession[];
  dirs: SpaceDir[];
  files: SpaceFile[];
  value: string;
  onSelectSpace: (id: string) => void;
  onSelectSession: (id: string) => void;
  onSelectFile: (space: Space, file: SpaceFile) => void;
  onAddDir: (space: Space, parent?: SpaceDir) => void;
  onAddSession: (space: Space, dir?: SpaceDir) => void;
  /** 新建插件：插件类型在对话框里选 */
  onAddSkill: (space: Space) => void;
  onRename: (space: Space) => void;
  onDelete: (space: Space) => void;
  onRenameDir: (space: Space, dir: SpaceDir) => void;
  onDeleteDir: (space: Space, dir: SpaceDir) => void;
  onDeleteSession: (session: ChatSession) => void;
}

function canWrite(space: Space) {
  return space.perm === 'write' || space.perm === 'manage';
}

// 排序基准：优先用最近消息时间，没有消息时用创建时间
function sessionTime(s: ChatSession) {
  return new Date(s.last_message_at ?? s.created_at).getTime() || 0;
}

function dirKey(spaceId: string, path: string) {
  return `${DIR_KEY_PREFIX}${spaceId}/${path}`;
}

function fileKey(spaceId: string, path: string) {
  return `${FILE_KEY_PREFIX}${spaceId}/${path}`;
}

/** 文件图标按类型区分：md 用说明文档色，json 用配置色，避免树里一片灰 */
function fileIcon(name: string) {
  const lower = name.toLowerCase();
  const className = lower.endsWith('.json')
    ? 'file-icon file-icon-json'
    : lower.endsWith('.md')
      ? 'file-icon file-icon-md'
      : 'file-icon';
  return <IconFile className={className} />;
}

/** 目录节点（含自身）的所有祖先 key，选中会话时用来展开路径 */
function dirAncestorKeys(spaceId: string, path: string): string[] {
  const segs = path.split('/');
  const keys = [spaceId];
  for (let i = 1; i <= segs.length; i += 1) {
    keys.push(dirKey(spaceId, segs.slice(0, i).join('/')));
  }
  return keys;
}

export default function SpaceTree({
  spaces,
  sessions,
  dirs,
  files,
  value,
  onSelectSpace,
  onSelectSession,
  onSelectFile,
  onAddDir,
  onAddSession,
  onAddSkill,
  onRename,
  onDelete,
  onRenameDir,
  onDeleteDir,
  onDeleteSession,
}: Props) {
  const spaceById = useMemo(() => new Map(spaces.map((s) => [s.id, s])), [spaces]);
  const sessionById = useMemo(() => new Map(sessions.map((s) => [s.id, s])), [sessions]);
  const dirByKey = useMemo(
    () => new Map(dirs.map((d) => [dirKey(d.space_id, d.path), d])),
    [dirs],
  );
  const fileByKey = useMemo(
    () => new Map(files.map((f) => [fileKey(f.space_id, f.path), f])),
    [files],
  );

  // 每个空间的会话数：只有一个时不允许删除
  const sessionCount = useMemo(() => {
    const counts = new Map<string, number>();
    sessions.forEach((s) => counts.set(s.space_id, (counts.get(s.space_id) ?? 0) + 1));
    return counts;
  }, [sessions]);

  const treeData = useMemo<TreeNodeData[]>(() => {
    const nodes = new Map<string, TreeNodeData>();
    spaces.forEach((s) => {
      nodes.set(s.id, {
        key: s.id,
        label: s.name,
        kind: 'space',
        refId: s.id,
        icon: <IconFolder />,
        children: [],
      });
    });

    // 目录是空间内可多级的真实文件夹，单独建节点后再按 path 挂到父节点
    const dirNodes = new Map<string, TreeNodeData>();
    [...dirs]
      .sort((a, b) => a.path.localeCompare(b.path))
      .forEach((d) => {
        const node: TreeNodeData = {
          key: dirKey(d.space_id, d.path),
          label: d.name,
          kind: 'dir',
          refId: `${d.space_id}/${d.path}`,
          // 一个目录就是一个插件：按 plugin.json 的 kind 显示统一插件图标；
          // 旧技能目录（.meta.json）仍用书本图标兜底，普通目录保持文件夹。
          icon: d.plugin_kind ? (
            <PluginIcon kind={d.plugin_kind} size="xs" />
          ) : d.is_skill ? (
            <IconBook className="skill-icon" />
          ) : (
            <IconFolderStroked className="dir-icon" />
          ),
          children: [],
        };
        dirNodes.set(dirKey(d.space_id, d.path), node);
      });

    // 会话挂在所属目录下；dir 为空或目录已消失时挂到空间根
    [...sessions]
      .sort((a, b) => sessionTime(b) - sessionTime(a))
      .forEach((sess) => {
        const node: TreeNodeData = {
          key: SESSION_KEY_PREFIX + sess.id,
          label: sess.title,
          kind: 'session',
          refId: sess.id,
          isLeaf: true,
          icon: <IconComment />,
        };
        const dirNode = sess.dir ? dirNodes.get(dirKey(sess.space_id, sess.dir)) : undefined;
        (dirNode ?? nodes.get(sess.space_id))?.children?.push(node);
      });

    // 目录挂到父目录：一级目录挂空间，多级目录挂上级目录
    [...dirs]
      .sort((a, b) => a.path.localeCompare(b.path))
      .forEach((d) => {
        const node = dirNodes.get(dirKey(d.space_id, d.path));
        if (!node) return;
        const cut = d.path.lastIndexOf('/');
        const parent = cut === -1 ? undefined : dirNodes.get(dirKey(d.space_id, d.path.slice(0, cut)));
        (parent ?? nodes.get(d.space_id))?.children?.push(node);
      });

    // 文件挂在所属目录下（dir 为空时挂空间根），排在会话与子目录之后
    [...files]
      .sort((a, b) => a.path.localeCompare(b.path))
      .forEach((f) => {
        const node: TreeNodeData = {
          key: fileKey(f.space_id, f.path),
          label: f.name,
          kind: 'file',
          refId: `${f.space_id}/${f.path}`,
          isLeaf: true,
          icon: fileIcon(f.name),
        };
        const dirNode = f.dir ? dirNodes.get(dirKey(f.space_id, f.dir)) : undefined;
        (dirNode ?? nodes.get(f.space_id))?.children?.push(node);
      });

    return spaces.filter((s) => !s.parent_id).map((s) => nodes.get(s.id)!);
  }, [spaces, sessions, dirs, files]);

  const parentKeys = useMemo(() => {
    const keys: string[] = [];
    const walk = (nodes: TreeNodeData[]) => {
      nodes.forEach((node) => {
        if (node.children && node.children.length > 0) {
          keys.push(node.key);
          walk(node.children);
        }
      });
    };
    walk(treeData);
    return keys;
  }, [treeData]);

  const [expandedKeys, setExpandedKeys] = useState<string[]>([]);
  // 已见过的父节点：用来区分「新增节点」和「用户手动折叠的节点」。
  // 早期实现每次数据刷新都把全部父节点并进 expandedKeys，导致折叠后又被弹开。
  const knownParents = useRef<Set<string>>(new Set());
  // 默认展开策略：只展开个人空间，且只到第一层（个人空间下的目录默认折叠）；
  // 共享空间、系统空间默认整体折叠。
  const defaultExpanded = useMemo(() => {
    const personal = spaces.find((s) => s.type === 'personal');
    return personal ? [personal.id] : [];
  }, [spaces]);
  useEffect(() => {
    if (parentKeys.length === 0) return;
    const known = knownParents.current;
    knownParents.current = new Set(parentKeys);
    if (known.size === 0) {
      setExpandedKeys(defaultExpanded.filter((key) => parentKeys.includes(key)));
      return;
    }
    // 没有新节点（含用户主动折叠）时保持用户的展开状态
    const added = parentKeys.filter((key) => !known.has(key));
    if (added.length === 0) return;
    setExpandedKeys((keys) => Array.from(new Set([...keys, ...added])));
  }, [parentKeys, defaultExpanded]);

  // 选中空间时展开它：默认折叠的空间点一下就展开，不必先点箭头
  useEffect(() => {
    if (
      !value ||
      value.startsWith(SESSION_KEY_PREFIX) ||
      value.startsWith(FILE_KEY_PREFIX) ||
      value.startsWith(DIR_KEY_PREFIX)
    ) {
      return;
    }
    setExpandedKeys((keys) => (keys.includes(value) ? keys : [...keys, value]));
  }, [value]);

  // 选中会话时确保所属空间与目录路径都展开
  useEffect(() => {
    if (!value.startsWith(SESSION_KEY_PREFIX)) return;
    const sess = sessionById.get(value.slice(SESSION_KEY_PREFIX.length));
    if (!sess) return;
    const need = sess.dir ? dirAncestorKeys(sess.space_id, sess.dir) : [sess.space_id];
    setExpandedKeys((keys) => Array.from(new Set([...keys, ...need])));
  }, [value, sessionById]);

  // 选中文件时展开它所在的目录链
  useEffect(() => {
    if (!value.startsWith(FILE_KEY_PREFIX)) return;
    const rest = value.slice(FILE_KEY_PREFIX.length);
    const cut = rest.indexOf('/');
    if (cut === -1) return;
    const spaceId = rest.slice(0, cut);
    const filePath = rest.slice(cut + 1);
    const parentCut = filePath.lastIndexOf('/');
    const need =
      parentCut === -1
        ? [spaceId]
        : dirAncestorKeys(spaceId, filePath.slice(0, parentCut));
    setExpandedKeys((keys) => Array.from(new Set([...keys, ...need])));
  }, [value]);

  // 增删会话/目录后触发树重建：已展开的节点不会自动补上新增子节点
  const treeSignature = [
    spaces.length,
    sessions.length,
    dirs.map((d) => `${d.space_id}/${d.path}`).join('|'),
    files.map((f) => `${f.space_id}/${f.path}`).join('|'),
  ].join('-');

  const renderLabel = (_label?: ReactNode, treeNode?: SemiTreeNodeData) => {
    const node = treeNode as TreeNodeData | undefined;
    if (!node) return null;

    if (node.kind === 'session') {
      const sess = sessionById.get(node.refId);
      const owner = sess ? spaceById.get(sess.space_id) : undefined;
      return (
        <span className="space-row">
          <span className="space-row-name session-name">{sess?.title ?? String(node.label ?? '')}</span>
          {sess && owner && canWrite(owner) && (sessionCount.get(sess.space_id) ?? 0) > 1 ? (
            <span className="space-row-actions" onClick={(e) => e.stopPropagation()}>
              <Button
                theme="borderless"
                size="small"
                type="danger"
                icon={<IconDelete />}
                aria-label="删除会话"
                onClick={() => onDeleteSession(sess)}
              />
            </span>
          ) : null}
        </span>
      );
    }

    if (node.kind === 'dir') {
      const dir = dirByKey.get(node.key);
      const owner = dir ? spaceById.get(dir.space_id) : undefined;
      if (!dir || !owner) return <span>{String(node.label ?? '')}</span>;
      return (
        <span className="space-row">
          <span
            className={`space-row-name ${dir.plugin_kind || dir.is_skill ? 'skill-name' : 'dir-name'}${
              dir.plugin_kind && dir.enabled === false ? ' plugin-row-off' : ''
            }`}
            title={
              dir.installed
                ? `${dir.description || dir.name}（已安装，只读）`
                : dir.description || undefined
            }
          >
            {dir.name}
            {/* 只读状态会继承到整棵子树，但锁只画在插件根目录上，子目录重复画会淹没层级 */}
            {dir.installed && dir.plugin_kind ? (
              <IconLock className="space-row-lock" aria-label="已安装只读" />
            ) : null}
          </span>
          {canWrite(owner) && !dir.installed ? (
            <span className="space-row-actions" onClick={(e) => e.stopPropagation()}>
              <Dropdown
                trigger="click"
                position="bottomRight"
                render={
                  <Dropdown.Menu>
                    <Dropdown.Item onClick={() => onAddSession(owner, dir)}>新建会话</Dropdown.Item>
                    <Dropdown.Item onClick={() => onAddDir(owner, dir)}>新建目录</Dropdown.Item>
                    {/* 插件草稿固定落在空间插件区，目录入口只是快捷方式，不跟随当前目录 */}
                    <Dropdown.Item onClick={() => onAddSkill(owner)}>新建插件</Dropdown.Item>
                  </Dropdown.Menu>
                }
              >
                <Button theme="borderless" size="small" icon={<IconPlus />} aria-label="在目录中新建" />
              </Dropdown>
              {/* 系统空间的内置目录（skills / tools / mcps）不可改名、不可删除 */}
              {!dir.builtin ? (
                <Dropdown
                  trigger="click"
                  position="bottomRight"
                  render={
                    <Dropdown.Menu>
                      <Dropdown.Item onClick={() => onRenameDir(owner, dir)}>
                        重命名{dir.plugin_kind ? '插件目录' : dir.is_skill ? '技能' : '目录'}
                      </Dropdown.Item>
                      <Dropdown.Item onClick={() => onDeleteDir(owner, dir)}>
                        删除{dir.plugin_kind ? '插件目录' : dir.is_skill ? '技能' : '目录'}
                      </Dropdown.Item>
                    </Dropdown.Menu>
                  }
                >
                  <Button
                    theme="borderless"
                    size="small"
                    icon={<IconMore />}
                    aria-label={dir.plugin_kind ? '插件操作' : dir.is_skill ? '技能操作' : '目录操作'}
                  />
                </Dropdown>
              ) : null}
            </span>
          ) : null}
        </span>
      );
    }

    // 文件节点：只展示名称，点击后右侧切换到内容面板
    if (node.kind === 'file') {
      const file = fileByKey.get(node.key);
      return (
        <span className="space-row">
          <span className="space-row-name file-name" title={file?.path}>
            {file?.name ?? String(node.label ?? '')}
          </span>
        </span>
      );
    }

    const space = spaceById.get(node.refId);
    if (!space) return <span>{String(node.label ?? '')}</span>;

    return (
      <span className="space-row">
        <span className="space-row-name">{space.name}</span>
        <span className="space-row-actions" onClick={(e) => e.stopPropagation()}>
          {canWrite(space) ? (
            <Dropdown
              trigger="click"
              position="bottomRight"
              render={
                <Dropdown.Menu>
                  <Dropdown.Item onClick={() => onAddDir(space)}>新建目录</Dropdown.Item>
                  <Dropdown.Item onClick={() => onAddSession(space)}>新建会话</Dropdown.Item>
                  {/* 六类插件共用「新建插件」，具体类型在对话框里选 */}
                  <Dropdown.Item onClick={() => onAddSkill(space)}>新建插件</Dropdown.Item>
                </Dropdown.Menu>
              }
            >
              <Button theme="borderless" size="small" icon={<IconPlus />} aria-label="添加" />
            </Dropdown>
          ) : null}
          {space.perm === 'manage' && !space.locked ? (
            <Dropdown
              trigger="click"
              position="bottomRight"
              render={
                <Dropdown.Menu>
                  <Dropdown.Item onClick={() => onRename(space)}>重命名空间</Dropdown.Item>
                  <Dropdown.Item onClick={() => onDelete(space)}>删除空间</Dropdown.Item>
                </Dropdown.Menu>
              }
            >
              <Button theme="borderless" size="small" icon={<IconMore />} aria-label="空间操作" />
            </Dropdown>
          ) : null}
        </span>
      </span>
    );
  };

  const handleSelect = (selected?: string | number | (string | number)[]) => {
    const key = String(Array.isArray(selected) ? selected[0] : selected ?? '');
    if (!key) return;
    if (key.startsWith(SESSION_KEY_PREFIX)) {
      onSelectSession(key.slice(SESSION_KEY_PREFIX.length));
      return;
    }
    if (key.startsWith(FILE_KEY_PREFIX)) {
      const rest = key.slice(FILE_KEY_PREFIX.length);
      const cut = rest.indexOf('/');
      if (cut === -1) return;
      const space = spaceById.get(rest.slice(0, cut));
      const file = fileByKey.get(key);
      if (space && file) onSelectFile(space, file);
      return;
    }
    // 目录只是文件目录，没有可进入的页面
    if (key.startsWith(DIR_KEY_PREFIX)) return;
    onSelectSpace(key);
  };

  return (
    <Tree
      key={treeSignature}
      className="space-tree"
      treeData={treeData}
      value={value}
      expandedKeys={expandedKeys}
      onExpand={(keys) => setExpandedKeys(keys as string[])}
      onSelect={(selected) => handleSelect(selected as string | number | (string | number)[])}
      renderLabel={renderLabel}
      blockNode
      showLine={false}
    />
  );
}
