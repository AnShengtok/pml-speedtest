// 打流测试 · 纯引擎版（headless sidecar）
// 只起本地服务并落盘真实端口（%APPDATA%\打流测试\engine.port），不开窗口也不回退系统浏览器；
// 窗口由宿主负责，Tauri 壳读到端口后把自己的 WebView 指过去即可。
// 与 PC 版共用 pml/app，因此上报配置、超时参数、端口避让策略完全一致。
package main

import (
	"pml/app"
)

func main() {
	app.Run(app.Options{Headless: true})
}
