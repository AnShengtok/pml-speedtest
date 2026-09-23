// Package app 是 PC 版与纯引擎版共用的启动流程：起本地服务、落盘真实端口、按需开原生窗口。
// 拆出来是为了让 Tauri 等宿主能拿到一个「只跑引擎、不带窗口」的 sidecar 入口，
// 同时保证两个入口的端口发现、上报配置与超时参数完全一致。
package app

import (
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"pml/engine"
	"pml/web"
)

// Options 描述宿主形态。
type Options struct {
	// Headless 为 true 时只跑引擎：不开窗口也不回退系统浏览器（sidecar 用）。
	Headless bool
	// OpenWindow 由宿主提供原生窗口实现，返回 false 表示该平台或该机器不可用。
	OpenWindow func(url, dataPath string) bool
	// FallbackBrowser 在原生窗口不可用时打开系统浏览器。
	FallbackBrowser func(url string)
}

// DataDir 返回引擎数据目录（与历史版本同一个 %APPDATA% 打流测试）。
func DataDir() string {
	if v := os.Getenv("PML_DATA"); v != "" {
		return v
	}
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = "."
	}
	d := filepath.Join(dir, "打流测试")
	_ = os.MkdirAll(d, 0o755)
	return d
}

func pickListener() (net.Listener, string, error) {
	start := 8799
	if v := os.Getenv("PML_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			start = n
		}
	}
	var last error
	for p := start; p < start+30; p++ {
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
		if err == nil {
			return ln, strconv.Itoa(p), nil
		}
		last = err
	}
	return nil, "", last
}

// Run 起服务并按 Options 决定宿主行为。窗口模式阻塞到窗口关闭，headless 常驻。
func Run(o Options) {
	if v := os.Getenv("PML_LOG"); v != "" {
		if f, err := os.OpenFile(v, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			log.SetOutput(f)
		} else {
			log.SetOutput(os.Stdout)
		}
	} else {
		log.SetOutput(os.Stdout)
	}
	dd := DataDir()
	web.Configure("pc", os.Getenv("PML_REPORT_URL"), os.Getenv("PML_REPORT_KEY"))
	dl, ul := web.Sources()
	engine.Init(dd, dl, ul)

	ln, port, err := pickListener()
	if err != nil {
		log.Printf("无可用端口: %v", err)
		return
	}
	url := "http://127.0.0.1:" + port + "/"
	// 端口发现：pickListener 会在 8799..8828 间避让，宿主读端口文件即可拿到真实端口。
	// 默认落在数据目录；宿主（如 Tauri sidecar）可用 PML_PORT_FILE 指定独占路径，
	// 避免与同时运行的 PC 版互相覆盖同一个 engine.port。
	portFile := os.Getenv("PML_PORT_FILE")
	if portFile == "" {
		portFile = filepath.Join(dd, "engine.port")
	}
	_ = os.WriteFile(portFile, []byte(port), 0o644)
	st := engine.Status()
	log.Printf("打流测试(原生引擎 v%s) 已启动: %s 下载源%v 上传源%v 数据目录:%s", engine.Version, url, st["dlSources"], st["ulSources"], dd)

	// 先让本地服务在后台常驻，窗口/浏览器随后访问它。
	// 只设读头/空闲超时，防 Slowloris；不设 ReadTimeout/WriteTimeout，否则会斩掉 /api/progress 长连接。
	srv := &http.Server{
		Handler:           web.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		if e := srv.Serve(ln); e != nil && e != http.ErrServerClosed {
			log.Printf("服务退出: %v", e)
			os.Exit(1)
		}
	}()
	time.Sleep(200 * time.Millisecond)

	if !o.Headless && os.Getenv("PML_NO_BROWSER") == "" {
		// 优先开独立原生窗口（Windows + 系统 WebView2），失败再回退系统浏览器。
		if o.OpenWindow != nil && o.OpenWindow(url, filepath.Join(dd, "webview2-"+port)) {
			return // 用户关闭窗口后正常退出，服务随进程结束。
		}
		if o.FallbackBrowser != nil {
			o.FallbackBrowser(url)
		}
	}

	// headless / 回退模式：保持服务常驻。
	select {}
}
