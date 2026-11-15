import { ApiError, redirectToLogin } from './client';
import type { ChatMessage, ChatSession, ToolCallRecord } from './types';

export interface StreamHandlers {
  onRunStarted?: (runId: string) => void;
  onReasoning?: (delta: string) => void;
  onText?: (delta: string) => void;
  onToolStart?: (tool: ToolCallRecord) => void;
  onToolResult?: (tool: ToolCallRecord) => void;
  onApproval?: (request: { approvalId: string; callId: string; tool: string; timeoutS: number }) => void;
  onDone?: (payload: { message: ChatMessage; session: ChatSession }) => void;
  onError?: (message: string) => void;
}

/** 事件载荷字段与后端 §7.3 运行流一致 */
interface StreamPayload {
  run_id?: string;
  role?: string;
  /** message.delta：正文增量 */
  content?: string;
  /** message.delta：思考过程增量 */
  reasoning?: string;
  /** 累计字符数：续流时作为 after_offset 回传 */
  offset?: number;
  call_id?: string;
  tool?: string;
  args?: string;
  /** tool.result：结果摘要 */
  summary?: string;
  status?: string;
  risk?: number;
  duration_ms?: number;
  approval_id?: string;
  timeout_s?: number;
  usage?: { in: number; out: number };
  /** message.done 携带落库后的消息与会话 */
  message?: ChatMessage | string;
  session?: ChatSession;
  /** run.finished 失败或中断时的可读文案 */
  error?: string;
  /** run.finished：中断原因（user_cancelled / wall_clock_exceeded） */
  reason?: string;
}

function toToolRecord(payload: StreamPayload): ToolCallRecord {
  return {
    id: payload.call_id ?? '',
    name: payload.tool ?? '',
    args: payload.args ?? '',
    result: payload.summary,
    status: (payload.status as ToolCallRecord['status']) ?? 'running',
    duration_ms: payload.duration_ms,
  };
}

/** 处理一条事件；返回 true 表示这是这一轮的终态事件（之后不会再有任何增量）。 */
function handleEvent(event: string, payload: StreamPayload, handlers: StreamHandlers): boolean {
  switch (event) {
    case 'run.started':
      handlers.onRunStarted?.(payload.run_id ?? '');
      break;
    case 'message.delta':
      // 思考过程与正文共用同一事件，用字段区分
      if (payload.reasoning) handlers.onReasoning?.(payload.reasoning);
      if (payload.content) handlers.onText?.(payload.content);
      break;
    case 'tool.call':
      handlers.onToolStart?.(toToolRecord(payload));
      break;
    case 'tool.result':
      handlers.onToolResult?.(toToolRecord(payload));
      break;
    case 'approval.req':
      handlers.onApproval?.({
        approvalId: payload.approval_id ?? '',
        callId: payload.call_id ?? '',
        tool: payload.tool ?? '',
        timeoutS: payload.timeout_s ?? 0,
      });
      break;
    case 'message.done':
      if (typeof payload.message === 'object' && payload.message && payload.session) {
        handlers.onDone?.({ message: payload.message, session: payload.session });
      }
      return true;
    case 'run.finished':
      // 失败与中断都以 run.finished 收尾（成功时由 message.done 更新界面）
      if (payload.status && payload.status !== 'succeeded') {
        const fallback =
          payload.status === 'cancelled' ? '回答已中断，已保留已生成的内容' : '生成失败，请重试';
        handlers.onError?.(payload.error || fallback);
      }
      return true;
    case 'error':
      handlers.onError?.(typeof payload.message === 'string' ? payload.message : '生成失败，请重试');
      break;
  }
  return false;
}

/**
 * 一条流的结局。
 *
 * 「流结束」不等于「这一轮结束」：代理掐连接、切页、dev 热更新都会让 body 提前
 * 关闭，而服务端的 run 还在跑（§15.12.1）。调用方必须看 finished 才能决定
 * 是收尾还是重新挂回 /runs/{id}/stream 续传（§15.12.2）。
 */
