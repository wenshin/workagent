import {
  IconBook,
  IconClose,
  IconFile,
  IconPaperclip,
  IconSearch,
  IconSend,
  IconTick,
} from '@douyinfe/semi-icons';
import { Button, Empty, Input, Popover, Spin, TextArea, Toast, Tooltip, Typography } from '@douyinfe/semi-ui';
import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';

import type {
  ChatMessage,
  ChatSession,
  MessagePart,
  SpaceFile,
  SpaceSkill,
  StreamItem,
  ToolCallRecord,
} from '../api/types';
import { decideApproval } from '../api/client';
import { useAgents } from '../api/hooks';
import MarkdownView from './Markdown';
import MCPAppKeyCard, { type MCPAppKeyData } from './MCPAppKeyCard';
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
  /** 卡在等人确认：气泡不该继续显示「生成中」，那会让人以为模型还在想 */
  waitingApproval?: boolean;
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
  /** 服务端留了这一轮的断点：可以从断点继续，而不是重新问一遍 */
  resumable?: boolean;
  /** 从断点继续（调用方负责先确认没有正在跑的轮次） */
  onResume?: () => void;
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

/** mcp_app_key 的回执同样是 JSON：交给应用密钥卡片，卡片自己取明文 */
function parseMCPAppRequest(tool: ToolCallRecord): MCPAppKeyData | null {
  if (tool.name !== 'mcp_app_key') return null;

  if (tool.result) {
    try {
      const parsed = JSON.parse(tool.result) as { mcp_app?: MCPAppKeyData };
      if (parsed.mcp_app) return { ...parsed.mcp_app, pending: false };
    } catch {
      // 回执被截断时退回参数兜底，不要因此丢掉整张卡片
    }
  }

  // 回执还没到：卡片先出来，用户可以先看清要做什么
  let app = '';
  let hint = '';
  if (tool.args) {
    try {
      const args = JSON.parse(tool.args) as { app?: string; purpose?: string };
      app = args.app ?? '';
      hint = args.purpose ?? '';
    } catch {
      // 参数不合法时按「待填应用名」展示
    }
  }
  return { app, hint, state: app ? 'checking' : 'needs_app', available: true, pending: true };
}

/** 需要用户动手的卡片（凭证、应用密钥、审批）必须整块露出，不能折进工具组 */
function isStandaloneTool(tool: ToolCallRecord): boolean {
  return (
    parseSecretRequest(tool) !== null ||
    parseMCPAppRequest(tool) !== null ||
    tool.status === 'approval'
  );
}

