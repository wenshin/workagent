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

function redirectToLogin() {
  if (window.location.pathname === LOGIN_PATH) {
    return;
  }
  const current = window.location.pathname + window.location.search;
  window.location.href = `${LOGIN_PATH}?redirect=${encodeURIComponent(current || '/')}`;
}

interface RequestOptions {
  method?: 'GET' | 'POST' | 'PATCH' | 'DELETE';
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
