package engine

import (
	"net"
	"sync/atomic"
	"testing"
)

// 尾窗切片：取末尾 k 个，不足则原样。
func TestTailF(t *testing.T) {
	q := []float64{1, 2, 3, 4, 5}
	if g := tailF(q, 3); len(g) != 3 || g[0] != 3 || g[2] != 5 {
		t.Fatalf("tailF(3)=%v", g)
	}
	if g := tailF(q, 99); len(g) != 5 {
		t.Fatalf("tailF(99)=%v", g)
	}
	if g := tailF(nil, 5); len(g) != 0 {
		t.Fatalf("tailF(nil)=%v", g)
	}
}

// 中枢稳健值：一半突发(800) 一半持续(100) 时，应偏向持续带，不被突发拉满。
func TestIqrMeanRejectsBurst(t *testing.T) {
	s := make([]float64, 0, 40)
	for i := 0; i < 10; i++ {
		s = append(s, 800)
	}
	for i := 0; i < 30; i++ {
		s = append(s, 100)
	}
	if m := iqrMean(s); m < 80 || m > 300 {
		t.Fatalf("iqrMean=%v 期望落在持续带 80-300", m)
	}
}

// sigmaTrim 抑制高离群点（500），保留稳态带。
func TestSigmaTrimSuppressesOutlier(t *testing.T) {
	s := []float64{100, 102, 98, 101, 99, 500, 100}
	if v := sigmaTrim(s, 200); v < 90 || v > 140 {
		t.Fatalf("sigmaTrim=%v 应抑制 500 离群", v)
	}
}

// p0 对空/单点序列安全，不 panic，返回 0。
func TestP0EmptySafe(t *testing.T) {
	if c, st := p0(nil, 0, 200, 0); c != 0 || st != 0 {
		t.Fatalf("p0(nil)=%v,%v", c, st)
	}
	one := []cumPt{{tMs: 1000, cum: 100}}
	if c, _ := p0(one, 1000, 200, 0); c != 0 {
		t.Fatalf("p0(single)=%v 期望 0", c)
	}
}

// bigJump 阈值：基线<80 不判大跳；>=80 且 >=1.18x 才判。
func TestBigJump(t *testing.T) {
	if bigJump(200, 70) {
		t.Fatalf("prev<80 不应判大跳")
	}
	if !bigJump(120, 90) {
		t.Fatalf("90->120(1.33x) 应判大跳")
	}
	if bigJump(95, 90) {
		t.Fatalf("90->95(1.06x) 不应判大跳")
	}
}

// --- §91 内核优化配套测试 ---

func approx(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

// setSlotWant：死源 want=0，活源均分预算并 clamp 到 [1,maxW]。
func TestSetSlotWantDeadMigration(t *testing.T) {
	slots := []*urlSlot{{}, {}, {}, {}}
	atomic.StoreInt32(&slots[1].dead, 1)
	atomic.StoreInt32(&slots[3].dead, 1) // 2 死 2 活
	setSlotWant(slots, 40, 20)
	if atomic.LoadInt32(&slots[0].want) != 20 || atomic.LoadInt32(&slots[2].want) != 20 {
		t.Fatalf("活源应各得 40/2=20，got %d/%d", slots[0].want, slots[2].want)
	}
	if atomic.LoadInt32(&slots[1].want) != 0 || atomic.LoadInt32(&slots[3].want) != 0 {
		t.Fatalf("死源 want 应为 0，got %d/%d", slots[1].want, slots[3].want)
	}
	// 全死：活源数兜底为 1，base=budget/1 但 clamp 到 maxW，且仍把死源置 0。
	all := []*urlSlot{{}, {}}
	atomic.StoreInt32(&all[0].dead, 1)
	atomic.StoreInt32(&all[1].dead, 1)
	setSlotWant(all, 40, 20)
	if all[0].want != 0 || all[1].want != 0 {
		t.Fatalf("全死时所有 want 应为 0，got %d/%d", all[0].want, all[1].want)
	}
}

// setSlotWant：base 低于 1 时兜底为 1（预算小于活源数）。
func TestSetSlotWantMinOne(t *testing.T) {
	slots := []*urlSlot{{}, {}, {}}
	setSlotWant(slots, 1, 20) // 3 活源分 1 预算 -> base=0 -> clamp 1
	for i, sl := range slots {
		if atomic.LoadInt32(&sl.want) != 1 {
			t.Fatalf("slot%d want=%d 期望 1", i, sl.want)
		}
	}
}

// round1 一位小数四舍五入，负数也正确（math.Round 语义）。
func TestRound1(t *testing.T) {
	cases := []struct{ in, want float64 }{
		{12.0, 12.0}, {12.04, 12.0}, {12.06, 12.1}, {0, 0},
		{-1.06, -1.1}, {-1.04, -1.0}, {883.64, 883.6},
	}
	for _, c := range cases {
		if got := round1(c.in); !approx(got, c.want) {
			t.Fatalf("round1(%v)=%v 期望 %v", c.in, got, c.want)
		}
	}
}

// countingConn：仅当 w!=nil 且 live!=nil（上行连接）才在 socket 层计数。
type fakeConn struct{ net.Conn }

func (fakeConn) Write(b []byte) (int, error) { return len(b), nil }

func TestCountingConnGate(t *testing.T) {
	var n int64
	var l int32
	cu := &countingConn{Conn: fakeConn{}, w: &n, live: &l}
	if _, err := cu.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("上行连接应计 5 字节，got %d", n)
	}
	var n2 int64
	cd := &countingConn{Conn: fakeConn{}, w: &n2, live: nil} // 下行/未注入 live：不计数
	if _, err := cd.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Fatalf("live==nil 不应计数，got %d", n2)
	}
}

// dlBufPool：取出的缓冲长度为 dlBuf，可安全归还复用。
func TestDlBufPoolReuse(t *testing.T) {
	b := dlBufPool.Get().([]byte)
	if len(b) != dlBuf {
		t.Fatalf("池缓冲长度=%d 期望 %d", len(b), dlBuf)
	}
	b[0] = 7
	dlBufPool.Put(b)
	b2 := dlBufPool.Get().([]byte)
	if len(b2) != dlBuf {
		t.Fatalf("复用后长度=%d 期望 %d", len(b2), dlBuf)
	}
	dlBufPool.Put(b2)
}