function ToolCard({
  tool,
  spaceId,
  onSend,
  onResume,
  flat,
}: {
  tool: ToolCallRecord;
  spaceId?: string;
  onSend?: (content: string) => void;
  /** 审批提交后服务端说「这一轮已中断、正在续跑」时重新挂流 */
  onResume?: () => void;
  /** flat：已在工具组的折叠区内，不要再套一层折叠 */
  flat?: boolean;
}) {
  const [busy, setBusy] = useState('');
  const secret = parseSecretRequest(tool);
  // 需要用户动手的卡片不能塞进折叠区：折叠起来等于没提示
  if (secret) {
    return <SecretRequestCard data={secret} onContinue={onSend} />;
  }
  const appKey = parseMCPAppRequest(tool);
  if (appKey) {
    return <MCPAppKeyCard data={appKey} spaceId={spaceId} onContinue={onSend} />;
  }

  const approval = tool.status === 'approval' ? tool.approval : undefined;
  const decide = async (decision: 'approve' | 'deny', scope: 'once' | 'turn') => {
    if (!approval || busy) return;
    const key = `${decision}:${scope}`;
    setBusy(key);
    try {
      const out = await decideApproval(approval.sessionId, approval.id, decision, scope);
      if (out.resumed) {
        // 当时的执行体已经不在了：决策已落库，服务端正从断点接着跑
        Toast.info('这一轮已中断，正在从断点继续');
        onResume?.();
      }
    } catch (err) {
      Toast.error(err instanceof Error ? err.message : '提交审批失败，请重试');
    } finally {
      setBusy('');
    }
  };

  // 风险等级常驻：不只出现在审批卡上。翻历史时「这一步是 L4 外发」比
  // 「成功」有用得多，而且不审批的调用（fs 写、只读查询）同样需要这个标注。
  const risk = approval?.risk || tool.risk || 0;
  const head = (
    <>
      <span className="msg-tool-name">{tool.name}</span>
      {risk > 0 ? <span className={`msg-risk msg-risk-${risk}`}>L{risk}</span> : null}
      <span className="msg-tool-status">{statusLabel(tool.status)}</span>
      {tool.duration_ms !== undefined ? <span className="msg-tool-time">{tool.duration_ms}ms</span> : null}
    </>
  );
  const body = (
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
  );
  // 「本轮」只覆盖这一条消息触发的执行：run 结束后记忆清空，下一轮重新确认
  const approvalBar = approval ? (
    <div className="msg-approval">
      <div className="msg-approval-head">{approval.reason || '该操作需要人工确认'}</div>
      {approval.detail ? <pre className="msg-approval-detail">{approval.detail}</pre> : null}
      <div className="msg-approval-actions">
        <Button
          size="small"
          theme="solid"
          type="primary"
          loading={busy === 'approve:once'}
          onClick={() => decide('approve', 'once')}
        >
          本次同意
        </Button>
        <Button size="small" type="primary" onClick={() => decide('approve', 'turn')}>
          本轮都同意
        </Button>
        <Button size="small" type="danger" onClick={() => decide('deny', 'once')}>
          本次拒绝
        </Button>
        <Button size="small" onClick={() => decide('deny', 'turn')}>
          本轮都拒绝
        </Button>
      </div>
      <div className="msg-approval-hint">
        「本轮」只对这一次执行生效，下一轮需重新确认
        {approval.timeoutS ? `；${Math.max(1, Math.round(approval.timeoutS / 60))} 分钟未处理自动拒绝` : ''}
      </div>
    </div>
  ) : null;

  if (flat) {
    return (
      <div className={`msg-tool msg-tool-flat msg-tool-${tool.status}`}>
        <div className="msg-tool-head">{head}</div>
        {approvalBar}
        {body}
      </div>
    );
  }
  return (
    <details
      className={`msg-tool msg-tool-${tool.status}`}
      open={tool.status === 'running' || !!approval}
    >
      <summary className="msg-tool-head">{head}</summary>
      {approvalBar}
      {body}
    </details>
  );
}

/**
 * 连续的工具调用折成一组，默认收起，只留一行提示（§ D.2 消息区）。
 *
 * 一轮里工具常常连着来好几条（列目录 → 读文件 → 再读），每张卡都占一行，
 * 长回答会被过程信息淹没。折起来之后时间线只剩「思考 → 工具组 → 正文」，
 * 需要查参数或结果再点开；和思考块一样属于过程信息，样式也保持一致。
 */
function ToolGroup({
  tools,
  spaceId,
  onSend,
  onResume,
}: {
  tools: ToolCallRecord[];
  spaceId?: string;
  onSend?: (content: string) => void;
  onResume?: () => void;
}) {
  const running = tools.some((t) => t.status === 'running');
  const failed = tools.filter((t) => t.status === 'error' || t.status === 'denied').length;
  return (
    <details className="msg-tools">
      <summary className="msg-tools-head">
        <span>调用了 {tools.length} 个工具</span>
        {running ? <span className="msg-tools-live">执行中…</span> : null}
        {failed > 0 ? <span className="msg-tools-fail">{failed} 个失败</span> : null}
        {/* 收起时靠名字一眼看出调过什么，不用点开 */}
        <span className="msg-tools-names">{tools.map((t) => t.name).join(' · ')}</span>
      </summary>
      <div className="msg-tools-body">
        {tools.map((tool) => (
          <ToolCard key={tool.id} tool={tool} spaceId={spaceId} onSend={onSend} onResume={onResume} flat />
        ))}
      </div>
    </details>
  );
}

