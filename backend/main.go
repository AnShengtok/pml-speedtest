package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
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

//go:embed admin.html
var adminHTML []byte

//go:embed builtin_baseline.json
var builtinBaseline []byte

const (
	dataDir    = "/var/lib/pml-geo"
	evFile     = "/var/lib/pml-geo/events.jsonl"
	secretF    = "/var/lib/pml-geo/secret"
	geoF       = "/var/lib/pml-geo/geoip.json"
	vendorDir  = "/srv/speedtest/vendor"
	siteKey    = "pml-web-2026"
	cookieNm   = "pml_admin"
	pubAddr    = "127.0.0.1:8085"
	admAddr    = ":8086"
	admPubAddr = "127.0.0.1:8087"
)

var (
	mu           sync.Mutex
	secret       []byte
	geoMu        sync.Mutex
	geoCache     = map[string]geoRec{}
	tsNet        *net.IPNet
	adminUser    = "root"
	adminPass    = ""
	rejectedRate atomic.Int64
	rejectedBad  atomic.Int64
	// trustedProxies：源站前面可信的反向代理出口（CIDR）。
	// 只有当直连对端(RemoteAddr)命中这些网段时，才相信 X-Forwarded-For / CF-Connecting-IP。
	trustedProxies []*net.IPNet
)

func init() {
	_, tsNet, _ = net.ParseCIDR("100.64.0.0/10")
	// 默认可信反代出口：example.com 的 openresty egress。可用 PML_TRUSTED_PROXIES 覆盖/追加（逗号分隔 CIDR）。
	def := "203.0.113.11/32"
	if env := strings.TrimSpace(os.Getenv("PML_TRUSTED_PROXIES")); env != "" {
		def = def + "," + env
	}
	for _, c := range strings.Split(def, ",") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if _, n, err := net.ParseCIDR(c); err == nil {
			trustedProxies = append(trustedProxies, n)
		} else if ip := net.ParseIP(c); ip != nil {
			oness := 32
			if ip.To4() == nil {
				oness = 128
			}
			trustedProxies = append(trustedProxies, &net.IPNet{IP: ip, Mask: net.CIDRMask(oness, oness*8)})
		}
	}
}

type geoRec struct {
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
	Country string  `json:"country"`
	CC      string  `json:"cc"`
	ISP     string  `json:"isp"`
	ASN     int     `json:"asn"`
	ASNOrg  string  `json:"asn_org"`
	Org     string  `json:"org"`
	City    string  `json:"city"`
	Region  string  `json:"region"`
}

func cors(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Content-Type,Range,X-Pml-Key")
	h.Set("Access-Control-Max-Age", "86400")
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// isTrustedProxy：内网/回环属内部跳点（如 Caddy 本地反代），或命中显式配置的可信反代出口。
func isTrustedProxy(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() {
		return true
	}
	for _, n := range trustedProxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP：大厂/开源通行的“可信代理链右起剥离”算法（对齐 nginx real_ip_recursive、
// realclientip-go RightmostTrusted、Cloudflare CF-Connecting-IP）。
//  1. 直连对端不是可信代理时，只信 socket，忽略可伪造的头（防客户端塞 XFF 冒充）。
//  2. 在可信反代后面：优先 CF-Connecting-IP（Cloudflare 会清洗客户端伪造值，单值最真）。
//  3. 否则从右往左扫描 XFF，返回第一个“非可信代理”的地址（最接近真实访客的一跳）。
//  4. XFF 全为可信代理（反代未透传真实 IP）时，退到最左值，行为不回退。
func clientIP(r *http.Request) string {
	remote := net.ParseIP(remoteIP(r))
	if remote == nil || !isTrustedProxy(remote) {
		return remoteIP(r)
	}
	if cf := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); cf != "" {
		if ip := net.ParseIP(cf); ip != nil && !isTrustedProxy(ip) {
			return ip.String()
		}
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			ip := net.ParseIP(strings.TrimSpace(parts[i]))
			if ip != nil && !isTrustedProxy(ip) {
				return ip.String()
			}
		}
		if ip := net.ParseIP(strings.TrimSpace(parts[0])); ip != nil && !ip.IsUnspecified() {
			return ip.String()
		}
	}
	if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
		if ip := net.ParseIP(xri); ip != nil && !isTrustedProxy(ip) {
			return ip.String()
		}
	}
	return remoteIP(r)
}

func tsOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			h.ServeHTTP(w, r)
			return
		}
		ip := net.ParseIP(remoteIP(r))
		if ip == nil || tsNet == nil || !tsNet.Contains(ip) {
			http.Error(w, "forbidden: tailscale-only", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func loadSecret() {
	if b, err := os.ReadFile(secretF); err == nil && len(b) >= 16 {
		secret = b
		return
	}
	b := make([]byte, 32)
	rand.Read(b)
	os.WriteFile(secretF, b, 0o600)
	secret = b
}

func want() []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("pml-admin-v1"))
	return m.Sum(nil)
}

func token() string { return hex.EncodeToString(want()) }

