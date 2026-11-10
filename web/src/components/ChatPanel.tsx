import { IconBook, IconClose, IconSend } from '@douyinfe/semi-icons';
import { Button, Empty, Popover, Spin, TextArea, Tooltip, Typography } from '@douyinfe/semi-ui';
import { useEffect, useRef, useState } from 'react';

import type { ChatMessage, ChatSession, MessagePart, SpaceSkill, ToolCallRecord } from '../api/types';
import MarkdownView from './Markdown';
import { PluginIcon } from './PluginVisual';

const { Text } = Typography;

// 输入框高度：单行（行高 21 + 上下内边距 12）≈ 33px，上限约 8 行。
// 拖拽改的是「最小高度」，内容变多时仍由 autosize 自动增高，超过上限改为框内滚动。
const INPUT_MIN_HEIGHT = 33;
const INPUT_MAX_HEIGHT = 200;

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
  /** 当前空间 + 系统空间合并后的技能 */
  skills: SpaceSkill[];
  /** 选择会话技能，空串表示清除 */
  onSelectSkill: (path: string) => void;
  onSend: (content: string) => Promise<void>;
  onStop: () => void;
}

function formatTime(iso: string) {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return '';
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

/** token 数：340 / 1.5k / 26k */
function formatTokens(n: number) {
  if (!n) return '0';
  if (n < 1000) return String(n);
  const k = n / 1000;
  return `${k >= 100 ? Math.round(k) : k.toFixed(1)}k`;
}

/** 耗时：820ms / 12.3s / 1m05s */
function formatDuration(ms: number) {
  if (!ms) return '';
  if (ms < 1000) return `${ms}ms`;
  const seconds = ms / 1000;
  if (seconds < 60) return `${seconds.toFixed(1)}s`;
  const minutes = Math.floor(seconds / 60);
  const rest = Math.round(seconds - minutes * 60);
  return rest > 0 ? `${minutes}m${String(rest).padStart(2, '0')}s` : `${minutes}m`;
}

/** 助手消息的用量：token 消耗（输入 ↑ / 输出 ↓）与整轮总耗时 */
function MessageStats({
  inTokens,
  outTokens,
  durationMs,
}: {
  inTokens?: number;
  outTokens?: number;
  durationMs?: number;
}) {
  const tokens =
    inTokens || outTokens ? `↑${formatTokens(inTokens ?? 0)} ↓${formatTokens(outTokens ?? 0)}` : '';
  const duration = formatDuration(durationMs ?? 0);
  if (!tokens && !duration) return null;
  return (
    <span className="chat-msg-stats" title="本轮 token 消耗（输入 ↑ / 输出 ↓）与总耗时">
      {tokens}
      {tokens && duration ? ' · ' : ''}
      {duration}
    </span>
  );
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
  spaceId,
}: {
  content: string;
  reasoning?: string;
  tools: ToolCallRecord[];
  live?: boolean;
  spaceId?: string;
}) {
  return (
    <>
      {reasoning ? <ReasoningBlock text={reasoning} live={live && !content && tools.length === 0} /> : null}
      {tools.map((tool) => (
        <ToolCard key={tool.id} tool={tool} />
      ))}
      {content ? (
        <div className="chat-msg-content chat-msg-markdown">
          <MarkdownView content={content} spaceId={spaceId} />
        </div>
      ) : null}
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
  skills,
  onSelectSkill,
  onSend,
  onStop,
}: Props) {
  const [value, setValue] = useState('');
  const [error, setError] = useState('');
  const [skillPickerOpen, setSkillPickerOpen] = useState(false);
  // 流式期间的本地计时，用于「生成中 · 12.3s」；落库后的总耗时以后端 duration_ms 为准
  const [elapsedMs, setElapsedMs] = useState(0);
  // 拖拽设定的输入框最小高度；0 表示不干预，交给 autosize 按内容决定
  const [inputMinHeight, setInputMinHeight] = useState(0);
  const startedAtRef = useRef<number | null>(null);
  const composerRef = useRef<HTMLDivElement>(null);
  const bottomRef = useRef<HTMLDivElement>(null);

  const busy = sending || Boolean(stream);

  useEffect(() => {
    setValue('');
    setError('');
  }, [session.id]);

  // 组件的 message 动作不直接发消息，而是把文本填进输入框：发不发、改不改由用户决定
  useEffect(() => {
    const onUiMessage = (event: Event) => {
      const text = (event as CustomEvent<{ text?: string }>).detail?.text;
      if (text) setValue(text);
    };
    window.addEventListener('wa-ui-message', onUiMessage);
    return () => window.removeEventListener('wa-ui-message', onUiMessage);
  }, []);

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: 'smooth', block: 'end' });
  }, [messages.length, session.id, stream?.text, stream?.tools.length, pendingUser]);

  useEffect(() => {
    if (!busy) {
      startedAtRef.current = null;
      setElapsedMs(0);
      return;
    }
    if (startedAtRef.current === null) startedAtRef.current = Date.now();
    const tick = () => setElapsedMs(Date.now() - (startedAtRef.current ?? Date.now()));
    tick();
    const timer = window.setInterval(tick, 200);
    return () => window.clearInterval(timer);
  }, [busy]);

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

  // 手柄在输入框上边框上：往上拖 = 变高，往下拖 = 变矮（最矮一行，双击恢复自动高度）
  const startResize = (e: React.MouseEvent<HTMLDivElement>) => {
    e.preventDefault();
    const box = composerRef.current?.querySelector('textarea');
    const startHeight = box?.offsetHeight || INPUT_MIN_HEIGHT;
    const startY = e.clientY;
    const clamp = (px: number) => Math.min(Math.max(px, INPUT_MIN_HEIGHT), INPUT_MAX_HEIGHT);

    const onMove = (ev: MouseEvent) => setInputMinHeight(clamp(Math.round(startHeight + (startY - ev.clientY))));
    const onUp = () => {
      window.removeEventListener('mousemove', onMove);
      window.removeEventListener('mouseup', onUp);
      document.body.style.userSelect = '';
    };

    window.addEventListener('mousemove', onMove);
    window.addEventListener('mouseup', onUp);
    // 拖拽期间别把页面文字选中
    document.body.style.userSelect = 'none';
    setInputMinHeight(clamp(startHeight));
  };

  const resetInputHeight = () => setInputMinHeight(0);

  const showEmpty = !loading && messages.length === 0 && !stream && !pendingUser;
  const activeSkill = skills.find((s) => s.path === session.skill_path);

  const pickSkill = (path: string) => {
    onSelectSkill(path);
    setSkillPickerOpen(false);
  };

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

        <Popover
          trigger="click"
          position="bottomRight"
          visible={skillPickerOpen}
          onVisibleChange={setSkillPickerOpen}
          content={
            <div className="skill-picker">
              <div className="skill-picker-head">技能（当前空间与系统空间）</div>
              {skills.length === 0 ? (
                <div className="skill-picker-empty">还没有技能，可让 Agent 帮你创建一个</div>
              ) : (
                skills.map((s) => (
                  <button
                    key={s.path}
                    type="button"
                    className={`skill-picker-item${s.path === session.skill_path ? ' is-active' : ''}`}
                    onClick={() => pickSkill(s.path)}
                  >
                    <PluginIcon kind="skill" size="sm" />
                    <span className="skill-picker-body">
                      <span className="skill-picker-name">
                        {s.name}
                        <span className={`skill-source skill-source-${s.source}`}>
                          {s.source === 'system' ? '系统' : '当前空间'}
                        </span>
                      </span>
                      {s.description ? <span className="skill-picker-desc">{s.description}</span> : null}
                    </span>
                  </button>
                ))
              )}
              {session.skill_path ? (
                <button type="button" className="skill-picker-clear" onClick={() => pickSkill('')}>
                  不使用技能
                </button>
              ) : null}
            </div>
          }
        >
          <Button
            size="small"
            theme="borderless"
            icon={<IconBook />}
            className={`chat-skill-btn${activeSkill ? ' is-active' : ''}`}
          >
            {activeSkill ? activeSkill.name : '选择技能'}
          </Button>
        </Popover>
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
                {m.role === 'assistant' ? (
                  <MessageStats
                    inTokens={m.in_tokens}
                    outTokens={m.out_tokens}
                    durationMs={m.duration_ms}
                  />
                ) : null}
              </div>
              {m.role === 'assistant' ? (
                <AssistantBody
                  content={m.content}
                  reasoning={m.reasoning}
                  tools={toRecords(m.parts)}
                  spaceId={session.space_id}
                />
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
              {busy ? (
                <span className="chat-msg-time">
                  {elapsedMs >= 1000 ? `生成中 · ${formatDuration(elapsedMs)}` : '生成中…'}
                </span>
              ) : null}
            </div>
            {!stream.reasoning && !stream.text && stream.tools.length === 0 ? (
              <div className="chat-msg-content chat-msg-pending">正在思考…</div>
            ) : (
              <AssistantBody
                content={stream.text}
                reasoning={stream.reasoning}
                tools={stream.tools}
                live
                spaceId={session.space_id}
              />
            )}
          </div>
        ) : null}

        <div ref={bottomRef} />
      </div>

      <div className="chat-input">
        {/* 输入框与操作按钮同一行：按钮嵌在输入框内部右侧 */}
        <div className="chat-composer" ref={composerRef}>
          <div
            className="chat-composer-resize"
            role="separator"
            aria-label="拖拽调整输入框高度"
            title="拖拽调整输入框高度，双击恢复自动高度"
            onMouseDown={startResize}
            onDoubleClick={resetInputHeight}
          >
            <span className="chat-composer-resize-bar" />
          </div>
          <TextArea
            className="chat-composer-input"
            value={value}
            onChange={setValue}
            placeholder={agentReady ? '描述你的任务…' : '模型服务未配置，暂时无法发送消息'}
            autosize={{ minRows: 1 }}
            textareaStyle={inputMinHeight ? { minHeight: inputMinHeight } : undefined}
            borderless
            disabled={busy || !agentReady}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && !e.shiftKey) {
                e.preventDefault();
                void send();
              }
            }}
          />
          <div className="chat-composer-actions">
            {busy ? (
              <Tooltip content="停止生成" position="topRight">
                <Button
                  className="chat-stop-btn"
                  theme="borderless"
                  type="danger"
                  icon={<IconClose />}
                  onClick={onStop}
                  aria-label="停止生成"
                />
              </Tooltip>
            ) : (
              <Button
                className="chat-send-btn"
                theme="solid"
                type="primary"
                icon={<IconSend />}
                loading={sending}
                disabled={!value.trim() || !agentReady}
                onClick={send}
                aria-label="发送"
              />
            )}
          </div>
        </div>
        <div className="chat-input-foot">
          <span className="chat-error">{error}</span>
          <span className="chat-input-hint">
            {busy ? `已用时 ${formatDuration(elapsedMs) || '0.0s'}` : 'Enter 发送，Shift + Enter 换行'}
          </span>
        </div>
      </div>
    </div>
  );
}