/** 按「是否必须独立展示」切分一串工具调用：两条以上才折叠 */
function groupTools(tools: ToolCallRecord[]): ToolCallRecord[][] {
  const groups: ToolCallRecord[][] = [];
  for (const tool of tools) {
    const last = groups[groups.length - 1];
    if (last && !isStandaloneTool(tool) && !isStandaloneTool(last[0])) {
      last.push(tool);
      continue;
    }
    groups.push([tool]);
  }
  return groups;
}

/** 工具序列的唯一渲染入口：单条照旧是一张卡，连续多条折成一组 */
function ToolRun({
  tools,
  spaceId,
  onSend,
  onResume,
}: {
  tools: ToolCallRecord[];
  spaceId?: string;
  onSend?: (content: string) => void;
  onResume?: () => void;
}) {
  return (
    <>
      {groupTools(tools).map((group) =>
        group.length > 1 ? (
          <ToolGroup
            key={`group-${group[0].id}`}
            tools={group}
            spaceId={spaceId}
            onSend={onSend}
            onResume={onResume}
          />
        ) : (
          <ToolCard
            key={group[0].id}
            tool={group[0]}
            spaceId={spaceId}
            onSend={onSend}
            onResume={onResume}
          />
        ),
      )}
    </>
  );
}

/** 时间线折成渲染块的产物：连续工具调用已并成一块 */
type TurnBlock =
  | { kind: 'reasoning'; text: string }
  | { kind: 'text'; text: string }
  | { kind: 'tools'; tools: ToolCallRecord[] };

/**
 * 把流式片段折成渲染块：只合并工具调用，思考与正文保持原样。
 *
 * 正文是回答本身、思考是过程旁白，两者必须直接可读；工具调用往往一连好几条，
 * 占地方又没有阅读价值，交给 ToolGroup 折起来。
 */
function toBlocks(items: StreamItem[]): TurnBlock[] {
  const blocks: TurnBlock[] = [];
  for (const item of items) {
    if (item.kind === 'reasoning') {
      blocks.push({ kind: 'reasoning', text: item.text });
      continue;
    }
    if (item.kind === 'text') {
      blocks.push({ kind: 'text', text: item.text });
      continue;
    }
    const last = blocks[blocks.length - 1];
    if (last && last.kind === 'tools') {
      last.tools.push(item.tool);
      continue;
    }
    blocks.push({ kind: 'tools', tools: [item.tool] });
  }
  return blocks;
}

