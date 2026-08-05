package middleware

import (
	"fmt"
	"testing"
	"time"
)

// fakeClock 手动推进的时钟。淘汰的 TTL 是 10 分钟，用真实时间没法测。
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }

func newTestLimiter(rps float64, burst int) (*limiter, *fakeClock) {
	// 起点刻意不用零值 time.Time：nextSweep/nextLog 的零值语义是"立刻可做"，
	// 若当前时间也是零值，now.Before(zero) 为 false 就靠巧合成立了。
	clk := &fakeClock{t: time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)}
	l := newLimiter(rps, burst)
	l.now = clk.now
	return l, clk
}

// TestLimiterEvictsIdleBuckets 淘汰这条路径此前**从未被执行过**。
//
// TTL 是 10 分钟，而所有既有测试都在毫秒级内跑完，所以那个 for-delete 循环从写出来
// 到现在一次都没删掉过任何东西——它可能一直是错的而测试全绿。注入时钟之后才能考察。
func TestLimiterEvictsIdleBuckets(t *testing.T) {
	l, clk := newTestLimiter(1, 5)

	for i := range 100 {
		l.allowIP(fmt.Sprintf("10.0.0.%d", i))
	}
	if got := len(l.buckets); got != 100 {
		t.Fatalf("前提不成立：应当有 100 个桶，实际 %d", got)
	}

	// 越过 TTL 与摊销闸门，再来一个请求触发扫描。
	clk.add(rateLimitIdleTTL + time.Second)
	l.allowIP("10.0.1.1")

	// 只应剩下刚才那个新 IP。
	if got := len(l.buckets); got != 1 {
		t.Errorf("过了 TTL 之后应当只剩 1 个桶（刚来的那个），实际 %d——"+
			"淘汰没生效，map 会随访问过的 IP 数无界增长，被扫段时就是内存泄漏", got)
	}
	if _, ok := l.buckets["10.0.1.1"]; !ok {
		t.Error("刚来的那个 IP 的桶被误删了")
	}
}

// TestLimiterKeepsActiveBucketsAcrossSweep 淘汰不能把**还在用**的桶删掉。
//
// 这是上一条的反面，必须同时钉住：一个只会"全删"的实现能让上一条通过，但它会在
// 每次扫描时把所有人的令牌数重置成满桶——限流于是每 10 分钟被清零一次。
func TestLimiterKeepsActiveBucketsAcrossSweep(t *testing.T) {
	l, clk := newTestLimiter(0.001, 3) // rps 极低，令牌基本不会自然补回来

	// 把 active 这个 IP 的令牌用光。
	for range 3 {
		if ok, _, _, _ := l.allowIP("1.1.1.1"); !ok {
			t.Fatal("前提不成立：前 3 次应当都放行")
		}
	}
	if ok, _, _, _ := l.allowIP("1.1.1.1"); ok {
		t.Fatal("前提不成立：第 4 次应当被拒")
	}

	// 时间推进但**不超过 TTL**，同时让摊销闸门到期，从而真的跑一次扫描。
	l.nextSweep = clk.now()
	clk.add(rateLimitIdleTTL / 2)
	l.allowIP("2.2.2.2") // 触发扫描

	if _, ok := l.buckets["1.1.1.1"]; !ok {
		t.Fatal("还在 TTL 内的桶被删掉了——那等于每次扫描都把所有人的令牌重置成满桶，" +
			"限流被周期性清零")
	}
	if ok, _, _, _ := l.allowIP("1.1.1.1"); ok {
		t.Error("扫描之后 1.1.1.1 又能取到令牌了——它的桶被重置了，限流失效")
	}
}

// TestLimiterSweepIsAmortized 扫描必须按时间摊销，不能每来一个新 IP 就全扫一遍。
//
// 原实现是后者，被扫段时退化成 O(n²)：实测 32k 个新 IP 要 4.7 秒、全程持锁，
// 也就是防护在最该起作用的时刻自己变成了瓶颈。
//
// 这里不测耗时（会成为一条看机器脸色的脆弱断言），而是直接钉住**扫描发生的次数**：
// 用 sweepLocked 更新的 nextSweep 作为观测点。
func TestLimiterSweepIsAmortized(t *testing.T) {
	l, clk := newTestLimiter(100, 100)

	l.allowIP("10.0.0.1") // 首次调用会扫一次（nextSweep 零值 = 立刻可做）
	first := l.nextSweep
	if first.IsZero() {
		t.Fatal("首次调用应当扫一次并推进 nextSweep")
	}

	// 再来 5000 个**新** IP，时间只推进 1 毫秒——远不到下一次扫描的时刻。
	// 用 172.x 网段，避免和上面的 10.0.0.1 / 下面的 10.9.9.9 撞键。
	for i := range 5000 {
		l.allowIP(fmt.Sprintf("172.%d.%d.%d", i/65536, i/256%256, i%256))
	}
	clk.add(time.Millisecond)
	l.allowIP("10.9.9.9")

	if !l.nextSweep.Equal(first) {
		t.Errorf("5000 个新 IP 期间又扫了（nextSweep 从 %v 变成 %v）——"+
			"说明扫描仍然挂在'出现新 IP'上，被扫段时会退化成 O(n²)", first, l.nextSweep)
	}
	// 5000 + 10.0.0.1 + 10.9.9.9
	if len(l.buckets) != 5002 {
		t.Errorf("前提不成立：应当攒下 5002 个桶，实际 %d", len(l.buckets))
	}
}

// TestLimiterThrottlesRejectionLog 被拒日志必须节流。
//
// 每次拒绝都打一行的话，日志量与攻击流量成正比——磁盘/stdout 成为第三个被放大的
// 资源。同时被静默的次数必须累加后随下一行报出来，否则量级信息全丢。
func TestLimiterThrottlesRejectionLog(t *testing.T) {
	l, clk := newTestLimiter(0.001, 1)

	l.allowIP("3.3.3.3") // 用掉唯一的令牌

	// 第一次拒绝应当打日志，且此前没有被静默的。
	_, _, shouldLog, skipped := l.allowIP("3.3.3.3")
	if !shouldLog || skipped != 0 {
		t.Fatalf("第一次拒绝应当打日志且 skipped=0，得到 shouldLog=%v skipped=%d", shouldLog, skipped)
	}

	// 紧接着的 50 次都在节流窗口内，都不该打。
	for i := range 50 {
		if _, _, sl, _ := l.allowIP("3.3.3.3"); sl {
			t.Fatalf("窗口内第 %d 次拒绝也打了日志——节流没生效", i+1)
		}
	}

	// 越过窗口，下一次要打，并且要把这 50 次报出来。
	clk.add(rateLimitLogInterval + time.Millisecond)
	_, _, shouldLog, skipped = l.allowIP("3.3.3.3")
	if !shouldLog {
		t.Fatal("越过节流窗口之后应当重新打日志")
	}
	if skipped != 50 {
		t.Errorf("应当报出被静默的 50 次，实际 %d——量级信息丢了，"+
			"运维会以为只被拒了两次", skipped)
	}
}
