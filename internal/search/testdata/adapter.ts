// 脚本搜索源示例：两段式，脚本自己不出网。
//
//   plan：把 query / limit 翻成一次 HTTP 请求（method / url / headers / body）
//   map ：把响应体翻成 [{title,url,snippet,published_at}]
//
// 宿主负责真正发请求（走 Guard：仅 https、域名白名单、拒绝私网 IP），
// 所以脚本拿不到网络权限，也拿不到「随便连一个地址」的能力。
// 密钥从 stdin 的 secret 字段来，不要写进 argv 或环境变量。

type Input = {
  phase: "plan" | "map";
  query?: string;
  limit?: number;
  endpoint?: string;
  secret?: string;
  status?: number;
  body?: string;
};

type Hit = { t: string; u: string; s?: string; d?: string };

const input: Input = JSON.parse(await new Response(Deno.stdin.readable).text());

const emit = (value: unknown): void => console.log(JSON.stringify(value));

if (input.phase === "plan") {
  emit({
    request: {
      method: "POST",
      url: input.endpoint ?? "https://example.com/search",
      headers: {
        "Content-Type": "application/json",
        "Authorization": `Bearer ${input.secret ?? ""}`,
      },
      body: JSON.stringify({ q: input.query, n: input.limit }),
    },
  });
} else {
  const data = JSON.parse(input.body ?? "{}");
  if ((data.code ?? 0) !== 0) {
    // 业务错误码要显式报错，不要让它变成「0 条结果」
    emit({ error: `接口返回 code=${data.code} ${data.msg ?? ""}`.trim() });
  } else {
    const items: Hit[] = data.items ?? [];
    emit({
      results: items.map((x) => ({
        title: x.t,
        url: x.u,
        snippet: x.s ?? "",
        published_at: x.d ?? "",
      })),
    });
  }
}
