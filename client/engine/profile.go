package engine

// 配置档（profiles）：与安卓原版 app 的 .conf 格式互通，
// 支持导入、按档切换、导出分享，并持久化到数据目录。

import (
	"encoding/json"
	"io"
	"os"
	"sort"
	"strings"
)

// Profile 一个测速配置档。
type Profile struct {
	ID       int      `json:"id"`
	Name     string   `json:"name"`
	Download []string `json:"downloadUrls"`
	Upload   []string `json:"uploadUrls"`
}

type store struct {
	Format   string    `json:"format"`
	Version  int       `json:"version"`
	Selected int       `json:"selectedProfileId"`
	Profiles []Profile `json:"profiles"`
}

var (
	profMu     sync_Mutex
	profiles   []Profile
	profSelect int = -1
)

func mkdirAll(p string) error { return os.MkdirAll(p, 0o755) }

func profPath() string {
	if dataDir == "" {
		return ""
	}
	return strings.TrimRight(dataDir, `/\`) + string(os.PathSeparator) + "profiles.json"
}

func loadProfiles() {
	p := profPath()
	if p == "" {
		return
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var s store
	if json.Unmarshal(b, &s) != nil {
		return
	}
	profiles = s.Profiles
	// PC/安卓客户端只统一使用内置源（源切换归管理后台），
	// 因此即便本地持久化过 selected 档，也不覆盖 curDL/curUL，强制回到内置。
	profSelect = -1
}

func saveProfilesLocked() {
	p := profPath()
	if p == "" {
		return
	}
	s := store{Format: AppSig, Version: 2, Selected: profSelect, Profiles: profiles}
	if b, err := json.MarshalIndent(s, "", "  "); err == nil {
		_ = os.WriteFile(p, b, 0o644)
	}
}

// collectURLs 从数组里提取字符串或 {url:...} 形式的链接。
func collectURLs(v interface{}, out *[]string) {
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

// deepFind 兼容非 app 格式的任意 JSON：递归搜集 download/upload 链接。
func deepFind(obj interface{}, dl, ul *[]string) {
	switch t := obj.(type) {
	case map[string]interface{}:
		for _, key := range []string{"downloadUrls", "downloadEndpoints", "download_urls", "download"} {
			if v, ok := t[key]; ok {
				collectURLs(v, dl)
			}
		}
		for _, key := range []string{"uploadUrls", "uploadEndpoints", "upload_urls", "upload"} {
			if v, ok := t[key]; ok {
				collectURLs(v, ul)
			}
		}
		for _, v := range t {
			deepFind(v, dl, ul)
		}
	case []interface{}:
		for _, v := range t {
			deepFind(v, dl, ul)
		}
	}
}

func strField(m map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok {
			return v
		}
	}
	return ""
}

func intField(m map[string]interface{}, keys ...string) (int, bool) {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int(v), true
		case string:
			var n int
			if _, err := fmtSscan(v, &n); err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

// ImportConf 导入 app 导出的 .conf（或含链接的任意 JSON）。
func ImportConf(data []byte) map[string]interface{} {
	data = []byte(strings.TrimPrefix(string(data), "\ufeff"))
	var raw interface{}
	if json.Unmarshal(data, &raw) != nil {
		return map[string]interface{}{"ok": false, "error": "解析失败：不是合法 JSON"}
	}
	var imported []Profile
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
					collectURLs(v, &dlu)
				}
				if v, ok := pm["uploadUrls"]; ok {
					collectURLs(v, &ulu)
				}
				id, hasID := intField(pm, "id", "profileId")
				if !hasID {
					id = i + 1
				}
				name := strField(pm, "name", "profileName", "title")
				if name == "" {
					name = "配置" + itoa(id)
				}
				imported = append(imported, Profile{ID: id, Name: name, Download: uniq(dlu), Upload: uniq(ulu)})
			}
		}
		if v, ok := intField(m, "selectedProfileId"); ok {
			sel = v
		}
	}
	if len(imported) == 0 {
		var dlu, ulu []string
		deepFind(raw, &dlu, &ulu)
		dlu, ulu = uniq(filterHTTP(dlu)), uniq(filterHTTP(ulu))
		if len(dlu) == 0 && len(ulu) == 0 {
			return map[string]interface{}{"ok": false, "error": "未找到任何下载/上传链接"}
		}
		imported = []Profile{{ID: 1, Name: "导入配置", Download: dlu, Upload: ulu}}
		sel = 1
	}
	if sel < 0 || sel > len(imported) {
		sel = imported[0].ID
	}
	profMu.Lock()
	profiles = append(profiles, imported...)
	if len(profiles) == len(imported) {
		profSelect = sel
	}
	// 合并同名/同 ID 档，ID 冲突时重编号
	profiles = mergeProfiles(profiles)
	if len(profiles) > 0 {
		found := false
		for _, p := range profiles {
			if p.ID == sel {
				found = true
			}
		}
		if !found {
			sel = profiles[0].ID
		}
		profSelect = sel
	}
	out := append([]Profile(nil), profiles...)
	saveProfilesLocked()
	profMu.Unlock()
	if err := applyProfile(sel); err != nil {
		return map[string]interface{}{"ok": false, "error": err.Error()}
	}
	dl, ul := sources()
	return map[string]interface{}{
		"ok": true, "imported": len(imported), "profiles": len(out),
		"selected": sel, "downloadUrls": len(dl), "uploadUrls": len(ul),
	}
}

func mergeProfiles(in []Profile) []Profile {
	seen := map[int]bool{}
	out := []Profile{}
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
		q.Download = uniq(filterHTTP(p.Download))
		q.Upload = uniq(filterHTTP(p.Upload))
		out = append(out, q)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func applyProfile(id int) error {
	profMu.Lock()
	var hit *Profile
	for i := range profiles {
		if profiles[i].ID == id {
			hit = &profiles[i]
		}
	}
	if hit == nil {
		profMu.Unlock()
		return errorsNew("配置档不存在: " + itoa(id))
	}
	dl, ul := hit.Download, hit.Upload
	profSelect = id
	saveProfilesLocked()
	profMu.Unlock()
	SetSources(dl, ul)
	return nil
}

// UseProfile 切换到指定配置档并立即生效。
func UseProfile(id int) map[string]interface{} {
	if err := applyProfile(id); err != nil {
		return map[string]interface{}{"ok": false, "error": err.Error()}
	}
	dl, ul := sources()
	return map[string]interface{}{"ok": true, "selected": id, "downloadUrls": len(dl), "uploadUrls": len(ul)}
}

// DeleteProfile 删除一个配置档。
func DeleteProfile(id int) map[string]interface{} {
	profMu.Lock()
	out := profiles[:0:0]
	for _, p := range profiles {
		if p.ID != id {
			out = append(out, p)
		}
	}
	profiles = out
	if profSelect == id {
		profSelect = -1
		if len(profiles) > 0 {
			profSelect = profiles[0].ID
		}
	}
	snap := append([]Profile(nil), profiles...)
	if profSelect >= 0 {
		for _, p := range snap {
			if p.ID == profSelect {
				SetSources(p.Download, p.Upload)
			}
		}
	} else {
		curDL = append([]string(nil), baseDL...)
		curUL = append([]string(nil), baseUL...)
	}
	saveProfilesLocked()
	profMu.Unlock()
	return map[string]interface{}{"ok": true, "profiles": len(snap)}
}

// ResetProfile 取消配置档选择，回到内置源。
func ResetProfile() {
	profMu.Lock()
	profSelect = -1
	profMu.Unlock()
}

// Profiles 列出全部配置档（含当前选中项与源数量）。
func Profiles() map[string]interface{} {
	profMu.Lock()
	snap := append([]Profile(nil), profiles...)
	sel := profSelect
	profMu.Unlock()
	list := []map[string]interface{}{}
	for _, p := range snap {
		list = append(list, map[string]interface{}{
			"id": p.ID, "name": p.Name,
			"downloadUrls": len(p.Download), "uploadUrls": len(p.Upload),
			"download": p.Download, "upload": p.Upload,
		})
	}
	return map[string]interface{}{"selected": sel, "profiles": list}
}

// ExportConf 导出为标准 app .conf（可直接分享/回导入手机 app）。
func ExportConf() []byte {
	profMu.Lock()
	snap := append([]Profile(nil), profiles...)
	sel := profSelect
	profMu.Unlock()
	if len(snap) == 0 {
		dl, ul := sources()
		snap = []Profile{{ID: 1, Name: "当前源", Download: dl, Upload: ul}}
		sel = 1
	}
	s := store{Format: AppSig, Version: 2, Selected: sel, Profiles: snap}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return []byte("{}")
	}
	return b
}

// WriteConfTo 把导出的 .conf 写入指定路径，返回绝对路径。
func WriteConfTo(path string) (string, error) {
	if err := os.WriteFile(path, ExportConf(), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// SaveUploadTo 供安卓/PC 保存分享文件。
func SaveUploadTo(path string, r io.Reader) (int64, error) {
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return io.Copy(f, r)
}