func authed(r *http.Request) bool {
	if ip := net.ParseIP(remoteIP(r)); ip != nil && tsNet != nil && tsNet.Contains(ip) {
		return true
	}
	c, err := r.Cookie(cookieNm)
	if err != nil {
		return false
	}
	got, e := hex.DecodeString(c.Value)
	return e == nil && hmac.Equal(got, want())
}

func loadGeoCache() {
	b, err := os.ReadFile(geoF)
	if err != nil {
		return
	}
	json.Unmarshal(b, &geoCache)
}

func saveGeoCache() {
	b, _ := json.Marshal(geoCache)
	os.WriteFile(geoF, b, 0o644)
}

func isPrivate(ip string) bool {
	return ip == "" || ip == "127.0.0.1" || ip == "::1" ||
		strings.HasPrefix(ip, "10.") || strings.HasPrefix(ip, "192.168.") ||
		strings.HasPrefix(ip, "172.16.") || strings.HasPrefix(ip, "100.") ||
		strings.HasPrefix(ip, "fe80")
}

func lookupGeo(ip string) geoRec {
	geoMu.Lock()
	if v, ok := geoCache[ip]; ok {
		geoMu.Unlock()
		return v
	}
	geoMu.Unlock()
	if isPrivate(ip) {
		return geoRec{}
	}
	var g geoRec
	c := &http.Client{Timeout: 4 * time.Second}
	resp, err := c.Get("https://api.ip.sb/geoip/" + ip)
	if err == nil {
		defer resp.Body.Close()
		var r struct {
			Country  string  `json:"country"`
			CC       string  `json:"country_code"`
			Lat      float64 `json:"latitude"`
			Lon      float64 `json:"longitude"`
			ISP      string  `json:"isp"`
			ASN      int     `json:"asn"`
			ASNOrg   string  `json:"asn_organization"`
			Org      string  `json:"organization"`
			City     string  `json:"city"`
			Region   string  `json:"region"`
		}
		json.NewDecoder(resp.Body).Decode(&r)
		g = geoRec{Lat: r.Lat, Lon: r.Lon, Country: r.Country, CC: r.CC, ISP: r.ISP, ASN: r.ASN, ASNOrg: r.ASNOrg, Org: r.Org, City: r.City, Region: r.Region}
	}
	if g.City == "" && g.ISP == "" {
		if fb := lookupIPAPI(ip); fb.City != "" || fb.ISP != "" {
			if g.Lat == 0 && g.Lon == 0 {
				g = fb
			} else {
				if g.City == "" { g.City = fb.City }
				if g.Region == "" { g.Region = fb.Region }
				if g.ISP == "" { g.ISP = fb.ISP }
				if g.Country == "" { g.Country = fb.Country }
				if g.CC == "" { g.CC = fb.CC }
			}
		}
	}
	if g.City != "" || g.ISP != "" {
		geoMu.Lock()
		geoCache[ip] = g
		saveGeoCache()
		geoMu.Unlock()
	}
	return g
}

// lookupIPAPI ip.sb 降级（缺 city+isp，常见于阿里云出口被限流）时的兜底源；
// lang=zh-CN 直接返回中文国家/省/市，as 字段在 isp 缺失时补网络运营者。
func lookupIPAPI(ip string) geoRec {
	var g geoRec
	c := &http.Client{Timeout: 4 * time.Second}
	resp, err := c.Get("http://ip-api.com/json/" + ip + "?lang=zh-CN&fields=status,country,countryCode,regionName,city,isp,as")
	if err != nil {
		return g
	}
	defer resp.Body.Close()
	var r struct {
		Status  string `json:"status"`
		Country string `json:"country"`
		CC      string `json:"countryCode"`
		Region  string `json:"regionName"`
		City    string `json:"city"`
		ISP     string `json:"isp"`
		AS      string `json:"as"`
	}
	json.NewDecoder(resp.Body).Decode(&r)
	if r.Status != "success" {
		return g
	}
	isp := r.ISP
	if isp == "" {
		isp = r.AS
	}
	return geoRec{Country: r.Country, CC: r.CC, City: r.City, Region: r.Region, ISP: isp}
}

type Ev struct {
	Ts      int64   `json:"ts"`
	Type    string  `json:"type"`
	IP      string  `json:"ip"`
	Cip     string  `json:"cip"`
	ISP     string  `json:"isp"`
	Region  string  `json:"region"`
	City    string  `json:"city"`
	Proxy   int     `json:"proxy"`
	Peer    string  `json:"peer"`
	Mode    string  `json:"mode"`
	Kind    string  `json:"kind"`
	PeakDl  float64 `json:"peakDl"`
	PeakUl  float64 `json:"peakUl"`
	Avg     float64 `json:"avg"`
	Bytes   float64 `json:"bytes"`
	Sec     float64 `json:"sec"`
	UA      string  `json:"ua"`
	Client  string  `json:"client"`
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
	Country string  `json:"country"`
	CC      string  `json:"cc"`
}

