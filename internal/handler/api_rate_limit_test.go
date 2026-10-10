package handler

import (
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
