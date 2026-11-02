import { ApiError, redirectToLogin } from './client';
import type { ChatMessage, ChatSession, ToolCallRecord } from './types';

export interface StreamHandlers {
  onReasoning?: (delta: string) => void;
  onText?: (delta: string) => void;
  onToolStart?: (tool: ToolCallRecord) => void;
  onToolResult?: (tool: ToolCallRecord) => void;
  onDone?: (payload: { message: ChatMessage; session: ChatSession }) => void;
  onError?: (message: string) => void;
}

interface StreamPayload {
  delta?: string;
  /** done 事件是消息对象，error 事件是错误文案 */
  message?: ChatMessage | string;
  tool?: ToolCallRecord;
  session?: ChatSession;
}

/**
 * 与 Agent 对话：POST 后读取 SSE 流。
 *
 * 浏览器原生 EventSource 只支持 GET，这里用 fetch + ReadableStream 手工解析。
 */
export async function streamChat(
  sessionId: string,
  content: string,
  handlers: StreamHandlers,
  signal?: AbortSignal,
): Promise<void> {
  let res: Response;
  try {
    res = await fetch(`/sessions/${sessionId}/chat`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ content }),
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
      case 'reasoning':
        handlers.onReasoning?.(payload.delta ?? '');
        break;
      case 'text':
        handlers.onText?.(payload.delta ?? '');
        break;
      case 'tool_start':
        if (payload.tool) handlers.onToolStart?.(payload.tool);
        break;
      case 'tool_result':
        if (payload.tool) handlers.onToolResult?.(payload.tool);
        break;
      case 'done':
        if (typeof payload.message === 'object' && payload.message && payload.session) {
          handlers.onDone?.({ message: payload.message, session: payload.session });
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
