import { IconSend } from '@douyinfe/semi-icons';
import { Button, Empty, Spin, TextArea, Typography } from '@douyinfe/semi-ui';
import { useEffect, useRef, useState } from 'react';

import type { ChatMessage, ChatSession } from '../api/types';

const { Text } = Typography;

interface Props {
  session: ChatSession;
  spaceName?: string;
  messages: ChatMessage[];
  loading: boolean;
  sending: boolean;
  onSend: (content: string) => Promise<void>;
}

function formatTime(iso: string) {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return '';
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

export default function ChatPanel({ session, spaceName, messages, loading, sending, onSend }: Props) {
  const [value, setValue] = useState('');
  const [error, setError] = useState('');
  const bottomRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    setValue('');
    setError('');
  }, [session.id]);

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: 'smooth', block: 'end' });
  }, [messages.length, session.id]);

  const send = async () => {
    const text = value.trim();
    if (!text || sending) return;
    setError('');
    try {
      await onSend(text);
      setValue('');
    } catch (e) {
      setError(e instanceof Error ? e.message : '发送失败，请重试');
    }
  };

  const hasAssistant = messages.some((m) => m.role === 'assistant');

  return (
    <div className="chat-panel">
      <div className="chat-head">
        <div className="chat-heading">
          <h2 className="panel-title">{session.title}</h2>
          <Text type="tertiary" size="small">
            {spaceName ? `${spaceName} · ` : ''}
            {session.title_auto ? '发送第一条消息后自动命名' : '已命名会话'}
          </Text>
        </div>
      </div>

      <div className="chat-body">
        {loading ? (
          <div className="chat-empty">
            <Spin />
          </div>
        ) : messages.length === 0 ? (
          <div className="chat-empty">
            <Empty
              title="开始新的对话"
              description="把任务描述给 Agent，第一条消息会成为这个会话的名称。"
            />
          </div>
        ) : (
          messages.map((m) => (
            <div key={m.id} className={`chat-msg chat-msg-${m.role}`}>
              <div className="chat-msg-meta">
                <span>{m.role === 'user' ? '我' : m.role === 'assistant' ? 'Agent' : '系统'}</span>
                <span className="chat-msg-time">{formatTime(m.created_at)}</span>
              </div>
              <div className="chat-msg-content">{m.content}</div>
            </div>
          ))
        )}

        {!hasAssistant && messages.length > 0 ? (
          <div className="chat-hint">Agent 运行时尚未接入，消息已保存到当前会话。</div>
        ) : null}

        <div ref={bottomRef} />
      </div>

      <div className="chat-input">
        <TextArea
          value={value}
          onChange={setValue}
          placeholder="描述你的任务，Enter 发送，Shift + Enter 换行"
          autosize={{ minRows: 2, maxRows: 8 }}
          disabled={sending}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && !e.shiftKey) {
              e.preventDefault();
              void send();
            }
          }}
        />
        <div className="chat-input-foot">
          <span className="chat-error">{error}</span>
          <Button
            theme="solid"
            type="primary"
            icon={<IconSend />}
            loading={sending}
            disabled={!value.trim()}
            onClick={send}
          >
            发送
          </Button>
        </div>
      </div>
    </div>
  );
}
