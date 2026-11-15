import { cancelRun, getActiveRun } from './client';
import { streamChat, streamRun, type StreamHandlers } from './stream';
import type { ActiveRunResponse, StreamItem } from './types';

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
  /** 本轮用户输入，用于在消息列表里先占位；续流时为空（用户消息已经落库） */
  pendingUser: string;
  /** 本轮引用的空间文件（相对 files/ 的路径），占位气泡里渲染成文件标签 */
  pendingRefs: string[];
  /** 思考、正文与工具调用按到达顺序排列：这是真实时间线，渲染时照序输出即可 */
  items: StreamItem[];
  /** 本轮是否写过空间文件，结束后据此刷新文件树与预览 */
  wroteFiles: boolean;
}

type Listener = () => void;

/**
 * 流被意外掐断后的重连间隔（次数即上限，失败后交给 onSettled 刷新消息）。
 *
 * 「流结束」不等于「这一轮结束」（§15.12.1）：切页、切标签、代理掐连接、dev 热更新
 * 都只让这一条订阅断掉，run 还在服务端跑。这里按内存缓冲重新挂回
 * `/runs/{id}/stream` 续上（§15.12.2），而不是把断开当成收尾——那正是
 * 「切个页面回来回答就没了」的成因。
 */
const RESUME_DELAYS_MS = [300, 800, 1500, 3000, 5000];

let current: LiveRunView | null = null;
let controller: AbortController | null = null;
/** 当前轮次的服务端 run_id：取消与续流都靠它定位 */
let currentRunId: string | null = null;
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

/**
 * 用户点「停止」：显式取消服务端的那一轮。
 *
 * 断开 SSE 只是解绑这一条流，服务端仍在继续跑（§15.12.1），所以必须
 * 单独发一次取消请求，否则按钮看起来生效、任务其实还在后台烧钱。
 */
export function stopLiveRun(): void {
  const runId = currentRunId;
  controller?.abort();
  if (runId) {
    void cancelRun(runId).catch(() => {
      // 已经结束或不在本进程：服务端返回 409，这里不需要打扰用户
    });
  }
}

export interface LiveRunOptions {
  sessionId: string;
  content: string;
  /** 引用的空间文件路径；服务端负责校验并拼进用户消息 */
  references?: string[];
  onError?: (message: string) => void;
  onSettled?: (info: { sessionId: string; wroteFiles: boolean; aborted: boolean }) => void;
}

/** 事件 → 视图的唯一映射，首发与续流共用，避免两条链路渲染不一致。 */
function buildHandlers(
  ctrl: AbortController,
  patch: (fn: (view: LiveRunView) => LiveRunView) => void,
  markWrote: () => void,
  onError?: (message: string) => void,
): StreamHandlers {
  return {
    onRunStarted: (runId) => {
      if (controller === ctrl) currentRunId = runId || null;
    },
    // 连续增量合并进同一段：每个 token 单独成段会把 Markdown 切碎，
    // 思考也会碎成几十个折叠块。中间插了工具或正文时，再开新的一段。
    onReasoning: (delta) =>
      patch((v) => {
        const last = v.items[v.items.length - 1];
        if (last && last.kind === 'reasoning') {
          return {
            ...v,
            items: [...v.items.slice(0, -1), { kind: 'reasoning', text: last.text + delta }],
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
      // Agent 改过空间文件（含新建插件目录）：结束后让树与当前预览重新拉取
      if (tool.status === 'ok' && (tool.name === 'file_write' || tool.name === 'wiki_write')) {
        markWrote();
      }
      patch((v) => ({
        ...v,
        items: v.items.map((item): StreamItem =>
          item.kind === 'tool' && item.tool.id === tool.id ? { kind: 'tool', tool } : item,
        ),
      }));
    },
    onError,
  };
}

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const done = () => {
      window.clearTimeout(timer);
      signal.removeEventListener('abort', done);
      resolve();
    };
    const timer = window.setTimeout(done, ms);
    signal.addEventListener('abort', done, { once: true });
  });
}

/**
 * 断流自愈：问一次「这一轮还在不在跑」，还在就挂回 `/runs/{id}/stream` 继续跟。
 *
 * 从 after_offset=0 补：服务端的内存缓冲留着这一轮的全部事件，先清掉已渲染的
 * 片段再补一遍，内容不会重复也不会缺段；跟着直播继续走，直到收到终态。
 *
 * @returns true 表示重新挂上并跑到了终态；false 表示 run 已经结束或问不到状态，
 *          调用方照常刷新消息即可。
 */
