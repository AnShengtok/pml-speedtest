// Package web 是三端共用的本地 HTTP 层：同一份 UI、同一套 API。
// 网页版由公网服务器提供（浏览器引擎），exe / apk 由本包内嵌资源提供（原生引擎）。
package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"pml/engine"
)

//go:embed all:assets
var assets embed.FS

// Wire the authoritative upload counter. On Windows NicTxBytes reports real NIC
// egress (dwOutOctets), matching the app's getTxBytes(iface) measure, so the
// upload figure can never be inflated by socket slow-start/buffer bursts. On other
// platforms NicTxBytes returns 0 and the engine falls back to socket counting.
func init() {
	engine.SetTxCounter(NicTxBytes)
	engine.SetRxCounter(NicRxBytes)
}

// 上报默认目标（可被环境变量覆盖）。
const (
	DefaultReportURL = "http://203.0.113.10/report"
	DefaultSiteKey   = "pml-web-your-own-key"
)

var (
	clientName = "pc"
	reportURL  = DefaultReportURL
	siteKey    = DefaultSiteKey
)

// Configure 设置来源端标识与上报地址。
func Configure(client, rURL, key string) {
	if client != "" {
		clientName = client
	}
	if rURL != "" {
		reportURL = rURL
	}
	if key != "" {
		siteKey = key
	}
	engine.SetBuiltinFetcher(fetchBuiltinFromServer)
}

func assetFS() fs.FS {
	f, err := fs.Sub(assets, "assets")
	if err != nil {
		log.Fatal(err)
	}
	return f
}

// Sources 读取内嵌的内置源清单。
func Sources() ([]string, []string) {
	b, err := fs.ReadFile(assetFS(), "sources.json")
	if err != nil {
		return nil, nil
	}
	var d struct {
		Download []string `json:"download"`
		Upload   []string `json:"upload"`
	}
	if json.Unmarshal(b, &d) != nil {
		return nil, nil
	}
	return d.Download, d.Upload
}

