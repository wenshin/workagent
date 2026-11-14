import { Spin, Typography } from '@douyinfe/semi-ui';
import { useEffect, useState } from 'react';
import { useParams } from 'react-router-dom';

import { viewShared } from '../api/client';
import type { ChatMessage } from '../api/types';
import MarkdownView from '../components/Markdown';

const { Text } = Typography;

interface SharedView {
  title: string;
  created_at: string;
  messages: ChatMessage[];
}

/**
 * 排障只读页：用户把这段对话临时授权出去，运维凭链接查看。
 *
 * 没有任何操作入口——不能发消息、不能跑 run，也不显示会话所属空间。
 * 链接过期或被吊销后服务端直接 404，前端不区分「不存在」与「已过期」。
 */
export default function SharePage() {
  const { token = '' } = useParams();
  const [data, setData] = useState<SharedView | null>(null);
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    viewShared(token)
      .then((res) => {
        if (!cancelled) setData(res);
      })
      .catch((e: unknown) => {
        if (!cancelled) setError(e instanceof Error ? e.message : '链接无效或已过期');
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [token]);

  if (loading) {
    return (
      <div className="boot-screen">
        <Spin size="large" />
      </div>
    );
  }

  if (error || !data) {
    return (
      <div className="share-page">
        <div className="share-empty">
          <Text type="tertiary">{error || '链接无效或已过期'}</Text>
        </div>
      </div>
    );
  }

  return (
    <div className="share-page">
      <header className="share-header">
        <div className="share-title">{data.title || '会话记录'}</div>
        <Text type="tertiary" size="small">
          只读分享 · 创建于 {data.created_at}
        </Text>
      </header>
      <div className="share-body">
        {data.messages.map((m) => (
          <div key={m.id} className={`chat-msg chat-msg-${m.role}`}>
            <div className="chat-msg-meta">
              <span>{m.role === 'user' ? '用户' : m.role === 'assistant' ? 'Agent' : '系统'}</span>
              <span className="chat-msg-time">{m.created_at}</span>
            </div>
            {m.role === 'assistant' ? (
              <div className="chat-msg-content">
                <MarkdownView content={m.content} />
              </div>
            ) : (
              <div className="chat-msg-content">{m.content}</div>
            )}
          </div>
        ))}
      </div>
    </div>
  );
}
