package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// webSources 给网页访客下发源池。清单本身不含私密信息，因此不要求站点密钥，
// 但做每 IP 限流，并允许浏览器缓存 30 秒削峰。
func webSources(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	webRlMu.Lock()
	pass := allowHit(rlSrcWin, clientIP(r), srcReqPerMin)
	webRlMu.Unlock()
	if !pass {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	dl, ul, upd := webPool()
	w.Header().Set("Cache-Control", "public, max-age=30")
	jsonOut(w, map[string]interface{}{"download": dl, "upload": ul, "updatedAt": upd})
}

// webSourceStat 接收浏览器回报的源健康。只接受清单内已知 URL，避免被用来投毒。
func webSourceStat(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	webRlMu.Lock()
	pass := allowHit(rlStWin, clientIP(r), statReqPerMin)
	webRlMu.Unlock()
	if !pass {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 512))
	var o struct {
		Kind string `json:"kind"`
		URL  string `json:"url"`
		OK   bool   `json:"ok"`
	}
	if json.Unmarshal(body, &o) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if (o.Kind != "download" && o.Kind != "upload") || !strings.HasPrefix(o.URL, "https://") {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !knownSource(o.Kind, o.URL) {
		http.Error(w, "unknown source", http.StatusNotFound)
		return
	}
	statApply(o.Kind, o.URL, o.OK)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// ---- 同源上行落点保护 ----
// 源站只作为"可确认到达"的兜底上行落点，必须假定会被大量访客同时打满，
// 所以设两道闸：全局在途并发上限，以及单访客每分钟字节配额。
// 超限时直接回 503/429，让前端立刻轮换到外部 CDN，而不是把源站拖垮。

type ulWinRec struct {
	B int64
	T time.Time
}

const (
	ulQuotaPerMin = 256 << 20
	ulMaxInflight = 32
)

var (
	ulMu       sync.Mutex
	ulUse      = map[string]*ulWinRec{}
	ulInflight int
)

// ulAcquire 返回本次可用的字节上限与被拒状态码（0 表示放行）。
func ulAcquire(ip string) (int64, int) {
	ulMu.Lock()
	defer ulMu.Unlock()
	now := time.Now()
	w := ulUse[ip]
	if w == nil || now.Sub(w.T) >= time.Minute {
		w = &ulWinRec{T: now}
		ulUse[ip] = w
	}
	if len(ulUse) > 20000 {
		for k, v := range ulUse {
			if now.Sub(v.T) >= time.Minute {
				delete(ulUse, k)
			}
		}
	}
	if ulInflight >= ulMaxInflight {
		return 0, http.StatusServiceUnavailable
	}
	left := int64(ulQuotaPerMin) - w.B
	if left <= 0 {
		return 0, http.StatusTooManyRequests
	}
	ulInflight++
	return left, 0
}

// ulRelease 归还并发额度，并把实际收到的字节计入该访客配额。
func ulRelease(ip string, n int64) {
	ulMu.Lock()
	defer ulMu.Unlock()
	if ulInflight > 0 {
		ulInflight--
	}
	if w := ulUse[ip]; w != nil {
		w.B += n
	}
}

// ulBusy 在源站被超额使用时给出可被前端识别的拒绝码，配合前端轮换外部 CDN。
func ulBusy(w http.ResponseWriter, code int) {
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "upstream quota", code)
}
