package plugin

import (
	"fmt"
	"os"
	"path/filepath"
)

// hostSDK 是注入插件的宿主运行时：协议通道（host）+ 出网客户端（wa）。
//
// 分工：
//   - 文件：沙箱按 fs 段挂载，脚本直接用 Deno.readTextFile / writeTextFile；
//   - 出网：脚本用 wa.fetch，走宿主给的 unix socket HTTP 代理（白名单在宿主侧判定）；
//   - 宿主只剩三件事——写日志、按名字下发凭证明文、下发安装方填的配置。
//
// 凭证为什么走宿主下发而不是环境变量：环境变量对插件进程里的一切代码可见，
// 包括它 import 进来的第三方依赖；走一次显式的 host.secret("名称") 调用，
// 宿主才能按清单逐个名字授权并留下审计。
const hostSDK = `// @ts-nocheck
// 平台注入的宿主运行时。不要修改这个文件：它由平台在启动时写入。
type Frame = {
  op: string; id?: number; method?: string; args?: any;
  data?: any; ok?: boolean; error?: string; message?: string;
};

const enc = new TextEncoder();
const dec = new TextDecoder();
const pending = new Map<number, { ok: (v: any) => void; fail: (e: Error) => void }>();
let seq = 0;
let buf = "";
let closed = false;
let invokeResolver: ((f: Frame) => void) | null = null;
const invokeQueue: Frame[] = [];

const write = (obj: unknown) => {
  Deno.stdout.writeSync(enc.encode(JSON.stringify(obj) + "\n"));
};

const failAll = (err: Error) => {
  closed = true;
  for (const [, p] of pending) p.fail(err);
  pending.clear();
};

const onFrame = (f: Frame) => {
  if (f.op === "result") {
    const id = f.id ?? 0;
    const p = pending.get(id);
    if (!p) return;
    pending.delete(id);
    if (f.ok) p.ok(f.data);
    else p.fail(new Error(f.error || "宿主返回失败"));
    return;
  }
  if (f.op === "invoke") {
    if (invokeResolver) { const r = invokeResolver; invokeResolver = null; r(f); }
    else invokeQueue.push(f);
  }
};

const pump = (async () => {
  const tmp = new Uint8Array(64 * 1024);
  while (true) {
    const n = await Deno.stdin.read(tmp);
    if (n === null) { failAll(new Error("宿主关闭了连接")); return; }
    buf += dec.decode(tmp.subarray(0, n));
    let i: number;
    while ((i = buf.indexOf("\n")) >= 0) {
      const line = buf.slice(0, i).trim();
      buf = buf.slice(i + 1);
      if (!line) continue;
      try { onFrame(JSON.parse(line)); } catch (_) { /* 坏帧忽略，协议层会超时回收 */ }
    }
  }
})();

export const host = {
  /**
   * 申请一次宿主能力：log（写日志）、secret.get（按名字取凭证明文，
   * 名字必须在清单里声明过）、config.get（取安装方填的配置，无需声明）。
   */
  call(method: string, args: any): Promise<any> {
    if (closed) return Promise.reject(new Error("插件会话已结束"));
    const id = ++seq;
    return new Promise((resolve, reject) => {
      pending.set(id, { ok: resolve, fail: reject });
      write({ op: "call", id, method, args });
    });
  },
  log(message: string, level = "info") {
    write({ op: "call", id: ++seq, method: "log", args: { level, message } });
  },
  /**
   * 取一个平台凭证的明文。只能取 plugin.json 里声明过的名字：
   *   "capabilities": [{ "method": "secret.use", "name": "dingtalk" }]
   * 没声明的名字会被宿主拒绝，不是返回空串。
   *
   * 拿到的是明文：别写进日志、别拼进抛给用户的错误信息。
   */
  async secret(name: string): Promise<string> {
    const r = await host.call("secret.get", { name });
    return r && typeof r.value === "string" ? r.value : "";
  },
  /**
   * 取安装方为这个插件填的配置：清单 config 段声明的键，已经用默认值补齐。
   *
   * 调一次就拿到全量键值，值一律是字符串——声明成 number 的字段拿到的也是
   * "3"，要算数就自己转。清单里没声明的键不会出现（换配置项等于换清单版本）。
   * 配置在保存后会重建运行时，所以不存在「读到上一次安装的值」。
   */
  async config(): Promise<Record<string, string>> {
    const r = await host.call("config.get", {});
    return r && typeof r.config === "object" && r.config ? r.config : {};
  },
};

// ---------------------------------------------------------------------------
// 出网：本机 unix socket 上的 HTTP 代理，TLS 与域名白名单都在宿主那侧。
//
// 为什么不是「插件自己建 TLS」：Deno 的 startTls 不接受 UnixConn
// （直接 BadResource），createHttpClient 的 createConnection 也已经被移除。
// 所以插件只发 absolute-form 的明文 HTTP/1.1，HTTPS 由代理去连。
// 只支持 HTTP/1.1 的请求-响应，不支持 WebSocket 与 HTTP/2。
// ---------------------------------------------------------------------------

class SockReader {
  private buf = new Uint8Array(0);
  private eof = false;
  constructor(private conn: Deno.Conn) {}
  private async more(): Promise<boolean> {
    if (this.eof) return false;
    const tmp = new Uint8Array(32 * 1024);
    const n = await this.conn.read(tmp);
    if (n === null) { this.eof = true; return false; }
    const next = new Uint8Array(this.buf.length + n);
    next.set(this.buf);
    next.set(tmp.subarray(0, n), this.buf.length);
    this.buf = next;
    return true;
  }
  async need(n: number): Promise<Uint8Array> {
    while (this.buf.length < n) {
      if (!(await this.more())) throw new Error("连接提前关闭");
    }
    const out = this.buf.slice(0, n);
    this.buf = this.buf.subarray(n);
    return out;
  }
  async line(max = 64 * 1024): Promise<string> {
    for (;;) {
      for (let i = 0; i + 1 < this.buf.length; i++) {
        if (this.buf[i] === 13 && this.buf[i + 1] === 10) {
          const s = dec.decode(this.buf.subarray(0, i));
          this.buf = this.buf.subarray(i + 2);
          return s;
        }
      }
      if (this.buf.length > max) throw new Error("响应头过长");
      if (!(await this.more())) throw new Error("连接提前关闭");
    }
  }
  async rest(): Promise<Uint8Array> {
    while (await this.more()) { /* 读干净 */ }
    return this.buf;
  }
}

const concat = (parts: Uint8Array[]): Uint8Array => {
  let total = 0;
  for (const p of parts) total += p.length;
  const out = new Uint8Array(total);
  let off = 0;
  for (const p of parts) { out.set(p, off); off += p.length; }
  return out;
};

const readBody = async (r: SockReader, headers: Headers): Promise<Uint8Array> => {
  const te = (headers.get("transfer-encoding") || "").toLowerCase();
  if (te.includes("chunked")) {
    const parts: Uint8Array[] = [];
    for (;;) {
      const sizeLine = await r.line();
      const size = parseInt(sizeLine.split(";")[0].trim(), 16);
      if (!isFinite(size) || size < 0) throw new Error("chunk 长度非法");
      if (size === 0) { await r.line(); break; }
      parts.push(await r.need(size));
      await r.line();
    }
    return concat(parts);
  }
  const cl = headers.get("content-length");
  if (cl) {
    const n = Number(cl);
    if (!isFinite(n) || n < 0) throw new Error("content-length 非法");
    return r.need(n);
  }
  return r.rest();
};

/** wa 是插件的运行时助手：文件路径与出网客户端。 */
export const wa = {
  /**
   * 把「相对空间工作区的路径」拼成绝对路径，直接喂给 Deno.readTextFile 等。
   * 没声明的目录在沙箱里根本不存在，报的是 ENOENT。
   */
  path(rel: string) {
    const root = Deno.env.get("WA_SPACE_DIR") || "";
    return root + "/" + String(rel).replace(/^\/+/, "");
  },

  /**
   * 发一次 HTTP 请求。域名必须在清单 capabilities.hosts 里，否则宿主代理直接拒绝。
   * 返回 { status, ok, headers, body }，body 已按 Content-Length / chunked 解码。
   */
  async fetch(input: any, init: any = {}) {
    const proxy = Deno.env.get("WA_EGRESS_PROXY");
    if (!proxy) {
      throw new Error("插件没有出网权限：清单里没有声明 http.request 与 hosts");
    }
    const u = new URL(String(input));
    const conn = await Deno.connect({ transport: "unix", path: proxy });

    try {
      const method = String(init.method || "GET").toUpperCase();
      const headers = new Headers(init.headers || {});
      headers.set("Host", u.host);
      headers.set("Connection", "close");
      let bodyBytes: Uint8Array | null = null;
      if (init.body != null) {
        bodyBytes = typeof init.body === "string" ? enc.encode(init.body) : init.body;
        if (!headers.has("Content-Length")) headers.set("Content-Length", String(bodyBytes.length));
      }

      // absolute-form：告诉宿主代理「要连哪个 https 目标」，TLS 由它完成。
      let head = method + " " + u.href + " HTTP/1.1\r\n";
      headers.forEach((v, k) => { head += k + ": " + v + "\r\n"; });
      head += "\r\n";
      await conn.write(enc.encode(head));
      if (bodyBytes) await conn.write(bodyBytes);

      const r = new SockReader(conn);
      const statusLine = await r.line();
      const m = statusLine.match(/^HTTP\/1\.[01] (\d{3})\s*(.*)$/);
      if (!m) throw new Error("响应状态行非法：" + statusLine);
      const status = Number(m[1]);
      const statusText = m[2] || "";
      const respHeaders = new Headers();
      for (;;) {
        const line = await r.line();
        if (line === "") break;
        const idx = line.indexOf(":");
        if (idx > 0) respHeaders.append(line.slice(0, idx).trim(), line.slice(idx + 1).trim());
      }
      const bytes = await readBody(r, respHeaders);
      const payload = dec.decode(bytes);
      return {
        ok: status >= 200 && status < 300,
        status,
        statusText,
        headers: respHeaders,
        body: payload,
        text: () => payload,
        json: () => JSON.parse(payload),
      };
    } finally {
      try { conn.close(); } catch (_) { /* ignore */ }
    }
  },
};

const nextInvoke = () => new Promise<Frame>((resolve) => {
  const q = invokeQueue.shift();
  if (q) return resolve(q);
  invokeResolver = resolve;
});

/** 入口协议：读完一帧 invoke，调用 handler，再写回 return / error。可反复调用。 */
export async function serve(handler: any) {
  if (typeof handler !== "function") {
    write({ op: "error", message: "插件入口必须 export default 一个函数" });
    return;
  }
  while (true) {
    const frame = await nextInvoke();
    try {
      const data = await handler(frame.data ?? {}, frame.args ?? {});
      write({ op: "return", data });
    } catch (e) {
      write({ op: "error", message: e instanceof Error ? e.message : String(e) });
    }
  }
}

export const ready = pump;
`

