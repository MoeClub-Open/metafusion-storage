package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MoeclubM/metafusion-storage/internal/config"
	"github.com/MoeclubM/metafusion-storage/internal/objects"
	"github.com/MoeclubM/metafusion-storage/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const catalogAssetServiceKey = "catalog-assets-test-key"

func catalogAssetInternalRouter(db *store.Store) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(gin.RecoveryWithWriter(io.Discard))
	New(db, nil, nil, nil, config.Config{CatalogLifecycleSecret: catalogAssetServiceKey}).registerCatalogLifecycle(r)
	return r
}

func catalogAssetInternalRequest(r *gin.Engine, path, credential, payload string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", credential)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func catalogAssetPayload(workspace, entity, asset, actor string, prepare bool) string {
	body, _ := json.Marshal(store.CatalogAssetReferences{WorkspaceID: workspace, Prepare: prepare,
		References: []store.CatalogAssetReference{{EntityID: entity, AssetID: asset, ActorIDs: []string{actor}}}})
	return string(body)
}

func TestCatalogAssetPrepareRequiresInternalCredential(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, configured := range []string{"", catalogAssetServiceKey} {
		r := gin.New()
		New(nil, nil, nil, nil, config.Config{CatalogLifecycleSecret: configured}).registerCatalogLifecycle(r)
		for _, credential := range []string{"", "Bearer user-session", "Bearer ", "Bearer " + catalogAssetServiceKey} {
			w := catalogAssetInternalRequest(r, "/internal/catalog/assets/prepare", credential, `{}`)
			want := http.StatusUnauthorized
			if configured == "" {
				want = http.StatusNotFound
			} else if credential == "Bearer "+catalogAssetServiceKey {
				want = http.StatusBadRequest
			}
			if w.Code != want {
				t.Fatalf("configured=%t authenticated=%t status=%d want=%d", configured != "", credential == "Bearer "+catalogAssetServiceKey, w.Code, want)
			}
		}
	}
}

