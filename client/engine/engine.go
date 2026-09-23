// Package engine 是三端（Windows exe / 安卓 apk / 网页）共用的打流引擎。
// 只依赖 Go 标准库，不含 syscall/unsafe/embed，以便 gomobile 交叉编译到安卓。
package engine

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 与安卓原版 app 对齐的关键参数：okhttp UA、1MB 分块、Range 从头、长连接复用。
const (
	UA        = "okhttp/4.12.0"
	chunkSize = 1 << 20
	dlBuf     = 1 << 17 // 下行单次读缓冲 128KB：拉满并发时内存≈连接数×该值
	AppSig    = "PAOMAN_SPEED_TEST_CONFIG"
	Version   = "1.0"
)

// 兜底源：内置源文件缺失时使用。
var fallbackDL = []string{
	"https://speed.cloudflare.com/__down?bytes=100000000000",
	"https://mirrors.163.com/ubuntu-releases/22.04/ubuntu-22.04.5-desktop-amd64.iso",
}
var fallbackUL = []string{
	"https://speed.cloudflare.com/__up",
	"https://httpbin.org/post",
	"https://postman-echo.com/post",
}

var (
	CTL           sync.Mutex
	ST            *Stream
	forceV4       int32
	srcMu         sync.Mutex
	baseDL        []string
	baseUL        []string
	curDL         []string
	curUL         []string
	hub           = newHub()
	dataDir       string
	maxPerUR      = 24
	dlReqTTL      = 25 * time.Second // 下行单请求硬超时，卡流即回收
	maxUp         = 160              // 上行总连接数上限，对齐 app 堆并发到吞吐平台
	warmupSec     = 5.0              // 预热期：启动时内核缓冲一次性灌入，头几秒不计峰值
	peakWin       = 15               // 峰值窗口 15 x 200ms = 3s
	curWin        = 5                // 实时窗口 5 x 200ms = 1s
	upBudget      = 6                // app highThroughput 预算：上行每端点并发在此均摊（对齐 app 堆并发到平台）
	upMinWorkers  = 4                // app uploadMinWorkersPerUrl
	upMaxWorkers  = 10               // app uploadMaxWorkersPerUrl
	upPayloadOnce sync.Once
	upPayloadBuf  []byte
	deadAfterMs   = 5000
	dlProbeEvery  = 5
	// app EngineTuningConfig 1:1：每源 worker 在 [min,max] 反馈浮动，高吞吐预算+硬线程上限
	dlMinPerUrl = 12  // app downloadMinWorkersPerUrl
	dlMaxPerUrl = 20  // app downloadMaxWorkersPerUrl
	highBudget  = 32  // app highThroughputWorkerBudget（每源高吞吐目标）
	maxThreads  = 256 // app maxWorkerThreads（全局硬上限，防失控）
	adaptive    = 1   // 掉速紧急补偿 ADD/REMOVE：0 关闭=纯静态，1 开启=复刻 app 反馈
)

// ---- SSE 广播 ----

type hubT struct {
	mu   sync.Mutex
	subs map[chan string]struct{}
}

func newHub() *hubT { return &hubT{subs: map[chan string]struct{}{}} }

func (h *hubT) add() chan string {
	c := make(chan string, 32)
	h.mu.Lock()
	h.subs[c] = struct{}{}
	h.mu.Unlock()
	return c
}

func (h *hubT) remove(c chan string) {
	h.mu.Lock()
	delete(h.subs, c)
	h.mu.Unlock()
}

func (h *hubT) broadcast(s string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.subs {
		select {
		case c <- s:
		default:
		}
	}
}

func emit(v map[string]interface{}) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	hub.broadcast(string(b))
}

// Subscribe 返回实时事件通道与退订函数。
func Subscribe() (chan string, func()) {
	c := hub.add()
	return c, func() { hub.remove(c) }
}

// ---- HTTP 客户端 ----

// newClient 创建打流用 HTTP 客户端。
// follow=true 用于下行（qq/百度/米哈游 等大源靠 302 跳到真实 CDN）；
// follow=false 用于上行（POST 被 302 改写成 GET 会让上行计数归零）。
// countingConn 在 socket 层统计交给内核的字节，避免用户态缓冲造成虚高速率。
type upLocalCtxKey struct{}

var upLocalKey upLocalCtxKey

// countingConn is the app's TrafficStats-equivalent for the upload side. When
// w!=nil and live!=nil this is an upload connection: every byte the kernel accepts
// from it is counted immediately. Kernel send is flow-controlled by the peer's
// advertised window, so the count equals bytes actually put on the wire (no retransmit
// double-count, no buffered-write inflation). Download conns pass live==nil and are
// never counted here, so the download path is untouched.
type countingConn struct {
	net.Conn
	w    *int64
	live *int32
}

func (c *countingConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 && c.w != nil && c.live != nil {
		atomic.AddInt64(c.w, int64(n))
	}
	return n, err
}

// newClient 创建打流客户端；buf 为写缓冲字节数，ulCount 非空时在 socket 层累计发送字节。
// txCounter, when set by the host, supplies cumulative NIC egress bytes used as
// the authoritative upload measure (matches the app TrafficStats primary source).
var txCounter func() uint64

// SetTxCounter wires a platform egress byte counter (e.g. Windows NIC dwOutOctets).
func SetTxCounter(f func() uint64) { txCounter = f }

// rxCounter, when set by the host, supplies cumulative NIC ingress bytes used as
// the authoritative download measure (wire bytes incl TLS/TCP overhead + retransmit),
// so the speedtest page matches the backend real-time NIC monitor.
var rxCounter func() uint64

// SetRxCounter wires a platform ingress byte counter (e.g. Windows NIC dwInOctets).
func SetRxCounter(f func() uint64) { rxCounter = f }

