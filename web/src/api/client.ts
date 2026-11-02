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

// 开发态留空走 Vite 代理；生产填 API 域名，例如 https://api.example.com
export const API_BASE = (import.meta.env.VITE_API_BASE ?? '').replace(/\/+$/, '');

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

export async function api<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { method = 'GET', body, signal } = options;

  let res: Response;
  try {
    res = await fetch(API_BASE + path, {
      method,
      signal,
      // 前后端跨域部署时需要显式携带 Cookie
      credentials: 'include',
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

  // 前端与 API 分离部署时，若 VITE_API_BASE 配错，请求会落到静态站点并返回 HTML
  if (!isJSON) {
    throw new ApiError('接口返回的不是 JSON，请检查 VITE_API_BASE 是否指向 API 服务', res.status, 'bad_response');
  }

  return data as T;
}
