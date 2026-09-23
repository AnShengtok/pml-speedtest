// DnsAddressOverride replica (g5/u9.java + tw.I0 dnsAddressSelections + tw ManualDnsResolver).
// Pin ONE resolved IP per host for the whole stream so every worker dials the SAME best edge,
// instead of Go per-dial resolution rotating across differently-loaded CDN edges.
// The chosen IP is the lowest-latency one (cheap TCP probe), matching app edge-selection.
package engine

import (
	"context"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var pinCache sync.Map // host -> ip

func hostOf(rawurl string) string {
	s := rawurl
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndexByte(s, '@'); i >= 0 {
		s = s[i+1:]
	}
	if h, _, err := net.SplitHostPort(s); err == nil {
		return h
	}
	return s
}

func portOf(rawurl string) string {
	if strings.HasPrefix(rawurl, "https://") {
		return "443"
	}
	if strings.HasPrefix(rawurl, "http://") {
		return "80"
	}
	return "443"
}

// probeIP dials each candidate ip:port and returns the lowest-latency reachable one.
// Falls back to the first candidate if probing finds nothing reachable.
func probeIP(ips []string, port string, timeout time.Duration) string {
	type r struct {
		ip  string
		rtt time.Duration
	}
	ch := make(chan r, len(ips))
	for _, ip := range ips {
		go func(ip string) {
			d := net.Dialer{Timeout: timeout}
			t0 := time.Now()
			c, err := d.Dial("tcp", net.JoinHostPort(ip, port))
			if err != nil {
				ch <- r{ip: ip, rtt: time.Hour}
				return
			}
			c.Close()
			ch <- r{ip: ip, rtt: time.Since(t0)}
		}(ip)
	}
	best := time.Hour
	out := ips[0]
	for range ips {
		x := <-ch
		if x.rtt < best {
			best = x.rtt
			out = x.ip
		}
	}
	return out
}

func pinHosts(urls []string, timeout time.Duration) {
	type hp struct {
		host string
		port string
	}
	seen := map[string]hp{}
	var wg sync.WaitGroup
	for _, u := range urls {
		h := hostOf(u)
		if h == "" || seen[h] != (hp{}) {
			continue
		}
		seen[h] = hp{host: h, port: portOf(u)}
	}
	for _, e := range seen {
		wg.Add(1)
		go func(e hp) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			a, err := net.DefaultResolver.LookupIPAddr(ctx, e.host)
			if err != nil || len(a) == 0 {
				return
			}
			var ips []string
			v4only := atomic.LoadInt32(&forceV4) == 1
			for _, x := range a {
				if v4only && x.IP.To4() == nil {
					continue
				}
				ips = append(ips, x.IP.String())
			}
			if len(ips) == 0 {
				return
			}
			var chosen string
			if len(ips) == 1 {
				chosen = ips[0]
			} else {
				chosen = probeIP(ips, e.port, 1200*time.Millisecond)
			}
			pinCache.Store(e.host, chosen)
		}(e)
	}
	wg.Wait()
}

func dialAddr(addr string) string {
	h, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if v, ok := pinCache.Load(h); ok {
		return net.JoinHostPort(v.(string), port)
	}
	return addr
}