func newClient(follow bool, buf int, ulCount *int64) *http.Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			n := network
			if atomic.LoadInt32(&forceV4) == 1 && (n == "tcp" || n == "") {
				n = "tcp4"
			}
			d := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
			c, err := d.DialContext(ctx, n, dialAddr(addr))
			if err != nil {
				return nil, err
			}
			if ulCount != nil {
				return &countingConn{Conn: c, w: ulCount}, nil
			}
			return c, nil
		},
		DisableCompression: true,
		// 打流必须禁用 HTTP/2：h2 会把同 host 的 N 个逻辑流复用到一条 TCP 上，
		// 并发数瞬间退化并受 h2 流控窗口限制——这是跑不满带宽的主因。
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          0,
		MaxIdleConnsPerHost:   64,
		MaxConnsPerHost:       64, // okhttp maxRequestsPerHost=64 对应上限（本机每源并发<<64，纯安全闸门）
		IdleConnTimeout:       5 * time.Minute,
		WriteBufferSize:       buf,
		ReadBufferSize:        1 << 17,
		ResponseHeaderTimeout: 10 * time.Second,
	}
	c := &http.Client{Transport: tr}
	if !follow {
		c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
	return c
}

// ---- 打流主体 ----

type Stream struct {
	mode       string
	t0         time.Time
	done       chan struct{}
	closeOnce  sync.Once
	ctx        context.Context
	cancel     context.CancelFunc
	dlBytes    int64
	ulBytes    int64
	txBase     uint64
	txUse      bool
	rxBase     uint64
	rxUse      bool
	dlPeak     float64
	ulPeak     float64
	dlConns    int
	ulConns    int
	ulConnsA   int32
	ulRampMax  int32
	wg         sync.WaitGroup
	tickerDone chan struct{}
	dlSlots    []*urlSlot
	ulSlots    []*urlSlot
	rrDl       int64
	rrUl       int64
	dlClient   *http.Client
	ulClient   *http.Client
	spawnSeq   int64
}

type urlSlot struct {
	norm string
	uh   int
	dead int32
	live int32
	okMs int64
	want int32 // 反馈目标并发（app n() 输出）
	cur  int32 // 当前存活 worker 数
}

func (s *Stream) pickLive(slots []*urlSlot, home int, rr *int64) *urlSlot {
	n := len(slots)
	if n == 0 {
		return nil
	}
	c := atomic.AddInt64(rr, 1)
	for k := 0; k < n; k++ {
		i := int((c + int64(k)) % int64(n))
		if i == home {
			continue
		}
		if atomic.LoadInt32(&slots[i].dead) == 0 {
			return slots[i]
		}
	}
	return nil
}

// pickRotate 在全部存活源间轮转（含 home），使各 worker 请求分散到不同 CDN 对象，
// 防止持续打流时单一热对象被 CDN 限速。返回 nil 表示无存活源。
func (s *Stream) pickRotate(slots []*urlSlot, rr *int64) *urlSlot {
	n := len(slots)
	if n == 0 {
		return nil
	}
	c := atomic.AddInt64(rr, 1)
	for k := 0; k < n; k++ {
		i := int((c + int64(k)) % int64(n))
		if atomic.LoadInt32(&slots[i].dead) == 0 {
			return slots[i]
		}
	}
	return nil
}

func (s *Stream) stopAll() {
	s.closeOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		close(s.done)
	})
}

func sleepStop(s *Stream, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-s.done:
		return true
	case <-t.C:
		return false
	}
}

func urlPhase(u string) int {
	h := 0
	for i := 0; i < len(u); i++ {
		h = h*31 + int(u[i])
	}
	if h < 0 {
		h = -h
	}
	return h
}

// dlGap replicates the app deterministic staggered pacing (g5/tw.java:1085):
// gap = (ok?120:cappedErr) + (workerIdx%budget)*60 + hash(iter,idx,url)%60 ms.
// The (idx%budget)*60 phase shifts same-source workers' reconnect instants apart,
// killing the drop-then-recover sawtooth from synchronized reconnection.
// Errors grow only within 120..500ms; there is NO 2s exponential backoff in the app.
func dlGap(workerIdx, uh, iter, consecErr int, success bool) time.Duration {
	const budget = 16
	h := (iter*7919 + workerIdx*65537 + uh*37) & 0x7fffffff
	h %= 60
	base := int64(120)
	if !success {
		base = 120 + int64(consecErr)*60
		if base > 500 {
			base = 500
		}
	}
	gap := base + int64(workerIdx%budget)*60 + int64(h)
	return time.Duration(gap) * time.Millisecond
}

// dlWorker replicates the app download main loop (g5/tw.java 4900-4970): keep-alive
// long connection repeatedly GETs the whole body until tail-trim/EOF/error, then
// replays with staggered pacing. Stability trio:
//  1. tail-trim: if Content-Length>=1MB stop at 0.88*size; unknown/small cap 50MiB
//     (skip the last 12% of each big file where throughput sags = per-file sawtooth cause);
//  2. deterministic staggered replay (dlGap) instead of exponential backoff;
//  3. per-request dlReqTTL hard timeout as a safety reaper for stuck streams.

// dlBufPool 复用下行读缓冲：worker 随 ADD/REMOVE 频繁生灭，池化避免每次新建 128KB 缓冲造成 GC churn。
var dlBufPool = sync.Pool{New: func() any { return make([]byte, dlBuf) }}

