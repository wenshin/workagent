import {
  IconBook,
  IconClose,
  IconFile,
  IconPaperclip,
  IconSearch,
  IconSend,
  IconTick,
} from '@douyinfe/semi-icons';
import { Button, Empty, Input, Popover, Spin, TextArea, Tooltip, Typography } from '@douyinfe/semi-ui';
import { useEffect, useMemo, useRef, useState } from 'react';

import type {
  ChatMessage,
  ChatSession,
  MessagePart,
  SpaceFile,
  SpaceSkill,
  StreamItem,
  ToolCallRecord,
} from '../api/types';
import MarkdownView from './Markdown';
import { PluginIcon } from './PluginVisual';
import SecretRequestCard, { type SecretRequestData } from './SecretRequestCard';

const { Text } = Typography;

// 输入框高度：单行（行高 21 + 上下内边距 12）≈ 33px，上限约 8 行。
// 拖拽改的是「最小高度」，内容变多时仍由 autosize 自动增高，超过上限改为框内滚动。
const INPUT_MIN_HEIGHT = 33;
const INPUT_MAX_HEIGHT = 200;

/** 流式过程中的临时视图 */
export interface StreamView {
  /** 思考、正文与工具调用按到达顺序排列：渲染时照序输出，保持真实时间线 */
  items: StreamItem[];
}

/**
 * 用户消息里的「引用文件」块标记，与后端 session.ReferenceMarker 保持一致。
 * 正文之后是服务端追加的文件清单，前端把它切成标签而不是一整段文本。
 */
const REFERENCE_MARKER = '\n\n[引用文件]';

/** 拆出正文与引用文件路径；没有标记时原样返回 */
export function splitReferences(content: string): { text: string; refs: string[] } {
  const idx = content.indexOf(REFERENCE_MARKER);
  if (idx < 0) return { text: content, refs: [] };
  const refs = content
    .slice(idx + REFERENCE_MARKER.length)
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line.startsWith('- '))
    .map((line) => line.slice(2).trim())
    .filter(Boolean);
  return { text: content.slice(0, idx), refs };
}

/** 引用文件标签：输入框上方可删除，消息气泡里只读 */
function ReferenceChips({ paths, onRemove }: { paths: string[]; onRemove?: (path: string) => void }) {
  if (paths.length === 0) return null;
  return (
    <div className="chat-refs">
      {paths.map((p) => {
        const name = p.split('/').pop() || p;
        return (
          <span key={p} className="chat-ref" title={p}>
            <IconFile size="small" className="chat-ref-icon" />
            <span className="chat-ref-name">{name}</span>
            {onRemove ? (
              <button
                type="button"
                className="chat-ref-remove"
                aria-label={`移除 ${name}`}
                onClick={() => onRemove(p)}
              >
                <IconClose size="small" />
              </button>
            ) : null}
          </span>
        );
      })}
    </div>
  );
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
  /** 单文件上传上限（MB），来自服务端配置 UPLOAD_MAX_FILE_MB */
  uploadMaxMb?: number;
  /** 当前空间 + 系统空间合并后的技能 */
  skills: SpaceSkill[];
  /** 选择会话技能，空串表示清除 */
  onSelectSkill: (path: string) => void;
  /** 打开选择器前重新取一次：技能可能刚在另一个页面新建/启停过 */
  onRefreshSkills?: () => void;
  /** 当前空间里可引用的文件 */
  spaceFiles: SpaceFile[];
  /** 上传文件到当前空间，返回服务端落盘后的文件（同名会被改名） */
  onUploadFiles: (files: File[]) => Promise<SpaceFile[]>;
  /** references 是相对空间 files/ 的引用文件路径 */
  onSend: (content: string, references: string[]) => Promise<void>;
  onStop: () => void;
  /** 本轮占位气泡里已引用的文件（流式过程中还没落库） */
  pendingRefs?: string[];
}

