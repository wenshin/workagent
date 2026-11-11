import { streamChat } from './stream';
import type { StreamItem } from './types';

/**
 * 正在进行的一轮对话。
 *
 * 状态放在模块级而不是组件里：路由从 `/` 切到 `/plugins/market` 时 React Router 会
 * 卸载 WorkspacePage（不同 Route 的子树 key 不同），若流跟着组件走，用户一离开页面
 * 连接就断了，回来只剩空白——这正是「切到其它页面后回答没有了」的原因。
 *
 * 放在模块级后，卸载的只是视图：SSE 继续被消费、内容继续累积，回到页面重新订阅
 * 就能接上；轮次结束时由 onSettled 刷新消息与文件缓存，与视图是否还在无关。
 */
export interface LiveRunView {
  sessionId: string;
  /** 本轮用户输入，用于在消息列表里先占位 */
  pendingUser: string;
  /** 思考、正文与工具调用按到达顺序排列：这是真实时间线，渲染时照序输出即可 */
  items: StreamItem[];
  /** 本轮是否写过空间文件，结束后据此刷新文件树与预览 */
  wroteFiles: boolean;
}

type Listener = () => void;

let current: LiveRunView | null = null;
let controller: AbortController | null = null;
const listeners = new Set<Listener>();

function emit() {
  listeners.forEach((listener) => listener());
}

export function subscribeLiveRun(listener: Listener): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

/** 当前快照；没有进行中的轮次时为 null（useSyncExternalStore 用）。 */
export function getLiveRun(): LiveRunView | null {
  return current;
}

/** 是否有轮次在跑；传 sessionId 时只判断该会话。 */
export function isLiveRunning(sessionId?: string): boolean {
  if (!current) return false;
  return sessionId ? current.sessionId === sessionId : true;
}

/** 停止当前轮次：用户点停止，或切到了另一个会话。 */
export function stopLiveRun(): void {
  controller?.abort();
}

export interface LiveRunOptions {
  sessionId: string;
  content: string;
  onError?: (message: string) => void;
  onSettled?: (info: { sessionId: string; wroteFiles: boolean; aborted: boolean }) => void;
}

/**
 * 启动一轮对话；同一时间只允许一轮。返回的 Promise 在轮次结束时 resolve，
 * 与组件生命周期无关。
 *
 * 换会话时不做中断：旧会话若还在跑就让它跑完并落库，回来仍能看到结果；
 * 只有用户在新会话里发消息时，才接管并停掉旧的那一轮。
 */
export function startLiveRun(opts: LiveRunOptions): Promise<void> {
  // 同一会话重复触发（连点发送）：忽略，避免两路事件写进同一份视图
  if (current && current.sessionId === opts.sessionId) return Promise.resolve();
  // 别的会话还在跑：接管。旧轮次可能正卡在工具调用上，abort 让它尽快收尾；
  // 它的 finally 会按 controller 比对，不会清掉这一轮的视图
  controller?.abort();

  const ctrl = new AbortController();
  controller = ctrl;
  current = {
    sessionId: opts.sessionId,
    pendingUser: opts.content,
    items: [],
    wroteFiles: false,
  };
  emit();

  // 本轮是否写过空间文件：用局部变量记录，被接管后 current 已经换人，读不到旧值
  let wrote = false;

  // 每次改动都换新对象：useSyncExternalStore 靠引用比较判断是否重渲染
  const patch = (fn: (view: LiveRunView) => LiveRunView) => {
    if (current === null || controller !== ctrl) return;
    current = fn(current);
    emit();
  };

  return (async () => {
    let aborted = false;
    try {
      await streamChat(
        opts.sessionId,
        opts.content,
        {
          // 连续增量合并进同一段：每个 token 单独成段会把 Markdown 切碎，
          // 思考也会碎成几十个折叠块。中间插了工具或正文时，再开新的一段。
          onReasoning: (delta) =>
            patch((v) => {
              const last = v.items[v.items.length - 1];
              if (last && last.kind === 'reasoning') {
                return {
                  ...v,
                  items: [
                    ...v.items.slice(0, -1),
                    { kind: 'reasoning', text: last.text + delta },
                  ],
                };
              }
              return { ...v, items: [...v.items, { kind: 'reasoning', text: delta }] };
            }),
          onText: (delta) =>
            patch((v) => {
              const last = v.items[v.items.length - 1];
              if (last && last.kind === 'text') {
                return {
                  ...v,
                  items: [...v.items.slice(0, -1), { kind: 'text', text: last.text + delta }],
                };
              }
              return { ...v, items: [...v.items, { kind: 'text', text: delta }] };
            }),
          onToolStart: (tool) => patch((v) => ({ ...v, items: [...v.items, { kind: 'tool', tool }] })),
          onToolResult: (tool) => {
            // Agent 改过空间文件（含新建技能目录）：结束后让树与当前预览重新拉取
            if (
              tool.status === 'ok' &&
              (tool.name === 'file_write' ||
                tool.name === 'wiki_write' ||
                tool.name === 'skill_create')
            ) {
              wrote = true;
            }
            patch((v) => ({
              ...v,
              items: v.items.map((item): StreamItem =>
                item.kind === 'tool' && item.tool.id === tool.id ? { kind: 'tool', tool } : item,
              ),
              wroteFiles: wrote,
            }));
          },
          onError: (message) => opts.onError?.(message),
        },
        ctrl.signal,
      );
    } catch (e) {
      // 用户主动停止不算错误
      aborted = e instanceof DOMException && e.name === 'AbortError';
      if (!aborted) opts.onError?.(e instanceof Error ? e.message : '发送失败，请重试');
    } finally {
      // 只有自己仍是当前轮次才清空视图：被新会话接管时不能把新一轮抹掉
      if (controller === ctrl) {
        current = null;
        controller = null;
        emit();
      }
      // 收尾回调对每一轮都要触发：被接管的旧会话同样要把已生成的内容刷进列表
      opts.onSettled?.({ sessionId: opts.sessionId, wroteFiles: wrote, aborted });
    }
  })();
}