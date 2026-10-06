package handler

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/MoeclubM/metafusion-storage/internal/auth"
	"github.com/MoeclubM/metafusion-storage/internal/catalog"
	"github.com/MoeclubM/metafusion-storage/internal/config"
	"github.com/MoeclubM/metafusion-storage/internal/objects"
	"github.com/MoeclubM/metafusion-storage/internal/store"
	"github.com/MoeclubM/metafusion-storage/internal/testutil"
)

// S02 双用户复用回归：摘要是内容标识，不是授权凭证。
// A 的私有绑定文件对 B 不可读时，B 即使知道 sha256 并持有 upload 码，
// 经 verify-hash / initiate（带或不带目标）/ bind / download 均不得泄露或取得；
// 公开文件与本人重试仍正常。字节去重（同 sha 同键）与授权（readable + 所有者/审核）分开判定。
func TestReuseRequiresReadabilityAndRebindPermission(t *testing.T) {
	dsn := testutil.DSN(t)
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err = st.Init(ctx); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	db := testutil.Database(t)
	if _, err = db.ExecContext(ctx, "DELETE FROM storage.bindings"); err != nil {
		t.Fatalf("clean bindings: %v", err)
	}
	if _, err = db.ExecContext(ctx, "DELETE FROM storage.assets"); err != nil {
		t.Fatalf("clean assets: %v", err)
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	jwks, kid := jwksServer(t, key)
	verifier := newVerifier(t, jwks.URL)

	sign := func(subject string) string {
		payload := jwt.MapClaims{
			"sub":                subject,
			"preferred_username": "tester-" + subject[:8],
			"token_use":          "session",
			"permissions":        []string{auth.PermissionAssetUpload},
			"iss":                testIssuer,
			"aud":                testAudience,
			"exp":                time.Now().Add(10 * time.Minute).Unix(),
			"iat":                time.Now().Unix(),
		}
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, payload)
		token.Header["kid"] = kid
		signed, err := token.SignedString(key)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return signed
	}
	aliceSub, bobSub := uuid.NewString(), uuid.NewString()
	aToken, bToken := sign(aliceSub), sign(bobSub)

	privateEntity, publicEntity, bobEntity := uuid.NewString(), uuid.NewString(), uuid.NewString()
	cat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/catalog/entities/"), "/identity")
		if strings.Contains(r.URL.Path, privateEntity) {
			// 私有实体仅对 A 可见：B 与匿名一律按不存在处理，不泄露存在性。
			if r.Header.Get("Authorization") != "Bearer "+aToken {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "not_found"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"canonical_id": privateEntity, "aliases": []string{}, "complete": true,
				"entity": map[string]any{"id": privateEntity, "kind": "track"},
			})
			return
		}
		// 公开实体与 B 自己的目标实体对任何请求者可见。
		_ = json.NewEncoder(w).Encode(map[string]any{
			"canonical_id": id, "aliases": []string{}, "complete": true,
			"entity": map[string]any{"id": id, "kind": "track"},
		})
	}))
	t.Cleanup(cat.Close)

	cfg := config.Config{Root: t.TempDir(), S3Bucket: "metafusion-test", PresignTTL: 5 * time.Minute, MaxPartCount: 10000}
	objs, err := objects.New(ctx, cfg)
	if err != nil {
		t.Fatalf("objects.New: %v", err)
	}
	if !objs.Local() {
		t.Fatalf("本用例只跑本地对象模式（复用鉴权与对象模式无关）")
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	New(st, objs, catalog.New(cat.URL), verifier, cfg).Register(r)
	do := func(token, method, path, body string) (int, string) {
		t.Helper()
		var req *http.Request
		if body != "" {
			req = httptest.NewRequest(method, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
		} else {
			req = httptest.NewRequest(method, path, nil)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	post := func(token, path string, payload map[string]any) (int, string) {
		t.Helper()
		raw, _ := json.Marshal(payload)
		return do(token, http.MethodPost, path, string(raw))
	}
	putBytes := func(token, assetID string, content []byte) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, "/api/storage/upload/stream/"+assetID, strings.NewReader(string(content)))
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	uploadComplete := func(ownerToken, name string, content []byte, target string) string {
		t.Helper()
		sha := sha256HexOf(content)
		code, resp := post(ownerToken, "/api/storage/upload/initiate", map[string]any{"file_name": name, "file_size": int64(len(content)), "sha256_hash": sha})
		if code != 200 {
			t.Fatalf("initiate %s 返回 %d（%s）", name, code, resp)
		}
		var out struct {
			AssetID string `json:"asset_id"`
		}
		if err := json.Unmarshal([]byte(resp), &out); err != nil || out.AssetID == "" {
			t.Fatalf("解析 initiate 失败: %v（%s）", err, resp)
		}
		if code, resp := putBytes(ownerToken, out.AssetID, content); code != 200 {
			t.Fatalf("stream 上传返回 %d（%s）", code, resp)
		}
		if target != "" {
			if code, resp := post(ownerToken, "/api/storage/bind", map[string]any{"asset_id": out.AssetID, "target_entity_id": target, "binding_role": "track_audio"}); code != 200 {
				t.Fatalf("绑定返回 %d（%s）", code, resp)
			}
		}
		return out.AssetID
	}
	bindingsOf := func(assetID string) int {
		bs, err := st.BindingsForAsset(ctx, assetID)
		if err != nil {
			t.Fatalf("查绑定: %v", err)
		}
		return len(bs)
	}

	// A 上传私有文件并绑到仅 A 可见的实体。
	privateBody := []byte("s02-private-content-0001")
	privateSHA := sha256HexOf(privateBody)
	privateAsset := uploadComplete(aToken, "private.flac", privateBody, privateEntity)

	// B 的探测均不得泄露或取得。
	if code, resp := post(bToken, "/api/storage/verify-hash", map[string]any{"sha256_hash": privateSHA}); code != 200 || !strings.Contains(resp, "\"exists\":false") || strings.Contains(resp, privateAsset) {
		t.Fatalf("B 探测私有摘要应 exists:false 且不带资产 id，实际 %d（%s）", code, resp)
	}
	if code, resp := post(bToken, "/api/storage/upload/initiate", map[string]any{"file_name": "guess.flac", "file_size": 24, "sha256_hash": privateSHA}); code != 404 || strings.Contains(resp, privateAsset) || strings.Contains(resp, "is_instant_upload") {
		t.Fatalf("B 秒传私有摘要应 404 且不返元数据，实际 %d（%s）", code, resp)
	}
	if n := bindingsOf(privateAsset); n != 1 {
		t.Fatalf("前置绑定数应为 1，实际 %d", n)
	}
	if code, resp := post(bToken, "/api/storage/upload/initiate", map[string]any{"file_name": "guess.flac", "file_size": 24, "sha256_hash": privateSHA, "target_entity_id": bobEntity}); code != 404 || strings.Contains(resp, privateAsset) {
		t.Fatalf("B 带目标秒传私有摘要应 404 且不建绑定，实际 %d（%s）", code, resp)
	}
	if n := bindingsOf(privateAsset); n != 1 {
		t.Fatalf("无权秒传不得新增绑定，实际 %d 条", n)
	}
	if code, resp := post(bToken, "/api/storage/bind", map[string]any{"asset_id": privateAsset, "target_entity_id": bobEntity, "binding_role": "track_audio"}); code != 404 {
		t.Fatalf("B 直接绑定私有资产应 404（不探存在性），实际 %d（%s）", code, resp)
	}
	if n := bindingsOf(privateAsset); n != 1 {
		t.Fatalf("无权绑定不得新增绑定，实际 %d 条", n)
	}
	for _, path := range []string{"/api/storage/assets/" + privateAsset, "/api/storage/download/" + privateAsset, "/api/storage/assets/" + privateAsset + "/content"} {
		if code, resp := do(bToken, http.MethodGet, path, ""); code != 404 {
			t.Fatalf("B 读私有资产 %s 应 404，实际 %d（%s）", path, code, resp)
		}
	}

	// 本人重试仍正常。
	if code, resp := post(aToken, "/api/storage/upload/initiate", map[string]any{"file_name": "private.flac", "file_size": 24, "sha256_hash": privateSHA}); code != 200 || !strings.Contains(resp, privateAsset) || !strings.Contains(resp, "\"is_instant_upload\":true") {
		t.Fatalf("A 重试私有文件应秒传命中自己资产，实际 %d（%s）", code, resp)
	}

	// 公开文件仍正常：B 可探测、可秒传元数据、可下载；新增绑定仍需所有者或审核。
	publicBody := []byte("s02-public-content-0002!")
	publicSHA := sha256HexOf(publicBody)
	publicAsset := uploadComplete(aToken, "public.flac", publicBody, publicEntity)
	if code, resp := post(bToken, "/api/storage/verify-hash", map[string]any{"sha256_hash": publicSHA}); code != 200 || !strings.Contains(resp, "\"exists\":true") {
		t.Fatalf("B 探测公开摘要应 exists:true，实际 %d（%s）", code, resp)
	}
	if code, resp := post(bToken, "/api/storage/upload/initiate", map[string]any{"file_name": "public.flac", "file_size": 24, "sha256_hash": publicSHA}); code != 200 || !strings.Contains(resp, publicAsset) || !strings.Contains(resp, "\"is_instant_upload\":true") {
		t.Fatalf("B 秒传公开文件应命中元数据，实际 %d（%s）", code, resp)
	}
	if code, resp := post(bToken, "/api/storage/upload/initiate", map[string]any{"file_name": "public.flac", "file_size": 24, "sha256_hash": publicSHA, "target_entity_id": bobEntity}); code != 403 {
		t.Fatalf("B 给公开文件加绑定应 403（可读但无再绑定权限），实际 %d（%s）", code, resp)
	}
	if code, resp := post(bToken, "/api/storage/bind", map[string]any{"asset_id": publicAsset, "target_entity_id": bobEntity, "binding_role": "track_audio"}); code != 403 {
		t.Fatalf("B 直接绑定公开资产应 403，实际 %d（%s）", code, resp)
	}
	if code, _ := do(bToken, http.MethodGet, "/api/storage/download/"+publicAsset, ""); code != 200 {
		t.Fatalf("B 下载公开文件应 200，实际 %d", code)
	}
}