func dlWorker(client *http.Client, url string, s *Stream, workerIdx, homeIdx int) {
	defer s.wg.Done()
	buf := dlBufPool.Get().([]byte)
	defer dlBufPool.Put(buf)
	home := s.dlSlots[homeIdx]
	defer atomic.AddInt32(&home.cur, -1)
	iter := 0
	consecErr := 0
	probe := 0
	nextAt := time.Now()
	for {
		select {
		case <-s.done:
			return
		default:
		}
		if d := time.Until(nextAt); d > 0 {
			if sleepStop(s, d) {
				return
			}
		}
		if atomic.LoadInt32(&home.cur) > atomic.LoadInt32(&home.want) {
			return // 目标下调：在请求间隙收摊退出(app REMOVE workers_cleared)
		}
		iter++
		tgt := home
		if atomic.LoadInt32(&home.dead) == 1 {
			probe++
			if probe%dlProbeEvery != 0 {
				if l := s.pickLive(s.dlSlots, homeIdx, &s.rrDl); l != nil {
					tgt = l
				}
			}
		} else if len(s.dlSlots) > 1 {
			// 主动轮换源：避免持续拉取同一热对象被 CDN 限速（>10G 掉速根因）
			if l := s.pickRotate(s.dlSlots, &s.rrDl); l != nil {
				tgt = l
			}
		}
		rctx, rcancel := context.WithTimeout(s.ctx, dlReqTTL)
		req, err := http.NewRequestWithContext(rctx, "GET", tgt.norm, nil)
		if err != nil {
			rcancel()
			consecErr++
			nextAt = time.Now().Add(dlGap(workerIdx, tgt.uh, iter, consecErr, false))
			continue
		}
		req.Header.Set("User-Agent", UA)
		req.Header.Set("Accept-Encoding", "identity")
		req.Header.Set("Range", "bytes=0-")
		req.Header.Set("Priority", "u=0, i") // 复刻 okhttp：最高优先级请求调度
		req.Header.Set("X-Warmup", "1")
		resp, err := client.Do(req)
		if err != nil {
			rcancel()
			consecErr++
			nextAt = time.Now().Add(dlGap(workerIdx, tgt.uh, iter, consecErr, false))
			continue
		}
		if resp.StatusCode >= 400 {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
			resp.Body.Close()
			rcancel()
			consecErr++
			nextAt = time.Now().Add(dlGap(workerIdx, tgt.uh, iter, consecErr, false))
			continue
		}
		consecErr = 0
		atomic.StoreInt64(&tgt.okMs, time.Now().UnixNano()/1e6)
		cl := resp.ContentLength
		tailLimit := int64(50 << 20)
		if cl >= 1<<20 {
			tailLimit = int64(float64(cl) * 0.88)
		}
		got := int64(0)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				got += int64(n)
				atomic.AddInt64(&s.dlBytes, int64(n))
			}
			if rerr != nil {
				break
			}
			if got >= tailLimit {
				break
			}
			select {
			case <-s.done:
				resp.Body.Close()
				rcancel()
				return
			default:
			}
		}
		resp.Body.Close()
		rcancel()
		nextAt = time.Now().Add(dlGap(workerIdx, tgt.uh, iter, 0, true))
	}
}

// newUpClient 上传专用客户端：复刻原 app——ResponseHeaderTimeout=0（长驻流式，服务端读完
// 才回头，绝不在测速窗口内提前掐断连接），大写缓冲使 chunk 更大更少系统调用，keep-alive 复用。
func newUpClient(ulCount *int64) *http.Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			n := network
			if atomic.LoadInt32(&forceV4) == 1 && (n == "tcp" || n == "") {
				n = "tcp4"
			}
			d := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
			cn, err := d.DialContext(ctx, n, dialAddr(addr))
			if err != nil {
				return nil, err
			}
			if ulCount != nil {
				if lp, ok := ctx.Value(upLocalKey).(*int32); ok {
					return &countingConn{Conn: cn, w: ulCount, live: lp}, nil
				}
			}
			return cn, nil
		},
		DisableCompression:    true,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          0,
		MaxIdleConnsPerHost:   64,
		MaxConnsPerHost:       64, // 与 okhttp maxRequestsPerHost=64 对齐
		IdleConnTimeout:       5 * time.Minute,
		WriteBufferSize:       128 * 1024,
		ReadBufferSize:        128 * 1024,
		ResponseHeaderTimeout: 0,
	}
	c := &http.Client{Transport: tr}
	// Do NOT follow redirects: a bare homepage answers 301/302/405 (or, if followed, a
	// GET on its landing page returns a spurious 200). Only a direct 2xx on the upload
	// POST proves the sink drained our body -> verified-live. Unverified sinks stay at 0.
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

var upChunkOnce sync.Once
var upChunkBuf []byte

// upChunk 返回全局复用的 256KB 抗压缩上传块（app 峰值极致模式 payload=262144，填充 i%251）。
const upFileBytes int64 = 1 << 62 // effectively infinite: one chunked POST per worker runs the whole window (matches cw.p while-loop); only ctx-cancel ends it

func upChunk() []byte {
	upChunkOnce.Do(func() {
		upChunkBuf = make([]byte, 256*1024)
		for i := range upChunkBuf {
			upChunkBuf[i] = byte(i % 251)
		}
	})
	return upChunkBuf
}

// chunkStream 是"永不 EOF、直到 ctx 取消"的无限请求体，对应 app 的 cw 写循环：
// 同一块 buffer 反复整块写、贯穿整个测速窗口。字节在 socket 层由 countingConn 计真实发送量，
// 这里只负责持续吐数据、不重复计数。ctx 结束时返回 EOF 让 Go 干净收尾并读响应。
type chunkStream struct {
	buf   []byte
	ctx   context.Context
	sent  int64
	max   int64
	conns *int32    // live upload connection count (per-conn pace share)
	start time.Time // this connection's start, for the Z0 elapsed window
}

func (cs *chunkStream) Read(pb []byte) (int, error) {
	select {
	case <-cs.ctx.Done():
		return 0, io.EOF
	default:
	}
	if cs.sent >= cs.max {
		return 0, io.EOF
	}
	cs.pace()
	c := copy(pb, cs.buf)
	cs.sent += int64(c)
	return c, nil
}

// pace reproduces the app's per-connection send throttle (tw.Z0, tw.java:776): it
// caps this connection's cumulative send to (aggregateTarget / liveConns) * elapsed,
// sleeping 1..120ms when ahead of the pace line. This stops the t0 flood that makes a
// CDN edge absorb several hundred Mbps before it throttles, so the measured upload
// equals the sustained uplink the app reports. Disabled when upPaceMbps == 0.
func (cs *chunkStream) pace() {
	t := atomic.LoadInt64(&upPaceMbps)
	if t <= 0 || cs.conns == nil {
		return
	}
	n := atomic.LoadInt32(cs.conns)
	if n < 1 {
		n = 1
	}
	perConnMbps := float64(t) / float64(n)
	elapsed := time.Since(cs.start).Seconds()
	if elapsed < 0.05 {
		elapsed = 0.05
	}
	allowed := int64(perConnMbps * 1e6 / 8.0 * elapsed)
	if cs.sent <= allowed {
		return
	}
	d := time.Duration(float64(cs.sent-allowed) * 8.0 / (perConnMbps * 1e6) * float64(time.Second))
	if d < time.Millisecond {
		d = time.Millisecond
	}
	if d > 120*time.Millisecond {
		d = 120 * time.Millisecond
	}
	select {
	case <-cs.ctx.Done():
	case <-time.After(d):
	}
}

