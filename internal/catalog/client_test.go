package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/MoeclubM/metafusion-storage/internal/upstream"
)

// 合并只广播事件、不改写别人表里的引用：绑定在旧身份上的文件必须靠"跟随重定向"
// 继续可见，否则实体的文件列表会静默少掉一批。
func TestVisibleFollowsMergedIdentity(t *testing.T) {
	const oldID, newID = "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"
	var resolved bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/catalog/entities/" + oldID + "/identity":
			resolved = true
			_ = json.NewEncoder(w).Encode(map[string]any{"canonical_id": newID, "aliases": []string{oldID}, "entity": map[string]any{"id": newID, "kind": "work"}, "complete": true})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	kind, err := New(srv.URL).Visible(context.Background(), oldID, "", "")
	if err != nil || !resolved || kind != "work" {
		t.Fatalf("未跟随合并重定向: kind=%q err=%v resolved=%v", kind, err, resolved)
	}
}

// 真正不可见的实体返回 ErrNotVisible（目录服务的**明确回答**）：跟随重定向不能变成
// "更宽松的可见性"，也不能把明确结论报成依赖故障。
func TestVisibleStillHidesUnresolvableEntity(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "not_found"})
	}))
	defer srv.Close()
	_, err := New(srv.URL).Visible(context.Background(), "33333333-3333-3333-3333-333333333333", "", "")
	if !errors.Is(err, ErrNotVisible) {
		t.Fatalf("不可见实体应返回 ErrNotVisible，实际 err=%v", err)
	}
	// 明确的 not_found 不重试，也不调用旧实体或 /resolve 路径。
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("not_found 应只请求一次，实际请求 %d 次", n)
	}
}

// 上游 5xx：必须报"上游不可用"而不是"不可见"，且重试有界。
// 旧实现两种情况都返回 ok=false，于是目录服务一抖就表现成"这个实体不存在"——
// 绑定在它上面的文件从列表里静默消失，运维在监控里看不到任何故障。
func TestVisibleReportsUpstreamUnavailableWithBoundedRetry(t *testing.T) {
	var hits int32
	var sawResolve atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if strings.HasSuffix(r.URL.Path, "/resolve") {
			sawResolve.Store(true)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_, err := New(srv.URL).Visible(context.Background(), "44444444-4444-4444-4444-444444444444", "", "")
	if err == nil {
		t.Fatal("上游 503 必须返回错误")
	}
	if errors.Is(err, ErrNotVisible) {
		t.Fatalf("上游不可用不能被当成不可见: %v", err)
	}
	if !upstream.IsUnavailable(err) {
		t.Fatalf("错误应包装成上游不可用（调用方据此回 503 机器码）: %v", err)
	}
	// Attempts=2 是有界重试的全部：一次调用最多两次尝试，不打第三次。
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Fatalf("503 应恰好尝试 2 次（策略 Attempts=2），实际 %d 次", n)
	}
	// 问不到上游时不该再去问 /resolve：跟随重定向的前提是"上游明确说 404"。
	if sawResolve.Load() {
		t.Fatal("上游不可用时不该继续请求 /resolve")
	}
}

// 200 但响应不合契约（读不出 kind）：按"问不到上游"报错，不能折成 404——
// 否则上游违约会被伪装成"实体不存在"，谁也发现不了。
func TestVisibleTreatsMalformedBodyAsUpstreamFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not a json body"))
	}))
	defer srv.Close()
	_, err := New(srv.URL).Visible(context.Background(), "55555555-5555-5555-5555-555555555555", "", "")
	if err == nil || errors.Is(err, ErrNotVisible) {
		t.Fatalf("响应违约应报上游失败而不是不可见，实际 err=%v", err)
	}
}

func TestVisibleWithoutBaseReportsDependencyFailure(t *testing.T) {
	for _, base := range []string{"", " \t "} {
		_, err := New(base).Visible(context.Background(), "66666666-6666-6666-6666-666666666666", "", "")
		var upstreamErr *upstream.Error
		if !errors.As(err, &upstreamErr) || upstreamErr.Reason != upstream.ReasonNotConfigured || errors.Is(err, ErrNotVisible) {
			t.Fatalf("未配置目录地址应返回 not_configured 依赖错误，实际 err=%v", err)
		}
	}
}
