//go:build !windows

package main

import "log"

// runNativeWindow is a no-op on non-Windows platforms. The desktop WebView2 window
// is Windows-only; on other OS the caller falls back to the system browser.
func runNativeWindow(url, dataPath string) bool {
	log.Println("native window unsupported on this platform, fallback to browser")
	return false
}