// hashJitter 复刻 app 确定性错峰抖动：(104729*attempt + urlHash*31) % 50，落在 0..49ms，
// 打散多 worker 同拍重连，避免合成脉冲。
func hashJitter(url string, attempt int) int {
	h := 0
	for i := 0; i < len(url); i++ {
		h = 31*h + int(url[i])
	}
	v := (104729*attempt + h*31) & 0x7fffffff
	return int(v % 50)
}

// upWorker 复刻原 app 上行：每 worker 维持一条 chunked 长驻 POST 流，直到测速窗口结束
// （s.ctx 取消）或连接被服务端切断；失败按线性退避 + 错峰抖动重开。ContentLength=-1 触发
// chunked；显式设 identity/Keep-Alive，绝不等待响应头计时，与 app 语义一致。
// Upload accounting (app TrafficStats model / 方案 A): bytes are counted at the socket
// layer by countingConn.Write, which adds every byte the kernel accepts from an upload
// connection to s.ulBytes. Kernel send is flow-controlled by the peer advertised window,
// so the count equals bytes actually put on the wire (no retransmit double-count, no
// user-space buffered-write inflation). The cold-start send-buffer transient is gated by
// warmup + the robust-plateau estimator in runTicker, not by per-request 2xx. The live
// pointer threaded via upLocalKey only marks a conn as an upload conn (its int value is
// not a health gate); a non-draining fake sink is bounded by the okMs health watchdog and
// attempt backoff in runTicker/upWorker, which stop allocating it budget once it is dead.
func upWorker(client *http.Client, url string, s *Stream, homeIdx int) {
	defer s.wg.Done()
	home := s.ulSlots[homeIdx]
	defer atomic.AddInt32(&home.cur, -1)
	attempt := 0
	probe := 0
	for {
		select {
		case <-s.done:
			return
		default:
		}
		if attempt > 0 {
			a := attempt
			if a > 8 {
				a = 8
			}
			d := time.Duration(a*150+hashJitter(url, attempt)) * time.Millisecond
			if sleepStop(s, d) {
				return
			}
		}
		if atomic.LoadInt32(&home.cur) > atomic.LoadInt32(&home.want) {
			return // 目标下调：收摊退出(app REMOVE)
		}
		tgt := home
		if atomic.LoadInt32(&home.dead) == 1 {
			probe++
			if probe%dlProbeEvery != 0 {
				if l := s.pickLive(s.ulSlots, homeIdx, &s.rrUl); l != nil {
					tgt = l
				}
			}
		}
		rctx := context.WithValue(s.ctx, upLocalKey, &tgt.live)
		req, err := http.NewRequestWithContext(rctx, "POST", tgt.norm, &chunkStream{buf: upChunk(), ctx: s.ctx, max: upFileBytes, conns: &s.ulConnsA, start: time.Now()})
		if err != nil {
			attempt++
			continue
		}
		// App model: okhttp streams the upload as Transfer-Encoding: chunked
		// (no Content-Length) with Expect cleared (no 100-continue). Several CDN/nginx
		// sinks reject a huge declared Content-Length with a premature 413 before ever
		// reading the body; chunked avoids that and lets the stream drain fully.
		req.ContentLength = -1
		req.Header.Set("Expect", "")
		req.Header.Set("User-Agent", UA)
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("Accept-Encoding", "identity")
		req.Header.Set("Priority", "u=0, i") // 复刻 okhttp：最高优先级请求调度
		req.Header.Set("Cache-Control", "no-cache")
		req.Header.Set("Connection", "keep-alive")
		req.Header.Set("X-Upload-Stream", "1")
		req.Header.Set("X-Heat-Up", "1")
		resp, err := client.Do(req)
		if err != nil {
			if s.ctx.Err() != nil {
				return
			}
			attempt++
			if attempt >= 6 {
				atomic.StoreInt32(&tgt.dead, 1)
			}
			continue
		}
		if resp.Body != nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		atomic.StoreInt64(&tgt.okMs, time.Now().UnixNano()/1e6)
		attempt = 0
	}
}

type countReader struct {
	r io.Reader
	n *int64
}

func (c *countReader) Read(p []byte) (int, error) {
	m, err := c.r.Read(p)
	if m > 0 {
		atomic.AddInt64(c.n, int64(m))
	}
	return m, err
}

// avgLast 取窗口尾部 k 个样本的均值。
func avgLast(q []float64, k int) float64 {
	if len(q) == 0 {
		return 0
	}
	if k > len(q) {
		k = len(q)
	}
	var sum float64
	for _, v := range q[len(q)-k:] {
		sum += v
	}
	return sum / float64(k)
}

// avgWin 返回 3 秒滑动均值，用于峰值判定。
func avgWin(q []float64) float64 { return avgLast(q, peakWin) }

func pushSample(q []float64, v float64) []float64 {
	q = append(q, v)
	if len(q) > peakWin {
		q = q[len(q)-peakWin:]
	}
	return q
}

func round1(v float64) float64 {
	return math.Round(v*10) / 10
}

// cumPt 累计字节时间序列的一点（tMs=单调毫秒，cum=累计字节），供 r0 算区间瞬时。
type cumPt struct {
	tMs int64
	cum int64
}

// trimSeq 维护 7000ms 滑动窗口（保留最近窗口 + 至少 2 点）。
func trimSeq(seq []cumPt, nowMs int64, winMs int64) []cumPt {
	for len(seq) > 2 && nowMs-seq[0].tMs > winMs {
		seq = seq[1:]
	}
	return seq
}

// pushCap 定长环形近似：追加并裁到 cap。
func pushCap(q []float64, v float64, cap int) []float64 {
	q = append(q, v)
	if len(q) > cap {
		q = q[len(q)-cap:]
	}
	return q
}

