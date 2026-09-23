package main

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// banRec 是一个源的健康记账。Fails 为当前连续失败数，Strike 为累计下架次数，
// BanAt 为最近一次下架时刻，Dead 表示反复腐烂后长期拉黑。
type banRec struct {
	Fails  int   `json:"fails"`
	Strike int   `json:"strike"`
	BanAt  int64 `json:"ban_at"`
}

var (
	statMu    sync.Mutex
	statMap   map[string]*banRec
	statDirty bool
)

func statLoad() {
	if statMap != nil {
		return
	}
	statMap = map[string]*banRec{}
	b, err := os.ReadFile(webStatF)
	if err != nil {
		return
	}
	var onDisk map[string]*banRec
	if json.Unmarshal(b, &onDisk) == nil {
		for k, v := range onDisk {
			if v != nil {
				statMap[k] = v
			}
		}
	}
}

// statSave 临时文件加改名落盘，避免半截写坏；仅在状态变化时执行。
func statSave() {
	if !statDirty {
		return
	}
	b, err := json.Marshal(statMap)
	if err != nil {
		return
	}
	tmp := webStatF + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, webStatF)
		statDirty = false
	}
}

// coolSeconds 让冷却时长随下架次数翻倍：12 小时、24 小时、48 小时……最长约 12 天。
// 这样既不会因为某个运营商或某次区域性抖动就把源永久误杀，
// 又能让真正腐烂的源越来越少被选中。
func coolSeconds(rec *banRec) int64 {
	s := rec.Strike - 1
	if s < 0 {
		s = 0
	}
	if s > 5 {
		s = 5
	}
	return int64(banCooling.Seconds()) << uint(s)
}

// bannedNow 判断是否处于下架期；冷却结束自动放回重试，实现自愈。
func bannedNow(rec *banRec, now int64) bool {
	if rec == nil {
		return false
	}
	if rec.BanAt == 0 {
		return false
	}
	if now-rec.BanAt < coolSeconds(rec) {
		return true
	}
	rec.BanAt = 0
	rec.Fails = 0
	statDirty = true
	return false
}

// filterBanned 从候选池剔除处于下架期的源。
func filterBanned(kind string, in []srcRef, now int64) []srcRef {
	if len(in) == 0 {
		return in
	}
	statMu.Lock()
	defer statMu.Unlock()
	statLoad()
	out := make([]srcRef, 0, len(in))
	for _, r := range in {
		if !bannedNow(statMap[kind+" "+r.URL], now) {
			out = append(out, r)
		}
	}
	statSave()
	return out
}

// statApply 记录一次浏览器回报：成功清零失败数，失败累计到阈值即下架。
func statApply(kind, rawURL string, ok bool) {
	key := kind + " " + rawURL
	statMu.Lock()
	defer statMu.Unlock()
	statLoad()
	rec := statMap[key]
	if rec == nil {
		rec = &banRec{}
		statMap[key] = rec
	}
	now := time.Now().Unix()
	if ok {
		if rec.Fails != 0 {
			rec.Fails = 0
			statDirty = true
		}
		statSave()
		return
	}
	if bannedNow(rec, now) {
		statSave()
		return
	}
	rec.Fails++
	if rec.Fails >= banFails {
		rec.Strike++
		rec.BanAt = now
		rec.Fails = 0
	}
	statDirty = true
	statSave()
}

// rlWin 是每 IP 每分钟计数窗口，用于给公开接口限流。
type rlWin struct {
	N int
	T time.Time
}

func allowHit(table map[string]*rlWin, ip string, cap int) bool {
	w := table[ip]
	now := time.Now()
	if w == nil || now.Sub(w.T) >= time.Minute {
		table[ip] = &rlWin{N: 1, T: now}
		if len(table) > 20000 {
			for k, v := range table {
				if now.Sub(v.T) >= time.Minute {
					delete(table, k)
				}
			}
		}
		return true
	}
	if w.N >= cap {
		return false
	}
	w.N++
	return true
}

var (
	webRlMu  sync.Mutex
	rlSrcWin = map[string]*rlWin{}
	rlStWin  = map[string]*rlWin{}
)
