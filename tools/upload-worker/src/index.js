// 上行测速靶子 · 部署在 Cloudflare Workers 边缘节点
// 目的：把上行打流从「自家源站」和「别人家的只读 CDN」彻底移开——
//   1) 自家服务器经不起大量用户持续 POST，且高峰期限速会让读数失真；
//   2) 只读 CDN 对 POST 会提前拒绝（403/404/405），字节没真正上链就返回，导致上行虚高。
// 这里读完整个请求体再回 200，并在响应里回显真实接收字节数，口径可验证。
export default {
  async fetch(request) {
    // 回显请求来源，兼容 file:// 与本地壳的 null origin，避免通配符与凭据冲突
    const origin = request.headers.get("Origin") || "*";
    const headers = {
      "Access-Control-Allow-Origin": origin,
      "Access-Control-Allow-Methods": "POST, OPTIONS",
      "Access-Control-Allow-Headers": "*",
      "Access-Control-Max-Age": "86400",
      "Cache-Control": "no-store",
    };

    if (request.method === "OPTIONS") {
      return new Response(null, { status: 204, headers });
    }

    if (request.method !== "POST") {
      return new Response("POST bytes here", { status: 200, headers });
    }

    // 逐块读完请求体：只有读完，才算真正把这段上行收到了边缘
    let received = 0;
    const body = request.body;
    if (body) {
      const reader = body.getReader();
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        if (value) received += value.byteLength;
      }
    }

    headers["Content-Type"] = "application/json";
    return new Response(JSON.stringify({ bytes: received }), { status: 200, headers });
  },
};