// PROV2CN 把 ip.sb 常见英文/拼音省州名映射为中文，供榜单/明细地区列去英文化。
var PROV2CN = map[string]string{
	"Anhui":"安徽","Beijing":"北京","Chongqing":"重庆","Fujian":"福建","Gansu":"甘肃","Guangdong":"广东","Guangxi":"广西","Guizhou":"贵州","Hainan":"海南","Hebei":"河北","Heilongjiang":"黑龙江","Henan":"河南","Hong Kong":"香港","Hubei":"湖北","Hunan":"湖南","Jiangsu":"江苏","Jiangxi":"江西","Jilin":"吉林","Liaoning":"辽宁","Macao":"澳门","Nei Menggu":"内蒙古","Inner Mongolia":"内蒙古","Ningxia":"宁夏","Qinghai":"青海","Shaanxi":"陕西","Shandong":"山东","Shanghai":"上海","Shanxi":"山西","Sichuan":"四川","Taiwan":"台湾","Tianjin":"天津","Xinjiang":"新疆","Xizang":"西藏","Tibet":"西藏","Yunnan":"云南","Zhejiang":"浙江",
}

// regionCN 明细「线路」列：保留 domestic/overseas 标记，国内省份译中文，国外归为「海外」。
func regionCN(cc, region string) string {
	if region == "domestic" || region == "overseas" {
		return region
	}
	if v := provCN(region); v != "" {
		return v
	}
	if cc != "" && cc != "CN" {
		return "海外"
	}
	return ""
}

func provCN(s string) string {
	if s == "" {
		return ""
	}
	if v, ok := PROV2CN[s]; ok {
		return v
	}
	return ""
}

// ispCN 把 ip.sb 的运营商归一化为中文；三大/广电之外的（含国外真实运营商）原样返回。
func ispCN(isp string) string {
	x := strings.ToLower(isp)
	switch {
	case strings.Contains(x, "mobile") || strings.Contains(x, "cmcc") || strings.Contains(isp, "移动"):
		return "中国移动"
	case strings.Contains(x, "unicom") || strings.Contains(isp, "联通"):
		return "中国联通"
	case strings.Contains(x, "telecom") || strings.Contains(x, "chinanet") || strings.Contains(isp, "电信"):
		return "中国电信"
	case strings.Contains(x, "broadnet") || strings.Contains(x, "cbn") || strings.Contains(isp, "广电"):
		return "中国广电"
	}
	if isProxyISP(isp, "", "") {
		return "数据中心"
	}
	return isp
}

// isProxyISP 判断是否为机房/云/CDN/代理出口（非家庭宽带运营商）。
func isProxyISP(isp, org, asn string) bool {
	s := strings.ToLower(isp + " " + org + " " + asn)
	for _, k := range []string{"cloud", "aliyun", "alibaba", "tencent", "amazon", "aws", "azure", "google llc", "zenlayer", "digitalocean", "vultr", "ovh", "hetzner", "cdn", "hosting", "datacenter", "data center", "机房", "数据中心", "云计算", "网络科技", "服务器", "网宿", "linode", "bytedance", "huawei cloud", "baidu", "oracle", "fastly", "akamai", "fdcservers", "layerhost", "zouter", "znet", "xunteng", "private customer", "serverprovider", "hostkey", "contabo", "chosepr", "buyvm", "racknerd", "hostinger", "namecheap", "scaleway", "gcore", "cdn77", "hostworld", "vps", "transit", "backbone", "kdata", "twdm", "seednet", "proxy", "vpn"} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

func geo(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		return
	}
	ip := clientIP(r)
	g := lookupGeo(ip)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	out, _ := json.Marshal(map[string]any{
		"ip": ip, "isp": g.ISP, "ispCN": ispCN(g.ISP), "proxy": isProxyISP(g.ISP, g.Org, g.ASNOrg),
		"city": g.City, "region": g.Region, "country": g.Country, "cc": g.CC, "asn": g.ASN,
	})
	w.Write(out)
}

func ul(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ip := clientIP(r)
	// 必须按真实访客 IP 计配额：8085 挂在 Caddy 之后，remoteIP 恒为 127.0.0.1，
	// 用它会让所有人共用一份配额，第一个访客就能把兜底落点吃满。
	left, code := ulAcquire(ip)
	if code != 0 {
		ulBusy(w, code)
		return
	}
	n, _ := io.Copy(io.Discard, io.LimitReader(r.Body, left))
	ulRelease(ip, n)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, "{\"ok\":true,\"bytes\":%d}", n)
}

var dlChunk = make([]byte, 512*1024)

