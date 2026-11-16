import { cancelSessionRun, getActiveRun, listPendingApprovals } from './client';
import { streamChat, streamResume, streamRun, type StreamHandlers } from './stream';
import type { ActiveRunResponse, PendingApproval, StreamItem } from './types';

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
/**
 * 正在发「停止」请求的会话，只在这一次请求在途时有效。
 *
 * 挡的是同一个空窗：`abort` 已经解绑本地流，但服务端还没把这一轮置终态，
 * 此刻 `active-run` 仍然回答「在跑」，「刷新后接着看」的自愈就会把刚停下的
 * 气泡重新挂回来——表现就是点了停止没反应、得点两次。服务端在取消请求返回
 * 之前就把 run 落了终态，所以这里只需覆盖「这一个请求在路上」的时间。
 */
const stoppingSessions = new Set<string>();
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

/** 这个会话是不是刚被停过、服务端还没确认收尾：续流要绕开它。 */
export function isStopping(sessionId: string): boolean {
  return stoppingSessions.has(sessionId);
}

/**
 * 用户点「停止」：本地解绑 + 服务端停这一轮，一次请求，没有轮询。
 *
 * 断开 SSE 只是解绑这一条流，服务端仍在继续跑（§15.12.1），所以必须显式取消；
 * 但取消按**会话**发——思考阶段 `run.started` 还没到，前端手里没有 run_id，
 * 靠 run_id 取消就得多一步「先问再取消」，那一步正是「要点两次」的来源。
 *
 * 服务端在返回前就把 run 置终态，所以这里的 200 就是「已经停了」：
 * 不用再问 active-run 确认，也就不会在确认的空窗里被断流自愈挂回来。
 */
export async function stopLiveRun(sessionId?: string): Promise<void> {
  const sid = sessionId ?? current?.sessionId ?? '';
  if (!sid) {
    // 连会话都定位不到：至少把本地这条流停住
    controller?.abort();
    return;
  }
  // 连点两下不必发两条取消；在途期间也不让续流把这个会话抢回去
  if (stoppingSessions.has(sid)) return;
  stoppingSessions.add(sid);
  controller?.abort();
  try {
    await cancelSessionRun(sid);
  } catch {
    // 网络抖动不影响本地已经停下：服务端没收到取消的话这一轮还在跑，
    // 下一次 active-run 会如实回答「在跑」，那就让它显示出来，而不是假装停住
  } finally {
    stoppingSessions.delete(sid);
  }
}

export interface LiveRunOptions {
  sessionId: string;
  content: string;
  /** 引用的空间文件路径；服务端负责校验并拼进用户消息 */
  references?: string[];
  /**
   * 从断点继续上一次被中断的执行。
   *
   * 与普通发送的区别只在请求端点与「没有新的用户消息」：服务端带着已有的工具回执
   * 接着走，已经执行过的工具不会再跑一遍。
   */
  resume?: boolean;
  onError?: (message: string) => void;
  onSettled?: (info: { sessionId: string; wroteFiles: boolean; aborted: boolean }) => void;
}

/** 事件 → 视图的唯一映射，首发与续流共用，避免两条链路渲染不一致。 */
function buildHandlers(
  patch: (fn: (view: LiveRunView) => LiveRunView) => void,
  markWrote: () => void,
  sessionId: string,
  onError?: (message: string) => void,
): StreamHandlers {
  return {
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
        waitingApproval: false,
        items: v.items.map((item): StreamItem =>
          item.kind === 'tool' && item.tool.id === tool.id ? { kind: 'tool', tool } : item,
        ),
      }));
    },
    // 命中审批规则：把请求挂到对应工具卡片上，用户点按钮后由 tool.result 更新结局。
    // 卡片还没到（理论上不会，tool.call 先发）时补一条，避免审批提示丢失。
    onApproval: (req) =>
      patch((v) => {
        const approval = {
          id: req.approvalId,
          sessionId,
          kind: req.kind,
          detail: req.detail,
          reason: req.reason,
          risk: req.risk,
          timeoutS: req.timeoutS,
        };
        const idx = v.items.findIndex((item) => item.kind === 'tool' && item.tool.id === req.callId);
        if (idx < 0) {
          return {
            ...v,
            waitingApproval: true,
            items: [
              ...v.items,
              {
                kind: 'tool',
                tool: {
                  id: req.callId,
                  name: req.tool,
                  args: '',
                  status: 'approval',
                  risk: req.risk || undefined,
                  approval,
                },
              },
            ],
          };
        }
        return {
          ...v,
          waitingApproval: true,
          items: v.items.map((item, i): StreamItem =>
            i === idx && item.kind === 'tool'
              ? { kind: 'tool', tool: { ...item.tool, status: 'approval', approval } }
              : item,
          ),
        };
      }),
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
/**
 * 用服务端的待办记录校准审批卡片。
 *
 * 重放 run_step 只能说明「这条请求出现过」，无法说明它是否已经被人点掉。
 * 刷新后若照单全收，用户会看到一个点不动的按钮；反过来，事件缓冲里没有的待办
 * （例如刚换进程）也要能补上。所以以 approval 表为准做一次对齐。
 */
