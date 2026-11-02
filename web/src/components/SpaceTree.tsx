import { IconComment, IconDelete, IconEdit, IconFolder, IconPlus } from '@douyinfe/semi-icons';
import { Button, Tree } from '@douyinfe/semi-ui';
import { Fragment, useEffect, useMemo, useRef, useState } from 'react';
import type { ReactNode } from 'react';

import type { Space } from '../api/types';

interface TreeNodeData {
  key?: string;
  value?: string | number;
  label?: ReactNode;
  isLeaf?: boolean;
  icon?: ReactNode;
  children?: TreeNodeData[];
}

interface Props {
  spaces: Space[];
  value: string;
  onSelect: (id: string) => void;
  onAddChild: (space: Space) => void;
  onRename: (space: Space) => void;
  onDelete: (space: Space) => void;
  onAddSession: (space: Space) => void;
}

function canWrite(space: Space) {
  return space.perm === 'write' || space.perm === 'manage';
}

export default function SpaceTree({ spaces, value, onSelect, onAddChild, onRename, onDelete, onAddSession }: Props) {
  const byId = useMemo(() => new Map(spaces.map((s) => [s.id, s])), [spaces]);

  const treeData = useMemo<TreeNodeData[]>(() => {
    const nodes = new Map<string, TreeNodeData>();
    spaces.forEach((s) => {
      nodes.set(s.id, {
        key: s.id,
        value: s.id,
        label: s.name,
        isLeaf: !spaces.some((c) => c.parent_id === s.id),
        icon: s.is_system_reserved ? <IconComment /> : <IconFolder />,
      });
    });

    const roots: TreeNodeData[] = [];
    spaces.forEach((s) => {
      const node = nodes.get(s.id);
      if (!node) return;
      const parent = s.parent_id ? nodes.get(s.parent_id) : undefined;
      if (parent) {
        parent.children = parent.children ?? [];
        parent.children.push(node);
      } else {
        roots.push(node);
      }
    });
    return roots;
  }, [spaces]);

  const parentKeys = useMemo(
    () => spaces.filter((s) => spaces.some((c) => c.parent_id === s.id)).map((s) => s.id),
    [spaces],
  );

  const [expandedKeys, setExpandedKeys] = useState<string[]>([]);
  const initialized = useRef(false);
  useEffect(() => {
    if (!initialized.current && parentKeys.length > 0) {
      setExpandedKeys(parentKeys);
      initialized.current = true;
    }
  }, [parentKeys]);

  const renderLabel = (_label?: ReactNode, node?: TreeNodeData) => {
    if (!node) return null;
    const space = byId.get(String(node.key));
    if (!space) return <span>{String(node.label ?? '')}</span>;

    return (
      <span className="space-row">
        <span className="space-row-name">{space.name}</span>
        <span className="space-row-actions" onClick={(e) => e.stopPropagation()}>
          {space.is_system_reserved && canWrite(space) ? (
            <Button
              theme="borderless"
              size="small"
              icon={<IconPlus />}
              aria-label="添加会话"
              onClick={() => onAddSession(space)}
            />
          ) : null}
          {!space.is_system_reserved && canWrite(space) ? (
            <Button
              theme="borderless"
              size="small"
              icon={<IconPlus />}
              aria-label="新建子空间"
              onClick={() => onAddChild(space)}
            />
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

  return (
    <Tree
      className="space-tree"
      treeData={treeData}
      value={value}
      expandedKeys={expandedKeys}
      onExpand={(keys) => setExpandedKeys(keys as string[])}
      onSelect={(selected) => onSelect(String(selected))}
      renderLabel={renderLabel}
      blockNode
      showLine={false}
      autoExpandParent
    />
  );
}