// r0 复刻 app：最近 ~3s 回看的区间瞬时 Mbps，分子=窗口内字节增量，分母=max(winMs,实际跨度)。
// 先以 winMs=2000 求；大跳改 1000 抬升读数。无增长返回 0。
func r0(seq []cumPt, nowMs int64, winMs int64) float64 {
	if len(seq) < 2 {
		return 0
	}
	guard := 0
	for i := range seq {
		if seq[i].tMs >= nowMs-2000 {
			guard = i
			break
		}
	}
	if seq[len(seq)-1].cum <= seq[guard].cum {
		return 0
	}
	w := len(seq) - 1
	for i := range seq {
		if nowMs-seq[i].tMs <= 3000 {
			w = i
			break
		}
	}
	span := seq[len(seq)-1].tMs - seq[w].tMs
	if span < winMs {
		span = winMs
	}
	if span < 1 {
		span = 1
	}
	return float64(seq[len(seq)-1].cum-seq[w].cum) * 8 * 1000 / float64(span) / 1e6
}

// gAlpha 时间归一：把"每秒 α"换算成"每 tick α"（app tw.java:4469）。
func gAlpha(a float64, intervalMs float64) float64 {
	if intervalMs >= 1000 {
		return a
	}
	return 1 - math.Pow(1-a, intervalMs/1000)
}

// bigJump 复刻 app h0：基线>=80Mbps 且 (>=1.18*prev 或 >=prev+120)。
func bigJump(n, prev float64) bool {
	if prev < 80 {
		return false
	}
	return n >= 1.18*prev || n >= prev+120
}

// p0 复刻 app 实时值：滑动窗瞬时 + 自适应 EMA（升快降慢），显示值带降速下限补偿。
// 返回 (显示 cur, 内部 state)。关键：显示用 cur、下轮记忆用 state，二者分离。
func p0(seq []cumPt, nowMs int64, intervalMs float64, prev float64) (float64, float64) {
	r := r0(seq, nowMs, 2000)
	bj := bigJump(r, prev)
	if bj {
		r = r0(seq, nowMs, 1000)
	}
	if r <= 0 {
		return 0, 0
	}
	var state float64
	if prev <= 0 {
		state = r
	} else if r > prev {
		a := 0.55
		if prev >= 80 && r >= prev*1.45 {
			a = 0.88
		} else if bj {
			a = 0.8
		}
		ga := gAlpha(a, intervalMs)
		state = ga*r + (1-ga)*prev
	} else {
		ga := gAlpha(0.22, intervalMs)
		state = ga*r + (1-ga)*prev
	}
	cur := state
	if bj {
		d := 0.72
		if prev < 80 || r < prev*1.45 {
			d = 0.58
		}
		cur = math.Max(state, (1-d)*state+d*r)
	}
	return cur, state
}

// sigmaTrim 复刻 app headline X：尾窗过滤>0 → 总体 σ → 保留 [μ-0.5σ, μ+1.5σ] → 均值。
// 这是"跑满"稳态峰值，天然抑制起步低值与瞬时毛刺/掉速，使曲线呈一条直线。
func sigmaTrim(hist []float64, intervalMs float64) float64 {
	ticksPerSec := 1000.0 / intervalMs
	n := ticksPerSec
	if n < 1 {
		n = 1
	}
	w := int(n) * 301
	if w < 1 {
		w = 1
	}
	if w > 512 {
		w = 512
	}
	if len(hist) > w {
		hist = hist[len(hist)-w:]
	}
	var pts []float64
	for _, x := range hist {
		if x > 0 {
			pts = append(pts, x)
		}
	}
	if len(pts) == 0 {
		return 0
	}
	var mu float64
	for _, x := range pts {
		mu += x
	}
	mu /= float64(len(pts))
	var vr float64
	for _, x := range pts {
		d := x - mu
		vr += d * d
	}
	sd := math.Sqrt(vr / float64(len(pts)))
	lo := mu - 0.5*sd
	hi := mu + 1.5*sd
	var keep []float64
	for _, x := range pts {
		if x >= lo && x <= hi {
			keep = append(keep, x)
		}
	}
	if len(keep) == 0 {
		keep = pts
	}
	var m float64
	for _, x := range keep {
		m += x
	}
	return m / float64(len(keep))
}

// tailF returns the last k samples of q (whole slice if shorter).
func tailF(q []float64, k int) []float64 {
	if len(q) > k {
		return q[len(q)-k:]
	}
	return q
}

// iqrMean returns a symmetric robust central estimate (mean of samples within
// [Q1,Q3]). Used for the upload headline so transient bufferbloat egress spikes
// (retransmit bursts) and deep TCP-window fades are both excluded, matching the
// sustained rate a user perceives. Falls back to sigmaTrim when too few samples.
func iqrMean(hist []float64) float64 {
	var pts []float64
	for _, x := range hist {
		if x > 0 {
			pts = append(pts, x)
		}
	}
	if len(pts) < 4 {
		return sigmaTrim(hist, 200.0)
	}
	sort.Float64s(pts)
	q := func(pp float64) float64 {
		idx := pp * float64(len(pts)-1)
		lo := int(math.Floor(idx))
		hi := int(math.Ceil(idx))
		if lo == hi {
			return pts[lo]
		}
		return pts[lo] + (pts[hi]-pts[lo])*(idx-float64(lo))
	}
	lo := q(0.25)
	hi := q(0.75)
	var m float64
	var c int
	for _, x := range pts {
		if x >= lo && x <= hi {
			m += x
			c++
		}
	}
	if c == 0 {
		return sigmaTrim(hist, 200.0)
	}
	return m / float64(c)
}