func writeJSON(w http.ResponseWriter, o interface{}) {
	b, _ := json.Marshal(o)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(b)
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	b, err := fs.ReadFile(assetFS(), "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	w.Write(b)
}

func handleAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/assets/")
	if name == "" || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	b, err := fs.ReadFile(assetFS(), name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasSuffix(name, ".js"):
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	case strings.HasSuffix(name, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(name, ".json"):
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(b)
}

func handleVendor(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/vendor/")
	if name == "" || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	b, err := fs.ReadFile(assetFS(), "vendor/"+name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(b)
}

func handleProgress(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	c, cancel := engine.Subscribe()
	defer cancel()
	st := engine.Status()
	emitHello := map[string]interface{}{"type": "hello"}
	for k, v := range st {
		emitHello[k] = v
	}
	if b, err := json.Marshal(emitHello); err == nil {
		io.WriteString(w, "data: "+string(b)+"\n\n")
		flusher.Flush()
	}
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	notify := r.Context().Done()
	for {
		select {
		case <-notify:
			return
		case s := <-c:
			io.WriteString(w, "data: "+s+"\n\n")
			flusher.Flush()
		case <-ping.C:
			io.WriteString(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// handleReport 把页面产生的事件补上来源端后转发到公网后台，
// 保证 exe / apk / 网页三端在后台用同一套字段、且可区分来源。
func handleReport(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Pml-Key")
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 8192))
	var o map[string]interface{}
	if json.Unmarshal(body, &o) != nil {
		o = map[string]interface{}{}
	}
	if _, ok := o["client"]; !ok {
		o["client"] = clientName
	}
	o["client"] = clientName
	nb, _ := json.Marshal(o)
	res := make(chan bool, 1)
	go func() {
		req, err := http.NewRequest("POST", reportURL, bytes.NewReader(nb))
		if err != nil {
			res <- false
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Pml-Key", siteKey)
		cl := &http.Client{Timeout: 8 * time.Second}
		resp, err := cl.Do(req)
		if err != nil {
			res <- false
			return
		}
		io.Copy(io.Discard, resp.Body)
		ok := resp.StatusCode < 400
		resp.Body.Close()
		res <- ok
	}()
	ok := false
	select {
	case ok = <-res:
	case <-time.After(2500 * time.Millisecond):
	}
	w.Header().Set("Content-Type", "application/json")
	if ok {
		w.Write([]byte(`{"ok":true}`))
	} else {
		w.Write([]byte(`{"ok":false,"error":"forward failed or timeout"}`))
	}
}

func handleImport(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	writeJSON(w, engine.ImportConf(body))
}

func handleUseProfile(w http.ResponseWriter, r *http.Request) {
	var o struct {
		ID int `json:"id"`
	}
	body, _ := io.ReadAll(r.Body)
	json.Unmarshal(body, &o)
	writeJSON(w, engine.UseProfile(o.ID))
}

func handleDeleteProfile(w http.ResponseWriter, r *http.Request) {
	var o struct {
		ID int `json:"id"`
	}
	body, _ := io.ReadAll(r.Body)
	json.Unmarshal(body, &o)
	writeJSON(w, engine.DeleteProfile(o.ID))
}

func handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var o struct {
			ForceV4 *bool `json:"forceV4"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &o)
		if o.ForceV4 != nil {
			engine.SetForceV4(*o.ForceV4)
		}
	}
	writeJSON(w, map[string]interface{}{"forceV4": engine.ForceV4(), "maxPerUrl": engine.MaxPerURL(), "version": engine.Version})
}

// sameOriginIntent 拦截跨站对状态变更接口的调用（CSRF）。本地服务虽只绑 127.0.0.1，
// 但用户浏览器里的任意网页仍可发 no-cors POST 触发打流/改配置，故对写操作校验来源：
// 仅放行同源（Sec-Fetch-Site 为 same-origin/none，或 Origin 主机等于 Host）。
func sameOriginIntent(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	if sfs := r.Header.Get("Sec-Fetch-Site"); sfs == "same-origin" || sfs == "none" {
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // 无 Origin（本机脚本/健康检查）：仅本机可达，放行
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// localGuard 用 sameOriginIntent 保护整张路由表（写操作跨站一律 403）。
func localGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !sameOriginIntent(r) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"ok":false,"error":"cross-origin blocked"}`)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	st := engine.Status()
	name, mbps := nicInfo()
	st["nic"] = name
	st["nicMbps"] = mbps
	writeJSON(w, st)
}

func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			handleIndex(w, r)
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/assets/", handleAsset)
	mux.HandleFunc("/vendor/", handleVendor)
	mux.HandleFunc("/api/progress", handleProgress)
	mux.HandleFunc("/api/status", handleStatus)
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, engine.Info()) })
	mux.HandleFunc("/api/settings", handleSettings)
	mux.HandleFunc("/api/import", handleImport)
	mux.HandleFunc("/api/profiles", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, engine.Profiles()) })
	mux.HandleFunc("/api/use_profile", handleUseProfile)
	mux.HandleFunc("/api/delete_profile", handleDeleteProfile)
	mux.HandleFunc("/api/reset_sources", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, engine.ResetSources()) })
	mux.HandleFunc("/api/admin/sources", handleAdminSources)
	mux.HandleFunc("/api/export", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="speed_profiles.conf"`)
		w.Write(engine.ExportConf())
	})
	mux.HandleFunc("/api/stream/start", func(w http.ResponseWriter, r *http.Request) {
		var o struct {
			Mode   string `json:"mode"`
			PerURL int    `json:"perUrl"`
			Conns  int    `json:"connections"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &o)
		per := o.PerURL
		if per == 0 {
			per = o.Conns
		}
		if per == 0 {
			per = engine.MaxPerURL() // 0=拉满：不再固定 10，避免截断原生引擎并发
		}
		writeJSON(w, engine.Start(o.Mode, per))
	})
	mux.HandleFunc("/api/stream/stop", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, engine.Stop())
	})
	mux.HandleFunc("/geo", handleGeo)
	mux.HandleFunc("/report", handleReport)
	mux.HandleFunc("/api/upload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.Header().Set("Allow", "POST, OPTIONS")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// 浏览器测速模式的上行落点：排空请求体即视为字节已抵达服务器，不做存储；1GB 上限兜底。
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<30))
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true,"client":"` + clientName + `","version":"` + engine.Version + `"}`))
	})
	return localGuard(mux)
}

// handleGeo 代理公网后台的 IP 查询，使原生版页面也能显示访客真实公网 IP。
func handleGeo(w http.ResponseWriter, r *http.Request) {
	base := strings.TrimSuffix(reportURL, "/report")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	ip := ""
	if base != "" && base != reportURL {
		cl := &http.Client{Timeout: 6 * time.Second}
		if resp, err := cl.Get(base + "/geo"); err == nil {
			bb, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			var g struct {
				IP string `json:"ip"`
			}
			if json.Unmarshal(bb, &g) == nil {
				ip = g.IP
			}
		}
	}
	isp := ""
	org := ""
	if ip != "" {
		cl := &http.Client{Timeout: 6 * time.Second}
		if resp, err := cl.Get("http://ip-api.com/json/" + ip + "?fields=status,isp,org"); err == nil {
			bb, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			var g struct {
				Status string `json:"status"`
				Isp    string `json:"isp"`
				Org    string `json:"org"`
			}
			if json.Unmarshal(bb, &g) == nil {
				isp = g.Isp
				org = g.Org
			}
		}
	}
	out, _ := json.Marshal(map[string]string{"ip": ip, "isp": isp, "org": org})
	w.Write(out)
}

// fetchBuiltinFromServer pulls the maintained builtin-source master list from the
// public backend (Caddy-proxied /api/builtin on the report host), gated by the
// same X-Pml-Key used for /report. It returns nil lists on any failure so the
// caller keeps its compiled/local defaults; offline startup is never blocked.
func fetchBuiltinFromServer() ([]string, []string) {
	base := strings.TrimSuffix(reportURL, "/report")
	if base == "" || base == reportURL {
		return nil, nil
	}
	req, err := http.NewRequest("GET", base+"/api/builtin", nil)
	if err != nil {
		return nil, nil
	}
	req.Header.Set("X-Pml-Key", siteKey)
	cl := &http.Client{Timeout: 2500 * time.Millisecond}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil
	}
	bb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var o struct {
		Download []string `json:"download"`
		Upload   []string `json:"upload"`
	}
	if json.Unmarshal(bb, &o) != nil {
		return nil, nil
	}
	return o.Download, o.Upload
}
