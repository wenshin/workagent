import { IconComment, IconDelete, IconEdit, IconFolder, IconPlus } from '@douyinfe/semi-icons';
import { Button, Dropdown, Tree } from '@douyinfe/semi-ui';
import type { TreeNodeData as SemiTreeNodeData } from '@douyinfe/semi-ui/lib/es/tree/interface';
import { Fragment, useEffect, useMemo, useRef, useState } from 'react';
import type { ReactNode } from 'react';

import type { ChatSession, Space } from '../api/types';

export const SESSION_KEY_PREFIX = 'session:';

interface TreeNodeData {
  key: string;
  label?: ReactNode;
  isLeaf?: boolean;
  icon?: ReactNode;
  kind: 'space' | 'session';
  refId: string;
  children?: TreeNodeData[];
}

interface Props {
  spaces: Space[];
  sessions: ChatSession[];
  value: string;
  onSelectSpace: (id: string) => void;
  onSelectSession: (id: string) => void;
  onAddChild: (space: Space) => void;
  onAddSession: (space: Space) => void;
  onRename: (space: Space) => void;
  onDelete: (space: Space) => void;
  onDeleteSession: (session: ChatSession) => void;
}

function canWrite(space: Space) {
  return space.perm === 'write' || space.perm === 'manage';
}

// 排序基准：优先用最近消息时间，没有消息时用创建时间
function sessionTime(s: ChatSession) {
  return new Date(s.last_message_at ?? s.created_at).getTime() || 0;
}

export default function SpaceTree({
  spaces,
  sessions,
  value,
  onSelectSpace,
  onSelectSession,
  onAddChild,
  onAddSession,
  onRename,
  onDelete,
  onDeleteSession,
}: Props) {
  const spaceById = useMemo(() => new Map(spaces.map((s) => [s.id, s])), [spaces]);
  const sessionById = useMemo(() => new Map(sessions.map((s) => [s.id, s])), [sessions]);

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

    // 会话排在目录之前，按最近活动时间从新到旧
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

    // 子空间挂在会话之后
    spaces.forEach((s) => {
      if (!s.parent_id) return;
      nodes.get(s.parent_id)?.children?.push(nodes.get(s.id)!);
    });

    return spaces.filter((s) => !s.parent_id).map((s) => nodes.get(s.id)!);
  }, [spaces, sessions]);

  const parentKeys = useMemo(() => {
    const keys: string[] = [];
    spaces.forEach((s) => {
      const hasChildSpace = spaces.some((c) => c.parent_id === s.id);
      const hasSession = sessions.some((c) => c.space_id === s.id);
      if (hasChildSpace || hasSession) keys.push(s.id);
    });
    return keys;
  }, [spaces, sessions]);

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

  // 增删会话/目录后补上新增的父节点，并触发树重建：已展开的节点不会自动补上新增子节点
  const treeSignature = `${spaces.length}-${sessions.length}`;
  useEffect(() => {
    setExpandedKeys((keys) => Array.from(new Set([...keys, ...parentKeys])));
  }, [treeSignature]);

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
                  <Dropdown.Item onClick={() => onAddChild(space)}>新建目录</Dropdown.Item>
                  <Dropdown.Item onClick={() => onAddSession(space)}>新建会话</Dropdown.Item>
                </Dropdown.Menu>
              }
            >
              <Button theme="borderless" size="small" icon={<IconPlus />} aria-label="添加" />
            </Dropdown>
          ) : null}
          {space.perm === 'manage' && !space.locked ? (
            <Fragment>
              <Button
                theme="borderless"
                size="small"
                icon={<IconEdit />}
                aria-label="重命名"
                onClick={() => onRename(space)}
              />
              <Button
                theme="borderless"
                size="small"
                type="danger"
                icon={<IconDelete />}
                aria-label="删除"
                onClick={() => onDelete(space)}
              />
            </Fragment>
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