func dl(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		cors(w)
		return
	}
	cors(w)
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if _, err := w.Write(dlChunk); err != nil {
			return
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// ---- anti-fraud: per-IP token bucket + plausibility bounds ----
type rlBucket struct {
	tokens float64
	last   time.Time
}

var (
	rlMu   sync.Mutex
	rlMaps = map[string]*rlBucket{}
)

const (
	rlCap    = 40.0 // burst allowance per IP
	rlRefill = 10.0 // tokens/sec per IP (~600/min)
	// 物理合理性上限。原先设到 100 Gbps 等于不设防：榜上混进过 2040/2133 Mbps
	// 的机房探测值。家用与移动网络不可能超过 2 Gbps，取 2000 既挡住明显伪造，
	// 又给对称千兆以上的机房访客留了 2 倍余量。
	maxMbps   = 2000.0
	rlMaxMaps = 20000 // bound memory
)

// allow 每 IP 令牌桶：真人测速远达不到该速率，仅拦截脚本洪水刷榜。
func allow(ip string) bool {
	rlMu.Lock()
	defer rlMu.Unlock()
	if len(rlMaps) > rlMaxMaps {
		cut := time.Now().Add(-30 * time.Minute)
		for k, b := range rlMaps {
			if b.last.Before(cut) {
				delete(rlMaps, k)
			}
		}
	}
	now := time.Now()
	b := rlMaps[ip]
	if b == nil {
		b = &rlBucket{tokens: rlCap, last: now}
		rlMaps[ip] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * rlRefill
	if b.tokens > rlCap {
		b.tokens = rlCap
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// plausible 拒绝越界/自相矛盾的伪造成绩（平均带宽远超宣称峰值即判假）。
func plausible(e Ev) bool {
	mbps := func(v float64) bool { return v >= 0 && v <= maxMbps }
	if !mbps(e.PeakDl) || !mbps(e.PeakUl) || !mbps(e.Avg) {
		return false
	}
	if e.Bytes < 0 || e.Bytes > 1e13 { // total bytes, ceiling 10 TB
		return false
	}
	if e.Sec < 0 || e.Sec > 86400 {
		return false
	}
	if e.Sec > 0.2 && e.Bytes > 0 {
		derived := e.Bytes * 8 / e.Sec / 1e6
		bound := 1.5*(e.PeakDl+e.PeakUl) + 50 // sum peaks (bidirectional-safe)
		if derived > bound {
			return false
		}
	}
	return true
}

func report(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	if r.Header.Get("X-Pml-Key") != siteKey {
		http.Error(w, "bad key", 403)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	var e Ev
	if json.Unmarshal(body, &e) != nil {
		http.Error(w, "bad json", 400)
		return
	}
	if e.Type != "visit" && e.Type != "test" {
		http.Error(w, "bad type", 400)
		return
	}
	e.Ts = time.Now().Unix()
	e.IP = clientIP(r)
	// 内网/回环来源一律不入库：本机与镜像自测会污染访客榜（曾出现 127.0.0.1
	// 单 IP 259 GB 占流量榜首），这类流量不是真实访客。
	if ip := net.ParseIP(e.IP); ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
		rejectedBad.Add(1)
		http.Error(w, "local source not recorded", http.StatusForbidden)
		return
	}
	// 洪水拦截：目标域名下同一 IP 一秒内测速 >= floodPerSec 次，直接 429 并记实时日志。
	if floodHostMatch(r) && floodCheck(e.IP, hostKey(r)) {
		http.Error(w, "flood detected", http.StatusTooManyRequests)
		return
	}
	if !allow(e.IP) {
		rejectedRate.Add(1)
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	// 反代不透传时，服务器 socket 只能看到代理出口 IP；若客户端自报了可用的公网真实出口
	// IP（浏览器直连 ip.sb 自查得到），用它覆盖入库/展示 IP，使「访客 IP」与已采信的上报
	// geo 同源一致。限速与本机来源判定仍按 socket 侧 IP，防伪造绕过。
	if e.Cip != "" {
		if cip := net.ParseIP(e.Cip); cip != nil && !cip.IsLoopback() && !cip.IsPrivate() && !cip.IsLinkLocalUnicast() && !cip.IsUnspecified() {
			e.IP = cip.String()
		}
	}
	if e.Type == "test" && !plausible(e) {
		rejectedBad.Add(1)
		http.Error(w, "implausible", http.StatusBadRequest)
		return
	}
	ua := r.UserAgent()
	if len(ua) > 200 {
		ua = ua[:200]
	}
	e.UA = ua
	// 客户端（浏览器/端）直查 ip.sb 得到的是用户真实出口 geo，优先采信；
	// 服务器单 IP 查 ip.sb 会被限流降级/串味（曾把同一 IP 写成 FDCservers/Xunteng 等乱值），
	// 仅在客户端未给出可用 geo 时，才回退服务器侧查询补全缺口。
	if e.ISP == "" || (e.Lat == 0 && e.Lon == 0) {
		g := lookupGeo(e.IP)
		if e.ISP == "" { e.ISP = g.ISP }
		if e.Country == "" { e.Country = g.Country }
		if e.CC == "" { e.CC = g.CC }
		if e.City == "" { e.City = g.City }
		if e.Lat == 0 && e.Lon == 0 { e.Lat = g.Lat; e.Lon = g.Lon }
	}
	line, _ := json.Marshal(e)
	mu.Lock()
	f, err := os.OpenFile(evFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		f.Write(line)
		f.Write([]byte("\n"))
		f.Close()
	}
	mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, "{\"ok\":true}")
}

// loadAdminCred 从环境变量或 root-only 文件读取管理员凭据，避免把口令写死进源码。
func loadAdminCred() {
	if u := os.Getenv("PML_ADMIN_USER"); u != "" {
		adminUser = u
	}
	if pw := os.Getenv("PML_ADMIN_PASS"); pw != "" {
		adminPass = pw
		return
	}
	if b, err := os.ReadFile(dataDir + "/admin_cred"); err == nil {
		parts := strings.SplitN(strings.TrimSpace(string(b)), ":", 2)
		if len(parts) == 2 {
			adminUser = parts[0]
			adminPass = parts[1]
		}
	}
}

// 公网入口暴露后必须挡住撞库：同一访客 IP 在窗口内失败到上限就暂时拒绝。
const loginFailLimit = 8
const loginFailWindow = 15 * time.Minute

var loginMu sync.Mutex
var loginFails = map[string][]time.Time{}

func loginBlocked(ip string) bool {
	loginMu.Lock()
	defer loginMu.Unlock()
	cut := time.Now().Add(-loginFailWindow)
	var keep []time.Time
	for _, t := range loginFails[ip] {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	loginFails[ip] = keep
	return len(keep) >= loginFailLimit
}

func loginFail(ip string) {
	loginMu.Lock()
	defer loginMu.Unlock()
	loginFails[ip] = append(loginFails[ip], time.Now())
}

func loginOk(ip string) {
	loginMu.Lock()
	defer loginMu.Unlock()
	delete(loginFails, ip)
}

func login(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var b struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1024))
	json.Unmarshal(body, &b)
	ip := clientIP(r)
	if loginBlocked(ip) {
		http.Error(w, "too many failed logins", http.StatusTooManyRequests)
		return
	}
	if adminPass == "" || b.Username != adminUser || b.Password != adminPass {
		loginFail(ip)
		time.Sleep(600 * time.Millisecond)
		http.Error(w, "wrong credentials", 401)
		return
	}
	loginOk(ip)
	http.SetCookie(w, &http.Cookie{
		Name:     cookieNm,
		Value:    token(),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400 * 30,
	})
	fmt.Fprint(w, "{\"ok\":true}")
}

func logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieNm, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/admin", http.StatusFound)
}

func admin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(adminHTML)
}

type Row struct {
	IP      string  `json:"ip"`
	ISP     string  `json:"isp"`
	Country string  `json:"country"`
	CC      string  `json:"cc"`
	Region  string  `json:"region"`
	City    string  `json:"city"`
	Mode    string  `json:"mode"`
	Kind    string  `json:"kind"`
	PeakDl  float64 `json:"peakDl"`
	PeakUl  float64 `json:"peakUl"`
	Bytes   float64 `json:"bytes"`
	Sec     float64 `json:"sec"`
	Ts      int64   `json:"ts"`
	Client  string  `json:"client"`
}

type Day struct {
	Day    string  `json:"day"`
	Tests  int     `json:"tests"`
	Visits int     `json:"visits"`
	Bytes  float64 `json:"bytes"`
}

type Point struct {
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
	CC       string  `json:"cc"`
	Country  string  `json:"country"`
	Province string  `json:"province"`
	City     string  `json:"city"`
	District string  `json:"district"`
	ISP      string  `json:"isp"`
	Mode     string  `json:"mode"`
	Kind     string  `json:"kind"`
	IP       string  `json:"ip"`
	PeakDl   float64 `json:"peakDl"`
	PeakUl   float64 `json:"peakUl"`
	Bytes    float64 `json:"bytes"`
	Ts       int64   `json:"ts"`
	Test     bool    `json:"test"`
}

type Agg struct {
	Visits       int     `json:"visits"`
	Tests        int     `json:"tests"`
	Online       int     `json:"online"`
	TotalBytes   float64 `json:"totalBytes"`
	TodayTests   int     `json:"todayTests"`
	TodayBytes   float64 `json:"todayBytes"`
	SumDl        float64 `json:"sumDl"`
	SumUl        float64 `json:"sumUl"`
	MaxDl        float64 `json:"maxDl"`
	MaxUl        float64 `json:"maxUl"`
	Days         []Day   `json:"days"`
	Recent       []Row   `json:"recent"`
	TopIps       []Row   `json:"topIps"`
	Points       []Point `json:"points"`
	LbDl         []Row   `json:"lbDl"`
	LbUl         []Row   `json:"lbUl"`
	LbBytes      []Row   `json:"lbBytes"`
	RejectedRate  int64      `json:"rejectedRate"`
	RejectedBad   int64      `json:"rejectedBad"`
	RejectedFlood int64      `json:"rejectedFlood"`
	Flood         []FloodRec `json:"flood"`
}

// robustFence 返回统计上界 (Q3 + 4*IQR)，并设 1000 Mbps 下限，
// 只裁掉显著离群的伪造高分，不影响正常千兆级测速。
func robustFence(vals []float64) float64 {
	const floor = 1000.0
	if len(vals) < 8 {
		return floor
	}
	s := append([]float64{}, vals...)
	sort.Float64s(s)
	q1 := s[len(s)/4]
	q3 := s[(len(s)*3)/4]
	f := q3 + 4*(q3-q1)
	if f < floor {
		f = floor
	}
	return f
}

func ipinfo(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		return
	}
	if !authed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	ip := strings.TrimSpace(r.URL.Query().Get("ip"))
	if ip == "" || isPrivate(ip) {
		http.Error(w, "bad ip", 400)
		return
	}
	c := &http.Client{Timeout: 6 * time.Second}
	resp, err := c.Get("https://api.ip.sb/geoip/" + ip)
	if err != nil {
		http.Error(w, "upstream", 502)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	io.Copy(w, resp.Body)
}

func stats(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if !authed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	mu.Lock()
	data, err := os.ReadFile(evFile)
	mu.Unlock()
	agg := Agg{Days: []Day{}, Recent: []Row{}, TopIps: []Row{}, Points: []Point{}, LbDl: []Row{}, LbUl: []Row{}, LbBytes: []Row{}}
	agg.RejectedRate = rejectedRate.Load()
	agg.RejectedBad = rejectedBad.Load()
	agg.RejectedFlood, agg.Flood = floodSnapshot()
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(agg)
		return
	}
	now := time.Now()
	today := now.Format("2006-01-02")
	onlineCut := now.Add(-120 * time.Second).Unix()
	var cutoff int64
	switch r.URL.Query().Get("range") {
	case "24h":
		cutoff = now.Add(-24 * time.Hour).Unix()
	case "7d":
		cutoff = now.Add(-7 * 24 * time.Hour).Unix()
	case "30d":
		cutoff = now.Add(-30 * 24 * time.Hour).Unix()
	}
	dayMap := map[string]*Day{}
	ipBytes := map[string]*Row{}
	ipMaxDl := map[string]*Row{}
	ipMaxUl := map[string]*Row{}
	onlineSet := map[string]bool{}
	var tests []Row
	var pts []Point
	var dlVals []float64
	var ulVals []float64
	for _, ln := range strings.Split(string(data), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		var e Ev
		if json.Unmarshal([]byte(ln), &e) != nil {
			continue
		}
		if cutoff > 0 && e.Ts < cutoff {
			continue
		}
		d := time.Unix(e.Ts, 0).Format("2006-01-02")
		dd := dayMap[d]
		if dd == nil {
			dd = &Day{Day: d}
			dayMap[d] = dd
		}
		if e.Ts >= onlineCut && e.IP != "" {
			onlineSet[e.IP] = true
		}
		pr, ct, ds := reverseGeoCN(e.Lat, e.Lon)
		pct := ct
		if pct == "" && e.City != "" { pct = e.City }
		if e.Lat != 0 || e.Lon != 0 {
			pts = append(pts, Point{Lat: e.Lat, Lon: e.Lon, CC: e.CC, Country: e.Country, Province: pr, City: pct, District: ds, Mode: e.Mode, Kind: e.Kind, IP: e.IP, ISP: e.ISP, PeakDl: e.PeakDl, PeakUl: e.PeakUl, Bytes: e.Bytes, Ts: e.Ts, Test: e.Type == "test"})
		}
		// 明细表用城市：优先中文反查市；无市则取中文区(覆盖港澳)；再退客户端自报城市(海外原文)
		rct := ct
		if rct == "" {
			if ds != "" { rct = ds } else if e.City != "" && e.City != "self" && e.City != "domestic" { rct = e.City }
		}
		if rct == "" { rct = provCN(e.Region) }
		if e.Type == "visit" {
			agg.Visits++
			dd.Visits++
			continue
		}
		agg.Tests++
		dd.Tests++
		dd.Bytes += e.Bytes
		agg.TotalBytes += e.Bytes
		agg.SumDl += e.PeakDl
		agg.SumUl += e.PeakUl
		if e.PeakDl > agg.MaxDl {
			agg.MaxDl = e.PeakDl
		}
		if e.PeakUl > agg.MaxUl {
			agg.MaxUl = e.PeakUl
		}
		if d == today {
			agg.TodayTests++
			agg.TodayBytes += e.Bytes
		}
		cl := e.Client
		if cl == "" {
			cl = "web"
		}
		tests = append(tests, Row{IP: e.IP, ISP: e.ISP, Country: e.Country, CC: e.CC, Region: regionCN(e.CC, e.Region), City: rct, Mode: e.Mode, Kind: e.Kind, PeakDl: e.PeakDl, PeakUl: e.PeakUl, Bytes: e.Bytes, Sec: e.Sec, Ts: e.Ts, Client: cl})
		if e.PeakDl > 0 {
			dlVals = append(dlVals, e.PeakDl)
		}
		if e.PeakUl > 0 {
			ulVals = append(ulVals, e.PeakUl)
		}
		upsertMax(ipMaxDl, e, rct, func(r *Row) float64 { return r.PeakDl }, func(r *Row, v float64) { r.PeakDl = v }, e.PeakDl)
		upsertMax(ipMaxUl, e, rct, func(r *Row) float64 { return r.PeakUl }, func(r *Row, v float64) { r.PeakUl = v }, e.PeakUl)
		tb := ipBytes[e.IP]
		if tb == nil {
			tb = &Row{IP: e.IP, ISP: e.ISP, Country: e.Country, CC: e.CC, Region: regionCN(e.CC, e.Region), City: rct}
			ipBytes[e.IP] = tb
		}
		tb.Bytes += e.Bytes
		tb.Sec += 1
	}
	agg.Online = len(onlineSet)
	dlFence := robustFence(dlVals)
	ulFence := robustFence(ulVals)
	agg.MaxDl = 0
	for _, v := range dlVals {
		if v <= dlFence && v > agg.MaxDl {
			agg.MaxDl = v
		}
	}
	agg.MaxUl = 0
	for _, v := range ulVals {
		if v <= ulFence && v > agg.MaxUl {
			agg.MaxUl = v
		}
	}
	for ip, r := range ipMaxDl {
		if r.PeakDl > dlFence {
			delete(ipMaxDl, ip)
		}
	}
	for ip, r := range ipMaxUl {
		if r.PeakUl > ulFence {
			delete(ipMaxUl, ip)
		}
	}
	sort.Slice(tests, func(i, j int) bool { return tests[i].Ts > tests[j].Ts })
	if len(tests) > 100 {
		tests = tests[:100]
	}
	agg.Recent = tests
	sort.Slice(pts, func(i, j int) bool { return pts[i].Ts > pts[j].Ts })
	if len(pts) > 400 {
		pts = pts[:400]
	}
	agg.Points = pts
	for _, v := range dayMap {
		agg.Days = append(agg.Days, *v)
	}
	sort.Slice(agg.Days, func(i, j int) bool { return agg.Days[i].Day < agg.Days[j].Day })
	if len(agg.Days) > 30 {
		agg.Days = agg.Days[len(agg.Days)-30:]
	}
	for _, v := range ipBytes {
		agg.TopIps = append(agg.TopIps, *v)
	}
	sort.Slice(agg.TopIps, func(i, j int) bool { return agg.TopIps[i].Bytes > agg.TopIps[j].Bytes })
	if len(agg.TopIps) > 20 {
		agg.TopIps = agg.TopIps[:20]
	}
	agg.LbDl = topFromMap(ipMaxDl, func(r *Row) float64 { return r.PeakDl }, 10)
	agg.LbUl = topFromMap(ipMaxUl, func(r *Row) float64 { return r.PeakUl }, 10)
	var bl []Row
	for _, v := range ipBytes {
		bl = append(bl, *v)
	}
	sort.Slice(bl, func(i, j int) bool { return bl[i].Bytes > bl[j].Bytes })
	if len(bl) > 10 {
		bl = bl[:10]
	}
	agg.LbBytes = bl
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(agg)
}