export interface StreamOutcome {
  /** 收到了 message.done / run.finished：这一轮真的结束了 */
  finished: boolean;
  /** 是否收到过任何一帧：false 说明请求根本没建立起来，重连没有意义 */
  received: boolean;
}

/**
 * 读取一条 SSE 流。chat 与 run 续流共用同一份解析，保证两条链路行为一致。
 */
async function readSSE(res: Response, handlers: StreamHandlers): Promise<StreamOutcome> {
  if (!res.body) {
    throw new ApiError('当前浏览器不支持流式响应', 0, 'stream_unsupported');
  }

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  const outcome: StreamOutcome = { finished: false, received: false };
  let buffer = '';

  const handleBlock = (block: string) => {
    let event = 'message';
    const dataLines: string[] = [];

    block.split('\n').forEach((rawLine) => {
      const line = rawLine.replace(/\r$/, '');
      if (line.startsWith('event:')) event = line.slice(6).trim();
      else if (line.startsWith('data:')) dataLines.push(line.slice(5).trim());
    });
    if (dataLines.length === 0) return;

    let payload: StreamPayload;
    try {
      payload = JSON.parse(dataLines.join('\n')) as StreamPayload;
    } catch {
      return;
    }
    outcome.received = true;
    if (handleEvent(event, payload, handlers)) outcome.finished = true;
  };

  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });

    let sep = buffer.indexOf('\n\n');
    while (sep >= 0) {
      handleBlock(buffer.slice(0, sep));
      buffer = buffer.slice(sep + 2);
      sep = buffer.indexOf('\n\n');
    }
  }

  if (buffer.trim()) handleBlock(buffer);
  return outcome;
}

async function ensureOK(res: Response): Promise<void> {
  if (res.status === 401) {
    redirectToLogin();
    throw new ApiError('未登录或登录已过期', 401, 'unauthenticated');
  }
  if (!res.ok) {
    const data = (await res.json().catch(() => ({}))) as { message?: string; code?: string };
    throw new ApiError(data.message ?? `请求失败（${res.status}）`, res.status, data.code ?? 'unknown');
  }
}

/**
 * 与 Agent 对话：POST 后读取 SSE 流。
 *
 * 浏览器原生 EventSource 只支持 GET，这里用 fetch + ReadableStream 手工解析。
 */
export async function streamChat(
  sessionId: string,
  content: string,
  references: string[],
  handlers: StreamHandlers,
  signal?: AbortSignal,
): Promise<StreamOutcome> {
  let res: Response;
  try {
    res = await fetch(`/sessions/${sessionId}/chat`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      // references 是相对空间 files/ 的路径；正文由服务端拼上引用清单落库
      body: JSON.stringify({ content, references }),
      signal,
    });
  } catch (err) {
    if (err instanceof DOMException && err.name === 'AbortError') throw err;
    throw new ApiError('网络异常，请检查服务是否正常', 0, 'network_error');
  }

  await ensureOK(res);
  return readSSE(res, handlers);
}

/**
 * 续上一次仍在执行的 run：刷新页面后从这里接着往下看。
 *
 * afterOffset 传客户端已渲染到的累计字符数，服务端只补发之后的增量；
 * 传 0 表示从头补（内存缓冲里这一轮的全部内容）。
 */
export async function streamRun(
  runId: string,
  afterOffset: number,
  handlers: StreamHandlers,
  signal?: AbortSignal,
): Promise<StreamOutcome> {
  let res: Response;
  try {
    res = await fetch(`/runs/${runId}/stream?after_offset=${afterOffset}`, { signal });
  } catch (err) {
    if (err instanceof DOMException && err.name === 'AbortError') throw err;
    throw new ApiError('网络异常，请检查服务是否正常', 0, 'network_error');
  }

  await ensureOK(res);
  return readSSE(res, handlers);
}
