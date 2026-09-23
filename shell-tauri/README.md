# Tauri 换壳（Phase 3 · 已跑通）

2026-09-11 实测通过：Go 引擎以 sidecar 形式内嵌，Tauri v2 出原生窗口与 NSIS 安装包。

## 一句话结论

Tauri 在 Windows 上用的也是 WebView2，所以换壳**不改变渲染结果、不让测速更快、不动任何取值口径**。
真正的收益是工程项：官方维护的窗口绑定（替掉第三方 Go 绑定）、官方安装包、以及后续可挂的
自动更新 / 托盘 / 单实例插件。

## 实测记录（本机 2026-09-11）

| 项 | 结果 |
|---|---|
| `cargo check` / `cargo build --release` | 通过，release 5m24s |
| `npx tauri build`（NSIS） | 通过，安装包约 4 MB（壳 + 引擎一起打进去） |
| 静默安装实测 | `/S` 加 `/D=<目录>` 退出码 0，装入 `pml-shell.exe` + `pml-engine.exe` |
| 安装版启动 | 原生窗口标题「打流测试」，无控制台黑框 |
| 壳内真跑一次测速 | 下行 595.3 / 上行 59.0 Mbps，用时 21s，共传 697.7 MB，弹窗与历史正常 |
| 关窗回收 | 壳与引擎进程同时消失，8799 端口释放，**无孤儿进程**（debug/release 各验一次） |
| 与 PC 版共存 | 引擎写壳自己的端口文件，`%APPDATA%\打流测试\engine.port` 不被覆盖 |

## 接线方式（端口发现协议）

壳与引擎之间只有一个约定：**端口文件**。

1. 壳在自己的配置目录（`%APPDATA%\com.pml.speedtest\pml`）下准备 `engine.port` 路径，
   先删旧文件，再通过环境变量 `PML_PORT_FILE` 传给引擎；同时用 `PML_LOG` 把引擎日志落到 `engine.log`。
2. 引擎（`pml/app`）在 8799..8828 之间避让选端口，把真实端口写进 `PML_PORT_FILE`。
3. 壳轮询该文件拿到端口，再 TCP 探测确认监听，最后创建指向 `http://127.0.0.1:<port>/` 的窗口。
4. 壳退出（`RunEvent::Exit`）时 `child.kill()` 回收引擎。

之所以让壳指定独占文件、而不是共用 `%APPDATA%\打流测试\engine.port`，是因为 PC 版也写这个文件；
两者同时运行时共用会互相覆盖，导致壳连到 PC 版的端口上。

## 构建步骤（可复现）

工具链全部装在 D 盘（本机长期约定：软件默认装 D 盘）：

| 组件 | 路径 |
|---|---|
| Rust | `D:\dev\rust`（`RUSTUP_HOME` / `CARGO_HOME` 指向其中的 `.rustup` / `.cargo`） |
| MSVC Build Tools | `D:\VS\BuildTools`（Windows SDK 由微软强制装在 C 盘 Windows Kits，无法改盘） |
| Go / Node | `D:\qwrt\tools\go`、`D:\qwrt\tools\node-v20.19.0-win-x64` |
| npm 缓存 | `D:\dev\npm-cache` |

一键出包：在 `shell-tauri` 目录执行 `powershell -NoProfile -ExecutionPolicy Bypass -File bootstrap.ps1`。

只要二进制不要安装包：`cargo build --release`，产物 `src-tauri\target\release\pml-shell.exe`
（构建时会把 sidecar 复制成同目录的 `pml-engine.exe`，两者必须放在一起）。

## 踩过的坑（再改这里先看这几条）

- `tauri-plugin-shell` v2 **没有** `sidecar` feature，写了 `cargo check` 直接解析失败。
- `sidecar()` 返回的是 `process::Command`，`spawn()` **不带参数**，
  返回 `(Receiver<CommandEvent>, CommandChild)`。
- Rust 的 `b"..."` 字节串**不能含中文**，要写成 `"中文\n".as_bytes()`。
- sidecar 的 stdout/stderr 是 piped，**必须一直抽干**，否则管道写满会把引擎堵死；
  引擎日志走 `PML_LOG` 落盘，抽干任务只做兜底。
- WebView 窗口必须在**主线程**创建，所以端口等待放在 `setup` 里同步做，不要另起线程。
- 端口文件先删后读，否则可能读到上一次遗留的端口，连到一个已经不存在的进程上。
- 版本快照：tauri 2.11.5 / tauri-plugin-shell 2.3.6 / rustc 1.98.1。

## 与 PC 版（`cmd/pc`）的关系

两者共用 `pml/app` 的启动流程，差别只在注入的宿主：

- `cmd/pc`：注入 `runNativeWindow`（Go + go-webview2 直开窗口），失败回退系统浏览器。
- `cmd/engine`：`Headless: true`，只跑服务不开窗口，给壳当 sidecar。

所以取值口径、上报配置、超时参数、端口避让策略天然一致，不存在「两套引擎各测各的」的风险。
`cmd/pc` 暂时保留，等壳跑稳一段时间再决定是否退役。