func upsertMax(m map[string]*Row, e Ev, city string, get func(*Row) float64, set func(*Row, float64), v float64) {
	cur := m[e.IP]
	if cur == nil {
		cur = &Row{IP: e.IP, ISP: e.ISP, Country: e.Country, CC: e.CC, Region: e.Region, City: city}
		m[e.IP] = cur
	}
	if cur.City == "" && city != "" {
		cur.City = city
	}
	if cur.Country == "" && e.Country != "" {
		cur.Country = e.Country
		cur.CC = e.CC
		cur.ISP = e.ISP
	}
	if v > 0 && v > get(cur) {
		set(cur, v)
	}
}

func topFromMap(m map[string]*Row, get func(*Row) float64, n int) []Row {
	var out []Row
	for _, v := range m {
		if get(v) > 0 {
			out = append(out, *v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return get(&out[i]) > get(&out[j]) })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// ===== offline China province/city/district reverse geocoding (DataV boundaries) =====
type cnRing []float64
type cnGeom struct {
	prov, city, dist       string
	minx, miny, maxx, maxy float64
	polys                  [][]cnRing
}

var cnByLevel [3][]cnGeom // 0=district 1=city 2=province

func cnRingCross(r cnRing, x, y float64) bool {
	in := false
	n := len(r) / 2
	for i := 0; i < n; i++ {
		j := i - 1
		if j < 0 {
			j = n - 1
		}
		xi, yi := r[2*i], r[2*i+1]
		xj, yj := r[2*j], r[2*j+1]
		if (yi > y) != (yj > y) {
			xint := (xj-xi)*(y-yi)/(yj-yi) + xi
			if x < xint {
				in = !in
			}
		}
	}
	return in
}
func (g *cnGeom) contains(x, y float64) bool {
	if x < g.minx || x > g.maxx || y < g.miny || y > g.maxy {
		return false
	}
	for _, poly := range g.polys {
		inside := false
		for _, r := range poly {
			if cnRingCross(r, x, y) {
				inside = !inside
			}
		}
		if inside {
			return true
		}
	}
	return false
}
func loadChinaGeo() {
	b, err := os.ReadFile(vendorDir + "/geo/china_all.json")
	if err != nil {
		return
	}
	var fc struct {
		Features []struct {
			Properties struct {
				Level    string `json:"level"`
				Province string `json:"province"`
				City     string `json:"city"`
				District string `json:"district"`
			} `json:"properties"`
			Geometry struct {
				Type        string          `json:"type"`
				Coordinates json.RawMessage `json:"coordinates"`
			} `json:"geometry"`
		} `json:"features"`
	}
	if json.Unmarshal(b, &fc) != nil {
		return
	}
	for _, f := range fc.Features {
		var polys [][]cnRing
		minx, miny, maxx, maxy := 1e9, 1e9, -1e9, -1e9
		addRing := func(rings [][][]float64) {
			var rs []cnRing
			for _, ring := range rings {
				var r cnRing
				for _, pt := range ring {
					x, y := pt[0], pt[1]
					if x < minx {
						minx = x
					}
					if x > maxx {
						maxx = x
					}
					if y < miny {
						miny = y
					}
					if y > maxy {
						maxy = y
					}
					r = append(r, x, y)
				}
				rs = append(rs, r)
			}
			polys = append(polys, rs)
		}
		switch f.Geometry.Type {
		case "Polygon":
			var p [][][]float64
			if json.Unmarshal(f.Geometry.Coordinates, &p) == nil {
				addRing(p)
			}
		case "MultiPolygon":
			var mp [][][][]float64
			if json.Unmarshal(f.Geometry.Coordinates, &mp) == nil {
				for _, p := range mp {
					addRing(p)
				}
			}
		}
		if len(polys) == 0 || minx > maxx {
			continue
		}
		g := cnGeom{prov: f.Properties.Province, city: f.Properties.City, dist: f.Properties.District, minx: minx, miny: miny, maxx: maxx, maxy: maxy, polys: polys}
		idx := 2
		switch f.Properties.Level {
		case "district":
			idx = 0
		case "city":
			idx = 1
		case "province":
			idx = 2
		}
		cnByLevel[idx] = append(cnByLevel[idx], g)
	}
	b = nil
}
func reverseGeoCN(lat, lon float64) (string, string, string) {
	if lon < 73 || lon > 136 || lat < 3 || lat > 54 {
		return "", "", ""
	}
	for _, lvl := range cnByLevel {
		for i := range lvl {
			if lvl[i].contains(lon, lat) {
				return lvl[i].prov, lvl[i].city, lvl[i].dist
			}
		}
	}
	return "", "", ""
}

// builtin serves the master builtin-source pool to authorized clients (apps and
// the future AI maintenance job). Read-only, gated by X-Pml-Key so third parties
// cannot enumerate the built-in source list. Empty lists when nothing set yet.
func builtin(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("X-Pml-Key") != siteKey {
		http.Error(w, "unauthorized", 401)
		return
	}
	serveBuiltinPool(w)
}

func main() {
	os.MkdirAll(dataDir, 0o755)
	loadSecret()
	loadAdminCred()
	loadGeoCache()
	loadChinaGeo()
	pub := http.NewServeMux()
	pub.HandleFunc("/geo", geo)
	// PML-DISABLED-selfhost-uldl pub.HandleFunc("/ul", ul)
	pub.HandleFunc("/report", report)
	pub.HandleFunc("/api/builtin", builtin)
	pub.HandleFunc("/api/sources", webSources)
	pub.HandleFunc("/api/sourcestat", webSourceStat)
	// PML-DISABLED-selfhost-uldl pub.HandleFunc("/dl", dl)
	adm := http.NewServeMux()
	adm.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("ok"))
	})
	adm.HandleFunc("/admin", admin)
	adm.HandleFunc("/api/stats", stats)
	adm.HandleFunc("/api/ipinfo", ipinfo)
	adm.HandleFunc("/api/admin/sources", adminSources)
	adm.HandleFunc("/api/admin/flood", adminFlood)
	adm.HandleFunc("/login", login)
	adm.HandleFunc("/logout", logout)
	adm.Handle("/vendor/", http.StripPrefix("/vendor/", http.FileServer(http.Dir(vendorDir))))
	go func() {
		fmt.Println("public geo/ul/report on", pubAddr)
		http.ListenAndServe(pubAddr, pub)
	}()
	go func() {
		// 公网后台入口：只绑回环，由 Caddy 反代进来。这里不加 tsOnly，但 authed()
		// 判定用的是 remoteIP，经 Caddy 后恒为 127.0.0.1，走不到 Tailscale 免密通道，
		// 所以 /api/stats 与 /api/admin/sources 仍然必须登录拿 cookie 才有数据。
		fmt.Println("admin (public, via caddy) on", admPubAddr)
		http.ListenAndServe(admPubAddr, adm)
	}()
	fmt.Println("admin on", admAddr, "(tailscale-only 100.64/10)")
	http.ListenAndServe(admAddr, tsOnly(adm))
}
