// Command bench 逐源实测吞吐：为源表排序与最优并发提供数据。只依赖标准库。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type srcs struct {
	Download []string `json:"download"`
	Upload   []string `json:"upload"`
}

type cw struct {
	net.Conn
	w *int64
}

func (c *cw) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		atomic.AddInt64(c.w, int64(n))
	}
	return n, err
}

func client(buf int, upCount *int64) *http.Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
			c, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			if upCount != nil {
				return &cw{Conn: c, w: upCount}, nil
			}
			return c, nil
		},
		DisableCompression:    true,
		ForceAttemptHTTP2:     false,
		MaxIdleConnsPerHost:   256,
		WriteBufferSize:       buf,
		ReadBufferSize:        1 << 17,
		ResponseHeaderTimeout: 8 * time.Second,
	}
	return &http.Client{Transport: tr}
}

func norm(u string) string {
	if !strings.HasPrefix(u, "http") {
		return "https://" + u
	}
	return u
}

func host(u string) string {
	h := u
	if i := strings.Index(h, "//"); i >= 0 {
		h = h[i+2:]
	}
	if i := strings.Index(h, "/"); i >= 0 {
		h = h[:i]
	}
	return h
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 46 {
		s = s[:46]
	}
	return strings.ReplaceAll(s, "\n", " ")
}

type out struct {
	mbps float64
	code int
	err  string
}

func runDL(url string, n, secs int) out {
	cli := client(1<<20, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(secs)*time.Second)
	defer cancel()
	var cnt int64
	var code int32
	var mu sync.Mutex
	lastErr := ""
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 1<<17)
			for {
				if ctx.Err() != nil {
					return
				}
				rctx, rc := context.WithTimeout(ctx, 20*time.Second)
				req, err := http.NewRequestWithContext(rctx, "GET", url, nil)
				if err != nil {
					rc()
					return
				}
				req.Header.Set("User-Agent", "okhttp/4.12.0")
				req.Header.Set("Accept-Encoding", "identity")
				req.Header.Set("Range", "bytes=0-")
				resp, err := cli.Do(req)
				if err != nil {
					mu.Lock()
					if lastErr == "" {
						lastErr = shortErr(err)
					}
					mu.Unlock()
					rc()
					return
				}
				atomic.StoreInt32(&code, int32(resp.StatusCode))
				got := int64(0)
				for {
					k, rerr := resp.Body.Read(buf)
					if k > 0 {
						got += int64(k)
						atomic.AddInt64(&cnt, int64(k))
					}
					if rerr != nil {
						break
					}
				}
				resp.Body.Close()
				rc()
				if got == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()
	mu.Lock()
	e := lastErr
	mu.Unlock()
	return out{float64(atomic.LoadInt64(&cnt)) * 8 / float64(secs) / 1e6, int(code), e}
}

func runUL(url string, n, secs int, payload []byte) out {
	var cnt int64
	cli := client(16<<10, &cnt)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(secs)*time.Second)
	defer cancel()
	var code int32
	var mu sync.Mutex
	lastErr := ""
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				rctx, rc := context.WithTimeout(ctx, 20*time.Second)
				req, err := http.NewRequestWithContext(rctx, "POST", url, bytes.NewReader(payload))
				if err != nil {
					rc()
					return
				}
				req.Header.Set("User-Agent", "okhttp/4.12.0")
				req.Header.Set("Content-Type", "application/octet-stream")
				req.ContentLength = int64(len(payload))
				resp, err := cli.Do(req)
				if err != nil {
					mu.Lock()
					if lastErr == "" {
						lastErr = shortErr(err)
					}
					mu.Unlock()
					rc()
					if ctx.Err() == nil {
						time.Sleep(150 * time.Millisecond)
					}
					continue
				}
				atomic.StoreInt32(&code, int32(resp.StatusCode))
				io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
				resp.Body.Close()
				rc()
				if ctx.Err() == nil {
					time.Sleep(150 * time.Millisecond)
				}
			}
		}()
	}
	wg.Wait()
	mu.Lock()
	e := lastErr
	mu.Unlock()
	return out{float64(atomic.LoadInt64(&cnt)) * 8 / float64(secs) / 1e6, int(code), e}
}

func main() {
	path := flag.String("src", "D:/qwrt/pml/web/assets/sources.json", "sources.json 路径")
	mode := flag.String("mode", "dl", "dl 或 ul")
	conns := flag.String("conns", "1,4,8", "并发档位，逗号分隔")
	secs := flag.Int("secs", 6, "每档秒数")
	only := flag.Int("only", -1, "只测某个下标")
	flag.Parse()
	b, err := os.ReadFile(*path)
	if err != nil {
		panic(err)
	}
	var s srcs
	if err := json.Unmarshal(b, &s); err != nil {
		panic(err)
	}
	var cs []int
	for _, p := range strings.Split(*conns, ",") {
		var n int
		if _, e := fmt.Sscanf(p, "%d", &n); e == nil && n > 0 {
			cs = append(cs, n)
		}
	}
	list := s.Download
	if *mode == "ul" {
		list = s.Upload
	}
	payload := make([]byte, 8<<20)
	fmt.Printf("== mode=%s secs=%d conns=%v ==\n", *mode, *secs, cs)
	type best struct {
		idx  int
		n    int
		mbps float64
	}
	var all []best
	for i, u := range list {
		if *only >= 0 && i != *only {
			continue
		}
		parts := make([]string, 0, len(cs))
		for _, n := range cs {
			var r out
			if *mode == "ul" {
				r = runUL(norm(u), n, *secs, payload)
			} else {
				r = runDL(norm(u), n, *secs)
			}
			line := fmt.Sprintf("%.1f@%d c%d", r.mbps, n, r.code)
			if r.err != "" {
				line += " " + r.err
			}
			parts = append(parts, line)
			all = append(all, best{i, n, r.mbps})
		}
		fmt.Printf("src#%-2d %-34s %s\n", i, host(u), strings.Join(parts, " | "))
	}
	sort.Slice(all, func(a, b int) bool { return all[a].mbps > all[b].mbps })
	fmt.Println("-- best combos --")
	seen := map[int]bool{}
	for _, v := range all {
		if len(seen) >= 8 {
			break
		}
		if seen[v.idx] {
			continue
		}
		seen[v.idx] = true
		fmt.Printf("  src#%-2d conns=%-2d %7.1f Mbps  %s\n", v.idx, v.n, v.mbps, host(list[v.idx]))
	}
}
