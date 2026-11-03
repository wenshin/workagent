import {
  IconComment,
  IconDelete,
  IconFolder,
  IconFolderStroked,
  IconMore,
  IconPlus,
} from '@douyinfe/semi-icons';
import { Button, Dropdown, Tree } from '@douyinfe/semi-ui';
import type { TreeNodeData as SemiTreeNodeData } from '@douyinfe/semi-ui/lib/es/tree/interface';
import { useEffect, useMemo, useRef, useState } from 'react';
import type { ReactNode } from 'react';

import type { ChatSession, Space, SpaceDir } from '../api/types';

export const SESSION_KEY_PREFIX = 'session:';
// 目录不是空间，单独用一套 key 前缀，选中时不做导航
const DIR_KEY_PREFIX = 'dir:';

interface TreeNodeData {
  key: string;
  label?: ReactNode;
  isLeaf?: boolean;
  icon?: ReactNode;
  kind: 'space' | 'session' | 'dir';
  refId: string;
  children?: TreeNodeData[];
}

interface Props {
  spaces: Space[];
  sessions: ChatSession[];
  dirs: SpaceDir[];
  value: string;
  onSelectSpace: (id: string) => void;
  onSelectSession: (id: string) => void;
  onAddSubspace: (space: Space) => void;
  onAddDir: (space: Space) => void;
  onAddSession: (space: Space) => void;
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

function dirKey(spaceId: string, name: string) {
  return `${DIR_KEY_PREFIX}${spaceId}/${name}`;
}

export default function SpaceTree({
  spaces,
  sessions,
  dirs,
  value,
  onSelectSpace,
  onSelectSession,
  onAddSubspace,
  onAddDir,
  onAddSession,
  onRename,
  onDelete,
  onRenameDir,
  onDeleteDir,
  onDeleteSession,
}: Props) {
  const spaceById = useMemo(() => new Map(spaces.map((s) => [s.id, s])), [spaces]);
  const sessionById = useMemo(() => new Map(sessions.map((s) => [s.id, s])), [sessions]);
  const dirByKey = useMemo(
    () => new Map(dirs.map((d) => [dirKey(d.space_id, d.name), d])),
    [dirs],
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

    // 会话挂在所属空间下，按最近活动时间从新到旧
    [...sessions]
      .sort((a, b) => sessionTime(b) - sessionTime(a))
      .forEach((sess) => {
        const parent = nodes.get(sess.space_id);
        if (!parent) return;
        parent.children?.push({
          key: SESSION_KEY_PREFIX + sess.id,
          label: sess.title,
          kind: 'session',
          refId: sess.id,
          isLeaf: true,
          icon: <IconComment />,
        });
      });

    // 子空间排在会话之后
    spaces.forEach((s) => {
      if (!s.parent_id) return;
      nodes.get(s.parent_id)?.children?.push(nodes.get(s.id)!);
    });

    // 目录排在最后：它是 fs 目录，不是空间
    dirs.forEach((d) => {
      const parent = nodes.get(d.space_id);
      if (!parent) return;
      parent.children?.push({
        key: dirKey(d.space_id, d.name),
        label: d.name,
        kind: 'dir',
        refId: `${d.space_id}/${d.name}`,
        isLeaf: true,
        // 空间用实心文件夹，目录用线框文件夹：一眼看出「目录不是空间」
        icon: <IconFolderStroked className="dir-icon" />,
      });
    });

    return spaces.filter((s) => !s.parent_id).map((s) => nodes.get(s.id)!);
  }, [spaces, sessions, dirs]);

  const parentKeys = useMemo(() => {
    const keys: string[] = [];
    spaces.forEach((s) => {
      const hasChildSpace = spaces.some((c) => c.parent_id === s.id);
      const hasSession = sessions.some((c) => c.space_id === s.id);
      const hasDir = dirs.some((d) => d.space_id === s.id);
      if (hasChildSpace || hasSession || hasDir) keys.push(s.id);
    });
    return keys;
  }, [spaces, sessions, dirs]);

  const [expandedKeys, setExpandedKeys] = useState<string[]>([]);
  const initialized = useRef(false);
  useEffect(() => {
    if (!initialized.current && parentKeys.length > 0) {
      setExpandedKeys(parentKeys);
      initialized.current = true;
    }
  }, [parentKeys]);

  // 选中会话时确保所属空间展开
  useEffect(() => {
    if (!value.startsWith(SESSION_KEY_PREFIX)) return;
    const sess = sessionById.get(value.slice(SESSION_KEY_PREFIX.length));
    if (!sess) return;
    setExpandedKeys((keys) => (keys.includes(sess.space_id) ? keys : [...keys, sess.space_id]));
  }, [value, sessionById]);

  // 增删会话/子空间/目录后补上新增的父节点，并触发树重建：已展开的节点不会自动补上新增子节点
  const treeSignature = `${spaces.length}-${sessions.length}-${dirs.length}`;
  useEffect(() => {
    setExpandedKeys((keys) => Array.from(new Set([...keys, ...parentKeys])));
  }, [treeSignature, parentKeys]);

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
      return (
        <span className="space-row">
          <span className="space-row-name dir-name">{dir?.name ?? String(node.label ?? '')}</span>
          {dir && owner && canWrite(owner) ? (
            <span className="space-row-actions" onClick={(e) => e.stopPropagation()}>
              <Dropdown
                trigger="click"
                position="bottomRight"
                render={
                  <Dropdown.Menu>
                    <Dropdown.Item onClick={() => onRenameDir(owner, dir)}>重命名目录</Dropdown.Item>
                    <Dropdown.Item onClick={() => onDeleteDir(owner, dir)}>删除目录</Dropdown.Item>
                  </Dropdown.Menu>
                }
              >
                <Button theme="borderless" size="small" icon={<IconMore />} aria-label="目录操作" />
              </Dropdown>
            </span>
          ) : null}
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
                  <Dropdown.Item onClick={() => onAddSubspace(space)}>新建子空间</Dropdown.Item>
                  <Dropdown.Item onClick={() => onAddDir(space)}>新建目录</Dropdown.Item>
                  <Dropdown.Item onClick={() => onAddSession(space)}>新建会话</Dropdown.Item>
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
      autoExpandParent
    />
  );
}