async function syncApprovals(
  sessionId: string,
  patch: (fn: (view: LiveRunView) => LiveRunView) => void,
): Promise<void> {
  let list: PendingApproval[];
  try {
    list = await listPendingApprovals(sessionId);
  } catch {
    return; // 补卡片是增强项，失败不该影响正文
  }
  const byCall = new Map(list.map((a) => [a.call_id, a]));
  patch((v) => {
    const seen = new Set<string>();
    const items = v.items.map((item): StreamItem => {
      if (item.kind !== 'tool' || item.tool.status !== 'approval') return item;
      seen.add(item.tool.id);
      const live = byCall.get(item.tool.id);
      // 服务端已经没有这条待办：说明已处理或本轮收尾了，别继续显示按钮
      if (!live) return { ...item, tool: { ...item.tool, status: 'running', approval: undefined } };
      return { ...item, tool: { ...item.tool, approval: toApprovalInfo(live, sessionId) } };
    });
    // 事件重放里没有、但服务端仍在等的待办：补一张卡片
    for (const a of list) {
      if (seen.has(a.call_id)) continue;
      items.push({
        kind: 'tool',
        tool: {
          id: a.call_id,
          name: a.tool,
          args: '',
          status: 'approval',
          risk: a.risk || undefined,
          approval: toApprovalInfo(a, sessionId),
        },
      });
    }
    return { ...v, items, waitingApproval: list.length > 0 };
  });
}

function toApprovalInfo(a: PendingApproval, sessionId: string) {
  return {
    id: a.approval_id,
    sessionId,
    kind: a.kind,
    detail: a.detail,
    reason: a.reason,
    risk: a.risk,
    timeoutS: a.timeout_s,
  };
}

export function startLiveRun(opts: LiveRunOptions): Promise<void> {
  // 同一会话重复触发（连点发送）：忽略，避免两路事件写进同一份视图
  if (current && current.sessionId === opts.sessionId) return Promise.resolve();
  // 用户在这个会话里重新发消息：上一轮的停止请求已经没意义了，撤掉这道闸
  stoppingSessions.delete(opts.sessionId);
  // 别的会话还在跑：接管。旧轮次可能正卡在工具调用上，abort 让它尽快收尾；
  // 它的 finally 会按 controller 比对，不会清掉这一轮的视图
  controller?.abort();

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

  const handlers = buildHandlers(patch, markWrote, opts.sessionId, opts.onError);
  const reset = () => patch((v) => ({ ...v, items: [] }));

  return (async () => {
    let aborted = false;
    try {
      const outcome = opts.resume
        ? await streamResume(opts.sessionId, handlers, ctrl.signal)
        : await streamChat(
            opts.sessionId,
            opts.content,
            opts.references ?? [],
            handlers,
            ctrl.signal,
          );
      // 没有终态事件就是断了（切页、代理掐连接、热更新……）：run 还在服务端跑，续上
      if (!outcome.finished) {
        await resumeStream(opts.sessionId, ctrl, reset, handlers);
      }
      await syncApprovals(opts.sessionId, patch);
    } catch (e) {
      // 用户主动停止不算错误
      aborted = e instanceof DOMException && e.name === 'AbortError';
      if (!aborted) {
        // 连接中途被掐断也是「流断了」：先试着重挂，真的续不上再报错
        const resumed = await resumeStream(opts.sessionId, ctrl, reset, handlers);
        if (!resumed) opts.onError?.(e instanceof Error ? e.message : '发送失败，请重试');
      }
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

/**
 * 从断点继续上一次被中断的执行。
 *
 * 与 startLiveRun 共用一套视图与收尾：不发新的用户消息，服务端带着已有的工具回执
 * 接着走，所以已经执行过的工具不会再执行一遍。
 */
export function resumeLiveRun(
  opts: Omit<LiveRunOptions, 'content' | 'references' | 'resume'>,
): Promise<void> {
  return startLiveRun({ ...opts, content: '', resume: true });
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
  // 刚被用户停掉的会话不允许自动重挂：服务端还没收尾时它看起来「还在跑」，
  // 挂上去就是「点了停止、气泡又回来」
  if (stoppingSessions.has(opts.sessionId)) return Promise.resolve();
  controller?.abort();

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

  const handlers = buildHandlers(patch, markWrote, opts.sessionId, opts.onError);
  const reset = () => patch((v) => ({ ...v, items: [] }));

  return (async () => {
    let aborted = false;
    try {
      const outcome = await streamRun(opts.runId, opts.afterOffset ?? 0, handlers, ctrl.signal);
      // 重放只说明「请求发生过」；谁点过按钮要看服务端的记录，否则刷新后会把
      // 已经处理掉的审批又显示成待办。
      await syncApprovals(opts.sessionId, patch);
      if (!outcome.finished) await resumeStream(opts.sessionId, ctrl, reset, handlers);
    } catch (e) {
      aborted = e instanceof DOMException && e.name === 'AbortError';
      if (!aborted) {
        const resumed = await resumeStream(opts.sessionId, ctrl, reset, handlers);
        if (!resumed) opts.onError?.(e instanceof Error ? e.message : '续接失败，请重试');
      }
    } finally {
      if (controller === ctrl) {
        current = null;
        controller = null;
        emit();
      }
      opts.onSettled?.({ sessionId: opts.sessionId, wroteFiles: wrote, aborted });
    }
  })();
}
