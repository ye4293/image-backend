package middleware

import (
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// rateLimitIdleTTL 多久没再访问就把这个 IP 的桶丢掉。
//
// 不淘汰的话 map 会随访问过的 IP 数无界增长——一次扫段就能让它涨到几十万条，
// 那是个只在被攻击时才发作的内存泄漏，而那正是最不该雪上加霜的时刻。
const rateLimitIdleTTL = 10 * time.Minute

// rateLimitLogInterval 被拒日志的最小间隔。
//
// 每次拒绝都打一行的话，被扫段时日志量与攻击流量成正比——磁盘和 stdout 于是变成
// 第三个被放大的资源（前两个见 limiter.sweepLocked 的注释）。而日志的用途是
// "发现 ClientIP 误配"，那件事一行就够看出来，一秒一行绰绰有余。
const rateLimitLogInterval = time.Second

// bucket 是单个 IP 的令牌桶。
//
// 只存"上次补充时刻"与"当前令牌数"，取用时按经过的时间惰性补充——不需要后台
// goroutine 定时给每个桶加令牌（那样 N 个 IP 就是 N 个定时器）。
type bucket struct {
	tokens   float64
	lastSeen time.Time
}

// limiter 是限流器的状态。
//
// 写成结构体而不是闭包捕获的一堆局部变量，为的是两件事：
//
//  1. **淘汰逻辑能被测试。** TTL 是 10 分钟，闭包版本没有任何办法在测试里推进时间，
//     于是淘汰这条路径从写出来到现在一次都没被执行过。now 做成字段后可以注入假时钟。
//  2. 长生命周期对象不要靠闭包持有环境——闭包会让整个外层作用域跟着它一起活着。
type limiter struct {
	rps   float64
	burst int
	// ttl / sweepEvery 分开成两个字段而不是共用一个常量：测试要能把 sweep 间隔
	// 调成 0（每次都扫）来单独考察淘汰判定，不受摊销闸门干扰。
	ttl         time.Duration
	sweepEvery  time.Duration
	logInterval time.Duration
	// now 可注入的时钟。生产是 time.Now。
	now func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
	// nextSweep / nextLog 都是"下次允许做这件事的最早时刻"。零值表示"立刻可做"，
	// 于是不需要为首次调用做特殊处理。
	nextSweep time.Time
	nextLog   time.Time
	// suppressed 上次打日志以来被静默掉的拒绝数，随下一行日志一起报出来——
	// 否则限流生效时日志上看起来只有零星几次，量级信息全丢了。
	suppressed int
}

func newLimiter(rps float64, burst int) *limiter {
	return &limiter{
		rps:         rps,
		burst:       burst,
		ttl:         rateLimitIdleTTL,
		sweepEvery:  rateLimitIdleTTL,
		logInterval: rateLimitLogInterval,
		now:         time.Now,
		buckets:     make(map[string]*bucket),
	}
}

// sweepLocked 淘汰过期条目。**按时间摊销，而不是"每出现一个新 IP 就全扫一遍"。**
//
// 原先是后者，而它在被扫段时退化成 O(n²)：每个请求都是新 IP，于是每个请求都全扫
// 一遍正在变大的 map，全程持锁。实测 2k/8k/32k 个新 IP 的单请求成本是
// 11µs / 34µs / 147µs——4 倍的量换 13 倍的成本，32k 个 IP 就要 4.7 秒。也就是说这个
// 防护在它最该起作用的时刻自己变成了瓶颈：**防御放大了攻击**。换成时间闸门后同一组
// 数据是 2.7µs / 2.8µs / 2.4µs，恒定。
//
// 代价（刻意接受，写在这里免得下一个人以为已经封死）：两次扫描之间 map 仍可无界
// 增长，上界约「新 IP 速率 × sweepEvery」。1000 req/s 全是新 IP 时约 60 万条、几十 MB。
// 要压到硬上限需要一个"到顶就扫"的旁路，但那条旁路会被"把 map 顶在上限上"重新退化成
// 每请求全扫——需要额外的进度保证才安全。当前不值得：这个中间件**默认关闭**
// （见 config.RateLimitRPS 的注释），真正在生产挡这条路径的是 nginx 的 limit_req。
func (l *limiter) sweepLocked(now time.Time) {
	if now.Before(l.nextSweep) {
		return
	}
	for k, v := range l.buckets {
		if now.Sub(v.lastSeen) > l.ttl {
			delete(l.buckets, k)
		}
	}
	l.nextSweep = now.Add(l.sweepEvery)
}

// allowIP 取一个令牌。返回是否放行、建议的 Retry-After 秒数、是否该打日志、
// 以及上次打日志以来被静默掉的次数。
//
// 日志的**判定与计数在锁内、真正的 log.Printf 在锁外**（由调用方做）：持锁写 I/O
// 会让被拒的请求互相排队，又是一条把攻击流量变成自身瓶颈的路径。
func (l *limiter) allowIP(ip string) (ok bool, retryAfter int, shouldLog bool, skipped int) {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweepLocked(now)

	b, exists := l.buckets[ip]
	if !exists {
		b = &bucket{tokens: float64(l.burst)}
		l.buckets[ip] = b
	} else {
		// 惰性补充：按距上次访问经过的秒数补令牌，上限是桶容量。
		b.tokens += now.Sub(b.lastSeen).Seconds() * l.rps
		if b.tokens > float64(l.burst) {
			b.tokens = float64(l.burst)
		}
	}
	b.lastSeen = now

	if b.tokens >= 1 {
		b.tokens--
		return true, 0, false, 0
	}

	// 还要多久才能攒够一个令牌，用于 Retry-After。至少 1 秒——回 0 会让客户端
	// 立刻重试，等于没限。
	retryAfter = max(int((1-b.tokens)/l.rps), 1)

	if now.Before(l.nextLog) {
		l.suppressed++
		return false, retryAfter, false, 0
	}
	skipped = l.suppressed
	l.suppressed = 0
	l.nextLog = now.Add(l.logInterval)
	return false, retryAfter, true, skipped
}

// RateLimit 按客户端 IP 限流。rps 是稳态速率，burst 是桶容量（允许的突发量）。
//
// **它的正确性完全依赖 c.ClientIP() 返回真实客户端 IP。** 而那取决于 gin 的
// SetTrustedProxies 配对：
//
//   - 不设 → gin 信任所有代理 → 任何人加一个 X-Forwarded-For 头就换一个新桶，
//     限流等于不存在。
//   - 设错（比如本项目部署在容器里、却只信任 127.0.0.1）→ gin 不信任真实来源，
//     于是**所有请求的 ClientIP 都是 docker 网桥网关那一个地址**，全站用户共享
//     一个桶，几个人同时注册就集体 429。而本地开发不过容器，完全复现不出来。
//
// 所以被拒时**必须把 ClientIP 打进日志**：那是发现上面第二种误配的唯一现场线索。
// 生产上线后要实际打一次超限，确认日志里是公网 IP 而不是 172.x。
//
// 进程内状态，多实例部署时每个实例各算一份——对"防脚本刷注册"这个目标够用，
// 真要全局精确就得上 Redis，而那会给一个纯防滥用的功能引入一个新的可用性依赖。
func RateLimit(rps float64, burst int) gin.HandlerFunc {
	l := newLimiter(rps, burst)

	return func(c *gin.Context) {
		ip := c.ClientIP()
		ok, retryAfter, shouldLog, skipped := l.allowIP(ip)
		if ok {
			c.Next()
			return
		}

		if shouldLog {
			log.Printf("[ratelimit] 拒绝 %s %s，ClientIP=%s（另有 %d 次未记录）"+
				"——生产环境这里应当是**公网** IP。若反代后面看到的是 172.x 这类内网地址，"+
				"说明 TRUSTED_PROXIES 没覆盖反代的来源，此时全站共用一个桶",
				c.Request.Method, c.Request.URL.Path, ip, skipped)
		}

		c.Header("Retry-After", strconv.Itoa(retryAfter))
		c.AbortWithStatusJSON(http.StatusTooManyRequests,
			gin.H{"code": 42900, "message": "too many requests"})
	}
}
