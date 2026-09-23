package main

import (
	"encoding/json"
	"os"
	"sort"
	"sync"
	"time"
)

var (
	poolMu      sync.Mutex
	poolAt      time.Time
	poolDL      []srcRef
	poolUL      []srcRef
	poolUpdated int64
)

// buildWebPool 依据探活结果拼出可发布池；探活缺失或过期时退回人工基线，
// 保证网页永远拿得到源，不会出现"点了没反应"。
func buildWebPool() (dl, ul []srcRef, updatedAt int64) {
	// 管理员在后台显式选定的配置优先级最高，视为人工判断结果。
	srcEnsureLoaded()
	srcMu.Lock()
	sel := srcData.Selected
	var pd, pu []string
	if sel >= 0 {
		if p, ok := sFind(sel); ok {
			pd, pu = p.Download, p.Upload
		}
	}
	srcMu.Unlock()

	b, err := os.ReadFile(healthF)
	if err == nil {
		var doc healthDoc
		if json.Unmarshal(b, &doc) == nil && doc.GeneratedAt > 0 &&
			time.Since(time.Unix(doc.GeneratedAt, 0)) < healthMaxAge {
			for _, it := range doc.LiveHealthy {
				ref := srcRef{URL: it.URL, Mbps: it.MedianMbps, Cors: it.Cors || corsKnown(it.URL)}
				// 上行门槛与候选源一致：必须能跨域确认到达且速率达标。服务器探活
				// 通过不等于访客浏览器能连通（两者网络位置不同），这里宁缺毋滥，
				// 腐烂端点交给 /api/sourcestat 的浏览器众包回报去下架。
				if it.Kind == "upload" {
					if ref.Cors && it.MedianMbps >= candULMinMbps {
						ul = append(ul, ref)
					}
					continue
				}
				dl = append(dl, ref)
			}
			// 候选源探活通过即自动纳入，池子才会换血而不只是腐烂。
			// 上传候选门槛更严：必须能跨域确认到达、且速率达标，
			// 否则宁可少一个源，也不要拿不可信的端点产出虚高读数。
			for _, it := range doc.Candidates {
				if !(it.Healthy || it.OKRounds >= 2) {
					continue
				}
				ref := srcRef{URL: it.URL, Mbps: it.MedianMbps, Cors: it.Cors || corsKnown(it.URL)}
				if it.Kind == "upload" {
					if ref.Cors && it.MedianMbps >= candULMinMbps {
						ul = append(ul, ref)
					}
					continue
				}
				dl = append(dl, ref)
			}
			updatedAt = doc.GeneratedAt
			if len(dl) > 0 || len(ul) > 0 {
				return dl, ul, updatedAt
			}
		}
	}

	if len(pd) == 0 && len(pu) == 0 {
		pd, pu = activeBuiltin()
	}
	for _, u := range pd {
		dl = append(dl, srcRef{URL: u, Cors: corsKnown(u)})
	}
	for _, u := range pu {
		ul = append(ul, srcRef{URL: u, Cors: corsKnown(u)})
	}
	return dl, ul, 0
}

// webPool 带 TTL 缓存，避免每个访客请求都读盘解析。
func webPool() (dl, ul []srcRef, updatedAt int64) {
	poolMu.Lock()
	defer poolMu.Unlock()
	if time.Since(poolAt) < poolTTL && (len(poolDL) > 0 || len(poolUL) > 0) {
		return poolDL, poolUL, poolUpdated
	}
	rawD, rawU, upd := buildWebPool()
	now := time.Now().Unix()
	poolDL = filterBanned("download", rawD, now)
	poolUL = filterBanned("upload", rawU, now)
	sortRefs(poolDL)
	sortRefs(poolUL)
	if len(poolDL) > maxPubDL {
		poolDL = poolDL[:maxPubDL]
	}
	if len(poolUL) > maxPubUL {
		poolUL = poolUL[:maxPubUL]
	}
	poolUpdated, poolAt = upd, time.Now()
	return poolDL, poolUL, poolUpdated
}

// sortRefs 让能确认到达的源排前面，其次按服务端实测速率；同档保持原序。
func sortRefs(a []srcRef) {
	sort.SliceStable(a, func(i, j int) bool {
		if a[i].Cors != a[j].Cors {
			return a[i].Cors
		}
		return a[i].Mbps > a[j].Mbps
	})
}

var (
	knownMu  sync.Mutex
	knownMap map[string]bool
	knownAt  time.Time
)

// knownSource 判断回报的 URL 是否在清单里（含已死的），防止有人拿回报接口注入陌生地址。
func knownSource(kind, rawURL string) bool {
	knownMu.Lock()
	defer knownMu.Unlock()
	if time.Since(knownAt) > poolTTL || knownMap == nil {
		m := map[string]bool{}
		if b, err := os.ReadFile(healthF); err == nil {
			var doc healthDoc
			if json.Unmarshal(b, &doc) == nil {
				for _, grp := range [][]healthItem{doc.LiveHealthy, doc.LiveDead, doc.Candidates} {
					for _, it := range grp {
						m[it.Kind+" "+it.URL] = true
					}
				}
			}
		}
		bd, bu := activeBuiltin()
		for _, u := range bd {
			m["download "+u] = true
		}
		for _, u := range bu {
			m["upload "+u] = true
		}
		knownMap, knownAt = m, time.Now()
	}
	return knownMap[kind+" "+rawURL]
}