/** 用户消息：正文 + 引用文件标签 */
function UserBody({ content, refs }: { content: string; refs?: string[] }) {
  const parsed = splitReferences(content);
  return (
    <>
      <div className="chat-msg-content">{parsed.text}</div>
      <ReferenceChips paths={refs ?? parsed.refs} />
    </>
  );
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

/**
 * 思考块：默认折叠。
 *
 * 一轮对话里思考可能出现多次（想一段 → 调工具 → 再想一段），每段独立成块，
 * 按时间线穿插在正文与工具卡之间。折叠是刻意的：思考是过程信息，
 * 全部展开会把时间线淹掉；折叠后只看标题就能数出「已经想到第几段」。
 */
function ReasoningBlock({ text, live }: { text: string; live?: boolean }) {
  return (
    <details className="msg-reasoning">
      <summary className="msg-reasoning-head">
        <span>思考过程</span>
        {live ? <span className="msg-reasoning-live">思考中…</span> : null}
        <span className="msg-reasoning-size">{text.length} 字</span>
      </summary>
      <div className="msg-reasoning-body">{text}</div>
    </details>
  );
}

/** secret_request 的回执是 JSON：解析出来交给专用录入卡片，解析不出仍按普通工具展示 */
function parseSecretRequest(tool: ToolCallRecord): SecretRequestData | null {
  if (tool.name !== 'secret_request') return null;

  // 回执先到就以回执为准（含 configured / fingerprint / can_write）
  if (tool.result) {
    try {
      const parsed = JSON.parse(tool.result) as { secret_request?: SecretRequestData };
      // 回执到了就以它为准（历史消息的回执没有 state 字段，靠 configured 判断即可）
      if (parsed.secret_request) return { ...parsed.secret_request, pending: false };
    } catch {
      // 回执被截断或包了前缀时继续走下面的参数兜底，不要因此丢掉整张卡片
    }
  }

  // 回执还没到（流式中途）或解析失败：用调用参数先渲染一张「检查中」的卡片，
  // 让用户第一时间看到组件，而不是等一个可能永远不来的结果事件。
  if (tool.args) {
    try {
      const args = JSON.parse(tool.args) as Partial<SecretRequestData>;
      if (args.scope && args.name) {
        return {
          scope: args.scope,
          name: args.name,
          label: args.label || args.name,
          hint: args.hint,
          configured: false,
          can_write: false,
          available: true,
          pending: true,
        };
      }
    } catch {
      return null;
    }
  }
  return null;
}

function ToolCard({ tool, onSend }: { tool: ToolCallRecord; onSend?: (content: string) => void }) {
  const secret = parseSecretRequest(tool);
  // 需要用户动手的卡片不能塞进折叠区：折叠起来等于没提示
  if (secret) {
    return <SecretRequestCard data={secret} onContinue={onSend} />;
  }
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
  tools = [],
  items,
  live,
  spaceId,
  onSend,
}: {
  content?: string;
  reasoning?: string;
  tools?: ToolCallRecord[];
  /** 流式过程中的有序片段；有值时按它渲染，不做分组 */
  items?: StreamItem[];
  live?: boolean;
  spaceId?: string;
  onSend?: (content: string) => void;
}) {
  const hasBody = items ? items.length > 0 : Boolean(content) || tools.length > 0;
  return (
    <>
      {reasoning ? <ReasoningBlock text={reasoning} live={live && !hasBody} /> : null}
      {items ? (
        // 流式：思考、正文与工具卡按实际到达顺序交错，
        // 而不是「思考钉在顶部、工具全在上、正文全在下」
        items.map((item, index) => {
          if (item.kind === 'reasoning') {
            return (
              <ReasoningBlock
                key={`reasoning-${index}`}
                text={item.text}
                // 只有最后一段思考才是「正在想」，前面的都已结束
                live={live && index === items.length - 1}
              />
            );
          }
          if (item.kind === 'text') {
            return (
              <div key={`text-${index}`} className="chat-msg-content chat-msg-markdown">
                <MarkdownView content={item.text} spaceId={spaceId} />
              </div>
            );
          }
          return <ToolCard key={`tool-${item.tool.id}`} tool={item.tool} onSend={onSend} />;
        })
      ) : (
        <>
          {tools.map((tool) => (
            <ToolCard key={tool.id} tool={tool} onSend={onSend} />
          ))}
          {content ? (
            <div className="chat-msg-content chat-msg-markdown">
              <MarkdownView content={content} spaceId={spaceId} />
            </div>
          ) : null}
        </>
      )}
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
  uploadMaxMb,
  skills,
  onSelectSkill,
  onRefreshSkills,
  spaceFiles,
  onUploadFiles,
  onSend,
  onStop,
  pendingRefs,
}: Props) {
  const [value, setValue] = useState('');
  const [error, setError] = useState('');
  const [skillPickerOpen, setSkillPickerOpen] = useState(false);
  // 本次要引用的文件（相对空间 files/ 的路径）：发送时随消息一起提交
  const [refs, setRefs] = useState<string[]>([]);
  const [refPickerOpen, setRefPickerOpen] = useState(false);
  const [refQuery, setRefQuery] = useState('');
  // 输入框里打 @ 触发的就地引用：start 是 @ 的位置，query 是 @ 之后到光标前的文本
  const [mention, setMention] = useState<{ start: number; query: string } | null>(null);
  const [uploading, setUploading] = useState(false);
  // 流式期间的本地计时，用于「生成中 · 12.3s」；落库后的总耗时以后端 duration_ms 为准
  const [elapsedMs, setElapsedMs] = useState(0);
  // 拖拽设定的输入框最小高度；0 表示不干预，交给 autosize 按内容决定
  const [inputMinHeight, setInputMinHeight] = useState(0);
  const startedAtRef = useRef<number | null>(null);
  const composerRef = useRef<HTMLDivElement>(null);
  const bottomRef = useRef<HTMLDivElement>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);

  const busy = sending || Boolean(stream);

  useEffect(() => {
    setValue('');
    setError('');
    setRefs([]);
    setRefQuery('');
    setMention(null);
  }, [session.id]);

  const removeRef = (path: string) => setRefs((prev) => prev.filter((p) => p !== path));

  const addRef = (path: string) =>
    setRefs((prev) => {
      if (prev.includes(path)) return prev;
      // 与后端 maxReferences 对齐：超出部分不再追加，避免发送后才被截断
      if (prev.length >= 20) return prev;
      return [...prev, path];
    });

  const toggleRef = (path: string) =>
    setRefs((prev) => {
      if (prev.includes(path)) return prev.filter((p) => p !== path);
      // 与后端 maxReferences 对齐：超出部分不再追加，避免发送后才被截断
      if (prev.length >= 20) return prev;
      return [...prev, path];
    });

  // 输入框里打 @ 就地选文件：@ 之后到光标前没有空白就当作在搜文件。
  // 中文里「总结@合同」通常不带空格，所以不能要求 @ 前面是空白。
  const handleChange = (next: string) => {
    setValue(next);
    const at = next.lastIndexOf('@');
    const token = at >= 0 ? next.slice(at + 1) : '';
    if (at >= 0 && !/[\s@]/.test(token)) {
      setMention({ start: at, query: token });
      return;
    }
    if (mention) setMention(null);
  };

  // 从 @ 候选里选一个：把 @query 从正文里删掉，文件变成引用标签，焦点还给输入框
  const selectFromMention = (path: string) => {
    if (!mention) return;
    setValue(value.slice(0, mention.start) + value.slice(mention.start + 1 + mention.query.length));
    setMention(null);
    addRef(path);
    requestAnimationFrame(() => {
      const box = composerRef.current?.querySelector<HTMLTextAreaElement>('textarea');
      if (!box) return;
      box.focus();
      box.setSelectionRange(box.value.length, box.value.length);
    });
  };

  // @ 浮层与按钮浮层共用一个 Popover：谁触发都渲染同一份文件列表
  const pickerVisible = refPickerOpen || mention !== null;
  const pickerQuery = mention ? mention.query : refQuery;

  // 上传成功即引用：用户传文件的下一步几乎总是让 Agent 读它
  const handleUpload = async (list: FileList | null) => {
    const picked = Array.from(list ?? []);
    if (picked.length === 0) return;
    // 已知上限时先本地挡一次，省掉一次注定 413 的往返
    if (uploadMaxMb) {
      const limitBytes = uploadMaxMb * 1024 * 1024;
      const tooBig = picked.find((f) => f.size > limitBytes);
      if (tooBig) {
        setError(`「${tooBig.name}」超过 ${uploadMaxMb}MB 上限`);
        if (fileInputRef.current) fileInputRef.current.value = '';
        return;
      }
    }
    setError('');
    setUploading(true);
    try {
      const saved = await onUploadFiles(picked);
      setRefs((prev) => {
        const next = [...prev];
        saved.forEach((f) => {
          if (!next.includes(f.path) && next.length < 20) next.push(f.path);
        });
        return next;
      });
    } catch (e) {
      setError(e instanceof Error ? e.message : '上传失败，请重试');
    } finally {
      setUploading(false);
      // 允许再次选择同一个文件
      if (fileInputRef.current) fileInputRef.current.value = '';
    }
  };

  const fileOptions = useMemo(() => {
    const q = pickerQuery.trim().toLowerCase();
    const list = q ? spaceFiles.filter((f) => f.path.toLowerCase().includes(q)) : spaceFiles;
    return list.slice(0, 100);
  }, [spaceFiles, pickerQuery]);

  // 组件的 message 动作不直接发消息，而是把文本填进输入框：发不发、改不改由用户决定
  useEffect(() => {
    const onUiMessage = (event: Event) => {
      const text = (event as CustomEvent<{ text?: string }>).detail?.text;
      if (text) {
        setValue(text);
        setMention(null);
      }
    };
    window.addEventListener('wa-ui-message', onUiMessage);
    return () => window.removeEventListener('wa-ui-message', onUiMessage);
  }, []);

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: 'smooth', block: 'end' });
  }, [messages.length, session.id, stream?.items, pendingUser]);

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
      await onSend(text, refs);
      setValue('');
      setRefs([]);
      setMention(null);
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
            {/* 会话可以挂在某个目录下开：把现场写进标题区，和树里的位置对得上 */}
            {session.dir ? `目录 ${session.dir} · ` : ''}
            {agentReady ? `模型 ${model || '已就绪'}` : '模型未配置，无法对话'}
          </Text>
        </div>

        <Popover
          trigger="click"
          position="bottomRight"
          visible={skillPickerOpen}
          onVisibleChange={(v) => {
            setSkillPickerOpen(v);
            // 每次打开都以服务端为准：技能可能在插件面板里刚新建、启用或停用
            if (v) onRefreshSkills?.();
          }}
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
          messages
            // 占位的空 streaming 消息由下面的实时视图接管，避免出现一条空气泡
            .filter((m) => !(m.streaming && !m.content))
            .map((m) => (
            <div key={m.id} className={`chat-msg chat-msg-${m.role}`}>
              <div className="chat-msg-meta">
                <span>{m.role === 'user' ? '我' : m.role === 'assistant' ? 'Agent' : '系统'}</span>
                {m.source === 'task_result' ? (
                  <span className="chat-msg-time">任务回执</span>
                ) : null}
                {m.interrupted ? <span className="chat-msg-time">已中断</span> : null}
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
                  onSend={(c) => void onSend(c, [])}
                />
              ) : (
                <UserBody content={m.content} />
              )}
            </div>
          ))
        )}

        {pendingUser ? (
          <div className="chat-msg chat-msg-user">
            <div className="chat-msg-meta">
              <span>我</span>
            </div>
            <UserBody content={pendingUser} refs={pendingRefs} />
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
            {stream.items.length === 0 ? (
              <div className="chat-msg-content chat-msg-pending">正在思考…</div>
            ) : (
              <AssistantBody
                items={stream.items}
                live
                spaceId={session.space_id}
                onSend={(c) => void onSend(c, [])}
              />
            )}
          </div>
        ) : null}

        <div ref={bottomRef} />
      </div>

      <div className="chat-input">
        {/* 输入框与操作按钮同一行：按钮嵌在输入框内部右侧；引用标签单独占上方一行 */}
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
          <ReferenceChips paths={refs} onRemove={removeRef} />
          <div className="chat-composer-row">
            <TextArea
              className="chat-composer-input"
              value={value}
              onChange={handleChange}
              placeholder={agentReady ? '描述你的任务…（输入 @ 引用空间文件）' : '模型服务未配置，暂时无法发送消息'}
              autosize={{ minRows: 1 }}
              textareaStyle={inputMinHeight ? { minHeight: inputMinHeight } : undefined}
              borderless
              disabled={busy || !agentReady}
              onKeyDown={(e) => {
                if (mention) {
                  if (e.key === 'Escape') {
                    e.preventDefault();
                    setMention(null);
                    return;
                  }
                  // 键盘优先选第一个候选：@ 列表打开时回车不该直接把半截文件名发出去
                  if (e.key === 'Enter' && !e.shiftKey && fileOptions.length > 0) {
                    e.preventDefault();
                    selectFromMention(fileOptions[0].path);
                    return;
                  }
                }
                if (e.key === 'Enter' && !e.shiftKey) {
                  e.preventDefault();
                  void send();
                }
              }}
            />
            <div className="chat-composer-actions">
              <input
                ref={fileInputRef}
                type="file"
                multiple
                hidden
                accept=".md,.json,.txt,.csv,.yaml,.yml"
                onChange={(e) => void handleUpload(e.target.files)}
              />
              <Tooltip
                content={`上传文件到当前空间（md / json / txt / csv / yaml，${
                  uploadMaxMb ? `单个 ≤ ${uploadMaxMb}MB` : '单个大小受服务端配置限制'
                }）`}
                position="topRight"
              >
                <Button
                  className="chat-tool-btn"
                  theme="borderless"
                  icon={<IconPaperclip />}
                  loading={uploading}
                  disabled={busy || !agentReady}
                  onClick={() => fileInputRef.current?.click()}
                  aria-label="上传文件"
                />
              </Tooltip>
              <Popover
                trigger="click"
                position="topRight"
                visible={pickerVisible}
                onVisibleChange={(v) => {
                  setRefPickerOpen(v);
                  if (!v) setMention(null);
                }}
                content={
                  <div className="file-picker">
                    <div className="file-picker-head">
                      {mention ? `引用文件：${mention.query || '全部'}` : '引用当前空间的文件（可多选）'}
                    </div>
                    {/* @ 触发时搜索词来自输入框里的 @query，不再放第二个搜索框 */}
                    {mention ? null : (
                      <Input
                        size="small"
                        prefix={<IconSearch />}
                        placeholder="搜索文件名或路径"
                        value={refQuery}
                        showClear
                        onChange={setRefQuery}
                      />
                    )}
                    <div className="file-picker-list">
                      {fileOptions.length === 0 ? (
                        <div className="file-picker-empty">
                          {spaceFiles.length === 0 ? '当前空间还没有文件，可先上传' : '没有匹配的文件'}
                        </div>
                      ) : (
                        fileOptions.map((f) => (
                          <button
                            key={`${f.space_id}/${f.path}`}
                            type="button"
                            className={`file-picker-item${refs.includes(f.path) ? ' is-active' : ''}`}
                            aria-pressed={refs.includes(f.path)}
                            onClick={() => (mention ? selectFromMention(f.path) : toggleRef(f.path))}
                          >
                            <IconFile className="file-picker-icon" />
                            <span className="file-picker-body">
                              <span className="file-picker-name">{f.name}</span>
                              <span className="file-picker-path">{f.dir || '空间根'}</span>
                            </span>
                            {refs.includes(f.path) ? <IconTick className="file-picker-check" /> : null}
                          </button>
                        ))
                      )}
                    </div>
                  </div>
                }
              >
                <Button
                  className={`chat-tool-btn${refs.length > 0 ? ' is-active' : ''}`}
                  theme="borderless"
                  icon={<IconFile />}
                  disabled={busy || !agentReady}
                  aria-label="引用文件"
                />
              </Popover>
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
