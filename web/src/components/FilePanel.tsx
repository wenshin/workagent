import { IconDownload, IconFile } from '@douyinfe/semi-icons';
import { Button, Empty, Spin, Toast, Typography } from '@douyinfe/semi-ui';
import { useState } from 'react';

import { downloadSpaceFile, isPreviewable } from '../api/client';
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

/**
 * 右侧文件内容面板。
 *
 * 两种形态：可预览的文本（md / json / txt …）渲染正文，下载入口放在头部右上角；
 * 预览不了的类型（excel、图片、压缩包…）正文区只给一个下载按钮。
 */
export default function FilePanel({ space, file }: Props) {
  const previewable = isPreviewable(file);
  // 预览不了的类型不请求正文（后端只认文本类型，请求会直接 400）
  const { data, isLoading, isError, error } = useFileContent(space?.id, file.path, previewable);
  const [downloading, setDownloading] = useState(false);
  const lower = file.name.toLowerCase();

  const handleDownload = async () => {
    if (!space) return;
    setDownloading(true);
    try {
      await downloadSpaceFile(space.id, file.path, file.name);
    } catch (e) {
      Toast.error(e instanceof Error ? e.message : '下载失败，请重试');
    }
  };

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
      <div className="file-panel-meta">
        <Text type="tertiary" size="small">
          {formatSize(file.size)}
          {data?.truncated ? ' · 仅展示前 256KB，可下载完整内容' : ''}
        </Text>
        <Button
          theme="borderless"
          size="small"
          icon={<IconDownload />}
          loading={downloading}
          aria-label="下载到本机"
          title="下载到本机"
          onClick={handleDownload}
        />
      </div>
    </div>
  );

  // 预览不了的文件：正文区只放下载按钮，说明为什么看不到内容
  if (!previewable) {
    return (
      <div className="file-panel">
        {header}
        <div className="panel-empty">
          <Empty title="该文件类型不支持预览" description="下载到本机后用本地应用打开">
            <Button icon={<IconDownload />} loading={downloading} onClick={handleDownload}>
              下载文件
            </Button>
          </Empty>
        </div>
      </div>
    );
  }

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
        {lower.endsWith('.md') ? (
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
