import { IconDownload, IconFile, IconImage, IconMusic, IconPdf, IconVideo } from '@douyinfe/semi-icons';
import { Button, Empty, Spin, Toast, Typography } from '@douyinfe/semi-ui';
import { useState, type ReactNode } from 'react';

import { downloadSpaceFile, previewKindOf, spaceFileRawUrl } from '../api/client';
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
 * 按服务端给的 `preview_kind` 分四种形态，每种都要有加载 / 失败出口：
 * - text：md 渲染富文本、json 格式化，其余按纯文本；
 * - image / video / audio：浏览器原生元素，源走 file/raw（支持 Range，视频能拖进度）；
 * - pdf：内嵌浏览器自带阅读器；
 * - none：只给下载按钮，并说明为什么看不到内容。
 * 下载入口始终在头部右上角；媒体解码失败时正文区退回下载，不留白屏。
 */
export default function FilePanel({ space, file }: Props) {
  const kind = previewKindOf(file);
  const isText = kind === 'text';
  // 只有文本类型请求正文：file 端点只认文本，其余请求只会 400
  const { data, isLoading, isError, error } = useFileContent(space?.id, file.path, isText);
  const [downloading, setDownloading] = useState(false);
  // 媒体解码失败（编码不受支持、文件被删）：退回下载入口，不留空白区域
  const [mediaError, setMediaError] = useState(false);
  const lower = file.name.toLowerCase();
  const rawUrl = space ? spaceFileRawUrl(space.id, file.path) : '';

  const handleDownload = async () => {
    if (!space) return;
    setDownloading(true);
    try {
      await downloadSpaceFile(space.id, file.path, file.name);
    } catch (e) {
      Toast.error(e instanceof Error ? e.message : '下载失败，请重试');
    } finally {
      setDownloading(false);
    }
  };

  const kindIcon =
    kind === 'image' ? (
      <IconImage />
    ) : kind === 'video' ? (
      <IconVideo />
    ) : kind === 'audio' ? (
      <IconMusic />
    ) : kind === 'pdf' ? (
      <IconPdf />
    ) : (
      <IconFile />
    );

  const header = (
    <div className="file-panel-head">
      <div className="file-panel-title">
        {kindIcon}
        <span className="file-panel-name">{file.name}</span>
        <span className="file-panel-path">
          {space?.name}
          {file.dir ? ` / ${file.dir}` : ''}
        </span>
      </div>
      <div className="file-panel-meta">
        <Text type="tertiary" size="small">
          {formatSize(file.size)}
          {isText && data?.truncated ? ' · 仅展示前 256KB，可下载完整内容' : ''}
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

  const wrap = (body: ReactNode) => (
    <div className="file-panel">
      {header}
      {body}
    </div>
  );

  /** 渲染不了时统一的出口：说明 + 下载，不留白屏 */
  const downloadFallback = (title: string, description: string) => (
    <div className="panel-empty">
      <Empty title={title} description={description}>
        <Button icon={<IconDownload />} loading={downloading} onClick={handleDownload}>
          下载文件
        </Button>
      </Empty>
    </div>
  );

  // 压缩包、可执行产物等：浏览器没有内建渲染器，只提供下载
  if (kind === 'none') {
    return wrap(downloadFallback('该文件类型不支持预览', '下载到本机后用本地应用打开'));
  }

  if (mediaError) {
    return wrap(
      downloadFallback(
        '该文件无法在当前浏览器预览',
        '可能是编码不受支持或文件已被删除，下载后用本地应用打开',
      ),
    );
  }

  if (kind === 'text') {
    if (isLoading) {
      return wrap(
        <div className="panel-empty">
          <Spin />
        </div>,
      );
    }

    if (isError || !data) {
      return wrap(
        <div className="panel-empty">
          <Empty
            title="文件读取失败"
            description={error instanceof Error ? error.message : '文件可能已被删除或无权访问'}
          />
        </div>,
      );
    }

    return wrap(
      <div className="file-panel-body">
        {lower.endsWith('.md') ? (
          <MarkdownView content={data.content} />
        ) : (
          <pre className="file-pre">
            <code>{lower.endsWith('.json') ? prettifyJson(data.content) : data.content}</code>
          </pre>
        )}
      </div>,
    );
  }

  // 空间还没加载出来时没有 raw 地址，先给加载态而不是空白 iframe / 空 src
  if (!rawUrl) {
    return wrap(
      <div className="panel-empty">
        <Spin />
      </div>,
    );
  }

  if (kind === 'pdf') {
    return wrap(
      <div className="file-panel-frame">
        <iframe className="file-pdf" src={rawUrl} title={file.name} />
      </div>,
    );
  }

  const media =
    kind === 'image' ? (
      <img
        className="file-image"
        src={rawUrl}
        alt={file.name}
        onError={() => setMediaError(true)}
      />
    ) : kind === 'video' ? (
      <video
        className="file-video"
        src={rawUrl}
        controls
        preload="metadata"
        onError={() => setMediaError(true)}
      />
    ) : (
      <audio
        className="file-audio"
        src={rawUrl}
        controls
        preload="metadata"
        onError={() => setMediaError(true)}
      />
    );

  return wrap(<div className="file-panel-body file-panel-media">{media}</div>);
}
