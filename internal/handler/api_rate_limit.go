package handler

import (
	"github.com/MoeclubM/metafusion-storage/internal/auth"
	"github.com/gin-gonic/gin"
	"strconv"
	"sync"
	"time"
)

// API 默认按已验证账号与路由计数；匿名按 IP。admin 组跳过业务计数。
func apiRateLimiter() gin.HandlerFunc {
	type window struct {
		start time.Time
		used  int
	}
	var mu sync.Mutex
	buckets := map[string]window{}
	return func(c *gin.Context) {
		key := "ip:" + c.ClientIP()
		if p := auth.Current(c); p != nil {
			if !p.IsThirdParty {
				for _, group := range p.Groups {
					if group == "admin" {
						c.Next()
						return
					}
				}
			}
			key = "user:" + p.ID
		}
		category := "write"
		if c.Request.Method == "GET" || c.Request.Method == "HEAD" {
			category = "read"
		}
		key += "|" + category
		now := time.Now()
		mu.Lock()
		if len(buckets) >= 65536 {
			for k, v := range buckets {
				if now.Sub(v.start) >= time.Minute {
					delete(buckets, k)
				}
			}
		}
		b := buckets[key]
		if now.Sub(b.start) >= time.Minute {
			b = window{start: now}
		}
		b.used++
		buckets[key] = b
		mu.Unlock()
		remaining := max(0, 180-b.used)
		reset := max(1, int(time.Until(b.start.Add(time.Minute)).Seconds())+1)
		c.Header("X-RateLimit-Limit", "180")
		c.Header("X-RateLimit-Remaining", strconv.Itoa(remaining))
		c.Header("X-RateLimit-Reset", strconv.Itoa(reset))
		if b.used > 180 {
			c.Header("Retry-After", strconv.Itoa(reset))
			c.AbortWithStatusJSON(429, gin.H{"error": "rate_limited"})
			return
		}
		c.Next()
	}
}