// bootstrap 是 deno 的入口：先把宿主运行时挂到全局，再动态加载插件入口。
//
// 挂全局而不是让插件 import：host_sdk.ts 在运行时目录里，插件目录里的
// `import "./host_sdk.ts"` 会按插件目录解析，根本找不到文件。全局变量
// 不依赖模块解析，插件脚本可以只有一个 index.ts。
const bootstrapSDK = `// @ts-nocheck
import { serve, ready, host, wa } from "./host_sdk.ts";

const g = globalThis as any;
g.host = host;
g.wa = wa;

const entry = Deno.args[0];
if (!entry) {
  console.error("missing plugin entry");
  Deno.exit(2);
}
await ready;
const mod = await import(entry);
await serve(mod.default);
`

// installSDK 把运行时写进运行时目录，供 bootstrap 导入。
func installSDK(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建插件运行时目录失败: %w", err)
	}
	files := map[string]string{
		"host_sdk.ts":  hostSDK,
		"bootstrap.ts": bootstrapSDK,
	}
	for name, body := range files {
		target := filepath.Join(dir, name)
		// 内容变化才写，避免每次启动都改 mtime 触发 deno 重新解析缓存。
		if cur, err := os.ReadFile(target); err == nil && string(cur) == body {
			continue
		}
		if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
			return fmt.Errorf("写入 %s 失败: %w", name, err)
		}
	}
	return nil
}