func TestCatalogAssetPrepareRejectsInvalidPayloadBeforeDatabase(t *testing.T) {
	workspace, entity, asset, actor := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	valid := catalogAssetPayload(workspace, entity, asset, actor, false)
	mutate := func(change func(map[string]any)) string {
		var in map[string]any
		if err := json.Unmarshal([]byte(valid), &in); err != nil {
			t.Fatal(err)
		}
		change(in)
		body, _ := json.Marshal(in)
		return string(body)
	}
	invalid := map[string]string{
		"empty":              `{}`,
		"unknown_field":      mutate(func(in map[string]any) { in["owner_id"] = actor }),
		"invalid_workspace":  mutate(func(in map[string]any) { in["workspace_id"] = "not-a-uuid" }),
		"missing_references": mutate(func(in map[string]any) { delete(in, "references") }),
		"empty_references":   mutate(func(in map[string]any) { in["references"] = []any{} }),
		"invalid_entity":     mutate(func(in map[string]any) { in["references"].([]any)[0].(map[string]any)["entity_id"] = "invalid" }),
		"invalid_asset":      mutate(func(in map[string]any) { in["references"].([]any)[0].(map[string]any)["asset_id"] = "invalid" }),
		"invalid_actor": mutate(func(in map[string]any) {
			in["references"].([]any)[0].(map[string]any)["actor_ids"] = []string{"invalid"}
		}),
		"invalid_moderator":   mutate(func(in map[string]any) { in["references"].([]any)[0].(map[string]any)["moderate_actor_id"] = "invalid" }),
		"duplicate_reference": mutate(func(in map[string]any) { refs := in["references"].([]any); in["references"] = append(refs, refs[0]) }),
		"too_many_authors": mutate(func(in map[string]any) {
			authors := make([]string, 101)
			for i := range authors {
				authors[i] = actor
			}
			in["references"].([]any)[0].(map[string]any)["actor_ids"] = authors
		}),
		"too_many_references": mutate(func(in map[string]any) {
			refs := make([]any, 10001)
			for i := range refs {
				refs[i] = in["references"].([]any)[0]
			}
			in["references"] = refs
		}),
		"trailing_json":    valid + `{}`,
		"trailing_garbage": valid + `invalid`,
	}
	r := catalogAssetInternalRouter(nil)
	for name, payload := range invalid {
		t.Run(name, func(t *testing.T) {
			w := catalogAssetInternalRequest(r, "/internal/catalog/assets/prepare", "Bearer "+catalogAssetServiceKey, payload)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_payload") {
				t.Fatalf("invalid request reached database or wrong failure: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestCatalogAssetPrepareKeepsCoverPrivateUntilLifecycleOnPostgres(t *testing.T) {
	h := newUploadHarness(t, false)
	ownerID, outsiderID, workspace, entity := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	owner, outsider := h.token(ownerID), h.token(outsiderID)
	content := onePixelPNGBytes(t)
	assetID := h.initiateWithMime(t, owner, "fork-cover.png", sha256HexOf(content), int64(len(content)), "image/png")
	if code, body := h.put(t, owner, assetID, content); code != http.StatusOK {
		t.Fatalf("upload cover: %d %s", code, body)
	}
	r := catalogAssetInternalRouter(h.db)
	stage := catalogAssetPayload(workspace, entity, assetID, ownerID, false)
	if w := catalogAssetInternalRequest(r, "/internal/catalog/assets/prepare", "Bearer "+catalogAssetServiceKey, stage); w.Code != http.StatusOK {
		t.Fatalf("stage cover: %d %s", w.Code, w.Body.String())
	}
	assertPrivate := func() {
		t.Helper()
		for _, credential := range []string{"", outsider} {
			if w := h.getContentIfNoneMatch(credential, assetID, `"`+sha256HexOf(content)+`"`); w.Code != http.StatusNotFound || w.Body.Len() == len(content) {
				t.Fatalf("private staged/prepared asset exposed bytes or conditional 304: %d %s", w.Code, w.Body.String())
			}
			if code, body := h.do(credential, http.MethodGet, "/api/storage/assets/"+assetID, ""); code != http.StatusNotFound {
				t.Fatalf("private asset metadata exposed: %d %s", code, body)
			}
		}
		if w := h.getContentIfNoneMatch(owner, assetID, ""); w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), content) {
			t.Fatalf("uploader lost private preview: %d", w.Code)
		}
	}
	assertPrivate()
	prepare := catalogAssetPayload(workspace, entity, assetID, ownerID, true)
	if w := catalogAssetInternalRequest(r, "/internal/catalog/assets/prepare", "Bearer "+catalogAssetServiceKey, prepare); w.Code != http.StatusOK {
		t.Fatalf("prepare cover for merge: %d %s", w.Code, w.Body.String())
	}
	assertPrivate()
	attack := catalogAssetPayload(uuid.NewString(), uuid.NewString(), assetID, outsiderID, true)
	if w := catalogAssetInternalRequest(r, "/internal/catalog/assets/prepare", "Bearer "+catalogAssetServiceKey, attack); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_catalog_asset") {
		t.Fatalf("nonowner claimed private upload: %d %s", w.Code, w.Body.String())
	}
	if w := catalogAssetInternalRequest(r, "/internal/catalog/assets/prepare", "Bearer "+owner, prepare); w.Code != http.StatusUnauthorized {
		t.Fatalf("user token authenticated internal protocol: %d", w.Code)
	}
	state := store.CatalogLifecycle{EntityID: entity, Kind: "work", State: "active", Version: 1, UpdatedAt: time.Now(), CoverAssetIDs: []string{assetID}}
	stateJSON, _ := json.Marshal(state)
	for i := 0; i < 2; i++ {
		if w := catalogAssetInternalRequest(r, "/internal/catalog/lifecycle", "Bearer "+catalogAssetServiceKey, string(stateJSON)); w.Code != http.StatusOK {
			t.Fatalf("merged lifecycle attempt %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	if w := h.getContentIfNoneMatch("", assetID, ""); w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), content) {
		t.Fatalf("merged cover must be anonymously readable: %d", w.Code)
	}
	if bindings, err := h.db.BindingsForAsset(t.Context(), assetID); err != nil || len(bindings) != 1 || bindings[0].CreatedBy != ownerID {
		t.Fatalf("merge duplicated binding or lost uploader attribution: %+v %v", bindings, err)
	}
}

func TestCatalogAssetPrivateStreamChecksScopeAndNeverCachesOnPostgres(t *testing.T) {
	h := newUploadHarness(t, false)
	ctx := t.Context()
	ownerID, workspace, entity := uuid.NewString(), uuid.NewString(), uuid.NewString()
	owner := h.token(ownerID)
	content := onePixelPNGBytes(t)
	assetID := h.initiateWithMime(t, owner, "private-review.png", sha256HexOf(content), int64(len(content)), "image/png")
	if code, body := h.put(t, owner, assetID, content); code != http.StatusOK {
		t.Fatalf("upload cover: %d %s", code, body)
	}
	asset := h.asset(t, assetID)
	// A separate local object adapter lets this fixture register only internal
	// routes, while retaining the same verified asset and original content.
	cfg := config.Config{Root: t.TempDir(), CatalogLifecycleSecret: catalogAssetServiceKey}
	objs, err := objects.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = objs.PutStream(ctx, asset.ObjectKey, bytes.NewReader(content), asset.SHA256); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	New(h.db, objs, nil, nil, cfg).registerCatalogLifecycle(r)
	in := store.CatalogAssetReferences{WorkspaceID: workspace, References: []store.CatalogAssetReference{{EntityID: entity, AssetID: assetID, ActorIDs: []string{ownerID}}}}
	if err = h.db.RetainCatalogAssets(ctx, in); err != nil {
		t.Fatal(err)
	}
	path := func(wid, eid, aid string) string {
		return "/internal/catalog/workspaces/" + wid + "/entities/" + eid + "/assets/" + aid + "/content"
	}
	get := func(target, credential, etag string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("Authorization", credential)
		req.Header.Set("If-None-Match", etag)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	goodPath, service := path(workspace, entity, assetID), "Bearer "+catalogAssetServiceKey
	etag := `"` + asset.SHA256 + `"`
	for _, credential := range []string{"", "Bearer " + owner, "Bearer wrong-service-key"} {
		if w := get(goodPath, credential, etag); w.Code != http.StatusUnauthorized {
			t.Fatalf("conditional request bypassed service credential: %d", w.Code)
		}
	}
	for _, target := range []string{
		path(uuid.NewString(), entity, assetID),
		path(workspace, uuid.NewString(), assetID),
		path(workspace, entity, uuid.NewString()),
		path("invalid", entity, assetID),
		path(workspace, "invalid", assetID),
		path(workspace, entity, "invalid"),
	} {
		if w := get(target, service, etag); w.Code != http.StatusNotFound {
			t.Fatalf("conditional request bypassed exact private scope: %s status=%d", target, w.Code)
		}
	}
	w := get(goodPath, service, "")
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), content) || w.Header().Get("Cache-Control") != "private, no-store" || w.Header().Get("ETag") != etag || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("private preview changed bytes/type or allowed cache: status=%d headers=%v", w.Code, w.Header())
	}
	if w = get(goodPath, service, etag); w.Code != http.StatusNotModified || w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("authorized private conditional response: status=%d headers=%v", w.Code, w.Header())
	}
	// Existing official cover references can be previewed for a new authorized
	// checkout before its own speculative reservation has reached storage.
	if err = h.db.Bind(ctx, store.Binding{ID: uuid.NewString(), AssetID: assetID, TargetEntityID: entity, TargetKind: "work", BindingRole: "cover_image", CreatedBy: ownerID}); err != nil {
		t.Fatal(err)
	}
	if w = get(path(uuid.NewString(), entity, assetID), service, ""); w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), content) {
		t.Fatalf("same-target existing cover proof rejected: status=%d", w.Code)
	}
	if w = get(path(uuid.NewString(), uuid.NewString(), assetID), service, etag); w.Code != http.StatusNotFound {
		t.Fatalf("public asset binding authorized unrelated target: %d", w.Code)
	}
	if err = h.db.SetBlocked(ctx, assetID, "review blocked fixture"); err != nil {
		t.Fatal(err)
	}
	if w = get(goodPath, service, etag); w.Code != http.StatusNotFound {
		t.Fatalf("conditional request bypassed current asset block: %d", w.Code)
	}
}

