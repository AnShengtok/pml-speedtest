# 上行测速靶子（Cloudflare Worker）

## 为什么要它

上行测速必须有「真的会把请求体收完」的靶子。历史上的两类靶子都不可信：

| 靶子类型 | 问题 |
| --- | --- |
| 自家服务器 `/ul` | 用户量大就打满带宽，高峰期限速 → 读数失真；已在源码以 `PML-DISABLED-selfhost-uldl` 主动下线 |
| 只读 CDN 资源（png/pdf/zip/apk 链接） | 对 POST 提前拒绝（403/404/405），字节没上链就返回 → 上行虚高假数据 |

本 Worker 跑在 Cloudflare 边缘：不吃源站带宽、免费额度足够、CORS 完全自控，并回显真实接收字节数。

## 部署

```powershell
cd D:\qwrt\pml\tools\upload-worker
npx --yes wrangler login      # 首次需要，浏览器授权 Cloudflare 账号
npx --yes wrangler deploy
```

部署成功后会打印形如 `https://pml-upload-target.<子域>.workers.dev` 的地址。

## 接入前端

拿到地址后，把它放到 `pml/web/assets/index.html` 里 `UL_FB` 数组的**第一位**，
并同步进 `pml/web/assets/sources.json` 的 `upload`，然后重新构建 PC/Tauri 三端。

## 上线前自检（务必做，口径与前端一致）

只有「2xx + CORS 可读 + 耗时随体积线性增长」的靶子才可信：

```bash
curl -s -o /dev/null -w "%{http_code} %{time_total}s\n" -X POST --data-binary @<(...) <URL>
```

对比 0.25MB / 1MB / 4MB 三档：耗时必须显著递增（说明边缘真的读完了 body）。
若三档耗时几乎相同，说明是提前拒绝，属假数据源，不可入池。
