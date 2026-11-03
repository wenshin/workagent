import { IconFile } from '@douyinfe/semi-icons';
import { Empty, Spin, Typography } from '@douyinfe/semi-ui';

import { useFileContent } from '../api/hooks';
import type { Space, SpaceFile } from '../api/types';
import MarkdownView from './Markdown';

const { Text } = Typography;

interface Props {
  space?: Space;
  file: SpaceFile;
}

function formatSize(size: number) {
  if (size < 1024) return `${size} B`;
  if (size < 1024 * 1024) return `${(size / 1024).toFixed(1)} KB`;
  return `${(size / 1024 / 1024).toFixed(1)} MB`;
}

/** json 尽量格式化输出，解析失败时退回原文 */
function prettifyJson(raw: string) {
  try {
    return JSON.stringify(JSON.parse(raw), null, 2);
  } catch {
    return raw;
  }
}

/** 右侧文件内容面板：md 走 Markdown 渲染，json 等按纯文本展示 */
export default function FilePanel({ space, file }: Props) {
  const { data, isLoading, isError, error } = useFileContent(space?.id, file.path);
  const lower = file.name.toLowerCase();
  const isMarkdown = lower.endsWith('.md');

  const header = (
    <div className="file-panel-head">
      <div className="file-panel-title">
        <IconFile className={lower.endsWith('.json') ? 'file-icon-json' : 'file-icon-md'} />
        <span className="file-panel-name">{file.name}</span>
        <span className="file-panel-path">
          {space?.name}
          {file.dir ? ` / ${file.dir}` : ''}
        </span>
      </div>
      <Text type="tertiary" size="small">
        {formatSize(file.size)}
        {data?.truncated ? ' · 内容较大，仅展示前 256KB' : ''}
      </Text>
    </div>
  );

  if (isLoading) {
    return (
      <div className="file-panel">
        {header}
        <div className="panel-empty">
          <Spin />
        </div>
      </div>
    );
  }

  if (isError || !data) {
    return (
      <div className="file-panel">
        {header}
        <div className="panel-empty">
          <Empty
            title="文件读取失败"
            description={error instanceof Error ? error.message : '文件可能已被删除或无权访问'}
          />
        </div>
      </div>
    );
  }

  return (
    <div className="file-panel">
      {header}
      <div className="file-panel-body">
        {isMarkdown ? (
          <MarkdownView content={data.content} />
        ) : (
          <pre className="file-pre">
            <code>{lower.endsWith('.json') ? prettifyJson(data.content) : data.content}</code>
          </pre>
        )}
      </div>
    </div>
  );
}