function toRecords(parts?: MessagePart[]): ToolCallRecord[] {
  return (parts ?? []).map((p) => ({
    id: p.id,
    name: p.name,
    args: p.args,
    result: p.result,
    status: p.status,
    duration_ms: p.duration_ms,
    risk: p.risk,
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
  onResume,
}: {
  content?: string;
  reasoning?: string;
  tools?: ToolCallRecord[];
  /** 流式过程中的有序片段；有值时按它渲染（仅连续工具调用会被并组） */
  items?: StreamItem[];
  live?: boolean;
  spaceId?: string;
  onSend?: (content: string) => void;
  onResume?: () => void;
}) {
  const blocks = items ? toBlocks(items) : null;
  const hasBody = blocks ? blocks.length > 0 : Boolean(content) || tools.length > 0;
  return (
    <>
      {reasoning ? <ReasoningBlock text={reasoning} live={live && !hasBody} /> : null}
      {blocks ? (
        // 流式：思考、正文与工具按实际到达顺序交错，
        // 而不是「思考钉在顶部、工具全在上、正文全在下」
        blocks.map((block, index) => {
          if (block.kind === 'reasoning') {
            return (
              <ReasoningBlock
                key={`reasoning-${index}`}
                text={block.text}
                // 只有最后一段思考才是「正在想」，前面的都已结束
                live={live && index === blocks.length - 1}
              />
            );
          }
          if (block.kind === 'text') {
            return (
              <div key={`text-${index}`} className="chat-msg-content chat-msg-markdown">
                <MarkdownView content={block.text} spaceId={spaceId} />
              </div>
            );
          }
          return (
            <ToolRun
              key={`tools-${block.tools[0].id}`}
              tools={block.tools}
              spaceId={spaceId}
              onSend={onSend}
              onResume={onResume}
            />
          );
        })
      ) : (
        <>
          {/* 历史消息没有时间位置，工具统一落在正文之前，连续调用同样折成一组 */}
          <ToolRun tools={tools} spaceId={spaceId} onSend={onSend} onResume={onResume} />
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
  resumable,
  onResume,
}: Props) {
  // 智能体清单只在同空间时查：子任务会话的 agent 来自发起方空间，被派发人无权读那份目录，
  // 跨空间只显示「发起方智能体」，不去请求一个必然 403 的接口。
  const agentSpaceId =
    session.agent_space_id && session.agent_space_id !== session.space_id
      ? undefined
      : session.space_id;
  const { data: agentList } = useAgents(agentSpaceId);
  const agentLabel = session.agent_id
    ? agentList?.agents.find((a) => a.id === session.agent_id)?.name ||
      (agentSpaceId ? session.agent_id : '发起方智能体')
    : '';

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
  // 滚动容器与「是否贴底」：用户往上翻过之后就不再把他拽回底部
  const bodyRef = useRef<HTMLDivElement>(null);
  const stickRef = useRef(true);
  // 最后一份流内容：流结束到回复落库之间的空窗期用它顶住列表（见下方 heldStream）
  const handoverRef = useRef<{
    sessionId: string;
    view: StreamView;
    /** 流刚结束时列表里的可见条数：超过它且末尾是已落库的 assistant，说明这一轮回复到了 */
    baseCount: number;
    armed: boolean;
  } | null>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);

  const busy = sending || Boolean(stream);

  // 容差 40px 仍算贴底，避免像素级误差把用户判成「已经上滚了」
  const handleBodyScroll = () => {
    const el = bodyRef.current;
    if (!el) return;
    stickRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
  };

  // 换会话：同步重置贴底与交接状态（必须在 layout 阶段的滚动之前生效）
  const sessionKeyRef = useRef(session.id);
  if (sessionKeyRef.current !== session.id) {
    sessionKeyRef.current = session.id;
    stickRef.current = true;
    handoverRef.current = null;
  }

  // 列表里可见的消息：流式的空占位（后端在首个正文增量时创建）由下面的实时气泡接管
  const visibleMessages = messages.filter((m) => !(m.streaming && !m.content));
  const lastVisible = visibleMessages[visibleMessages.length - 1];

  // 流结束的瞬间 stream 变 null，而这一轮回复还没落库：此时若立刻卸载实时气泡，列表高度
  // 会先塌回去、等消息拉回来再长出来，滚动条看上去就是「先跳到顶、再回到底」。
  // 所以把最后一份流内容按住，等回复真的进了列表；有回复时不再渲染按住的那份，
  // 两者在同一次渲染里交接，既不会重复也不会出现空窗。
  if (stream) {
    handoverRef.current = { sessionId: session.id, view: stream, baseCount: 0, armed: false };
  } else if (handoverRef.current && handoverRef.current.sessionId === session.id && !handoverRef.current.armed) {
    handoverRef.current = { ...handoverRef.current, baseCount: visibleMessages.length, armed: true };
  }
  const handover = !stream && handoverRef.current?.sessionId === session.id ? handoverRef.current : null;
  const replyLanded = Boolean(
    handover &&
      visibleMessages.length > handover.baseCount &&
      lastVisible?.role === 'assistant' &&
      !lastVisible.streaming,
  );
  // 只有真的写出了正文才顶住：失败或只有思考的空轮次不该留一个假气泡
  const heldStream =
    handover && !replyLanded && handover.view.items.some((item) => item.kind === 'text')
      ? handover.view
      : null;
  /** 实际要画出来的实时内容：流式中是真流，交接空窗期是按住的那份 */
  const liveView: StreamView | null = stream ?? heldStream;

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

  // 贴底时才跟随到底部。用 scrollTop 直接赋值，而不是 scrollIntoView({behavior:'smooth'})：
  // 平滑动画会被每个增量重启，长度还在变化时看着就是「先跳到顶再回到底」。
  // 放在 layout 阶段执行，避免先绘制出未贴底的帧。
  useLayoutEffect(() => {
    const el = bodyRef.current;
    if (!el || !stickRef.current) return;
    el.scrollTop = el.scrollHeight;
  }, [messages, session.id, stream?.items, pendingUser, heldStream]);

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
    // 自己发的消息一定跟到底部，哪怕之前往上翻着看
    stickRef.current = true;
    try {
      await onSend(text, refs);
      setValue('');
      setRefs([]);
      setMention(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : '发送失败，请重试');
    }
  };

  // 「继续」= 再发一条消息（凭证卡片录完/审批通过后接着跑）。同一会话不能并发，
  // 正在生成时先不提供按钮——点了也只会被后端拒绝，白等一次；真失败要报出来，
  // 别像以前那样吞成未捕获的 rejection。
  const continueRun: ((content: string) => void) | undefined = busy
    ? undefined
    : (content: string) => {
        onSend(content, []).catch((e) =>
          setError(e instanceof Error ? e.message : '发送失败，请重试'),
        );
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

  const showEmpty = !loading && messages.length === 0 && !liveView && !pendingUser;
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
            {/* 智能体是会话的行为设定：写明是谁在回答，切到别的会话时不至于混淆 */}
            {agentLabel ? `智能体 ${agentLabel} · ` : ''}
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

      <div className="chat-body" ref={bodyRef} onScroll={handleBodyScroll}>
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
          visibleMessages.map((m) => (
            <div key={m.id} className={`chat-msg chat-msg-${m.role}`}>
              <div className="chat-msg-meta">
                <span>
                  {m.source === 'task_brief'
                    ? '任务委托'
                    : m.role === 'user'
                      ? '我'
                      : m.role === 'assistant'
                        ? 'Agent'
                        : '系统'}
                </span>
                {m.source === 'task_result' ? (
                  <span className="chat-msg-time">任务回执</span>
                ) : null}
                {m.source === 'task_remind' ? (
                  <span className="chat-msg-time">催办</span>
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
                  onSend={continueRun}
                  onResume={onResume}
                />
              ) : m.source === 'task_brief' ? (
                // 任务委托里有代码与编号清单，按 Markdown 渲染；普通输入仍走纯文本，避免误把用户打的符号当语法
                <div className="chat-msg-content">
                  <MarkdownView content={m.content} spaceId={session.space_id} />
                </div>
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

        {/* 流式气泡：流结束后由 heldStream 继续顶住，直到这一轮回复进了上面的列表 */}
        {liveView ? (
          <div className="chat-msg chat-msg-assistant">
            <div className="chat-msg-meta">
              <span>Agent</span>
              {busy ? (
                <span className="chat-msg-time">
                  {liveView.waitingApproval
                    ? '等待确认…'
                    : elapsedMs >= 1000
                      ? `生成中 · ${formatDuration(elapsedMs)}`
                      : '生成中…'}
                </span>
              ) : null}
            </div>
            {liveView.items.length === 0 ? (
              <div className="chat-msg-content chat-msg-pending">正在思考…</div>
            ) : (
              <AssistantBody
                items={liveView.items}
                live={Boolean(stream)}
                spaceId={session.space_id}
                onSend={continueRun}
                onResume={onResume}
              />
            )}
          </div>
        ) : null}
      </div>

      <div className="chat-input">
        {/* 断点续跑：上一轮被系统侧中断（卡死、服务重启、到点）时给出入口。
            用户主动停止的不提供——那是明确的意思表示，不该再被「继续」拉回去。 */}
        {resumable && !busy && onResume ? (
          <div className="chat-resume-bar">
            <span>上一轮执行被中断，可以从断点继续（已完成的工具不会重跑）</span>
            <Button size="small" theme="borderless" onClick={onResume}>
              继续
            </Button>
          </div>
        ) : null}
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
