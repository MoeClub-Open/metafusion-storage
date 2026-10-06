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
	"github.com/MoeclubM/metafusion-storage/internal/upstream"
)

// 目录服务不可用必须回 503 + upstream_unavailable，而不是伪装成"实体不存在"的 404。
// 三种上游形态各自对应一种结论：503 与"响应违约"都是依赖故障，只有目录的明确回答才是 404。
func TestEntityFilesReportsUpstreamUnavailable(t *testing.T) {
	cases := []struct {
		name     string
		catalog  http.HandlerFunc
		wantCode int
		wantErr  string
	}{
		{
			name:     "目录服务 503",
			catalog:  func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) },
			wantCode: http.StatusServiceUnavailable,
			wantErr:  upstream.CodeUpstreamUnavailable,
		},
		{
			name:     "目录服务 200 但响应违约",
			catalog:  func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("not a json body")) },
			wantCode: http.StatusServiceUnavailable,
			wantErr:  upstream.CodeUpstreamUnavailable,
		},
		{
			name: "目录服务明确说不可见",
			catalog: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "not_found"})
			},
			wantCode: http.StatusNotFound,
			wantErr:  "not_found",
		},
		{
			name:     "目录身份接口缺失",
			catalog:  func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
			wantCode: http.StatusServiceUnavailable,
			wantErr:  upstream.CodeUpstreamUnavailable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cat := httptest.NewServer(tc.catalog)
			defer cat.Close()

			gin.SetMode(gin.TestMode)
			r := gin.New()
			// 可见性判定在触碰数据库**之前**完成，因此空库足够判定这条分支
			// （与 TestAuthBoundaryBeforeDatabase 同一装配）。
			New(&store.Store{}, &objects.Store{}, catalog.New(cat.URL), newVerifier(t, "http://127.0.0.1:1/jwks"), config.Config{}).Register(r)

			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/storage/entities/"+uuid.NewString()+"/files", nil))
			if w.Code != tc.wantCode {
				t.Fatalf("状态码 %d，期望 %d（%s）", w.Code, tc.wantCode, w.Body.String())
			}
			// 响应体形状必须与文件里其它错误一致：{\"error\": \"<机器码>\"}，前端与 bot 按码分支。
			var got struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("解析响应失败: %v", err)
			}
			if got.Error != tc.wantErr {
				t.Fatalf("错误码 %q，期望 %q", got.Error, tc.wantErr)
			}
		})
	}
}

// 同一条判定的另一个调用点：读文件（readable）问不到目录服务时也必须回 503，
// 而不是把依赖故障折成 404。这条路径要真库（assets + bindings 两行）。
func TestAssetReadReportsUpstreamUnavailable(t *testing.T) {
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

	// 匿名请求者不是上传者，因此 readable 必须去问目录服务——这正是被测的那一步。
	assetID, entityID := uuid.NewString(), uuid.NewString()
	sum := sha256.Sum256([]byte(assetID))
	if err = st.CreateAsset(ctx, store.Asset{
		ID: assetID, SHA256: hex.EncodeToString(sum[:]), SizeBytes: 3, DeclaredSize: 3,
		MimeType: "application/octet-stream", FileName: "probe.bin", ObjectKey: "probe/" + assetID,
		Status: "complete", UploaderID: uuid.NewString(),
	}); err != nil {
		t.Fatalf("create asset: %v", err)
	}
	if err = st.Bind(ctx, store.Binding{
		ID: uuid.NewString(), AssetID: assetID, TargetEntityID: entityID,
		TargetKind: "track", BindingRole: "track_audio", CreatedBy: uuid.NewString(),
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.DB().ExecContext(context.Background(), "DELETE FROM storage.bindings WHERE asset_id=$1", assetID)
		_, _ = st.DB().ExecContext(context.Background(), "DELETE FROM storage.assets WHERE id=$1", assetID)
	})

	for _, tc := range []struct {
		name     string
		catalog  http.HandlerFunc
		wantCode int
		wantErr  string
	}{
		{
			name:     "目录服务不可用",
			catalog:  func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) },
			wantCode: http.StatusServiceUnavailable,
			wantErr:  upstream.CodeUpstreamUnavailable,
		},
		{
			name: "绑定目标可见",
			catalog: func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"canonical_id": entityID, "aliases": []string{}, "complete": true,
					"entity": map[string]any{"id": entityID, "kind": "track"},
				})
			},
			wantCode: http.StatusOK,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cat := httptest.NewServer(tc.catalog)
			defer cat.Close()
			gin.SetMode(gin.TestMode)
			r := gin.New()
			New(st, &objects.Store{}, catalog.New(cat.URL), newVerifier(t, "http://127.0.0.1:1/jwks"), config.Config{}).Register(r)

			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/storage/assets/"+assetID, nil))
			if w.Code != tc.wantCode {
				t.Fatalf("状态码 %d，期望 %d（%s）", w.Code, tc.wantCode, w.Body.String())
			}
			if tc.wantErr == "" {
				return
			}
			var got struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("解析响应失败: %v", err)
			}
			if got.Error != tc.wantErr {
				t.Fatalf("错误码 %q，期望 %q", got.Error, tc.wantErr)
			}
		})
	}
}
