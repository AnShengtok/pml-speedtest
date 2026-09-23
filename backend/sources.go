package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Admin-side source profile store: mirrors the desktop "settings -> sources"
// experience (import a .conf, switch the active config, delete, and a hidden
// manual override). The active profile is what /api/builtin serves to clients.

type srcProf struct {
	ID       int      `json:"id"`
	Name     string   `json:"name"`
	Download []string `json:"downloadUrls"`
	Upload   []string `json:"uploadUrls"`
}

type srcStore struct {
	Selected int       `json:"selectedProfileId"`
	Profiles []srcProf `json:"profiles"`
}

const manualProfID = 900000001

var (
	srcMu       sync.Mutex
	srcData     srcStore
	srcLoaded   bool
	builtinPool = struct {
		Download []string `json:"download"`
		Upload   []string `json:"upload"`
	}{}
)

func jsonOut(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func srcFile() string { return filepath.Join(dataDir, "admin_profiles.json") }

func ensureBuiltinPool() {
	if len(builtinPool.Download) > 0 || len(builtinPool.Upload) > 0 {
		return
	}
	_ = json.Unmarshal(builtinBaseline, &builtinPool)
}

// ---- AI 探测新增源：独立活池文件 ai_sources.json ----
// 维护器探测健康后自动写入并纳入使用（免人工审核）；后台可逐条删除，出问题即删。

func aiFile() string { return filepath.Join(dataDir, "ai_sources.json") }

func loadAiSources() ([]string, []string) {
	b, err := os.ReadFile(aiFile())
	if err != nil {
		return nil, nil
	}
	var o struct {
		Download []string `json:"download"`
		Upload   []string `json:"upload"`
	}
	if json.Unmarshal(b, &o) != nil {
		return nil, nil
	}
	return o.Download, o.Upload
}

func saveAiSources(dl, ul []string) error {
	o := struct {
		Download []string `json:"download"`
		Upload   []string `json:"upload"`
	}{sUniq(sHTTP(dl)), sUniq(sHTTP(ul))}
	b, _ := json.MarshalIndent(o, "", "  ")
	tmp := aiFile() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, aiFile())
}

// activeBuiltin = 编译基线 ∪ AI 探测源（去重，基线在前）。
func activeBuiltin() ([]string, []string) {
	ensureBuiltinPool()
	adl, aul := loadAiSources()
	dl := sUniq(append(append([]string{}, builtinPool.Download...), adl...))
	ul := sUniq(append(append([]string{}, builtinPool.Upload...), aul...))
	return dl, ul
}

// addToAi 幂等加入 AI 源（去重 + 上限保护，超出丢弃尾部）。
func addToAi(dl, ul []string) {
	curD, curU := loadAiSources()
	const capN = 40
	nd := sUniq(append(curD, sHTTP(dl)...))
	nu := sUniq(append(curU, sHTTP(ul)...))
	if len(nd) > capN {
		nd = nd[:capN]
	}
	if len(nu) > capN {
		nu = nu[:capN]
	}
	_ = saveAiSources(nd, nu)
}

// removeFromAi 从 AI 源删除指定 URL。
func removeFromAi(kind, url string) {
	curD, curU := loadAiSources()
	keep := func(in []string) []string {
		out := []string{}
		for _, u := range in {
			if u != url {
				out = append(out, u)
			}
		}
		return out
	}
	if kind == "upload" {
		curU = keep(curU)
	} else {
		curD = keep(curD)
	}
	_ = saveAiSources(curD, curU)
}

func srcEnsureLoaded() {
	if srcLoaded {
		return
	}
	srcLoaded = true
	srcData.Selected = -1
	b, err := os.ReadFile(srcFile())
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, &srcData)
}

func srcSaveLocked() {
	b, err := json.MarshalIndent(srcData, "", "  ")
	if err == nil {
		_ = os.WriteFile(srcFile(), b, 0o644)
	}
}

func sUniq(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func sHTTP(in []string) []string {
	out := []string{}
	for _, s := range in {
		if strings.HasPrefix(s, "http") {
			out = append(out, s)
		}
	}
	return out
}

func sCollect(v interface{}, out *[]string) {
	arr, ok := v.([]interface{})
	if !ok {
		return
	}
	for _, it := range arr {
		var u string
		switch x := it.(type) {
		case string:
			u = x
		case map[string]interface{}:
			if s, ok := x["url"].(string); ok {
				u = s
			}
		}
		if strings.HasPrefix(u, "http") {
			*out = append(*out, u)
		}
	}
}

func sDeep(obj interface{}, dl, ul *[]string) {
	switch t := obj.(type) {
	case map[string]interface{}:
		for _, k := range []string{"downloadUrls", "downloadEndpoints", "download_urls", "download"} {
			if v, ok := t[k]; ok {
				sCollect(v, dl)
			}
		}
		for _, k := range []string{"uploadUrls", "uploadEndpoints", "upload_urls", "upload"} {
			if v, ok := t[k]; ok {
				sCollect(v, ul)
			}
		}
		for _, v := range t {
			sDeep(v, dl, ul)
		}
	case []interface{}:
		for _, v := range t {
			sDeep(v, dl, ul)
		}
	}
}

func sStr(m map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok {
			return v
		}
	}
	return ""
}

