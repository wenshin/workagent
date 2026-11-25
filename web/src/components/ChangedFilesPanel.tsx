import { IconFile } from '@douyinfe/semi-icons';
import { Button, Empty, Typography } from '@douyinfe/semi-ui';

import type { Space, SpaceFile } from '../api/types';
import FilePanel from './FilePanel';

const { Text } = Typography;

interface Props {
  space?: Space;
  /** 当前空间的文件清单：路径 → 元信息（预览要 size / preview_kind） */
  files: SpaceFile[];
  /** 本会话写入过的文件路径，按写入顺序 */
  paths: string[];
  /** 当前预览的文件路径，空串表示展示清单 */
  selectedPath: string;
  onSelect: (path: string) => void;
}

/**
 * 会话分栏右侧：本会话修改过的文件。
 *
 * 清单由工具回执推导（`sessionWrittenPaths`），与消息底部的「本轮写入」同源；
 * 点一条在面板内预览，不跳走——左边还留着对话上下文。
 */
export default function ChangedFilesPanel({
  space,
  files,
  paths,
  selectedPath,
  onSelect,
}: Props) {
  const selected = selectedPath ? files.find((file) => file.path === selectedPath) : undefined;

  if (selectedPath) {
    return (
      <div className="changed-files">
        {selected ? (
          <FilePanel
            key={`${selected.space_id}/${selected.path}`}
            space={space}
            file={selected}
            onBack={() => onSelect('')}
          />
        ) : (
          <div className="panel-empty">
            {/* 文件被删或改名后停在空态，必须给一条回到清单的路 */}
            <Empty title="文件不存在" description="可能已被删除或移动">
              <Button onClick={() => onSelect('')}>返回文件清单</Button>
            </Empty>
          </div>
        )}
      </div>
    );
  }

  return (
    <div className="changed-files">
      <div className="changed-files-head">
        <span className="changed-files-title">本会话修改的文件</span>
        <Text type="tertiary" size="small">
          {paths.length} 个
        </Text>
      </div>
      <div className="changed-files-body">
        {paths.length === 0 ? (
          <div className="panel-empty">
            <Empty
              title="还没有修改过文件"
              description="Agent 写入文件后会出现在这里，点开可以直接预览"
            />
          </div>
        ) : (
          <ul className="changed-files-list">
            {paths.map((path) => {
              const name = path.split('/').pop() || path;
              const dir = path.slice(0, path.length - name.length).replace(/\/+$/, '');
              return (
                <li key={path}>
                  <button
                    type="button"
                    className="changed-files-item"
                    title={`打开 ${path}`}
                    onClick={() => onSelect(path)}
                  >
                    <IconFile size="small" className="changed-files-icon" />
                    <span className="changed-files-name">{name}</span>
                    {dir ? <span className="changed-files-dir">{dir}</span> : null}
                  </button>
                </li>
              );
            })}
          </ul>
        )}
      </div>
    </div>
  );
}
