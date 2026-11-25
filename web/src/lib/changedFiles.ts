import type { ChatMessage, MessageToolPart, StreamItem, ToolStatus } from '../api/types';

/** 判断「写过哪个文件」只需要这三个字段：历史 parts 与实时工具记录都满足 */
export interface WrittenToolCall {
  name: string;
  args: string;
  status: ToolStatus;
}

/** 工具参数是流式 JSON：参数还没生成完时解析失败，这一帧直接跳过 */
export function parseToolArgs(args: string): Record<string, unknown> | null {
  try {
    const value: unknown = JSON.parse(args);
    return value && typeof value === 'object' ? (value as Record<string, unknown>) : null;
  } catch {
    return null;
  }
}

/**
 * 写入的空间文件，按路径去重、保留首次出现的位置。
 *
 * 只认工具回执：`file_write` 成功才算写过——失败的那次（例如参数超限）不能出现在
 * 「已写入」里；append 分段写同一路径按一次计。wiki_write 落在系统空间，
 * 普通成员打不开，不放进这份可点击的清单。
 */
export function writtenPaths(tools: Iterable<WrittenToolCall>): string[] {
  const out: string[] = [];
  for (const tool of tools) {
    if (tool.name !== 'file_write' || tool.status !== 'ok') continue;
    const args = parseToolArgs(tool.args);
    const path = typeof args?.path === 'string' ? args.path.trim() : '';
    if (path && !out.includes(path)) out.push(path);
  }
  return out;
}

/**
 * 整个会话写入过的文件：历史消息按时间顺序，再接当前这一轮的实时工具记录。
 *
 * 分栏右侧的清单与每条消息底部的「本轮写入」共用同一份判定（`writtenPaths`），
 * 否则同一轮里两处给出的文件名会不一致。
 */
export function sessionWrittenPaths(
  messages: ChatMessage[],
  stream?: StreamItem[] | null,
): string[] {
  const out: string[] = [];
  const add = (paths: string[]) => {
    for (const path of paths) if (!out.includes(path)) out.push(path);
  };

  for (const message of messages) {
    add(writtenPaths((message.parts ?? []).filter((p): p is MessageToolPart => p.type === 'tool')));
  }
  if (stream?.length) {
    add(
      writtenPaths(
        stream
          .filter((item): item is Extract<StreamItem, { kind: 'tool' }> => item.kind === 'tool')
          .map((item) => item.tool),
      ),
    );
  }
  return out;
}
