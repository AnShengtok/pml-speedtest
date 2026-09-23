// 打流测试 · 电脑原生版（Windows exe）
// 内嵌与网页版同一套 UI，由 Go 引擎直接打流：无 CORS 限制、可高并发、支持全部导入源。
// Windows 下通过系统 WebView2 运行时开一个独立原生窗口（自带标题栏与任务栏图标），不再是浏览器标签页。
// 通用启动流程（端口发现、engine.port 落盘、服务常驻）在 pml/app 里，与 headless 引擎共用；
// 本文件只负责把自己的原生窗口实现注入进去。
package main

import (
	"runtime"

	"pml/app"
)

func main() {
	// WebView2 主循环必须运行在主 OS 线程上，整个进程从主线程启动。
	runtime.LockOSThread()
	app.Run(app.Options{OpenWindow: runNativeWindow, FallbackBrowser: openBrowser})
}
