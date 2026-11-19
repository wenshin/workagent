import type {
  ActiveRunResponse,
  ChatMessage,
  PendingApproval,
  SessionShare,
  SessionTask,
  SharesResponse,
  SpaceFile,
  TasksResponse,
  UploadFilesResponse,
} from './types';

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;

  constructor(message: string, status: number, code = 'unknown') {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
  }
}

const LOGIN_PATH = '/login';

export function redirectToLogin() {
  if (window.location.pathname === LOGIN_PATH) {
    return;
  }
  const current = window.location.pathname + window.location.search;
  window.location.href = `${LOGIN_PATH}?redirect=${encodeURIComponent(current || '/')}`;
}

interface RequestOptions {
  method?: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE';
  body?: unknown;
  signal?: AbortSignal;
}

interface ErrorBody {
  message?: string;
  code?: string;
}

// 前端永远是同源调用：开发态由 Vite 代理转发，生产由 Nginx/CDN 反向代理转发到 API 服务。
export async function api<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { method = 'GET', body, signal } = options;

  let res: Response;
  try {
    res = await fetch(path, {
      method,
      signal,
      headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch (err) {
    if (err instanceof DOMException && err.name === 'AbortError') {
      throw err;
    }
    throw new ApiError('网络异常，请检查服务是否正常', 0, 'network_error');
  }

  if (res.status === 401) {
    redirectToLogin();
    throw new ApiError('未登录或登录已过期', 401, 'unauthenticated');
  }

  // 204/205 是「成功但没有正文」：插件启停、删除一类接口都这么回，响应里既没有
  // body 也没有 content-type，不能当成反代返回了静态页。
  if (res.status === 204 || res.status === 205) {
    return null as T;
  }

  const contentType = res.headers.get('content-type') ?? '';
  const isJSON = contentType.includes('application/json');
  const data: unknown = isJSON ? await res.json().catch(() => ({})) : null;

  if (!res.ok) {
    const err = (data ?? {}) as ErrorBody;
    throw new ApiError(err.message ?? `请求失败（${res.status}）`, res.status, err.code ?? 'unknown');
  }

  // 反向代理未覆盖该路径时会返回静态页面的 HTML，这里显式报错而不是当成空数据
  if (!isJSON) {
    throw new ApiError('接口返回的不是 JSON，请检查反向代理是否已转发该路径', res.status, 'bad_response');
  }

  return data as T;
}

/**
 * 显式停止一轮执行。
 *
 * 服务端的 run 已经与 HTTP 请求解耦（§15.12.1），断开连接不再等于停止，
 * 所以「停止」必须发这个请求，不能只 abort 本地 fetch。
 */
export async function cancelRun(runId: string): Promise<void> {
  await api<{ run_id: string; status: string }>(`/runs/${runId}/cancel`, { method: 'POST' });
}

/**
 * 按会话停止当前这一轮（幂等）。
 *
 * 「停止」按钮不该依赖 run_id：思考阶段 `run.started` 还没到，前端手里没有 id。
 * 返回即代表服务端已把这一轮置为终态，调用方可以直接收视图，不用再确认。
 */
export async function cancelSessionRun(sessionId: string): Promise<void> {
  await api<{ session_id: string; status: string }>(`/sessions/${sessionId}/cancel`, {
    method: 'POST',
  });
}

/** 会话里还没处理的确认：刷新后补卡片，已处理掉的不再显示按钮。 */
export async function listPendingApprovals(sessionId: string): Promise<PendingApproval[]> {
  const res = await api<{ approvals: PendingApproval[] }>(`/sessions/${sessionId}/approvals`);
  return res.approvals ?? [];
}

/**
 * 回一次审批。
 *
 * scope=turn 表示「本轮内同类动作都照这个结论」，run 结束即失效——下一轮重新确认。
 */
export async function decideApproval(
  sessionId: string,
  approvalId: string,
  decision: 'approve' | 'deny',
  scope: 'once' | 'turn',
): Promise<{ resumed: boolean }> {
  const res = await api<{ resumed?: boolean }>(`/sessions/${sessionId}/approvals/${approvalId}`, {
    method: 'POST',
    body: { decision, scope },
  });
  // 202 = 决策已落库，但当时没有执行体在等（这一轮中断过），服务端正在从断点续跑
  return { resumed: Boolean(res?.resumed) };
}

/** 刷新后的第一问：这个会话有没有在跑的 run，有就续流。 */
export function getActiveRun(sessionId: string): Promise<ActiveRunResponse> {
  return api<ActiveRunResponse>(`/sessions/${sessionId}/active-run`);
}

/**
 * 这个会话有没有可以接着走的执行。
 *
 * 判据是服务端有没有断点，与「上一轮是不是失败」无关：进程崩溃时连失败都来不及写，
 * 而那正是最需要「继续」的时候。
 */
export function getResumable(
  sessionId: string,
): Promise<{ resumable: boolean; run_id?: string; started_at?: string }> {
  return api(`/sessions/${sessionId}/resume`);
}

/** 派发任务列表 */
export function listTasks(sessionId: string): Promise<TasksResponse> {
  return api<TasksResponse>(`/sessions/${sessionId}/tasks`);
}

export function cancelTask(taskId: string): Promise<{ status: string }> {
  return api<{ status: string }>(`/tasks/${taskId}/cancel`, { method: 'POST' });
}

/** 再次咨询：向同一子会话催办，不新建任务 */
export function remindTask(taskId: string): Promise<SessionTask> {
  return api<SessionTask>(`/tasks/${taskId}/remind`, { method: 'POST' });
}

/** 生成排障只读链接：默认 24h，最多 7 天 */
export function createShare(sessionId: string, ttlHours: number): Promise<SessionShare> {
  return api<SessionShare>(`/sessions/${sessionId}/share`, {
    method: 'POST',
    body: JSON.stringify({ ttl_hours: ttlHours }),
  });
}

export function listShares(sessionId: string): Promise<SharesResponse> {
  return api<SharesResponse>(`/sessions/${sessionId}/share`);
}

export function revokeShare(sessionId: string, shareId: string): Promise<{ status: string }> {
  return api<{ status: string }>(`/sessions/${sessionId}/share/${shareId}`, { method: 'DELETE' });
}

/** 用链接查看一段对话：唯一不需要登录的读取路径 */
export function viewShared(token: string): Promise<{
  title: string;
  created_at: string;
  messages: ChatMessage[];
  read_only: boolean;
}> {
  return api(`/share/${encodeURIComponent(token)}`);
}

/**
 * 上传允许的文件类型，必须与后端 `previewable` 一致：
 * 上传口只收能预览、`file_read` 也读得到的文本类型，accept 只是把选择器过滤一遍。
 * 隐藏配置没有常规后缀（.env / .gitignore），单独列出来，否则在选择器里根本看不到它们。
 */
export const UPLOAD_ACCEPT = '.md,.json,.txt,.csv,.yaml,.yml,.env,.gitignore,.gitattributes,.editorconfig,.npmrc,.dockerignore';
/** 同一份白名单的展示文案，用在 tooltip 与菜单说明里 */
export const UPLOAD_ACCEPT_LABEL = 'md / json / txt / csv / yaml 与 .env 等隐藏配置';

/** 本地预挡超限文件：省掉一次注定 413 的往返；返回第一个超限文件 */
export function firstOversize(files: File[], maxMb?: number): File | undefined {
  if (!maxMb) return undefined;
  const limit = maxMb * 1024 * 1024;
  return files.find((f) => f.size > limit);
}

// 预览白名单与上传白名单同源：能传上来的文本类型就是能渲染的类型
const PREVIEW_SUFFIXES = UPLOAD_ACCEPT.split(',')
  .map((suffix) => suffix.trim().toLowerCase())
  .filter(Boolean);

/**
 * 文件能否在右侧面板渲染（md 走 Markdown，其余按纯文本）。
 *
 * 服务端清单里的 `previewable` 是权威值（它认识 .env 这类隐藏配置）；
 * 老版本后端没有这个字段时按扩展名兜底，否则 md / json 会被误判成「只能下载」。
 */
export function isPreviewable(file: Pick<SpaceFile, 'name' | 'previewable'>): boolean {
  if (typeof file.previewable === 'boolean') return file.previewable;
  const name = file.name.toLowerCase();
  return PREVIEW_SUFFIXES.some((suffix) => name.endsWith(suffix));
}

/**
 * 上传文件到空间：multipart/form-data，字段名 files，dir 是相对空间 files/ 的目录。
 * 单独一个函数是因为 api() 只会发 JSON，而上传必须让浏览器自己带 boundary。
 */
export async function uploadSpaceFiles(
  spaceId: string,
  files: File[],
  dir?: string,
): Promise<SpaceFile[]> {
  const form = new FormData();
  files.forEach((file) => form.append('files', file));
  if (dir) form.append('dir', dir);

  let res: Response;
  try {
    res = await fetch(`/spaces/${spaceId}/files`, { method: 'POST', body: form });
  } catch {
    throw new ApiError('网络异常，请检查服务是否正常', 0, 'network_error');
  }

  if (res.status === 401) {
    redirectToLogin();
    throw new ApiError('未登录或登录已过期', 401, 'unauthenticated');
  }

  const data: unknown = await res.json().catch(() => ({}));
  if (!res.ok) {
    const err = (data ?? {}) as ErrorBody;
    throw new ApiError(err.message ?? `上传失败（${res.status}）`, res.status, err.code ?? 'unknown');
  }

  return (data as UploadFilesResponse).files ?? [];
}

/**
 * 下载空间文件到本机：取原始字节（后端不截断），再用 <a download> 落盘。
 * 不能把地址直接丢给浏览器——401/404 时会被当成一段 JSON 页面打开，用户看不到原因。
 */
export async function downloadSpaceFile(spaceId: string, path: string, name: string): Promise<void> {
  let res: Response;
  try {
    res = await fetch(`/spaces/${spaceId}/file/download?path=${encodeURIComponent(path)}`);
  } catch {
    throw new ApiError('网络异常，请检查服务是否正常', 0, 'network_error');
  }

  if (res.status === 401) {
    redirectToLogin();
    throw new ApiError('未登录或登录已过期', 401, 'unauthenticated');
  }
  if (!res.ok) {
    const data: unknown = await res.json().catch(() => ({}));
    const err = (data ?? {}) as ErrorBody;
    throw new ApiError(err.message ?? `下载失败（${res.status}）`, res.status, err.code ?? 'unknown');
  }

  const url = URL.createObjectURL(await res.blob());
  const link = document.createElement('a');
  link.href = url;
  link.download = name;
  document.body.appendChild(link);
  link.click();
  link.remove();
  // 立刻 revoke 会让部分浏览器的下载中断，等一帧后再释放
  window.setTimeout(() => URL.revokeObjectURL(url), 1000);
}

/**
 * 上传智能体头像：multipart 字段名 icon。
 * 前端已经裁成正方形，服务端还会再裁再压一次——直接打接口也不会把大图落进空间。
 */
export async function uploadAgentIcon(
  spaceId: string,
  slug: string,
  blob: Blob,
): Promise<{ icon_url?: string }> {
  const form = new FormData();
  form.append('icon', blob, 'icon.png');

  let res: Response;
  try {
    res = await fetch(`/spaces/${spaceId}/plugins/${slug}/icon`, { method: 'POST', body: form });
  } catch {
    throw new ApiError('网络异常，请检查服务是否正常', 0, 'network_error');
  }

  if (res.status === 401) {
    redirectToLogin();
    throw new ApiError('未登录或登录已过期', 401, 'unauthenticated');
  }

  const data: unknown = await res.json().catch(() => ({}));
  if (!res.ok) {
    const err = (data ?? {}) as ErrorBody;
    throw new ApiError(err.message ?? `上传失败（${res.status}）`, res.status, err.code ?? 'unknown');
  }
  return data as { icon_url?: string };
}

export function deleteAgentIcon(spaceId: string, slug: string): Promise<null> {
  return api<null>(`/spaces/${spaceId}/plugins/${slug}/icon`, { method: 'DELETE' });
}