async function resumeStream(
  sessionId: string,
  ctrl: AbortController,
  reset: () => void,
  handlers: StreamHandlers,
  /** 挂上之后回报 run_id：断了重连时「停止」按钮仍要能找到这一轮 */
  claim: (runId: string) => void,
): Promise<boolean> {
  for (const delayMs of RESUME_DELAYS_MS) {
    if (ctrl.signal.aborted) return false;
    await sleep(delayMs, ctrl.signal);
    if (ctrl.signal.aborted) return false;

    let active: ActiveRunResponse;
    try {
      active = await getActiveRun(sessionId);
    } catch {
      return false;
    }
    if (!active.active || !active.run) return false;

    reset();
    claim(active.run.id);
    try {
      const outcome = await streamRun(active.run.id, 0, handlers, ctrl.signal);
      if (outcome.finished) return true;
    } catch (e) {
      if (e instanceof DOMException && e.name === 'AbortError') return false;
      // 续接本身失败：按下一档退避再来
    }
  }
  return false;
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
  // 旧轮次交给服务端跑完：这里只解绑流，不取消它
  currentRunId = null;

  const ctrl = new AbortController();
  controller = ctrl;
  current = {
    sessionId: opts.sessionId,
    pendingUser: opts.content,
    pendingRefs: opts.references ?? [],
    items: [],
    wroteFiles: false,
  };
  emit();

  // 本轮是否写过空间文件：用局部变量记录，被接管后 current 已经换人，读不到旧值
  let wrote = false;
  const markWrote = () => {
    wrote = true;
  };

  // 每次改动都换新对象：useSyncExternalStore 靠引用比较判断是否重渲染
  const patch = (fn: (view: LiveRunView) => LiveRunView) => {
    if (current === null || controller !== ctrl) return;
    current = { ...fn(current), wroteFiles: wrote };
    emit();
  };

  const handlers = buildHandlers(ctrl, patch, markWrote, opts.onError);
  const reset = () => patch((v) => ({ ...v, items: [] }));
  const claim = (runId: string) => {
    if (controller === ctrl) currentRunId = runId;
  };

  return (async () => {
    let aborted = false;
    try {
      const outcome = await streamChat(
        opts.sessionId,
        opts.content,
        opts.references ?? [],
        handlers,
        ctrl.signal,
      );
      // 没有终态事件就是断了（切页、代理掐连接、热更新……）：run 还在服务端跑，续上
      if (!outcome.finished) {
        await resumeStream(opts.sessionId, ctrl, reset, handlers, claim);
      }
    } catch (e) {
      // 用户主动停止不算错误
      aborted = e instanceof DOMException && e.name === 'AbortError';
      if (!aborted) {
        // 连接中途被掐断也是「流断了」：先试着重挂，真的续不上再报错
        const resumed = await resumeStream(opts.sessionId, ctrl, reset, handlers, claim);
        if (!resumed) opts.onError?.(e instanceof Error ? e.message : '发送失败，请重试');
      }
    } finally {
      // 只有自己仍是当前轮次才清空视图：被新会话接管时不能把新一轮抹掉
      if (controller === ctrl) {
        current = null;
        controller = null;
        currentRunId = null;
        emit();
      }
      // 收尾回调对每一轮都要触发：被接管的旧会话同样要把已生成的内容刷进列表
      opts.onSettled?.({ sessionId: opts.sessionId, wroteFiles: wrote, aborted });
    }
  })();
}

export interface AttachRunOptions {
  sessionId: string;
  runId: string;
  /** 从第几个字符之后开始补：默认 0，表示这一轮的全部内容 */
  afterOffset?: number;
  onError?: (message: string) => void;
  onSettled?: (info: { sessionId: string; wroteFiles: boolean; aborted: boolean }) => void;
}

/**
 * 续上一次仍在执行的 run：刷新页面、切回标签页、从别的页面回来都走这里。
 *
 * 服务端把这一轮的增量留在进程内缓冲里，补发完之后继续跟着直播走——所以
 * 刷新不会丢内容，也不需要平台逐 token 落库（§15.12.2）。
 */
export function attachLiveRun(opts: AttachRunOptions): Promise<void> {
  if (current && current.sessionId === opts.sessionId) return Promise.resolve();
  controller?.abort();
  currentRunId = opts.runId;

  const ctrl = new AbortController();
  controller = ctrl;
  current = {
    sessionId: opts.sessionId,
    // 用户消息已经落库，不再补一个占位气泡
    pendingUser: '',
    pendingRefs: [],
    items: [],
    wroteFiles: false,
  };
  emit();

  let wrote = false;
  const markWrote = () => {
    wrote = true;
  };
  const patch = (fn: (view: LiveRunView) => LiveRunView) => {
    if (current === null || controller !== ctrl) return;
    current = { ...fn(current), wroteFiles: wrote };
    emit();
  };

  const handlers = buildHandlers(ctrl, patch, markWrote, opts.onError);
  const reset = () => patch((v) => ({ ...v, items: [] }));
  const claim = (runId: string) => {
    if (controller === ctrl) currentRunId = runId;
  };

  return (async () => {
    let aborted = false;
    try {
      const outcome = await streamRun(opts.runId, opts.afterOffset ?? 0, handlers, ctrl.signal);
      if (!outcome.finished) await resumeStream(opts.sessionId, ctrl, reset, handlers, claim);
    } catch (e) {
      aborted = e instanceof DOMException && e.name === 'AbortError';
      if (!aborted) {
        const resumed = await resumeStream(opts.sessionId, ctrl, reset, handlers, claim);
        if (!resumed) opts.onError?.(e instanceof Error ? e.message : '续接失败，请重试');
      }
    } finally {
      if (controller === ctrl) {
        current = null;
        controller = null;
        currentRunId = null;
        emit();
      }
      opts.onSettled?.({ sessionId: opts.sessionId, wroteFiles: wrote, aborted });
    }
  })();
}
