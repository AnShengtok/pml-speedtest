package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// 内置源的持久化接口：后台（含 AI 自动更新、探测新源地址）通过它维护“内置源池”。
type builtinSources struct {
	Download  []string `json:"download"`
	Upload    []string `json:"upload"`
	UpdatedAt int64    `json:"updatedAt"`
}

func builtinPath() string {
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, "builtin_sources.json")
}

// LoadBuiltinOverride 启动时若存在后台更新过的内置源，则覆盖编译期内置默认源。
func LoadBuiltinOverride() {
	p := builtinPath()
	if p == "" {
		return
	}
	b, err := os.ReadFile(p)
	if err != nil || len(b) == 0 {
		return
	}
	var o builtinSources
	if json.Unmarshal(b, &o) != nil {
		return
	}
	srcMu.Lock()
	if d := uniq(filterHTTP(o.Download)); len(d) > 0 {
		baseDL = d
	}
	if u := uniq(filterHTTP(o.Upload)); len(u) > 0 {
		baseUL = u
	}
	srcMu.Unlock()
}

// SetBuiltin 更新“内置源池”并持久化，供后台 AI 自动写入。
// 仅改变内置基线，不覆盖用户已导入的配置档；重启或“恢复内置”后生效。
func SetBuiltin(dl, ul []string) map[string]interface{} {
	srcMu.Lock()
	if d := uniq(filterHTTP(dl)); len(d) > 0 {
		baseDL = d
	}
	if u := uniq(filterHTTP(ul)); len(u) > 0 {
		baseUL = u
	}
	d, u := baseDL, baseUL
	srcMu.Unlock()
	p := builtinPath()
	if p != "" {
		payload, err := json.MarshalIndent(builtinSources{Download: d, Upload: u, UpdatedAt: time.Now().Unix()}, "", "  ")
		if err == nil && len(payload) > 0 {
			_ = os.WriteFile(p, payload, 0o644)
		}
	}
	return map[string]interface{}{"ok": true, "downloadUrls": len(d), "uploadUrls": len(u)}
}

// Builtin 返回当前内置源池（后台读取用）。
func Builtin() map[string]interface{} {
	srcMu.Lock()
	d := append([]string(nil), baseDL...)
	u := append([]string(nil), baseUL...)
	srcMu.Unlock()
	return map[string]interface{}{"download": d, "upload": u}
}

// builtinFetcher is an optional callback that pulls the master builtin-source
// pool from the remote maintenance backend (Tailscale admin). Init calls it so a
// running install adopts freshly maintained sources without rebuilding.
var builtinFetcher func() (dl []string, ul []string)

// SetBuiltinFetcher registers the remote pull callback. Call before Init.
func SetBuiltinFetcher(f func() (dl []string, ul []string)) {
	builtinFetcher = f
}