func sInt(m map[string]interface{}, keys ...string) (int, bool) {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int(v), true
		case string:
			if n, err := strconv.Atoi(v); err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

func sMerge(in []srcProf) []srcProf {
	seen := map[int]bool{}
	out := []srcProf{}
	next := 1
	for _, p := range in {
		id := p.ID
		if seen[id] {
			for seen[next] {
				next++
			}
			id = next
		}
		seen[id] = true
		q := p
		q.ID = id
		q.Download = sUniq(sHTTP(p.Download))
		q.Upload = sUniq(sHTTP(p.Upload))
		out = append(out, q)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func sFind(id int) (srcProf, bool) {
	for _, p := range srcData.Profiles {
		if p.ID == id {
			return p, true
		}
	}
	return srcProf{}, false
}

// sParseConf parses the app .conf (or any JSON that carries links) into profiles.
func sParseConf(data []byte) ([]srcProf, int) {
	data = []byte(strings.TrimPrefix(string(data), "\ufeff"))
	var raw interface{}
	if json.Unmarshal(data, &raw) != nil {
		return nil, -1
	}
	var imported []srcProf
	sel := -1
	if m, ok := raw.(map[string]interface{}); ok {
		if arr, ok := m["profiles"].([]interface{}); ok {
			for i, it := range arr {
				pm, ok := it.(map[string]interface{})
				if !ok {
					continue
				}
				var dlu, ulu []string
				if v, ok := pm["downloadUrls"]; ok {
					sCollect(v, &dlu)
				}
				if v, ok := pm["uploadUrls"]; ok {
					sCollect(v, &ulu)
				}
				id, has := sInt(pm, "id", "profileId")
				if !has {
					id = i + 1
				}
				name := sStr(pm, "name", "profileName", "title")
				if name == "" {
					name = "配置" + strconv.Itoa(id)
				}
				imported = append(imported, srcProf{ID: id, Name: name, Download: dlu, Upload: ulu})
			}
		}
		if v, ok := sInt(m, "selectedProfileId"); ok {
			sel = v
		}
	}
	if len(imported) == 0 {
		var dlu, ulu []string
		sDeep(raw, &dlu, &ulu)
		dlu, ulu = sUniq(sHTTP(dlu)), sUniq(sHTTP(ulu))
		if len(dlu) == 0 && len(ulu) == 0 {
			return nil, -1
		}
		imported = []srcProf{{ID: 1, Name: "导入配置", Download: dlu, Upload: ulu}}
		sel = 1
	}
	return sMerge(imported), sel
}

// serveBuiltinPool writes the currently active source pool (for clients) as JSON.
func serveBuiltinPool(w http.ResponseWriter) {
	ensureBuiltinPool()
	srcEnsureLoaded()
	srcMu.Lock()
	sel := srcData.Selected
	var d, u []string
	if sel >= 0 {
		if p, ok := sFind(sel); ok {
			d, u = p.Download, p.Upload
		}
	}
	srcMu.Unlock()
	if len(d) == 0 && len(u) == 0 {
		d, u = activeBuiltin()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"download": d, "upload": u})
}

func sList(w http.ResponseWriter) {
	ensureBuiltinPool()
	srcEnsureLoaded()
	srcMu.Lock()
	sel := srcData.Selected
	list := []map[string]interface{}{}
	for _, p := range srcData.Profiles {
		list = append(list, map[string]interface{}{
			"id": p.ID, "name": p.Name,
			"download": p.Download, "upload": p.Upload,
			"dlN": len(p.Download), "ulN": len(p.Upload),
		})
	}
	srcMu.Unlock()
	active := map[string]interface{}{}
	if sel >= 0 {
		for _, it := range list {
			if it["id"] == sel {
				active = map[string]interface{}{"source": "profile", "name": it["name"]}
			}
		}
	}
	if len(active) == 0 {
		active = map[string]interface{}{"source": "builtin", "name": "内置源（客户端默认）"}
	}
	adl, aul := loadAiSources()
	jsonOut(w, map[string]interface{}{
		"selected": sel,
		"builtin":  map[string]interface{}{"download": builtinPool.Download, "upload": builtinPool.Upload, "dlN": len(builtinPool.Download), "ulN": len(builtinPool.Upload)},
		"ai":       map[string]interface{}{"download": adl, "upload": aul, "dlN": len(adl), "ulN": len(aul)},
		"profiles": list,
		"active":   active,
	})
}

func sImport(w http.ResponseWriter, data string) {
	profs, sel := sParseConf([]byte(data))
	if len(profs) == 0 {
		jsonOut(w, map[string]interface{}{"ok": false, "error": "未找到任何下载/上传链接（或不是合法 .conf/JSON）"})
		return
	}
	srcEnsureLoaded()
	srcMu.Lock()
	srcData.Profiles = sMerge(append(srcData.Profiles, profs...))
	if srcData.Selected < 0 {
		if sel >= 0 {
			srcData.Selected = sel
		} else {
			srcData.Selected = profs[0].ID
		}
	}
	snap := append([]srcProf(nil), srcData.Profiles...)
	srcSaveLocked()
	srcMu.Unlock()
	jsonOut(w, map[string]interface{}{"ok": true, "imported": len(profs), "profiles": len(snap)})
}

func sUse(w http.ResponseWriter, id int) {
	srcEnsureLoaded()
	srcMu.Lock()
	if id >= 0 {
		if _, ok := sFind(id); !ok {
			srcMu.Unlock()
			jsonOut(w, map[string]interface{}{"ok": false, "error": "配置档不存在"})
			return
		}
	}
	srcData.Selected = id
	srcSaveLocked()
	srcMu.Unlock()
	jsonOut(w, map[string]interface{}{"ok": true, "selected": id})
}

func sDelete(w http.ResponseWriter, id int) {
	srcEnsureLoaded()
	srcMu.Lock()
	out := srcData.Profiles[:0:0]
	for _, p := range srcData.Profiles {
		if p.ID != id {
			out = append(out, p)
		}
	}
	srcData.Profiles = out
	if srcData.Selected == id {
		srcData.Selected = -1
	}
	srcSaveLocked()
	srcMu.Unlock()
	jsonOut(w, map[string]interface{}{"ok": true, "profiles": len(out)})
}

func sManual(w http.ResponseWriter, dl, ul []string) {
	dl, ul = sUniq(sHTTP(dl)), sUniq(sHTTP(ul))
	srcEnsureLoaded()
	srcMu.Lock()
	found := false
	for i := range srcData.Profiles {
		if srcData.Profiles[i].ID == manualProfID {
			srcData.Profiles[i].Download = dl
			srcData.Profiles[i].Upload = ul
			found = true
		}
	}
	if !found {
		srcData.Profiles = append(srcData.Profiles, srcProf{ID: manualProfID, Name: "手填覆盖", Download: dl, Upload: ul})
	}
	if len(dl) > 0 || len(ul) > 0 {
		srcData.Selected = manualProfID
	}
	srcData.Profiles = sMerge(srcData.Profiles)
	srcSaveLocked()
	srcMu.Unlock()
	jsonOut(w, map[string]interface{}{"ok": true, "downloadUrls": len(dl), "uploadUrls": len(ul)})
}

// adminSources is the authed dispatcher for source maintenance.
func adminSources(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		return
	}
	if !authed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	switch r.Method {
	case http.MethodGet:
		sList(w)
	case http.MethodPost:
		body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		var o struct {
			Action   string   `json:"action"`
			ID       int      `json:"id"`
			Data     string   `json:"data"`
			Kind     string   `json:"kind"`
			URL      string   `json:"url"`
			Download []string `json:"download"`
			Upload   []string `json:"upload"`
		}
		if json.Unmarshal(body, &o) != nil {
			jsonOut(w, map[string]interface{}{"ok": false, "error": "bad json"})
			return
		}
		switch o.Action {
		case "import":
			sImport(w, o.Data)
		case "use":
			sUse(w, o.ID)
		case "delete":
			sDelete(w, o.ID)
		case "reset":
			sUse(w, -1)
		case "manual":
			sManual(w, o.Download, o.Upload)
		case "add_ai":
			addToAi(o.Download, o.Upload)
			jsonOut(w, map[string]interface{}{"ok": true})
		case "delete_ai":
			removeFromAi(o.Kind, o.URL)
			jsonOut(w, map[string]interface{}{"ok": true})
		default:
			jsonOut(w, map[string]interface{}{"ok": false, "error": "unknown action"})
		}
	default:
		http.Error(w, "method not allowed", 405)
	}
}
