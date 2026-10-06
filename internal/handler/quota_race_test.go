package handler

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/MoeclubM/metafusion-storage/internal/auth"
	"github.com/MoeclubM/metafusion-storage/internal/catalog"
	"github.com/MoeclubM/metafusion-storage/internal/config"
	"github.com/MoeclubM/metafusion-storage/internal/maintenance"
	"github.com/MoeclubM/metafusion-storage/internal/objects"
	"github.com/MoeclubM/metafusion-storage/internal/store"
	"github.com/MoeclubM/metafusion-storage/internal/testutil"
)

// quotaHarness 是带配额的最小端到端脚手架：真实路由 + 真实落库 + 真实对象（本地/假 S3）。
type quotaHarness struct {
	t     *testing.T
	r     *gin.Engine
	db    *store.Store
	cfg   config.Config
	key   *rsa.PrivateKey
	kid   string
	s3    *fakeHarnessS3
	local bool
}

func newQuotaHarness(t *testing.T, useS3 bool) *quotaHarness {
	t.Helper()
	db := testutil.Database(t)
	st, err := store.Open(context.Background(), testutil.DSN(t))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err = st.Init(ctx); err != nil {
		t.Fatalf("init schema: %v", err)
	}
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
	h := &quotaHarness{t: t, db: st, key: key, kid: kid}
	cfg := config.Config{
		Root: t.TempDir(), S3Bucket: "metafusion-test",
		PresignTTL: 5 * time.Minute, MaxPartCount: 10000,
		UserQuotaMB: 1,
	}
	if useS3 {
		h.s3 = newFakeHarnessS3(t)
		cfg.S3Endpoint = h.s3.addr
		cfg.S3PublicEndpoint = h.s3.addr
		cfg.S3AccessKey = "test-key"
		cfg.S3SecretKey = "test-secret"
	} else {
		h.local = true
	}
	h.cfg = cfg
	objs, err := objects.New(ctx, cfg)
	if err != nil {
		t.Fatalf("objects.New: %v", err)
	}
	cat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/catalog/entities/"), "/identity")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"canonical_id": id, "aliases": []string{}, "complete": true,
			"entity": map[string]any{"id": id, "kind": "track"},
		})
	}))
	t.Cleanup(cat.Close)
	gin.SetMode(gin.TestMode)
	h.r = gin.New()
	New(st, objs, catalog.New(cat.URL), newVerifier(t, jwks.URL), cfg).Register(h.r)
	return h
}

func (h *quotaHarness) token(subject string) string {
	h.t.Helper()
	payload := jwt.MapClaims{
		"sub": subject, "preferred_username": "tester", "token_use": "session",
		"permissions": []string{auth.PermissionAssetUpload},
		"iss":         testIssuer, "aud": testAudience,
		"exp": time.Now().Add(10 * time.Minute).Unix(), "iat": time.Now().Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, payload)
	token.Header["kid"] = h.kid
	signed, err := token.SignedString(h.key)
	if err != nil {
		h.t.Fatalf("sign: %v", err)
	}
	return signed
}

