package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/MoeclubM/metafusion-storage/internal/catalog"
	"github.com/MoeclubM/metafusion-storage/internal/config"
	"github.com/MoeclubM/metafusion-storage/internal/objects"
	"github.com/MoeclubM/metafusion-storage/internal/store"
	"github.com/MoeclubM/metafusion-storage/internal/testutil"
)

// X01 全量验收：A→C、B→C 两个分支再 C→D，绑定仍挂在旧 ID 上（合并只广播事件、
// 不改写引用），从存活身份 D 必须一次看见此前全部文件且按绑定 ID 去重。
// 目录 identity 已返回历史别名全集（正向链 + 反向遍历，去重），本用例的假目录按此
// 契约返回：从 D 与从分支旧 ID 查都收齐 {A B C}。
func TestListEntityFilesAggregatesFullAliasSet(t *testing.T) {
	dsn := testutil.DSN(t) // 未设置 STORAGE_TEST_DSN 时跳过
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err = st.Init(ctx); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	aID, bID, cID, dID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	mkAsset := func(name string) string {
		t.Helper()
		id := uuid.NewString()
		sum := sha256.Sum256([]byte(id))
		if err := st.CreateAsset(ctx, store.Asset{
			ID: id, SHA256: hex.EncodeToString(sum[:]), SizeBytes: 3, DeclaredSize: 3,
			MimeType: "application/octet-stream", FileName: name, ObjectKey: "probe/" + id,
			Status: "complete", UploaderID: uuid.NewString(),
		}); err != nil {
			t.Fatalf("create asset: %v", err)
		}
		return id
	}
	assets := map[string]string{} // target entity -> asset
	for target, name := range map[string]string{aID: "a.flac", bID: "b.flac", cID: "c.flac", dID: "d.flac"} {
		assetID := mkAsset(name)
		assets[target] = assetID
		if err := st.Bind(ctx, store.Binding{
			ID: uuid.NewString(), AssetID: assetID, TargetEntityID: target,
			TargetKind: "track", BindingRole: "track_audio", CreatedBy: uuid.NewString(),
		}); err != nil {
			t.Fatalf("bind: %v", err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		for _, assetID := range assets {
			_, _ = st.DB().ExecContext(bg, "DELETE FROM storage.bindings WHERE asset_id=$1", assetID)
			_, _ = st.DB().ExecContext(bg, "DELETE FROM storage.assets WHERE id=$1", assetID)
		}
	})

	// 假目录按 identity 契约返回全集：从 D 与从任一历史 ID 查，canonical 都是 D，
	// aliases 都是 {A B C}（目录 31c63e4：正向链 + 反向遍历，去重）。
	fullAliases := []string{aID, bID, cID}
	cat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		entity := func(id string) map[string]any { return map[string]any{"id": id, "kind": "track"} }
		for _, id := range []string{aID, bID, cID, dID} {
			if p == "/api/catalog/entities/"+id+"/identity" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"canonical_id": dID, "aliases": fullAliases, "entity": entity(dID), "complete": true,
				})
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "not_found"})
	}))
	defer cat.Close()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	New(st, &objects.Store{}, catalog.New(cat.URL), newVerifier(t, "http://127.0.0.1:1/jwks"), config.Config{}).Register(r)
	assetSetOf := func(entityID string) map[string]bool {
		t.Helper()
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/storage/entities/"+entityID+"/files", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s/files 返回 %d（%s）", entityID, w.Code, w.Body.String())
		}
		var out struct {
			Files []map[string]any `json:"files"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("解析文件列表失败: %v", err)
		}
		assets, bindings := map[string]bool{}, map[string]bool{}
		for _, f := range out.Files {
			if id, _ := f["id"].(string); id != "" {
				if bindings[id] {
					t.Fatalf("绑定 %s 重复出现", id)
				}
				bindings[id] = true
			}
			if a, _ := f["asset"].(map[string]any); a != nil {
				if id, _ := a["id"].(string); id != "" {
					assets[id] = true
				}
			}
		}
		return assets
	}

	want := map[string]bool{}
	for _, assetID := range assets {
		want[assetID] = true
	}
	// 从存活身份 D：A/B/C/D 四处绑定一次可见，且无重复。
	if got := assetSetOf(dID); len(got) != 4 {
		t.Fatalf("从 D 应看见全部 4 份文件，实际 %d（%v）", len(got), got)
	} else {
		for id := range want {
			if !got[id] {
				t.Fatalf("从 D 缺少资产 %s（实际 %v）", id, got)
			}
		}
	}
	// 从分支旧 ID：同样收齐全集。
	if got := assetSetOf(aID); len(got) != 4 {
		t.Fatalf("从分支旧 ID 应收齐全集，实际 %d（%v）", len(got), got)
	}
}
