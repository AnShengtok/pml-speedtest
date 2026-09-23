//go:build !windows

package web

import (
	"os"
	"strconv"
	"strings"
	"sync"
)

var (
	nicOverrideName string
	nicOverrideMbps float64
)

// SetNicLabel lets a platform (e.g. Android via WifiManager negotiated link speed)
// publish its NIC label and Mbps into the status payload.
func SetNicLabel(name string, mbps float64) {
	nicOverrideName = name
	nicOverrideMbps = mbps
}

// nicInfo reports the active NIC label and link speed (Mbps, 0 if unknown).
// Auto-detection is Windows-only; other platforms rely on SetNicLabel.
func nicInfo() (string, float64) {
	return nicOverrideName, nicOverrideMbps
}

// devMu guards the cumulative counters below. On Android/Linux the engine reads
// both directions once per tick, so we take a single /proc/net/dev snapshot and
// advance both accumulators together for consistency.
var (
	devMu     sync.Mutex
	devRxPrev uint64
	devTxPrev uint64
	devRxAcc  uint64
	devTxAcc  uint64
	devInited bool
)

// procNetDev sums cumulative rx/tx bytes across all interfaces except loopback,
// which is exactly what Android TrafficStats.getTotalRxBytes/TxBytes report. This
// is the authoritative on-wire measure the original app trusted, so the speedtest
// no longer inflates upload via socket-write buffering.
func procNetDev() (rx uint64, tx uint64, ok bool) {
	b, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		name := strings.TrimSpace(line[:i])
		if name == "lo" {
			continue
		}
		f := strings.Fields(line[i+1:])
		if len(f) < 9 {
			continue
		}
		r, e1 := strconv.ParseUint(f[0], 10, 64)
		t, e2 := strconv.ParseUint(f[8], 10, 64)
		if e1 == nil {
			rx += r
		}
		if e2 == nil {
			tx += t
		}
		ok = true
	}
	return
}

// snapshot advances the accumulators with the delta since the previous call and
// returns the running cumulative totals (monotonic, never decreasing).
func snapshot() (rxAcc, txAcc uint64) {
	rx, tx, ok := procNetDev()
	if !ok {
		devMu.Lock()
		rxAcc, txAcc = devRxAcc, devTxAcc
		devMu.Unlock()
		return
	}
	devMu.Lock()
	defer devMu.Unlock()
	if !devInited {
		devRxPrev, devTxPrev = rx, tx
		devInited = true
		return devRxAcc, devTxAcc
	}
	if rx >= devRxPrev {
		devRxAcc += rx - devRxPrev
	}
	if tx >= devTxPrev {
		devTxAcc += tx - devTxPrev
	}
	devRxPrev, devTxPrev = rx, tx
	return devRxAcc, devTxAcc
}

// NicTxBytes reports cumulative NIC egress bytes (real on-wire upload) so the
// engine can prefer it over socket-write counting, which inflates the figure.
func NicTxBytes() uint64 {
	_, tx := snapshot()
	return tx
}

// NicRxBytes reports cumulative NIC ingress bytes (real on-wire download),
// mirroring NicTxBytes.
func NicRxBytes() uint64 {
	rx, _ := snapshot()
	return rx
}