func (h *quotaHarness) do(token, method, path, body string) (int, string) {
	h.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

func (h *quotaHarness) initiateRaw(token string, size int64, sha string) (int, string) {
	h.t.Helper()
	body, _ := json.Marshal(map[string]any{"file_name": "q.bin", "file_size": size, "sha256_hash": sha})
	return h.do(token, http.MethodPost, "/api/storage/upload/initiate", string(body))
}

func (h *quotaHarness) assetID(resp string) string {
	h.t.Helper()
	var out struct {
		AssetID string `json:"asset_id"`
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil || out.AssetID == "" {
		h.t.Fatalf("initiate 响应无 asset_id: %s err=%v", resp, err)
	}
	return out.AssetID
}

func quotaSHA(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// 并发初始化：配额只够一份时恰好一份 200，其余 413（预留与占位同事务，无超发）。
func TestQuotaConcurrentInitiateSingleWinner(t *testing.T) {
	h := newQuotaHarness(t, false)
	token := h.token("77777777-7777-7777-7777-777777777777")
	mb := int64(1) << 20
	const n = 5
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sha := quotaSHA([]byte(fmt.Sprintf("concurrent-%d-%s", i, uuid.NewString())))
			code, _ := h.initiateRaw(token, mb, sha)
			codes[i] = code
		}(i)
	}
	wg.Wait()
	ok, over := 0, 0
	for _, c := range codes {
		switch c {
		case 200:
			ok++
		case 413:
			over++
		default:
			t.Fatalf("并发初始化应只有 200/413，实际 %v", codes)
		}
	}
	if ok != 1 || over != n-1 {
		t.Fatalf("配额只够一份时应恰好一份成功，实际 %v", codes)
	}
}

// 0 声明的流式落定按实际兑现：超预算即 413 且资产保持 pending；空文件仍可过。
func TestQuotaZeroDeclaredStreamRedeem(t *testing.T) {
	h := newQuotaHarness(t, false)
	token := h.token("77777777-7777-7777-7777-777777777777")
	ctx := context.Background()
	mb := int64(1) << 20
	// 先占满到只剩 10 字节：直接落库（配额只拦入口，不拦存量行）。
	fillSHA := quotaSHA([]byte("filler-" + uuid.NewString()))
	lease := time.Now().Add(time.Hour)
	if err := h.db.CreateAsset(ctx, store.Asset{
		ID: uuid.NewString(), SHA256: fillSHA, DeclaredSize: mb - 10,
		MimeType: "application/octet-stream", FileName: "fill.bin",
		ObjectKey: "objects/" + fillSHA[:2] + "/" + fillSHA + "/fill.bin",
		Status:    "pending", UploaderID: "77777777-7777-7777-7777-777777777777",
		UploadExpiresAt: &lease,
	}); err != nil {
		t.Fatalf("filler: %v", err)
	}
	big := []byte("01234567890123456789") // 20 字节，超剩余额
	code, resp := h.initiateRaw(token, 0, quotaSHA(big))
	if code != 200 {
		t.Fatalf("0 声明只预留 0，应先通过，实际 %d（%s）", code, resp)
	}
	id := h.assetID(resp)
	req := httptest.NewRequest(http.MethodPut, "/api/storage/upload/stream/"+id, strings.NewReader(string(big)))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.r.ServeHTTP(w, req)
	if w.Code != 413 || !strings.Contains(w.Body.String(), "quota_exceeded") {
		t.Fatalf("超预算的 0 声明落定应 413 quota_exceeded，实际 %d（%s）", w.Code, w.Body.String())
	}
	got, err := h.db.Asset(ctx, id)
	if err != nil {
		t.Fatalf("读资产: %v", err)
	}
	if got.Status != "pending" || got.HashVerified {
		t.Fatalf("超预算资产不得完成: status=%s verified=%v", got.Status, got.HashVerified)
	}
	// 空文件（0 声明、0 实际）在剩余额度内仍可过：未知大小不是一律拒绝。
	emptySHA := quotaSHA([]byte{})
	code, resp = h.initiateRaw(token, 0, emptySHA)
	if code != 200 {
		t.Fatalf("空文件 0 声明应通过，实际 %d（%s）", code, resp)
	}
	eid := h.assetID(resp)
	req = httptest.NewRequest(http.MethodPut, "/api/storage/upload/stream/"+eid, strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	h.r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("空文件落定应 200，实际 %d（%s）", w.Code, w.Body.String())
	}
}

// 错误正数声明：声明 10、实际 5，流式落定即 409 且刚写入的对象被清除。
func TestQuotaWrongDeclaredStreamMismatch(t *testing.T) {
	h := newQuotaHarness(t, false)
	token := h.token("77777777-7777-7777-7777-777777777777")
	ctx := context.Background()
	body := []byte("12345")
	code, resp := h.initiateRaw(token, 10, quotaSHA(body))
	if code != 200 {
		t.Fatalf("initiate: %d（%s）", code, resp)
	}
	id := h.assetID(resp)
	req := httptest.NewRequest(http.MethodPut, "/api/storage/upload/stream/"+id, strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.r.ServeHTTP(w, req)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "size_mismatch") {
		t.Fatalf("声明与实际不符应 409 size_mismatch，实际 %d（%s）", w.Code, w.Body.String())
	}
	got, err := h.db.Asset(ctx, id)
	if err != nil || got.Status != "pending" {
		t.Fatalf("不符资产应保持 pending: %+v err=%v", got, err)
	}
}

// 直传路径的 0 声明同样按实际兑现：超预算的 complete 即 413。
func TestQuotaPresignedCompleteRedeem(t *testing.T) {
	h := newQuotaHarness(t, true)
	token := h.token("77777777-7777-7777-7777-777777777777")
	ctx := context.Background()
	mb := int64(1) << 20
	fillSHA := quotaSHA([]byte("s3filler-" + uuid.NewString()))
	lease := time.Now().Add(time.Hour)
	if err := h.db.CreateAsset(ctx, store.Asset{
		ID: uuid.NewString(), SHA256: fillSHA, DeclaredSize: mb - 10,
		MimeType: "application/octet-stream", FileName: "fill.bin",
		ObjectKey: "objects/" + fillSHA[:2] + "/" + fillSHA + "/fill.bin",
		Status:    "pending", UploaderID: "77777777-7777-7777-7777-777777777777",
		UploadExpiresAt: &lease,
	}); err != nil {
		t.Fatalf("filler: %v", err)
	}
	big := []byte("01234567890123456789")
	code, resp := h.initiateRaw(token, 0, quotaSHA(big))
	if code != 200 {
		t.Fatalf("initiate: %d（%s）", code, resp)
	}
	id := h.assetID(resp)
	a, err := h.db.Asset(ctx, id)
	if err != nil {
		t.Fatalf("asset: %v", err)
	}
	put, err := http.NewRequest(http.MethodPut, h.s3.objectURL(a.ObjectKey), strings.NewReader(string(big)))
	if err != nil {
		t.Fatalf("build put: %v", err)
	}
	presp, err := http.DefaultClient.Do(put)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	presp.Body.Close()
	if presp.StatusCode != 200 {
		t.Fatalf("put: %d", presp.StatusCode)
	}
	cbody, _ := json.Marshal(map[string]any{"asset_id": id})
	code, resp = h.do(token, http.MethodPost, "/api/storage/upload/complete", string(cbody))
	if code != 413 || !strings.Contains(resp, "quota_exceeded") {
		t.Fatalf("超预算的 0 声明 complete 应 413 quota_exceeded，实际 %d（%s）", code, resp)
	}
	got, err := h.db.Asset(ctx, id)
	if err != nil || got.Status != "pending" || got.HashVerified {
		t.Fatalf("超预算资产不得完成: %+v err=%v", got, err)
	}
}

// 回收后余额恢复：占位过期被清理后，统计归零、同等规模可重新占位。
func TestQuotaReleasedAfterReclaim(t *testing.T) {
	h := newQuotaHarness(t, false)
	token := h.token("77777777-7777-7777-7777-777777777777")
	ctx := context.Background()
	half := int64(1) << 19 // 512KiB
	code, resp := h.initiateRaw(token, half, quotaSHA([]byte("reclaim-"+uuid.NewString())))
	if code != 200 {
		t.Fatalf("initiate: %d（%s）", code, resp)
	}
	id := h.assetID(resp)
	u, err := h.db.UserUsage(ctx, "77777777-7777-7777-7777-777777777777")
	if err != nil || u.PendingBytes != half {
		t.Fatalf("占位应计入预算 pending=%d err=%v", u.PendingBytes, err)
	}
	if err := h.db.SetUploadExpiry(ctx, id, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("expire: %v", err)
	}
	cfg := h.cfg
	cfg.PendingTTLHours = 72
	// 脚手架内的对象句柄未导出：同 Root 再开一个本地对象句柄（同一目录即同一字节）。
	objs, oerr := objects.New(ctx, config.Config{Root: h.cfg.Root, S3Bucket: "metafusion-test", PresignTTL: time.Minute})
	if oerr != nil {
		t.Fatalf("objects: %v", oerr)
	}
	rep2, err := maintenance.RunCleanup(ctx, h.db, objs, cfg)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if rep2.ObjectAlreadyMissing != 1 {
		t.Fatalf("过期占位应被回收: %+v", rep2)
	}
	u, err = h.db.UserUsage(ctx, "77777777-7777-7777-7777-777777777777")
	if err != nil || u.PendingBytes != 0 || u.CompleteBytes != 0 {
		t.Fatalf("回收后余额应归零: %+v err=%v", u, err)
	}
	code, _ = h.initiateRaw(token, half, quotaSHA([]byte("again-"+uuid.NewString())))
	if code != 200 {
		t.Fatalf("回收后应可重新占位，实际 %d", code)
	}
}
