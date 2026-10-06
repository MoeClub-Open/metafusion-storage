package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MoeclubM/metafusion-storage/internal/upstream"
)

// X01 身份契约：identity 端点一次返回存活身份与历史别名（只读投影，不改写引用）。
func TestIdentityReturnsCanonicalAndAliases(t *testing.T) {
	const oldID = "11111111-1111-1111-1111-111111111111"
	const newID = "22222222-2222-2222-2222-222222222222"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/catalog/entities/"+oldID+"/identity" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"canonical_id": newID,
			"aliases":      []string{oldID},
			"complete":     true,
			"entity":       map[string]any{"id": newID, "kind": "work"},
		})
	}))
	defer srv.Close()

	v, err := New(srv.URL).Identity(context.Background(), oldID, "", "")
	if err != nil {
		t.Fatalf("Identity 失败: %v", err)
	}
	if v.CanonicalID != newID || v.Entity.Kind != "work" || !v.Complete {
		t.Fatalf("存活身份不符: %+v", v)
	}
	if len(v.Aliases) != 1 || v.Aliases[0] != oldID {
		t.Fatalf("历史别名不符: %+v", v.Aliases)
	}
	if got := AliasSet(v.CanonicalID, append([]string{oldID}, v.Aliases...)...); len(got) != 2 {
		t.Fatalf("聚合集合应含请求 ID 与 canonical（去重）：%v", got)
	}
}

// 旧目录缺少 identity 是协议错误，不能退回不完整的别名集合。
func TestIdentityRejectsLegacyDirectory(t *testing.T) {
	const oldID = "33333333-3333-3333-3333-333333333333"
	const newID = "44444444-4444-4444-4444-444444444444"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/catalog/entities/" + oldID + "/resolve":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": newID, "kind": "track"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	_, err := New(srv.URL).Identity(context.Background(), oldID, "", "")
	if err == nil || errors.Is(err, ErrNotVisible) {
		t.Fatalf("缺失接口应返回依赖错误，实际 %v", err)
	}
}

// 真正不可见的实体返回明确的 not_found，按 ErrNotVisible 处理。
func TestIdentityStillHidesUnresolvableEntity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "not_found"})
	}))
	defer srv.Close()
	if _, err := New(srv.URL).Identity(context.Background(), "55555555-5555-5555-5555-555555555555", "", ""); !errors.Is(err, ErrNotVisible) {
		t.Fatalf("不可见实体应返回 ErrNotVisible，实际 err=%v", err)
	}
}

// AliasSet：{canonical + 全部请求 ID} 去重，空串丢弃（与互动侧同一契约）。
func TestAliasSetDedups(t *testing.T) {
	got := AliasSet("d", "a", "d", "", "b", "a")
	want := []string{"a", "d", "b"}
	if len(got) != len(want) {
		t.Fatalf("AliasSet 去重不符：%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AliasSet 顺序不符：%v", got)
		}
	}
	if got := AliasSet("d", "d"); len(got) != 1 || got[0] != "d" {
		t.Fatalf("单 ID 应退化为自身：%v", got)
	}
}

func TestIdentityRejectsIncompleteAliasSet(t *testing.T) {
	for _, test := range []struct{ name, body string }{
		{"missing completeness marker", `{"canonical_id":"d","aliases":["a"],"entity":{"id":"d","kind":"work"}}`},
		{"partial alias set", `{"canonical_id":"d","aliases":["a"],"entity":{"id":"d","kind":"work"},"complete":false}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			hits := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			v, err := New(server.URL).Identity(context.Background(), "a", "", "")
			var upstreamErr *upstream.Error
			if !errors.As(err, &upstreamErr) || upstreamErr.Reason != upstream.ReasonBadResponse || v.CanonicalID != "" {
				t.Fatalf("前向或部分身份响应必须拒绝聚合：v=%+v err=%v", v, err)
			}
			if hits != 1 {
				t.Fatalf("契约失败不得触发旧接口回退：hits=%d", hits)
			}
		})
	}
}

func TestIdentityContractFailuresAreNotVisibilityDecisions(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"missing endpoint", 404, `404 page not found`},
		{"forbidden", 403, `{"error":"forbidden"}`},
		{"empty kind", 200, `{"canonical_id":"a","entity":{"id":"a"}}`},
		{"conflicting canonical", 200, `{"canonical_id":"a","entity":{"id":"b","kind":"work"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			hits := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			_, err := New(server.URL).Visible(context.Background(), "a", "", "")
			if err == nil || errors.Is(err, ErrNotVisible) {
				t.Fatalf("protocol failure became a visibility decision: %v", err)
			}
			if hits != 1 {
				t.Fatalf("unexpected legacy fallback: %d requests", hits)
			}
		})
	}
}
