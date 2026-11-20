package handler

import (
	"github.com/MoeclubM/metafusion-storage/internal/config"
	"github.com/gin-gonic/gin"
	"net/http/httptest"
	"testing"
)

func TestAPIRateLimitStopsWritesAndSeparatesReads(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(apiRateLimiter())
	writes := 0
	r.POST("/api/test", func(c *gin.Context) { writes++; c.Status(200) })
	r.GET("/api/test", func(c *gin.Context) { c.Status(200) })
	for i := 0; i < 181; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("POST", "/api/test", nil))
		want := 200
		if i == 180 {
			want = 429
		}
		if w.Code != want {
			t.Fatalf("request %d status %d want %d", i, w.Code, want)
		}
	}
	if writes != 180 {
		t.Fatalf("limited request executed: %d writes", writes)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/test", nil))
	if w.Code != 200 {
		t.Fatal("read budget shared with writes")
	}
}

func TestAPIRateLimitConfigurableAndUnlimited(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(apiRateLimiterWithConfig(config.Config{
		RateLimitConfigured: true,
		RateLimitReadRPM:    0,
		RateLimitWriteRPM:   4,
	}))
	r.GET("/api/test", func(c *gin.Context) { c.Status(200) })
	r.POST("/api/test", func(c *gin.Context) { c.Status(200) })

	for i := 0; i < 500; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/api/test", nil))
		if w.Code != 200 {
			t.Fatalf("unlimited read limited at %d: status=%d", i, w.Code)
		}
	}
	for i := 0; i < 5; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("POST", "/api/test", nil))
		if i < 4 && (w.Code != 200 || w.Header().Get("X-RateLimit-Limit") != "4") {
			t.Fatalf("write %d expected 200 and limit 4, got code=%d header=%q", i, w.Code, w.Header().Get("X-RateLimit-Limit"))
		}
		if i == 4 && w.Code != 429 {
			t.Fatalf("write 5 expected 429, got %d", w.Code)
		}
	}
}
