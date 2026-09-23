package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 端到端验证 report 里的洪水拦截接线：目标域名 + 公网 XFF，一秒内越阈后返回 429 并记日志。
func TestReportFloodWiring(t *testing.T) {
	oldPer := floodPerSec
	floodPerSec = 3
	defer func() { floodPerSec = oldPer }()
	floodMu.Lock()
	floodWin = map[string]*fSec{}
	floodLog = floodLog[:0]
	floodMu.Unlock()
	rejectedFlood.Store(0)

	body := `{"type":"visit","isp":"TestISP","lat":1,"lon":1}`
	doPost := func() int {
		r := httptest.NewRequest(http.MethodPost, "/report", strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:5555" // 可信代理(回环)
		r.Header.Set("X-Forwarded-For", "9.9.9.9")
		r.Header.Set("X-Pml-Key", siteKey)
		r.Host = "speedtest.example.com"
		w := httptest.NewRecorder()
		report(w, r)
		return w.Code
	}
	// 前 2 次放行(200)，第 3 次起 429
	if c := doPost(); c != http.StatusOK {
		t.Fatalf("第1次应放行，got %d", c)
	}
	if c := doPost(); c != http.StatusOK {
		t.Fatalf("第2次应放行，got %d", c)
	}
	for i := 3; i <= 6; i++ {
		if c := doPost(); c != http.StatusTooManyRequests {
			t.Fatalf("第%d次应 429，got %d", i, c)
		}
	}
	n, log := floodSnapshot()
	if n != 4 {
		t.Fatalf("rejectedFlood=%d 期望 4", n)
	}
	if len(log) != 1 || log[0].IP != "9.9.9.9" {
		t.Fatalf("洪水日志异常: %+v", log)
	}
}

// 非目标域名不应触发洪水拦截（走原有 allow 令牌桶）。
func TestReportFloodHostGate(t *testing.T) {
	oldPer := floodPerSec
	floodPerSec = 2
	defer func() { floodPerSec = oldPer }()
	floodMu.Lock()
	floodWin = map[string]*fSec{}
	floodMu.Unlock()
	body := `{"type":"visit","isp":"TestISP","lat":1,"lon":1}`
	for i := 0; i < 5; i++ {
		r := httptest.NewRequest(http.MethodPost, "/report", strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:5555"
		r.Header.Set("X-Forwarded-For", "9.9.9.10")
		r.Header.Set("X-Pml-Key", siteKey)
		r.Host = "some-other-domain.com" // 非目标域名
		w := httptest.NewRecorder()
		report(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("非目标域名第%d次应放行(未触发洪水)，got %d", i+1, w.Code)
		}
	}
}


// 雷池改写场景（回归）：外层 WAF 把 Host 改成源站 IP(203.0.113.10)、真实客户端 IP 落在
// X-Forwarded-For。修复前 §92 因 Host 不匹配永不触发；此测试锁定修复后仍能按真实 IP 拦截。
func TestReportFloodBehindWaf(t *testing.T) {
	oldPer := floodPerSec
	floodPerSec = 3
	defer func() { floodPerSec = oldPer }()
	floodMu.Lock()
	floodWin = map[string]*fSec{}
	floodLog = floodLog[:0]
	floodMu.Unlock()
	rejectedFlood.Store(0)

	body := `{"type":"visit","isp":"TestISP","lat":1,"lon":1}`
	doPost := func() int {
		r := httptest.NewRequest(http.MethodPost, "/report", strings.NewReader(body))
		r.RemoteAddr = "203.0.113.11:5555"          // 雷池出口(可信代理)
		r.Header.Set("X-Forwarded-For", "8.8.8.8")   // 真实攻击 IP
		r.Header.Set("X-Pml-Key", siteKey)
		r.Host = "203.0.113.10"                    // 雷池改写后的 Host
		w := httptest.NewRecorder()
		report(w, r)
		return w.Code
	}
	if c := doPost(); c != http.StatusOK {
		t.Fatalf("第1次应放行，got %d", c)
	}
	if c := doPost(); c != http.StatusOK {
		t.Fatalf("第2次应放行，got %d", c)
	}
	for i := 3; i <= 6; i++ {
		if c := doPost(); c != http.StatusTooManyRequests {
			t.Fatalf("雷池改写 Host 第%d次应 429，got %d", i, c)
		}
	}
	n, log := floodSnapshot()
	if n != 4 {
		t.Fatalf("rejectedFlood=%d 期望 4", n)
	}
	if len(log) != 1 || log[0].IP != "8.8.8.8" {
		t.Fatalf("洪水日志应按真实攻击 IP 记录: %+v", log)
	}
}
