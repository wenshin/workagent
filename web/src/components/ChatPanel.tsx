import { IconClose, IconSend } from '@douyinfe/semi-icons';
import { Button, Empty, Spin, TextArea, Typography } from '@douyinfe/semi-ui';
import { useEffect, useRef, useState } from 'react';

import type { ChatMessage, ChatSession, MessagePart, ToolCallRecord } from '../api/types';

const { Text } = Typography;

/** 流式过程中的临时视图 */
export interface StreamView {
  reasoning: string;
  text: string;
  tools: ToolCallRecord[];
}

interface Props {
  session: ChatSession;
  spaceName?: string;
  messages: ChatMessage[];
  loading: boolean;
  sending: boolean;
  stream: StreamView | null;
  pendingUser: string | null;
  agentReady: boolean;
  model?: string;
  onSend: (content: string) => Promise<void>;
  onStop: () => void;
}

function formatTime(iso: string) {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return '';
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

function prettyJSON(raw: string) {
  if (!raw) return '';
  try {
    return JSON.stringify(JSON.parse(raw), null, 2);
  } catch {
    return raw;
  }
}

function statusLabel(status: ToolCallRecord['status']) {
  if (status === 'running') return '执行中';
  if (status === 'ok') return '成功';
  if (status === 'approval') return '待审批';
  if (status === 'denied') return '已拒绝';
  return '失败';
}

function ReasoningBlock({ text, live }: { text: string; live?: boolean }) {
  return (
    <details className="msg-reasoning" open={live}>
      <summary className="msg-reasoning-head">
        <span>思考过程</span>
        {live ? <span className="msg-reasoning-live">生成中…</span> : null}
      </summary>
      <div className="msg-reasoning-body">{text}</div>
    </details>
  );
}

function ToolCard({ tool }: { tool: ToolCallRecord }) {
  return (
    <details className={`msg-tool msg-tool-${tool.status}`} open={tool.status === 'running'}>
      <summary className="msg-tool-head">
        <span className="msg-tool-name">{tool.name}</span>
        <span className="msg-tool-status">{statusLabel(tool.status)}</span>
        {tool.duration_ms !== undefined ? <span className="msg-tool-time">{tool.duration_ms}ms</span> : null}
      </summary>
      <div className="msg-tool-body">
        <div className="msg-tool-label">参数</div>
        <pre className="msg-tool-pre">{prettyJSON(tool.args) || '{}'}</pre>
        {tool.result ? (
          <>
            <div className="msg-tool-label">结果</div>
            <pre className="msg-tool-pre">{tool.result}</pre>
          </>
        ) : null}
      </div>
    </details>
  );
}

function toRecords(parts?: MessagePart[]): ToolCallRecord[] {
  return (parts ?? []).map((p) => ({
    id: p.id,
    name: p.name,
    args: p.args,
    result: p.result,
    status: p.status,
    duration_ms: p.duration_ms,
  }));
}

function AssistantBody({
  content,
  reasoning,
  tools,
  live,
}: {
  content: string;
  reasoning?: string;
  tools: ToolCallRecord[];
  live?: boolean;
}) {
  return (
    <>
      {reasoning ? <ReasoningBlock text={reasoning} live={live && !content && tools.length === 0} /> : null}
      {tools.map((tool) => (
        <ToolCard key={tool.id} tool={tool} />
      ))}
      {content ? <div className="chat-msg-content">{content}</div> : null}
    </>
  );
}

export default function ChatPanel({
  session,
  spaceName,
  messages,
  loading,
  sending,
  stream,
  pendingUser,
  agentReady,
  model,
  onSend,
  onStop,
}: Props) {
  const [value, setValue] = useState('');
  const [error, setError] = useState('');
  const bottomRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    setValue('');
    setError('');
  }, [session.id]);

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: 'smooth', block: 'end' });
  }, [messages.length, session.id, stream?.text, stream?.tools.length, pendingUser]);

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

  const busy = sending || Boolean(stream);
  const showEmpty = !loading && messages.length === 0 && !stream && !pendingUser;

  return (
    <div className="chat-panel">
      <div className="chat-head">
        <div className="chat-heading">
          <h2 className="panel-title">{session.title}</h2>
          <Text type="tertiary" size="small">
            {spaceName ? `${spaceName} · ` : ''}
            {agentReady ? `模型 ${model || '已就绪'}` : '模型未配置，无法对话'}
          </Text>
        </div>
      </div>

      <div className="chat-body">
        {loading ? (
          <div className="chat-empty">
            <Spin />
          </div>
        ) : showEmpty ? (
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
              {m.role === 'assistant' ? (
                <AssistantBody content={m.content} reasoning={m.reasoning} tools={toRecords(m.parts)} />
              ) : (
                <div className="chat-msg-content">{m.content}</div>
              )}
            </div>
          ))
        )}

        {pendingUser ? (
          <div className="chat-msg chat-msg-user">
            <div className="chat-msg-meta">
              <span>我</span>
            </div>
            <div className="chat-msg-content">{pendingUser}</div>
          </div>
        ) : null}

        {stream ? (
          <div className="chat-msg chat-msg-assistant">
            <div className="chat-msg-meta">
              <span>Agent</span>
              {busy ? <span className="chat-msg-time">生成中…</span> : null}
            </div>
            {!stream.reasoning && !stream.text && stream.tools.length === 0 ? (
              <div className="chat-msg-content chat-msg-pending">正在思考…</div>
            ) : (
              <AssistantBody content={stream.text} reasoning={stream.reasoning} tools={stream.tools} live />
            )}
          </div>
        ) : null}

        <div ref={bottomRef} />
      </div>

      <div className="chat-input">
        <TextArea
          value={value}
          onChange={setValue}
          placeholder={
            agentReady ? '描述你的任务，Enter 发送，Shift + Enter 换行' : '模型服务未配置，暂时无法发送消息'
          }
          autosize={{ minRows: 2, maxRows: 8 }}
          disabled={busy || !agentReady}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && !e.shiftKey) {
              e.preventDefault();
              void send();
            }
          }}
        />
        <div className="chat-input-foot">
          <span className="chat-error">{error}</span>
          {busy ? (
            <Button theme="borderless" type="danger" icon={<IconClose />} onClick={onStop}>
              停止生成
            </Button>
          ) : (
            <Button
              theme="solid"
              type="primary"
              icon={<IconSend />}
              loading={sending}
              disabled={!value.trim() || !agentReady}
              onClick={send}
            >
              发送
            </Button>
          )}
        </div>
      </div>
    </div>
  );
}
