package web

import (
	"encoding/json"
	"io"
	"net/http"

	"pml/engine"
)

// handleAdminSources 暴露“内置源”的后台维护接口：
// 供后台（含 AI 自动更新、探测新源地址）读取与写入内置源池。
// 通过 X-Pml-Key 头鉴权，密钥与上报站点一致。
func handleAdminSources(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Pml-Key") != siteKey {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"ok":false,"error":"unauthorized"}`)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, engine.Builtin())
	case http.MethodPost:
		var o struct {
			Download []string `json:"download"`
			Upload   []string `json:"upload"`
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err := json.Unmarshal(body, &o); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "bad json"})
			return
		}
		writeJSON(w, engine.SetBuiltin(o.Download, o.Upload))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
