// 网页访客侧的测速源池下发与众包健康回收。
//
// 为什么需要这一层：网页端上下行全部打第三方 CDN，而清单以前硬编码在 index.html 里，
// 会随时间腐烂（服务端体检显示 23 个源已死 10 个）。更糟的是把静态文件地址当上传点时，
// 对方直接拒绝 POST 并提前断连，浏览器却已经把本地发送缓冲算成"已上传"，
// 于是产出上千 Mbps 的假上行。改为服务端统一下发后形成闭环：
//
//	准入 = 维护器探活结果 source_health.json，只有真收完请求体的源才发布；
//	回收 = 浏览器实时回报，同一源连续 3 次失败才下架（防误杀）；
//	自愈 = 下架 12 小时后自动放回重试，反复腐烂则长期拉黑。
//
// 三个环节都不需要重新发版网页。
package main

import (
	"strings"
	"time"
)

const (
	healthF      = dataDir + "/source_health.json"
	webStatF     = dataDir + "/web_source_stats.json"
	poolTTL      = 30 * time.Second
	healthMaxAge = 48 * time.Hour

	// 众包回收阈值：连续 3 次失败才下架（防误杀），冷却期满自动放回重试。
	banFails   = 3
	banCooling = 12 * time.Hour

	srcReqPerMin  = 60
	statReqPerMin = 180

	maxPubDL = 10
	maxPubUL = 8

	// 探活候选纳入上传池的最低中位速率门槛（Mbps）。
	candULMinMbps = 20.0
)

// healthItem 是维护器探活结果里的一个源条目。
type healthItem struct {
	URL        string  `json:"url"`
	Kind       string  `json:"kind"`
	Healthy    bool    `json:"healthy"`
	OKRounds   int     `json:"ok_rounds"`
	Rounds     int     `json:"rounds"`
	MedianMbps float64 `json:"median_mbps"`
	Cors       bool    `json:"cors"`
}

// healthDoc 是 source_health.json 的整体结构。
type healthDoc struct {
	GeneratedAt int64        `json:"generatedAt"`
	Rounds      int          `json:"rounds"`
	LiveHealthy []healthItem `json:"live_healthy"`
	LiveDead    []healthItem `json:"live_dead"`
	Candidates  []healthItem `json:"candidates"`
}

// srcRef 是下发给网页的单个源：URL + 服务端实测速率 + 是否可跨域确认到达。
type srcRef struct {
	URL  string  `json:"url"`
	Mbps float64 `json:"mbps,omitempty"`
	Cors bool    `json:"cors,omitempty"`
}

// corsKnown 兜底判断：能读到响应的源，前端才按"服务端确认字节"计数；
// 否则退化成按请求往返完成结算。探活文件没写 cors 字段时用它补齐。
func corsKnown(rawURL string) bool {
	for _, h := range []string{"mbd.baidu.com", "speed.cloudflare.com", "httpbin.org", "wegame.gtimg.com", "echo.apifox.com"} {
		if strings.Contains(rawURL, "://"+h) {
			return true
		}
	}
	return false
}
