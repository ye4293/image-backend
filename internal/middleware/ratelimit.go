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

// bucket 是单个 IP 的令牌桶。
//
// 只存"上次补充时刻"与"当前令牌数"，取用时按经过的时间惰性补充——不需要后台
// goroutine 定时给每个桶加令牌（那样 N 个 IP 就是 N 个定时器）。
type bucket struct {
	tokens   float64
	lastSeen time.Time
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
	var (
		mu      sync.Mutex
		buckets = make(map[string]*bucket)
	)

	return func(c *gin.Context) {
		ip := c.ClientIP()
		now := time.Now()

		mu.Lock()
		b, ok := buckets[ip]
		if !ok {
			// 顺带清理过期条目。放在"新 IP 出现"这个时机而不是每次请求都扫：
			// 正常流量下几乎不触发，被扫段时反而扫得最勤，正好是需要它的时候。
			for k, v := range buckets {
				if now.Sub(v.lastSeen) > rateLimitIdleTTL {
					delete(buckets, k)
				}
			}
			b = &bucket{tokens: float64(burst)}
			buckets[ip] = b
		} else {
			// 惰性补充：按距上次访问经过的秒数补令牌，上限是桶容量。
			b.tokens += now.Sub(b.lastSeen).Seconds() * rps
			if b.tokens > float64(burst) {
				b.tokens = float64(burst)
			}
		}
		b.lastSeen = now

		if b.tokens < 1 {
			// 还要多久才能攒够一个令牌，用于 Retry-After。至少 1 秒——回 0 会让
			// 客户端立刻重试，等于没限。
			wait := max(int((1-b.tokens)/rps), 1)
			mu.Unlock()

			log.Printf("[ratelimit] 拒绝 %s %s，ClientIP=%s"+
				"（生产环境这里应当是**公网** IP。若反代后面看到的是 172.x 这类内网地址，"+
				"说明 TRUSTED_PROXIES 没覆盖反代的来源，此时全站共用一个桶）",
				c.Request.Method, c.Request.URL.Path, ip)

			c.Header("Retry-After", strconv.Itoa(wait))
			c.AbortWithStatusJSON(http.StatusTooManyRequests,
				gin.H{"code": 42900, "message": "too many requests"})
			return
		}
		b.tokens--
		mu.Unlock()

		c.Next()
	}
}
