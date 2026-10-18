package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/MoeclubM/metafusion-storage/internal/store"
	"github.com/google/uuid"
)

func TestAssetReferenceConfirmationUsesDedicatedCredentialAndRejectsRedirects(t *testing.T) {
	ref := store.CatalogAssetReferenceCheck{WorkspaceID: uuid.NewString(), EntityID: uuid.NewString(), AssetID: uuid.NewString()}
	var leaked atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Store(r.Header.Get("Authorization") != "")
		w.WriteHeader(200)
	}))
	defer other.Close()
	redirect := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/storage/asset-references" || r.Header.Get("Authorization") != "Bearer reference-service-test-key" {
			t.Error("wrong internal contract")
		}
		if redirect {
			http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
			return
		}
		var body struct {
			References []store.CatalogAssetReferenceCheck `json:"references"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body.References) != 1 || body.References[0] != ref {
			t.Errorf("reference scope changed: %+v", body.References)
		}
		body.References[0].Referenced = true
		body.References[0].Committed = true
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer server.Close()
	check := New(server.URL).AssetReferences("reference-service-test-key")
	out, err := check(context.Background(), []store.CatalogAssetReferenceCheck{ref})
	if err != nil || len(out) != 1 || !out[0].Committed || !out[0].Referenced {
		t.Fatalf("confirmation: %+v %v", out, err)
	}
	redirect = true
	if _, err = check(context.Background(), []store.CatalogAssetReferenceCheck{ref}); err == nil || leaked.Load() {
		t.Fatal("redirect followed or credential leaked")
	}
	if _, err = New(server.URL).AssetReferences("")(context.Background(), []store.CatalogAssetReferenceCheck{ref}); err == nil {
		t.Fatal("empty service credential accepted")
	}
}