func TestCatalogAssetPublicWritesCannotBypassMergedCoverFactsOnPostgres(t *testing.T) {
	h := newUploadHarness(t, false)
	ctx := t.Context()
	ownerID, entity, workspace := uuid.NewString(), uuid.NewString(), uuid.NewString()
	owner := h.token(ownerID)
	content := onePixelPNGBytes(t)
	assetID := h.initiateWithMime(t, owner, "catalog-cover.png", sha256HexOf(content), int64(len(content)), "image/png")
	if code, body := h.put(t, owner, assetID, content); code != http.StatusOK {
		t.Fatalf("upload cover: %d %s", code, body)
	}
	post := func(path string, payload any) (int, string) {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return h.do(owner, http.MethodPost, path, string(raw))
	}
	assertNoBindings := func() {
		t.Helper()
		if bindings, err := h.db.BindingsForAsset(ctx, assetID); err != nil || len(bindings) != 0 {
			t.Fatalf("rejected public cover write created binding: %+v %v", bindings, err)
		}
	}
	if code, body := post("/api/storage/bind", map[string]any{"asset_id": assetID, "target_entity_id": entity, "binding_role": "cover_image"}); code != http.StatusConflict || !strings.Contains(body, "catalog_managed_binding") {
		t.Fatalf("public bind bypassed catalog merge: %d %s", code, body)
	}
	assertNoBindings()
	if code, body := post("/api/storage/upload/initiate", map[string]any{
		"file_name": "catalog-cover.png", "file_size": len(content), "sha256_hash": sha256HexOf(content),
		"mime_type": "image/png", "target_entity_id": entity, "binding_role": "cover_image",
	}); code != http.StatusConflict || !strings.Contains(body, "catalog_managed_binding") {
		t.Fatalf("targeted upload bypassed catalog merge: %d %s", code, body)
	}
	assertNoBindings()
	in := store.CatalogAssetReferences{WorkspaceID: workspace, References: []store.CatalogAssetReference{{EntityID: entity, AssetID: assetID, ActorIDs: []string{ownerID}}}}
	if err := h.db.RetainCatalogAssets(ctx, in); err != nil {
		t.Fatal(err)
	}
	in.Prepare = true
	if err := h.db.RetainCatalogAssets(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := h.db.ApplyCatalogLifecycle(ctx, store.CatalogLifecycle{EntityID: entity, Kind: "work", State: "active", Version: 1, UpdatedAt: time.Now(), CoverAssetIDs: []string{assetID}}); err != nil {
		t.Fatal(err)
	}
	cover, err := h.db.BindingByReference(ctx, assetID, entity, "cover_image")
	if err != nil {
		t.Fatal(err)
	}
	if code, body := h.do(owner, http.MethodDelete, "/api/storage/bindings/"+cover.ID, ""); code != http.StatusConflict || !strings.Contains(body, "catalog_managed_binding") {
		t.Fatalf("public unbind revoked merged cover: %d %s", code, body)
	}
	if preserved, err := h.db.BindingByReference(ctx, assetID, entity, "cover_image"); err != nil || preserved.ID != cover.ID {
		t.Fatalf("rejected cover unbind changed binding: %+v %v", preserved, err)
	}
	if w := h.getContentIfNoneMatch("", assetID, ""); w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), content) {
		t.Fatalf("rejected cover unbind broke anonymous image: %d", w.Code)
	}
	code, body := post("/api/storage/bind", map[string]any{"asset_id": assetID, "target_entity_id": entity, "binding_role": "scans"})
	if code != http.StatusOK {
		t.Fatalf("ordinary file binding stopped working: %d %s", code, body)
	}
	var result struct {
		Binding store.Binding `json:"binding"`
	}
	if err = json.Unmarshal([]byte(body), &result); err != nil || result.Binding.ID == "" || result.Binding.BindingRole != "scans" {
		t.Fatalf("ordinary binding response malformed: %s err=%v", body, err)
	}
	if code, body = h.do(owner, http.MethodDelete, "/api/storage/bindings/"+result.Binding.ID, ""); code != http.StatusOK {
		t.Fatalf("ordinary file unbind stopped working: %d %s", code, body)
	}
	if bindings, err := h.db.BindingsForAsset(ctx, assetID); err != nil || len(bindings) != 1 || bindings[0].ID != cover.ID {
		t.Fatalf("ordinary unbind changed catalog-managed cover: %+v %v", bindings, err)
	}
}
