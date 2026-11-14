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
  /** run.finished：中断原因（client_disconnected / wall_clock_exceeded） */
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
): Promise<void> {
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

  if (res.status === 401) {
    redirectToLogin();
    throw new ApiError('未登录或登录已过期', 401, 'unauthenticated');
  }

  if (!res.ok) {
    const data = (await res.json().catch(() => ({}))) as { message?: string; code?: string };
    throw new ApiError(data.message ?? `请求失败（${res.status}）`, res.status, data.code ?? 'unknown');
  }

  if (!res.body) {
    throw new ApiError('当前浏览器不支持流式响应', 0, 'stream_unsupported');
  }

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
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
        break;
      case 'run.finished':
        // 失败与中断都以 run.finished 收尾（成功时由 message.done 更新界面）
        if (payload.status && payload.status !== 'succeeded') {
          const fallback =
            payload.status === 'cancelled' ? '回答已中断，已保留已生成的内容' : '生成失败，请重试';
          handlers.onError?.(payload.error || fallback);
        }
        break;
      case 'error':
        handlers.onError?.(typeof payload.message === 'string' ? payload.message : '生成失败，请重试');
        break;
    }
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
}