// runTicker 复刻原 app 网络波动曲线：每 tick 取累计字节差分 → cum 序列(7s) → p0 自适应 EMA
// 出实时 cur；原始区间瞬时入 hist(512) → sigmaTrim 出稳健峰值 peak（单调不降）；avg=总bytes/时长。
func runTicker(s *Stream) {
	defer close(s.tickerDone)
	const intervalMs = 200.0
	const warmupPeak = 3.0 // 预热门(秒)：起始 socket 发送缓冲一次性灌入(非真实上线)+未进稳态，
	// app 用 TrafficStats(真实上线) 天然规避；PC 侧 socket 计数含积压，故门控峰值/EMA/曲线点。
	pd := atomic.LoadInt64(&s.dlBytes)
	pu := atomic.LoadInt64(&s.ulBytes)
	last := time.Now()
	var seqD, seqU []cumPt
	var histD, histU []float64
	var curHistU, curHistD []float64
	var liveD, liveU []float64
	// liveD/liveU 是实时表显专用样本池，不受 warmup 门控，与喂给峰值的 curHist 分开。
	var stateD, stateU float64
	lastMon := time.Now()
	lastRamp := time.Now()
	for {
		t := time.NewTimer(time.Duration(intervalMs) * time.Millisecond)
		select {
		case <-s.done:
			t.Stop()
			return
		case <-t.C:
		}
		now := time.Now()
		dt := now.Sub(last).Seconds()
		if dt < 1e-6 {
			dt = 1e-6
		}
		cd := atomic.LoadInt64(&s.dlBytes)
		cu := atomic.LoadInt64(&s.ulBytes)
		// Authoritative upload figure = THIS PROCESS only: the sum of bytes our upload
		// connections wrote (cu = s.ulBytes, via countingConn). This mirrors the app TrafficStats
		// getUidTxBytes (per-process). It deliberately does NOT use the whole-NIC dwOutOctets,
		// which on a real machine folds in background/tunnel traffic and inflates upload. The
		// cold-start send-buffer transient is filtered by warmup + the p0 adaptive EMA ratchet.
		// Download authoritative figure = per-connection socket body count (cd =
		// s.dlBytes, loaded above). Reads are bounded by bytes actually received, so there
		// is no upload-style send-buffer inflation, and it avoids the NIC dwInOctets
		// counter that was offset/adapter unreliable and forced download to ~0.
		nowMs := now.UnixNano() / 1e6
		instD := float64(cd-pd) * 8 / dt / 1e6
		instU := float64(cu-pu) * 8 / dt / 1e6
		pd, pu, last = cd, cu, now
		// 实时表显每个 tick 都收，保证流量在跑表针就跟着动。
		liveD = pushCap(liveD, instD, 512)
		liveU = pushCap(liveU, instU, 512)
		seqD = trimSeq(append(seqD, cumPt{nowMs, cd}), nowMs, 7000)
		seqU = trimSeq(append(seqU, cumPt{nowMs, cu}), nowMs, 7000)
		el := now.Sub(s.t0).Seconds()
		if el < 0.1 {
			el = 0.1
		}
		var curD, curU float64
		if el > warmupPeak {
			histD = pushCap(histD, instD, 512)
			histU = pushCap(histU, instU, 512)
			var stD, stU float64
			_, stD = p0(seqD, nowMs, intervalMs, stateD)
			_, stU = p0(seqU, nowMs, intervalMs, stateU)
			stateD, stateU = stD, stU
			// Headline = the SUSTAINED PLATEAU of both directions, computed as a robust central
			// (iqrMean / sigmaTrim) over the post-warmup raw instantaneous samples -- exactly the
			// app's X() over steady TrafficStats samples. The old p0 EMA (fast-up / slow-down)
			// latched the startup value and left the big gauge stuck high for many seconds.
			// Note: on this PC the first ~5s of an upload is a REAL on-wire burst (the NIC egress
			// counter confirms ~700Mbps) as a nearby CDN edge absorbs into its ingest buffer and
			// then throttles to the sustained uplink; the plateau converges onto that sustained
			// rate, so run the test ~15s and the reading settles to the true steady value.
			if el > warmupPeak+2.0 {
				curHistU = pushCap(curHistU, instU, 512)
				curHistD = pushCap(curHistD, instD, 512)
				// Live headline tracks the RECENT sustained rate (trailing ~3s) so it drops off
				// the CDN-edge absorb burst within seconds once the sink throttles, instead of
				// being dragged high by the whole-run average. peak_ul keeps the full-window
				// plateau as the capacity figure.
				curU = iqrMean(tailF(curHistU, 15))
				curD = sigmaTrim(tailF(curHistD, 15), intervalMs)
			}
			// ulPeak = robust capacity over the whole post-warmup window (the §8-validated
			// iqrMean that lands on the sustained uplink ~100). A trailing window was tried
			// and rejected: when a sink stalls at the very end it collapses to the stall rate.
			s.ulPeak = iqrMean(curHistU)
		}
		// 预热期内用短窗实时值兜底出数。原口径要等 warmupPeak+2s 才给 cur，
		// 表现为流量已经跑起来了但仪表盘钉在 0，且三分支各自重置 t0，每个分支开头都复现。
		// sigmaTrim 上界是 mu+1.5sd，能压住上行首个 tick 的 socket 发送缓冲一次性灌入。
		if el <= warmupPeak+2.0 {
			curD = sigmaTrim(tailF(liveD, 10), intervalMs)
			curU = sigmaTrim(tailF(liveU, 10), intervalMs)
		}
		if curD > 0 {
			s.dlPeak = math.Max(s.dlPeak, curD)
		}
		if el > warmupSec+2.0 && now.Sub(lastMon) >= time.Second {
			lastMon = now
			thr := now.UnixNano()/1e6 - int64(deadAfterMs)
			for i := range s.dlSlots {
				if atomic.LoadInt64(&s.dlSlots[i].okMs) < thr {
					atomic.StoreInt32(&s.dlSlots[i].dead, 1)
				} else {
					atomic.StoreInt32(&s.dlSlots[i].dead, 0)
				}
			}
			for i := range s.ulSlots {
				if atomic.LoadInt64(&s.ulSlots[i].okMs) < thr {
					atomic.StoreInt32(&s.ulSlots[i].dead, 1)
				} else {
					atomic.StoreInt32(&s.ulSlots[i].dead, 0)
				}
			}
			// 复刻 app 掉速紧急补偿：源健康度变化后每秒重算 want 并 ADD/REMOVE 迁移并发。
			if adaptive == 1 {
				if s.mode == "download" || s.mode == "bidirectional" {
					setSlotWant(s.dlSlots, dlBudget, dlMaxPerUrl)
					spawnSlots(s, s.dlSlots, true)
					s.dlConns = countSlots(s.dlSlots)
				}
				if s.mode == "upload" || s.mode == "bidirectional" {
					setSlotWant(s.ulSlots, maxUp, upMaxWorkers)
					spawnSlots(s, s.ulSlots, false)
					s.ulConns = countSlots(s.ulSlots)
				}
			}
		}
		// Gradual upload worker ramp (app §4): +1 per-url worker every 400ms until
		// upMaxWorkers. Spread cold-start offered load (avoid a t0 flood that makes a
		// CDN edge throttle and skew the peak) yet reach full concurrency in ~2.4s
		// (was ~6s) so upload 拉起更快、测速下行→上行切换过渡更丝滑。
		if (s.mode == "upload" || s.mode == "bidirectional") && now.Sub(lastRamp) >= 400*time.Millisecond {
			lastRamp = now
			if r := atomic.LoadInt32(&s.ulRampMax); r < int32(upMaxWorkers) {
				setSlotWant(s.ulSlots, maxUp, int(r+1))
				spawnSlots(s, s.ulSlots, false)
				atomic.StoreInt32(&s.ulRampMax, r+1)
				s.ulConns = countSlots(s.ulSlots)
			}
		}
		atomic.StoreInt32(&s.ulConnsA, int32(s.ulConns))
		emit(map[string]interface{}{
			"type":     "tick",
			"mode":     s.mode,
			"t":        round1(el),
			"cur_dl":   round1(curD),
			"cur_ul":   round1(curU),
			"peak_dl":  round1(s.dlPeak),
			"peak_ul":  round1(s.ulPeak),
			"avg_dl":   round1(float64(cd) * 8 / el / 1e6),
			"avg_ul":   round1(float64(cu) * 8 / el / 1e6),
			"dl_bytes": cd,
			"ul_bytes": cu,
		})
	}
}

