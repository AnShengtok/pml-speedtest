# pml-speedtest（持续打流测速 · 开源版）

> 官网 · 在线测速与打流测试：<https://speedtest.xyz201704.com/>
> 下载与功能介绍：<https://speedtest.xyz201704.com/app/>


多端同源的持续打流测速工具：同一份前端 + Go 测速内核 + 后台看板。

> 本仓库为**脱敏开源版**：源池配置均为**公共示例端点**（Cloudflare / httpbin / 镜像站等），不含任何私有基础设施、凭据或第三方借用端点。请在自己的环境里替换为可直连且允许压测的靶子。

## 组成
- **client/** 客户端
  - `engine/`：Go 测速内核（多 Worker 并发、稳健峰值 robustPeak、按网卡绑定、DNS 固定）
  - `cmd/`：`pc`（桌面 exe，内嵌 WebView2 + go:embed 前端）/`bench`/`engine`
  - `web/`：网页引擎与本地服务；前端资源在 `web/assets/`
  - `mobile/`：gomobile 绑定，用于安卓分支
- **backend/**：Go 后台（stdlib）——数据接收 `/report`、看板 `/admin`、`/api/stats`，含洪水拦截与离线省市区反查
- **tools/upload-worker/**：Cloudflare Worker 上传靶子（可选）
- **docs/**：测量规范

## 构建
- PC：`cd client && go build -trimpath -ldflags "-H windowsgui" -o build/pml-pc.exe ./cmd/pc`
- 后台：`cd backend && go build -o pml-geo .`
- Worker：`cd tools/upload-worker && npx wrangler deploy`

## 配置
- `backend/builtin_baseline.json`、`backend/maintainer.json`、`client/web/assets/sources.json` 均为示例端点，按需替换。
- 引擎参数（并发/窗口/峰值口径等）由后台下发；`oh` 为协议开销补偿系数（默认 1.0）。

## 许可
MIT
