// Package mobile 是给 gomobile bind 用的安卓入口层。
// 安卓端与 PC 端跑完全相同的 engine + web，WebView 直接加载本机 127.0.0.1 页面，
// 因此三端（exe / apk / 网页）是同一份引擎与同一份 UI。
package mobile

import (
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"sync"

	"pml/engine"
	"pml/web"
)

var (
	mu     sync.Mutex
	srv    *http.Server
	ln     net.Listener
	inited bool
)

func jsonOf(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{"ok":false,"error":"marshal failed"}`
	}
	return string(b)
}

// Init 初始化引擎。dataDir 用 Context.getFilesDir()，dl/ul 传空即使用内嵌源。
func Init(dataDir string, reportURL string, reportKey string) string {
	mu.Lock()
	defer mu.Unlock()
	dl, ul := web.Sources()
	engine.Init(dataDir, dl, ul)
	if reportKey == "" {
		reportKey = web.DefaultSiteKey
	}
	web.Configure("android", reportURL, reportKey)
	inited = true
	return jsonOf(map[string]interface{}{"ok": true, "status": engine.Status()})
}

// Serve 在 127.0.0.1:port 启动与 PC 版同一套服务，返回实际端口。
func Serve(port int) string {
	mu.Lock()
	defer mu.Unlock()
	if !inited {
		Init("", "", "")
	}
	if srv != nil {
		return jsonOf(map[string]interface{}{"ok": true, "port": curPort})
	}
	start := port
	if start <= 0 {
		start = 8799
	}
	var lastErr error
	for p := start; p < start+30; p++ {
		l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
		if err != nil {
			lastErr = err
			continue
		}
		ln = l
		curPort = p
		srv = &http.Server{Handler: web.Handler()}
		go func() { _ = srv.Serve(ln) }()
		return jsonOf(map[string]interface{}{"ok": true, "port": p})
	}
	msg := "no free port"
	if lastErr != nil {
		msg = lastErr.Error()
	}
	return jsonOf(map[string]interface{}{"ok": false, "error": msg})
}

var curPort int

// Port 返回当前服务端口（0 表示未启动）。
func Port() int { return curPort }

// Shutdown 停止本地服务。
func Shutdown() string {
	mu.Lock()
	defer mu.Unlock()
	if srv != nil {
		_ = srv.Close()
		srv = nil
		ln = nil
	}
	return jsonOf(map[string]interface{}{"ok": true})
}

// Start 直接驱动引擎（供不经 HTTP 的原生调用）。
func Start(mode string, per int) string { return jsonOf(engine.Start(mode, per)) }

// Stop 停止打流。
func Stop() string { return jsonOf(engine.Stop()) }

// Status 引擎状态 JSON。
func Status() string { return jsonOf(engine.Status()) }

// Info 引擎信息 JSON。
func Info() string { return jsonOf(engine.Info()) }

// Profiles 配置档列表 JSON。
func Profiles() string { return jsonOf(engine.Profiles()) }

// UseProfile 切换配置档。
func UseProfile(id int) string { return jsonOf(engine.UseProfile(id)) }

// ImportConf 导入 .conf 文本内容。
func ImportConf(text string) string { return jsonOf(engine.ImportConf([]byte(text))) }

// ResetSources 恢复内置源。
func ResetSources() string { return jsonOf(engine.ResetSources()) }

// SetNic 由安卓层回填当前网卡与 WiFi 协商速率(Mbps)，供状态页"网口信息"显示。
func SetNic(name string, mbps float64) { web.SetNicLabel(name, mbps) }

// SetForceV4 切换强制 IPv4。
func SetForceV4(on bool) string { engine.SetForceV4(on); return Status() }

// ExportConf 返回可分享的 .conf 文本。
func ExportConf() string { return string(engine.ExportConf()) }

// EngineVersion 版本号。
func EngineVersion() string { return engine.Version }
