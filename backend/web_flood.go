package main

// 洪水拦截（紧急）：针对指定 Host（默认 speedtest.example.com 与源站 IP 203.0.113.10）
// 的测速上报，检测"同一 IP 一秒内测速 >= floodPerSec 次"的脚本洪水，直接 429 拦截并记入实时日志，
// 供管理后台右上角实时查看、并在"已拦截异常上报"卡计入。限速判定按 socket 侧 clientIP，防伪造绕过。
// 注：外层雷池 WAF 会把转发给源站的 Host 改写成源站 IP，故必须同时匹配源站 IP，详见 floodHosts 默认值注释。

import (
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// floodPerSec：一秒内允许的测速上报次数，超过即判洪水。真人远达不到。env PML_FLOOD_PER_SEC 可调。
var floodPerSec = envIntGeo("PML_FLOOD_PER_SEC", 20)

// floodHosts：仅对这些 Host 生效（逗号分隔，去端口小写匹配）。env PML_FLOOD_HOSTS 可追加/覆盖。
var floodHosts = func() map[string]bool {
	m := map[string]bool{}
	v := strings.TrimSpace(os.Getenv("PML_FLOOD_HOSTS"))
	if v == "" {
		// 雷池 WAF 会把转发给源站的 Host 改写成源站 IP(203.0.113.10) 且默认不透传
		// X-Forwarded-Host，所以两个都要列：原始域名（直连/透传时命中）+ 源站 IP
		// （经雷池改写时命中）。否则走雷池的洪水在源站永远匹配不到、判定根本不触发。
		v = "speedtest.example.com,203.0.113.10"
	}
	for _, h := range strings.Split(v, ",") {
		h = normHost(h)
		if h != "" {
			m[h] = true
		}
	}
	return m
}()

const floodLogCap = 300 // 实时日志环形上限

var (
	rejectedFlood atomic.Int64
	floodMu       sync.Mutex
	floodWin      = map[string]*fSec{}               // ip -> 当前秒计数
	floodLog      = make([]FloodRec, 0, floodLogCap) // 最近洪水事件（新→旧）
)

type fSec struct {
	sec int64 // Unix 秒
	n   int   // 该秒内命中次数
}

// FloodRec 一条洪水拦截记录。
type FloodRec struct {
	IP   string `json:"ip"`
	Host string `json:"host"`
	Rate int    `json:"rate"` // 触发时该秒累计次数
	Ts   int64  `json:"ts"`
}

func envIntGeo(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// normHost 去端口、去空白、转小写。
func normHost(h string) string {
	h = strings.TrimSpace(strings.ToLower(h))
	if i := strings.LastIndexByte(h, ':'); i > 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return h
}

// hostKey 取请求真实 Host：优先 X-Forwarded-Host 首值（经反代），否则 r.Host。
func hostKey(r *http.Request) string {
	if xfh := r.Header.Get("X-Forwarded-Host"); xfh != "" {
		if i := strings.IndexByte(xfh, ','); i >= 0 {
			xfh = xfh[:i]
		}
		return normHost(xfh)
	}
	return normHost(r.Host)
}

// floodHostMatch 判断该请求是否落在受洪水保护的目标域名上。
func floodHostMatch(r *http.Request) bool {
	if len(floodHosts) == 0 {
		return false
	}
	return floodHosts[hostKey(r)]
}

// floodCheck 记录一次该 IP 本秒的测速上报，返回是否应拦截（>= 阈值）。
// 命中阈值的那一秒仅记一条日志（避免每请求刷屏），rejectedFlood 每个被拦请求 +1。
func floodCheck(ip, host string) bool {
	if ip == "" {
		return false
	}
	// 内网/回环不参与洪水拦截（本机自测、健康检查等）。
	if x := net.ParseIP(ip); x != nil && (x.IsLoopback() || x.IsPrivate() || x.IsLinkLocalUnicast()) {
		return false
	}
	now := time.Now()
	curSec := now.Unix()
	floodMu.Lock()
	w := floodWin[ip]
	if w == nil || w.sec != curSec {
		w = &fSec{sec: curSec, n: 0}
		floodWin[ip] = w
	}
	w.n++
	n := w.n
	// 粗略回收，防 map 随攻击 IP 无限增长。
	if len(floodWin) > 20000 {
		for k, v := range floodWin {
			if curSec-v.sec >= 60 {
				delete(floodWin, k)
			}
		}
	}
	blocked := n >= floodPerSec
	if blocked {
		rejectedFlood.Add(1)
		if n == floodPerSec { // 该秒首次越阈：记一条日志
			floodLog = append([]FloodRec{{IP: ip, Host: host, Rate: n, Ts: now.Unix()}}, floodLog...)
			if len(floodLog) > floodLogCap {
				floodLog = floodLog[:floodLogCap]
			}
		}
	}
	floodMu.Unlock()
	return blocked
}

// floodSnapshot 返回累计拦截数与最近日志副本（新→旧），供后台实时展示。
func floodSnapshot() (int64, []FloodRec) {
	floodMu.Lock()
	defer floodMu.Unlock()
	out := make([]FloodRec, len(floodLog))
	copy(out, floodLog)
	return rejectedFlood.Load(), out
}

// adminFlood 轻量实时接口（不读事件文件），后台右上角每 ~3s 轮询。
func adminFlood(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if !authed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	n, log := floodSnapshot()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	jsonOut(w, map[string]interface{}{"rejectedFlood": n, "flood": log})
}
