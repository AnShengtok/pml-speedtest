//go:build windows

package web

import (
	"encoding/binary"
	"net"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	iphlpapi       = syscall.NewLazyDLL("iphlpapi.dll")
	procGetIfEntry = iphlpapi.NewProc("GetIfEntry")
	nicOverrideName string
	nicOverrideMbps float64
)

// SetNicLabel lets a platform without auto-detection (e.g. Android via WifiManager
// negotiated link speed) publish its NIC label and Mbps into the status payload.
func SetNicLabel(name string, mbps float64) {
	nicOverrideName = name
	nicOverrideMbps = mbps
}

// defaultIface returns the interface index and name carrying the default route by
// opening a UDP socket to a public address and matching the chosen local IP.
func defaultIface() (int, string) {
	c, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return 0, ""
	}
	defer c.Close()
	la, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok {
		return 0, ""
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return 0, ""
	}
	for _, ifi := range ifaces {
		if isVirtualIface(ifi.Name) {
			continue
		}
		addrs, e := ifi.Addrs()
		if e != nil {
			continue
		}
		for _, ad := range addrs {
			n, isNet := ad.(*net.IPNet)
			if isNet && n.IP.Equal(la.IP) {
				return ifi.Index, ifi.Name
			}
		}
	}
	return 0, ""
}

// linkMbps reads MIB_IFROW.dwSpeed (bits per second) for the given interface index.
// wszName[256] occupies bytes 0..511; dwIndex at 512; dwSpeed at 524.
func linkMbps(index int) float64 {
	buf := make([]byte, 856)
	binary.LittleEndian.PutUint32(buf[512:], uint32(index))
	r, _, _ := procGetIfEntry.Call(uintptr(unsafe.Pointer(&buf[0])))
	if r != 0 {
		return 0
	}
	return float64(binary.LittleEndian.Uint32(buf[524:])) / 1e6
}

func nicInfo() (string, float64) {
	if nicOverrideName != "" || nicOverrideMbps > 0 {
		return nicOverrideName, nicOverrideMbps
	}
	idx, name := defaultIface()
	if idx == 0 {
		return name, 0
	}
	return name, linkMbps(idx)
}

// isVirtualIface reports whether an interface name belongs to a virtual/tunnel
// adapter (Tailscale, WireGuard, TAP/TUN, Hyper-V, VM, loopback). defaultIface
// uses it so the physical default-route NIC is chosen and background tunnel
// traffic is not mistaken for the test's own egress/ingress.
func isVirtualIface(name string) bool {
	low := strings.ToLower(name)
	for _, v := range []string{
		"tailscale", "wintun", "wireguard", "tap", "tun", "wg",
		"vethernet", "hyper-v", "vmware", "virtualbox", "loopback", "pseudo",
		"isatap", "zero trust", "bluetooth",
	} {
		if strings.Contains(low, v) {
			return true
		}
	}
	return false
}

// nicAccum tracks a wrap-aware monotonic byte total for one direction on the
// default interface. The interface index is re-resolved (throttled) instead of
// cached forever, so a route change (Wi-Fi to Ethernet, reconnect) is picked up;
// on change the baseline resets so the total does not jump. dwOffset is the
// MIB_IFROW byte offset (dwInOctets=572, dwOutOctets=576).
type nicAccum struct {
	idx    int
	prev   uint32
	acc    uint64
	inited bool
	last   time.Time
}

const nicResolveEvery = 2 * time.Second

func (a *nicAccum) poll(offset int) uint64 {
	if nicOverrideName != "" {
		return 0
	}
	if a.idx == 0 || time.Since(a.last) > nicResolveEvery {
		if idx2, _ := defaultIface(); idx2 != 0 {
			if idx2 != a.idx {
				a.idx = idx2
				a.inited = false
			}
			a.last = time.Now()
		}
	}
	if a.idx == 0 {
		return a.acc
	}
	buf := make([]byte, 856)
	binary.LittleEndian.PutUint32(buf[512:], uint32(a.idx))
	if r, _, _ := procGetIfEntry.Call(uintptr(unsafe.Pointer(&buf[0]))); r != 0 {
		return a.acc
	}
	cur := binary.LittleEndian.Uint32(buf[offset:])
	if !a.inited {
		a.prev = cur
		a.inited = true
		return 0
	}
	if cur >= a.prev {
		a.acc += uint64(cur - a.prev)
	} else {
		a.acc += (1 << 32) - uint64(a.prev) + uint64(cur)
	}
	a.prev = cur
	return a.acc
}

var (
	txAccum nicAccum
	rxAccum nicAccum
)

// NicTxBytes returns a monotonic count of bytes egressed on the default interface
// (MIB_IFROW.dwOutOctets). Now that the per-connection socket count is the
// authoritative upload figure, this is only a one-sided upper-bound cross-check.
func NicTxBytes() uint64 { return txAccum.poll(576) }

// NicRxBytes mirrors NicTxBytes for ingress (dwInOctets=572).
func NicRxBytes() uint64 { return rxAccum.poll(572) }
