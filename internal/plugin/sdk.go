package plugin

import (
	"fmt"
	"os"
	"path/filepath"
)

// hostSDK 是注入插件的宿主 ABI。插件本身没有任何权限（deno 默认全拒绝），
// 所有 I/O 都必须经 globalThis.host，于是策略、审计、密钥替换只有一份实现。
const hostSDK = `// @ts-nocheck
// 平台注入的宿主 ABI。不要修改这个文件：它由平台在启动时写入。
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
  /** 申请一次宿主能力。method 见平台文档：http.request / fs.read / fs.list / fs.write / log */
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

// bootstrap 是 deno 的入口：先装宿主 ABI，再动态加载插件入口。
// 用参数传 entry 而不是环境变量：argv 里只有路径，不含任何秘密。
const bootstrapSDK = `// @ts-nocheck
import { serve, ready } from "./host_sdk.ts";

const entry = Deno.args[0];
if (!entry) {
  console.error("missing plugin entry");
  Deno.exit(2);
}
await ready;
const mod = await import(entry);
await serve(mod.default);
`

// installSDK 把 ABI 写进运行时目录，供 --preload 之外的 bootstrap 导入。
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
