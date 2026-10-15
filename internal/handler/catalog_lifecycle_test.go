package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MoeclubM/metafusion-storage/internal/config"
	"github.com/MoeclubM/metafusion-storage/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func TestCatalogLifecycleRequiresConfiguredServiceCredential(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, configured := range []string{"", "service-test-key"} {
		h := New(nil, nil, nil, nil, config.Config{CatalogLifecycleSecret: configured})
		r := gin.New()
		h.registerCatalogLifecycle(r)
		for _, credential := range []string{"", "Bearer user-session", "Bearer ", "Bearer service-test-key"} {
			req := httptest.NewRequest(http.MethodPost, "/internal/catalog/lifecycle", strings.NewReader(`{}`))
			req.Header.Set("Authorization", credential)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			want := http.StatusUnauthorized
			if configured == "" {
				want = http.StatusNotFound
			} else if credential == "Bearer service-test-key" {
				want = http.StatusBadRequest
			}
			if w.Code != want {
				t.Fatalf("configured=%t credential match=%t status=%d want=%d", configured != "", credential == "Bearer service-test-key", w.Code, want)
			}
			req = httptest.NewRequest(http.MethodGet, "/internal/catalog/binding-targets?limit=0", nil)
			req.Header.Set("Authorization", credential)
			w = httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != want {
				t.Fatalf("inventory credential match=%t status=%d want=%d", credential == "Bearer service-test-key", w.Code, want)
			}
		}
	}
}

func TestCatalogLifecycleRejectsIncompleteAliasSnapshots(t *testing.T) {
	id, alias := uuid.NewString(), uuid.NewString()
	state := store.CatalogLifecycle{EntityID: id, Kind: "work", Status: "active", Version: 2,
		UpdatedAt: time.Now(), Aliases: []string{alias}, AliasVersions: map[string]int64{alias: 2}}
	if !validCatalogLifecycle(state) {
		t.Fatal("complete draft snapshot rejected")
	}
	state.AliasVersions = nil
	if validCatalogLifecycle(state) {
		t.Fatal("incomplete alias version map accepted")
	}
	state.AliasVersions = map[string]int64{alias: 2}
	state.Aliases = append(state.Aliases, id)
	if validCatalogLifecycle(state) {
		t.Fatal("canonical identity accepted as its own alias")
	}
}

func TestCatalogBindingInventoryRejectsInvalidCursorAndLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := New(nil, nil, nil, nil, config.Config{CatalogLifecycleSecret: "inventory-test-key"})
	r := gin.New()
	h.registerCatalogLifecycle(r)
	for _, query := range []string{
		"after=not-a-uuid", "after=urn:uuid:11111111-1111-1111-1111-111111111111",
		"after=11111111111111111111111111111111", "limit=101", "limit=-1", "limit=abc",
	} {
		req := httptest.NewRequest(http.MethodGet, "/internal/catalog/binding-targets?"+query, nil)
		req.Header.Set("Authorization", "Bearer inventory-test-key")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid inventory query %q reached the database: status=%d", query, w.Code)
		}
	}
}
