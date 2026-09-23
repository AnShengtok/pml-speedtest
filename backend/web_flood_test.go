package main

import (
	"net/http/httptest"
	"testing"
)

func TestFloodCheckThreshold(t *testing.T) {
	old := floodPerSec
	floodPerSec = 5
	defer func() { floodPerSec = old }()
	floodMu.Lock()
	floodWin = map[string]*fSec{}
	floodLog = floodLog[:0]
	floodMu.Unlock()
	rejectedFlood.Store(0)

	ip := "8.8.8.8"
	for i := 1; i <= 8; i++ {
		blocked := floodCheck(ip, "speedtest.example.com")
		wantBlocked := i >= 5
		if blocked != wantBlocked {
			t.Fatalf("第%d次 blocked=%v 期望 %v", i, blocked, wantBlocked)
		}
	}
	n, log := floodSnapshot()
	if n != 4 { // 第 5..8 次被拦
		t.Fatalf("rejectedFlood=%d 期望 4", n)
	}
	if len(log) != 1 {
		t.Fatalf("flood 日志条数=%d 期望 1（每秒每 IP 仅一条）", len(log))
	}
	if log[0].Rate != 5 || log[0].IP != ip {
		t.Fatalf("日志内容异常: %+v", log[0])
	}
}

func TestFloodPrivateIPExempt(t *testing.T) {
	old := floodPerSec
	floodPerSec = 3
	defer func() { floodPerSec = old }()
	floodMu.Lock()
	floodWin = map[string]*fSec{}
	floodMu.Unlock()
	for i := 0; i < 10; i++ {
		if floodCheck("127.0.0.1", "speedtest.example.com") {
			t.Fatal("回环 IP 不应被洪水拦截")
		}
	}
}

func TestHostKeyAndMatch(t *testing.T) {
	if got := normHost("SpeedTest.EXAMPLE.COM:8443"); got != "speedtest.example.com" {
		t.Fatalf("normHost=%q", got)
	}
	r := httptest.NewRequest("POST", "/report", nil)
	r.Host = "speedtest.example.com"
	if !floodHostMatch(r) {
		t.Fatal("目标域名应命中洪水保护")
	}
	r2 := httptest.NewRequest("POST", "/report", nil)
	r2.Host = "other.example.com"
	if floodHostMatch(r2) {
		t.Fatal("非目标域名不应命中")
	}
	r3 := httptest.NewRequest("POST", "/report", nil)
	r3.Header.Set("X-Forwarded-Host", "speedtest.example.com, proxy.internal")
	if !floodHostMatch(r3) {
		t.Fatal("X-Forwarded-Host 首值应命中")
	}
}
