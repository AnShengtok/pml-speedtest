//go:build windows

package main

import (
	_ "embed"
	"log"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
)

//go:embed app.ico
var appIcon []byte

const (
	dwmwaUseImmersiveDarkMode   = 20 // Win10 2004+/Win11; 19 on older builds
	dwmwaUseImmersiveDarkModeOl = 19
	wmSetIcon                   = 0x0080
	iconSmall                   = 0
	iconBig                     = 1
	imageIcon                   = 1
	lrLoadFromFile              = 0x00000010
)

// Color attributes only exist on Windows 11; the calls fail and are ignored on Win10.
const (
	dwmwaBorderColor  = 34
	dwmwaCaptionColor = 35
	dwmwaTextColor    = 36
	dwmwaColorDefault = 0xFFFFFFFE
)

// COLORREF is 0x00BBGGRR. These mirror the page palette in web/assets/index.html
// (body background #070b16, text #eef2fb) so the caption blends into the dark UI.
const (
	colorrefPanelBg = 0x00160B07
	colorrefPanelFg = 0x00FBF2EE
)

var (
	user32                 = syscall.NewLazyDLL("user32.dll")
	dwmapi                 = syscall.NewLazyDLL("dwmapi.dll")
	procSendMessageW       = user32.NewProc("SendMessageW")
	procLoadImageW         = user32.NewProc("LoadImageW")
	procDwmSetWindowAttrib = dwmapi.NewProc("DwmSetWindowAttribute")
)

// styleNativeWindow makes the WebView2 host window match the dark UI: immersive dark
// title bar plus the app icon on the title bar and taskbar.
func styleNativeWindow(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	enable := int32(1)
	for _, attr := range []uint32{dwmwaUseImmersiveDarkMode, dwmwaUseImmersiveDarkModeOl} {
		r, _, _ := procDwmSetWindowAttrib.Call(hwnd, uintptr(attr), uintptr(unsafe.Pointer(&enable)), unsafe.Sizeof(enable))
		if r == 0 {
			break
		}
	}
	for _, c := range []struct {
		attr  uint32
		value uint32
	}{
		{dwmwaCaptionColor, colorrefPanelBg},
		{dwmwaBorderColor, colorrefPanelBg},
		{dwmwaTextColor, colorrefPanelFg},
	} {
		v := c.value
		if r, _, _ := procDwmSetWindowAttrib.Call(hwnd, uintptr(c.attr), uintptr(unsafe.Pointer(&v)), unsafe.Sizeof(v)); r != 0 {
			def := uint32(dwmwaColorDefault)
			procDwmSetWindowAttrib.Call(hwnd, uintptr(c.attr), uintptr(unsafe.Pointer(&def)), unsafe.Sizeof(def))
		}
	}
	if ico := loadAppIcon(); ico != 0 {
		procSendMessageW.Call(hwnd, wmSetIcon, iconSmall, ico)
		procSendMessageW.Call(hwnd, wmSetIcon, iconBig, ico)
	}
}

func loadAppIcon() uintptr {
	dir := filepath.Join(os.TempDir(), "pml-icon")
	_ = os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, "app.ico")
	if _, err := os.Stat(p); err != nil || len(appIcon) == 0 {
		if len(appIcon) == 0 {
			return 0
		}
		if err := os.WriteFile(p, appIcon, 0o644); err != nil {
			return 0
		}
	}
	w, _ := syscall.UTF16PtrFromString(p)
	h, _, _ := procLoadImageW.Call(0, uintptr(unsafe.Pointer(w)), imageIcon, 0, 0, lrLoadFromFile)
	return h
}

// runNativeWindow opens an independent native window embedding the system WebView2 runtime,
// pointing at the local server URL. The desktop exe becomes its own installable window with a
// dark title bar and app icon, no longer a browser tab. Must run on the main OS thread.
func runNativeWindow(url, dataPath string) bool {
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     false,
		AutoFocus: true,
		DataPath:  dataPath,
		WindowOptions: webview2.WindowOptions{
			Title:  "打流测试",
			Width:  1280,
			Height: 880,
			Center: true,
		},
	})
	if w == nil {
		log.Println("webview2 init failed, fallback to browser")
		return false
	}
	defer w.Destroy()
	w.SetSize(1280, 880, webview2.HintNone)
	styleNativeWindow(uintptr(w.Window()))
	w.Navigate(url)
	w.Run()
	return true
}
