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

interface RequestOptions {
  method?: 'GET' | 'POST' | 'PATCH' | 'DELETE';
  body?: unknown;
  signal?: AbortSignal;
}

interface ErrorBody {
  message?: string;
  code?: string;
}

const LOGIN_PATH = '/login';

function redirectToLogin() {
  const base = import.meta.env.BASE_URL.replace(/\/$/, '');
  const current = window.location.pathname.replace(base, '') + window.location.search;
  if (window.location.pathname.endsWith(LOGIN_PATH)) {
    return;
  }
  window.location.href = `${base}${LOGIN_PATH}?redirect=${encodeURIComponent(current || '/')}`;
}

export async function api<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { method = 'GET', body, signal } = options;

  let res: Response;
  try {
    res = await fetch(path, {
      method,
      signal,
      credentials: 'same-origin',
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

  const data: unknown = await res.json().catch(() => ({}));

  if (!res.ok) {
    const err = data as ErrorBody;
    throw new ApiError(err.message ?? `请求失败（${res.status}）`, res.status, err.code ?? 'unknown');
  }

  return data as T;
}