func normURL(u string) string {
	if strings.HasPrefix(u, "fetch+") {
		u = u[6:]
	}
	if i := strings.IndexByte(u, '#'); i >= 0 {
		u = u[:i]
	}
	return u
}

func sources() ([]string, []string) {
	srcMu.Lock()
	defer srcMu.Unlock()
	d := append([]string(nil), curDL...)
	u := append([]string(nil), curUL...)
	return d, u
}

func filterHTTP(in []string) []string {
	out := []string{}
	for _, u := range in {
		if strings.HasPrefix(u, "http") {
			out = append(out, u)
		}
	}
	return out
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// SetSources 覆盖当前生效的下载/上传源（空列表表示保持原样）。
func SetSources(dl, ul []string) {
	srcMu.Lock()
	if len(dl) > 0 {
		curDL = uniq(filterHTTP(dl))
	}
	if len(ul) > 0 {
		curUL = uniq(filterHTTP(ul))
	}
	srcMu.Unlock()
}

// ResetSources 回到内置源。
func ResetSources() map[string]interface{} {
	srcMu.Lock()
	curDL = append([]string(nil), baseDL...)
	curUL = append([]string(nil), baseUL...)
	srcMu.Unlock()
	ResetProfile()
	dl, ul := sources()
	return map[string]interface{}{"ok": true, "downloadUrls": len(dl), "uploadUrls": len(ul)}
}

// ---- 启停 ----

func stopCurrentLocked() *Stream {
	if ST != nil {
		old := ST
		ST = nil
		old.stopAll()
		old.wg.Wait()
		<-old.tickerDone
		return old
	}
	return nil
}

// setSlotWant 复刻 app g5/tw.java n() 反馈稳态：把总预算在"活源"间均分，每源 clamp 到
// [1,maxW]（app dlMaxWorkersPerUrl/upMaxWorkersPerUrl），死源 want=0——其 worker 在下个
// 请求间隙自然退出，额度随活源数下降自动抬高，实现 app 的 ADD/REMOVE 迁移效果。
func setSlotWant(slots []*urlSlot, budget, maxW int) {
	n := len(slots)
	if n == 0 {
		return
	}
	a := 0
	for i := range slots {
		if atomic.LoadInt32(&slots[i].dead) == 0 {
			a++
		}
	}
	if a == 0 {
		a = 1
	}
	base := budget / a
	if base < 1 {
		base = 1
	}
	if base > maxW {
		base = maxW
	}
	for i := range slots {
		w := int32(base)
		if atomic.LoadInt32(&slots[i].dead) == 1 {
			w = 0
		}
		atomic.StoreInt32(&slots[i].want, w)
	}
}

// spawnSlots 把每个 slot 的存活 worker 补到 want（ADD）；超额靠 worker 顶部 want 检查回收。
func spawnSlots(s *Stream, slots []*urlSlot, isDl bool) int {
	for i := range slots {
		for int(atomic.LoadInt32(&slots[i].cur)) < int(atomic.LoadInt32(&slots[i].want)) {
			atomic.AddInt32(&slots[i].cur, 1)
			s.wg.Add(1)
			idx := int(atomic.AddInt64(&s.spawnSeq, 1))
			if isDl {
				go dlWorker(s.dlClient, slots[i].norm, s, idx, i)
			} else {
				go upWorker(s.ulClient, slots[i].norm, s, i)
			}
		}
	}
	return countSlots(slots)
}

func countSlots(slots []*urlSlot) int {
	total := 0
	for i := range slots {
		total += int(atomic.LoadInt32(&slots[i].cur))
	}
	return total
}

// Start 启动持续打流，mode 为 download/upload/bidirectional，per 为每个源的并发连接数。
func Start(mode string, per int) map[string]interface{} {
	if mode != "download" && mode != "upload" && mode != "bidirectional" {
		mode = "download"
	}
	if per < 1 {
		per = maxPerUR // 客户端不指定即“拉满”：默认打满并发
	}
	if per > maxPerUR {
		per = maxPerUR
	}
	dl, ul := sources()
	CTL.Lock()
	stopCurrentLocked()
	s := &Stream{mode: mode, t0: time.Now(), done: make(chan struct{}), tickerDone: make(chan struct{})}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	ST = s
	dlClient := newClient(true, 1<<20, nil)
	ulClient := newUpClient(&s.ulBytes)
	if txCounter != nil {
		s.txBase = txCounter()
		s.txUse = true
	}
	if rxCounter != nil {
		s.rxBase = rxCounter()
		s.rxUse = true
	}

	nowMs0 := time.Now().UnixNano() / 1e6
	s.dlSlots = make([]*urlSlot, len(dl))
	for i, u := range dl {
		t := normURL(u)
		s.dlSlots[i] = &urlSlot{norm: t, uh: urlPhase(t), okMs: nowMs0}
	}
	s.ulSlots = make([]*urlSlot, len(ul))
	for i, u := range ul {
		t := normURL(u)
		s.ulSlots[i] = &urlSlot{norm: t, uh: urlPhase(t), okMs: nowMs0}
	}
	pinCache.Range(func(k, _ any) bool { pinCache.Delete(k); return true })
	pinAll := make([]string, 0, len(s.dlSlots)+len(s.ulSlots))
	for i := range s.dlSlots {
		pinAll = append(pinAll, s.dlSlots[i].norm)
	}
	for i := range s.ulSlots {
		pinAll = append(pinAll, s.ulSlots[i].norm)
	}
	// 异步预解析固定 IP：worker 立即 spawn，pinCache 在后台 ~1s 内填充；
	// 未命中时 dialAddr 回退普通 DNS 解析，连接照常建立，避免启动死等
	// 所有源的 DNS + 多 IP 源 probeIP（每源最多 1200ms）导致的拉起慢。
	go pinHosts(pinAll, 3*time.Second)
	s.dlClient, s.ulClient = dlClient, ulClient
	nD, nU := 0, 0
	if mode == "download" || mode == "bidirectional" {
		setSlotWant(s.dlSlots, dlBudget, dlMaxPerUrl)
		nD = spawnSlots(s, s.dlSlots, true)
	}
	if mode == "upload" || mode == "bidirectional" {
		// App §4: ramp per-url workers upMinWorkers -> upMaxWorkers over ~2.4s instead of
		// opening every connection at t0. The t0 flood is what makes a CDN edge absorb a
		// multi-hundred-Mbps spike before throttling; a gradual ramp spreads the offered
		// load so upload rises smoothly to the sustained value. The ticker advances
		// ulRampMax by 1 every 400ms and the adaptive health loop maintains upMaxWorkers.
		atomic.StoreInt32(&s.ulRampMax, int32(upMinWorkers))
		setSlotWant(s.ulSlots, maxUp, upMinWorkers)
		nU = spawnSlots(s, s.ulSlots, false)
		atomic.StoreInt32(&s.ulConnsA, int32(nU))
	}
	s.dlConns = nD
	s.ulConns = nU
	go runTicker(s)
	CTL.Unlock()
	emit(map[string]interface{}{"type": "started", "mode": mode, "dl_conns": nD, "ul_conns": nU, "perUrl": per})
	res := map[string]interface{}{"started": true, "mode": mode, "perUrl": per, "dl_sources": 0, "ul_sources": 0, "dl_conns": nD, "ul_conns": nU}
	if mode != "upload" {
		res["dl_sources"] = len(dl)
	}
	if mode != "download" {
		res["ul_sources"] = len(ul)
	}
	return res
}

// Stop 停止打流并返回本次最终结果。
func Stop() map[string]interface{} {
	CTL.Lock()
	s := stopCurrentLocked()
	CTL.Unlock()
	if s == nil {
		return map[string]interface{}{"stopped": false, "reason": "未在运行"}
	}
	el := time.Since(s.t0).Seconds()
	if el < 1e-6 {
		el = 1e-6
	}
	db := atomic.LoadInt64(&s.dlBytes)
	ub := atomic.LoadInt64(&s.ulBytes)
	// Final bytes are this-process socket counts (ub = s.ulBytes, db = s.dlBytes).
	// download final bytes stay the per-connection socket body count (db = s.dlBytes).
	final := map[string]interface{}{
		"mode":     s.mode,
		"seconds":  round1(el),
		"peak_dl":  round1(s.dlPeak),
		"peak_ul":  round1(s.ulPeak),
		"avg_dl":   round1(float64(db) * 8 / el / 1e6),
		"avg_ul":   round1(float64(ub) * 8 / el / 1e6),
		"dl_bytes": db,
		"ul_bytes": ub,
	}
	emit(map[string]interface{}{"type": "stopped", "final": final})
	return map[string]interface{}{"stopped": true, "final": final}
}

// Status 返回运行状态。
func Status() map[string]interface{} {
	CTL.Lock()
	running := ST != nil
	var m string
	var dc, uc int
	if running {
		m = ST.mode
		dc = ST.dlConns
		uc = ST.ulConns
	}
	CTL.Unlock()
	dl, ul := sources()
	return map[string]interface{}{
		"running": running, "mode": m,
		"forceV4": atomic.LoadInt32(&forceV4) == 1,
		"dlConns": dc, "ulConns": uc,
		"dlSources": len(dl), "ulSources": len(ul),
		"version": Version, "engine": "native", "maxPerUrl": maxPerUR,
	}
}

// Info 返回引擎信息与当前源列表。
func Info() map[string]interface{} {
	dl, ul := sources()
	return map[string]interface{}{
		"app": AppSig, "version": Version, "ua": UA,
		"builtinDownload": dl, "builtinUpload": ul,
	}
}

// SetForceV4 切换是否强制走 IPv4。
func SetForceV4(on bool) {
	v := int32(0)
	if on {
		v = 1
	}
	atomic.StoreInt32(&forceV4, v)
}

// ForceV4 返回当前是否强制 IPv4。
func ForceV4() bool { return atomic.LoadInt32(&forceV4) == 1 }

// MaxPerURL 返回单源并发上限。
func MaxPerURL() int { return maxPerUR }

// DataDir 返回可写数据目录（用于持久化导入的配置档）。
func DataDir() string { return dataDir }

// Init 初始化引擎：dataDir 为持久化目录（可为空），dl/ul 为内置源。
func Init(dataDirArg string, dl, ul []string) {
	dataDir = dataDirArg
	if dataDir != "" {
		_ = mkdirAll(dataDir)
	}
	baseDL = uniq(filterHTTP(dl))
	if len(baseDL) == 0 {
		baseDL = fallbackDL
	}
	baseUL = uniq(filterHTTP(ul))
	if len(baseUL) == 0 {
		baseUL = fallbackUL
	}
	LoadBuiltinOverride()
	if builtinFetcher != nil {
		fdl, ful := builtinFetcher()
		srcMu.Lock()
		if d := uniq(filterHTTP(fdl)); len(d) > 0 {
			baseDL = d
		}
		if u := uniq(filterHTTP(ful)); len(u) > 0 {
			baseUL = u
		}
		srcMu.Unlock()
	}
	curDL = append([]string(nil), baseDL...)
	curUL = append([]string(nil), baseUL...)
	loadProfiles()
}

// SortStrings 供上层稳定输出源列表。
func SortStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
