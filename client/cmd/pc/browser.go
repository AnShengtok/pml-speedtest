package main

import (
	"os/exec"
	"runtime"
)

// openBrowser 在非 Windows 或 WebView2 初始化失败时，退回系统浏览器。
func openBrowser(url string) {
	switch runtime.GOOS {
	case "windows":
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		_ = exec.Command("open", url).Start()
	default:
		_ = exec.Command("xdg-open", url).Start()
	}
}
