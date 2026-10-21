package handler

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/MoeclubM/metafusion-storage/internal/auth"
	"github.com/MoeclubM/metafusion-storage/internal/catalog"
	"github.com/MoeclubM/metafusion-storage/internal/config"
	"github.com/MoeclubM/metafusion-storage/internal/objects"
	"github.com/MoeclubM/metafusion-storage/internal/store"
	"github.com/MoeclubM/metafusion-storage/internal/testutil"
)

// uploadHarness 把 complete 的落定条件放到真实 HTTP 契约上验证：
// 真的分发路由、真的验签令牌、真的落库（本地对象模式或假 S3 预签名直传）。
// 两种对象模式跑同一组用例——差别只在"内容怎么进对象存储"，
// 落定前的回读校验与查重口径必须完全一致。
type uploadHarness struct {
	t   *testing.T
	r   *gin.Engine
	db  *store.Store
	key *rsa.PrivateKey
	kid string

	local bool
	s3    *fakeHarnessS3

	// visible 决定假目录服务对某个实体回什么：默认全部可见，用例不被可见性边界挡住；
	// 需要"绑定后来变不可见"的用例在请求发出前改它即可（判定发生在每次读请求里，
	// 正是真实服务的形状）。绑定的目标必须在绑定时可见（bind 会问目录），所以这类用例
	// 先绑再翻。
	visible func(entityID string) bool
}

func newUploadHarness(t *testing.T, useS3 bool) *uploadHarness {
	t.Helper()
	return newUploadHarnessWith(t, useS3, 0)
}

// newUploadHarnessWith 支持带上回读校验的大小上限（MB，0 为不限制）。
func newUploadHarnessWith(t *testing.T, useS3 bool, verifyMaxMB int) *uploadHarness {
	return newUploadHarnessConfig(t, useS3, verifyMaxMB, nil)
}

func newUploadHarnessConfig(t *testing.T, useS3 bool, verifyMaxMB int, configure func(*config.Config)) *uploadHarness {
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
	// 用例之间互不影响：先删子表再删母表（绑定对 assets 有外键）。
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
	h := &uploadHarness{t: t, db: st, key: key, kid: kid}

	// MaxPartCount 必须与 config.Load() 的默认值一致：直接构造 Config 时零值会让
	// 任何 part_count>0 的请求都撞上 too_many_parts，测试就测不到真实判定了。
	cfg := config.Config{
		Root:         t.TempDir(),
		S3Bucket:     "metafusion-test",
		PresignTTL:   5 * time.Minute,
		MaxPartCount: 10000,
		VerifyMaxMB:  verifyMaxMB,
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
	if configure != nil {
		configure(&cfg)
	}
	objs, err := objects.New(ctx, cfg)
	if err != nil {
		t.Fatalf("objects.New: %v", err)
	}
	if objs.Local() != !useS3 {
		t.Fatalf("对象模式不符: local=%v useS3=%v", objs.Local(), useS3)
	}

	// 目录服务只被问可见性：默认所有实体都可见，用例不被可见性边界挡住；
	// 换了 h.visible 的用例按实体 id 得到 200 或 404（404 = 目录明确回答不可见，
	// 与"问不到上游"区分开，后者由 catalog_upstream_test.go 覆盖）。
	cat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/catalog/entities/"), "/identity")
		if h.visible != nil && !h.visible(id) {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "not_found"})
			return
		}
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

func TestInternalS3TransfersDoNotExposeSigningEndpoint(t *testing.T) {
	h := newUploadHarnessConfig(t, true, 0, func(cfg *config.Config) { cfg.S3PublicEndpoint = "" })
	token := h.token("11111111-2222-4333-8444-555555555555")
	content := "internal S3 streamed content"
	digest := sha256.Sum256([]byte(content))
	body, _ := json.Marshal(map[string]any{
		"file_name": "internal.txt", "file_size": len(content),
		"sha256_hash": hex.EncodeToString(digest[:]), "part_count": 3,
	})
	code, resp := h.do(token, http.MethodPost, "/api/storage/upload/initiate", string(body))
	var out struct {
		AssetID   string   `json:"asset_id"`
		DirectURL string   `json:"direct_upload_url"`
		Presigned []string `json:"presigned_urls"`
		UploadID  string   `json:"upload_id"`
	}
	if code != 200 || json.Unmarshal([]byte(resp), &out) != nil || out.AssetID == "" ||
		out.DirectURL != "/api/storage/upload/stream/"+out.AssetID || len(out.Presigned) != 0 || out.UploadID != "" {
		t.Fatalf("internal S3 must return only a service upload URL: %d %s", code, resp)
	}
	if code, resp = h.do(token, http.MethodPut, out.DirectURL, content); code != 200 {
		t.Fatalf("stream upload failed: %d %s", code, resp)
	}
	if code, resp = h.complete(token, out.AssetID); code != 200 {
		t.Fatalf("complete failed: %d %s", code, resp)
	}
	asset := h.asset(t, out.AssetID)
	if !h.s3.hasObject(asset.ObjectKey) {
		t.Fatal("S3 upload silently switched to local storage")
	}
	path := "/api/storage/download/" + out.AssetID
	if code, resp = h.do(token, http.MethodGet, path, ""); code != 200 || resp != content {
		t.Fatalf("download must return bytes, not internal signed URLs: %d %s", code, resp)
	}
	if code, resp = h.do("", http.MethodGet, path, ""); code != 404 {
		t.Fatalf("unbound private asset leaked to anonymous caller: %d %s", code, resp)
	}
	h.bind(t, token, out.AssetID, "01a0a88d-cd52-745a-9266-b62aea6f8296")
	if code, resp = h.do("", http.MethodGet, path, ""); code != 200 || resp != content {
		t.Fatalf("public S3 download must return bytes to anonymous caller: %d %s", code, resp)
	}
	_, err := h.db.DB().Exec(`UPDATE storage.assets SET blocked=true WHERE id=$1`, out.AssetID)
	if err != nil {
		t.Fatal(err)
	}
	if code, resp = h.do("", http.MethodGet, path, ""); code != 404 {
		t.Fatalf("blocked asset still downloadable: %d %s", code, resp)
	}
}

// token 用给定 subject 签一个普通用户令牌：存储侧只认令牌里的 id 作为上传者身份。
//
// permissions 必须带 storage.asset.upload：上传与绑定自 6c4b3cf 起按该码收口
// （requireUpload 中间件），而 member 组默认持有它；不带码的令牌在这里只会在
// initiate 第一步就 403，用例根本走不到被测逻辑（缺码的语义另由
// TestUploadRequiresUploadPermission 覆盖）。
func (h *uploadHarness) token(subject string) string {
	h.t.Helper()
	payload := jwt.MapClaims{
		"sub":                subject,
		"preferred_username": "tester",
		"token_use":          "session",
		"permissions":        []string{auth.PermissionAssetUpload},
		"iss":                testIssuer,
		"aud":                testAudience,
		"exp":                time.Now().Add(10 * time.Minute).Unix(),
		"iat":                time.Now().Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, payload)
	token.Header["kid"] = h.kid
	signed, err := token.SignedString(h.key)
	if err != nil {
		h.t.Fatalf("sign: %v", err)
	}
	return signed
}

// do 发一次带身份的请求：body 非空即按 JSON 提交。
func (h *uploadHarness) do(token, method, path, body string) (int, string) {
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

func (h *uploadHarness) asset(t *testing.T, id string) store.Asset {
	t.Helper()
	a, err := h.db.Asset(context.Background(), id)
	if err != nil {
		t.Fatalf("读资产 %s: %v", id, err)
	}
	return a
}

func (h *uploadHarness) byHash(t *testing.T, sha string) (store.Asset, error) {
	t.Helper()
	return h.db.VerifiedAssetByHash(context.Background(), sha)
}

// initiate 走真实入口建一份待上传资产，返回资产 id。
func (h *uploadHarness) initiate(t *testing.T, token, name, sha string, size int64, parts int) string {
	t.Helper()
	in := map[string]any{"file_name": name, "file_size": size, "sha256_hash": sha, "part_count": parts}
	body, _ := json.Marshal(in)
	code, resp := h.do(token, http.MethodPost, "/api/storage/upload/initiate", string(body))
	if code != 200 {
		t.Fatalf("initiate 返回 %d（请求 %s / 响应 %s）", code, body, resp)
	}
	var out struct {
		IsInstantUpload bool   `json:"is_instant_upload"`
		AssetID         string `json:"asset_id"`
		DirectUploadURL string `json:"direct_upload_url"`
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		t.Fatalf("解析 initiate 响应失败: %v（%s）", err, resp)
	}
	if out.IsInstantUpload {
		t.Fatalf("首次上传不该命中秒传: %s", resp)
	}
	if h.local && out.DirectUploadURL == "" {
		t.Fatalf("本地模式应返回服务端接收地址: %s", resp)
	}
	return out.AssetID
}

// initiateOutcome 是抽查秒传判定用的响应投影。
func (h *uploadHarness) initiateOutcome(t *testing.T, token, name, sha string, size int64) (instant bool, assetID string, presigned int) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"file_name": name, "file_size": size, "sha256_hash": sha})
	code, resp := h.do(token, http.MethodPost, "/api/storage/upload/initiate", string(body))
	if code != 200 {
		t.Fatalf("initiate 返回 %d（%s）", code, resp)
	}
	var out struct {
		IsInstantUpload bool     `json:"is_instant_upload"`
		AssetID         string   `json:"asset_id"`
		PresignedURLs   []string `json:"presigned_urls"`
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		t.Fatalf("解析 initiate 响应失败: %v（%s）", err, resp)
	}
	return out.IsInstantUpload, out.AssetID, len(out.PresignedURLs)
}

// put 按当前对象模式把内容送进对象存储：本地模式走服务端流式接收，S3 模式走预签名直传。
// 本地模式的流式接收要求上传者/审核者，因此用发起上传的同一个身份发请求。
func (h *uploadHarness) put(t *testing.T, token, assetID string, content []byte) (int, string) {
	t.Helper()
	if h.local {
		return h.do(token, http.MethodPut, "/api/storage/upload/stream/"+assetID, string(content))
	}
	asset := h.asset(t, assetID)
	req, err := http.NewRequest(http.MethodPut, h.s3.objectURL(asset.ObjectKey), strings.NewReader(string(content)))
	if err != nil {
		t.Fatalf("build put: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("预签名直传失败: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// complete 走真实完成接口：S3 模式下 complete 会先合并再回读校验。
func (h *uploadHarness) complete(token, assetID string) (int, string) {
	h.t.Helper()
	body, _ := json.Marshal(map[string]any{"asset_id": assetID})
	return h.do(token, http.MethodPost, "/api/storage/upload/complete", string(body))
}

// verifyByHash 用上传者身份做秒传探测：探测本身允许匿名，但未绑定的资产
// 对匿名者不可读（readable 判定），匿名探测永远只能得到 exists=false。
func (h *uploadHarness) verifyByHash(token, sha string) map[string]any {
	h.t.Helper()
	body, _ := json.Marshal(map[string]any{"sha256_hash": sha})
	code, resp := h.do(token, http.MethodPost, "/api/storage/verify-hash", string(body))
	if code != 200 {
		h.t.Fatalf("verify-hash 返回 %d（%s）", code, resp)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		h.t.Fatalf("解析 verify-hash 响应失败: %v（%s）", err, resp)
	}
	return out
}

// 回读校验的错误码与状态码是纯函数映射：上线前就把口径固定住，
// 避免"对象存储故障"和"内容对不上"被混成同一个 5xx。
func TestVerifyErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantCode   string
		wantStatus int
	}{
		{"摘要不符", objects.ErrHashMismatch, "hash_mismatch", 409},
		{"超限", objects.ErrVerifyTooLarge, "hash_verify_too_large", 413},
		{"超时", objects.ErrVerifyTimeout, "verify_timeout", 408},
		{"读取故障", errors.New("connection reset"), "storage_unavailable", 503},
	}
	for _, tc := range cases {
		code, status := verifyError(tc.err)
		if code != tc.wantCode || status != tc.wantStatus {
			t.Fatalf("%s: verifyError = (%s,%d)，期望 (%s,%d)", tc.name, code, status, tc.wantCode, tc.wantStatus)
		}
	}
}

func sha256HexOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// 双模式共用一组 body：正确内容与"等长的错内容"（必须逐字节等长，
// 否则用例只是又一次 size_mismatch，证明不了"等长也会被摘要校验拦住"）。
var (
	correctBody = []byte("correct-content-0000000000a")
	wrongBody   = []byte("wrong-content-000000000000b")
)

func assertEqualLength(t *testing.T) {
	t.Helper()
	if len(correctBody) != len(wrongBody) {
		t.Fatalf("用例前提不成立：两份内容必须等长（%d vs %d）", len(correctBody), len(wrongBody))
	}
	if sha256HexOf(correctBody) == sha256HexOf(wrongBody) {
		t.Fatal("用例前提不成立：两份内容必须摘要不同")
	}
}

// runBothObjectModes 让同一组断言在两种对象模式下各跑一遍。
func runBothObjectModes(t *testing.T, fn func(t *testing.T, h *uploadHarness)) {
	t.Helper()
	for _, useS3 := range []bool{false, true} {
		name := "本地对象模式"
		if useS3 {
			name = "假S3预签名直传"
		}
		t.Run(name, func(t *testing.T) { fn(t, newUploadHarness(t, useS3)) })
	}
}

// 正确内容 → complete 成功、资产 complete 且 hash_verified=true，同一 sha256 立刻可秒传。
func TestUploadCompleteVerifiesContent(t *testing.T) {
	runBothObjectModes(t, func(t *testing.T, h *uploadHarness) {
		assertEqualLength(t)
		token := h.token("11111111-1111-1111-1111-111111111111")
		sha := sha256HexOf(correctBody)

		assetID := h.initiate(t, token, "track.flac", sha, int64(len(correctBody)), 1)
		if code, resp := h.put(t, token, assetID, correctBody); code != 200 {
			t.Fatalf("上传正确内容返回 %d（%s）", code, resp)
		}
		if code, resp := h.complete(token, assetID); code != 200 {
			t.Fatalf("complete 正确内容应 200，实际 %d（%s）", code, resp)
		}
		got := h.asset(t, assetID)
		if got.Status != "complete" || !got.HashVerified {
			t.Fatalf("完成态不符: status=%s hash_verified=%v", got.Status, got.HashVerified)
		}
		if got.SizeBytes != int64(len(correctBody)) {
			t.Fatalf("size_bytes = %d，期望 %d", got.SizeBytes, len(correctBody))
		}

		instant, againID, _ := h.initiateOutcome(t, token, "track2.flac", sha, int64(len(correctBody)))
		if !instant || againID != assetID {
			t.Fatalf("已验过的内容应秒传到同一资产: instant=%v id=%s want=%s", instant, againID, assetID)
		}
		if probe := h.verifyByHash(token, sha); probe["exists"] != true {
			t.Fatalf("秒传探测应命中已验内容: %s", probe)
		}
	})
}

// 等长的错内容 → complete 失败、资产不可用、该 sha256 的查重不再命中。
// 反证：旧实现（complete 只比大小）下这条用例会失败——等长错内容会被判为完成，
// 之后所有按该 sha256 秒传的人都会拿到错内容。
func TestUploadRejectsEqualLengthWrongContent(t *testing.T) {
	runBothObjectModes(t, func(t *testing.T, h *uploadHarness) {
		assertEqualLength(t)
		alice := h.token("11111111-1111-1111-1111-111111111111")
		bob := h.token("22222222-2222-2222-2222-222222222222")
		sha := sha256HexOf(correctBody)

		assetID := h.initiate(t, alice, "track.flac", sha, int64(len(correctBody)), 1)
		// 两种模式的校验位置不同，判定必须都落在"错内容不得发布"上：
		// 本地模式在服务端接收时边收边算（错内容根本不落盘），S3 模式把校验放在 complete。
		if h.local {
			if code, resp := h.put(t, alice, assetID, wrongBody); code != 409 || !strings.Contains(resp, "hash_mismatch") {
				t.Fatalf("本地模式错内容应在上传时被拒 409 hash_mismatch，实际 %d（%s）", code, resp)
			}
		} else {
			if code, resp := h.put(t, alice, assetID, wrongBody); code != 200 {
				t.Fatalf("S3 模式错内容应只是被对象存储收下（校验在完成时），实际 %d（%s）", code, resp)
			}
			code, resp := h.complete(alice, assetID)
			if code != 409 || !strings.Contains(resp, "hash_mismatch") {
				t.Fatalf("S3 模式等长错内容应 409 hash_mismatch，实际 %d（%s）", code, resp)
			}
		}

		// 资产保持不可用：既不 complete，也没有 hash_verified。
		got := h.asset(t, assetID)
		if got.Status == "complete" || got.HashVerified {
			t.Fatalf("校验失败的资产不得置为完成: status=%s hash_verified=%v", got.Status, got.HashVerified)
		}
		if _, err := h.byHash(t, sha); err == nil {
			t.Fatal("未验证资产不得被 VerifiedAssetByHash 命中")
		}
		if probe := h.verifyByHash(bob, sha); probe["exists"] != false {
			t.Fatalf("秒传探测不该命中未验证内容: %s", probe)
		}
		// 上传者本人：不能秒传，但按既有口径继续"续传"（重复提交即续传），
		// 这样她重传正确内容才能收尾。
		if instant, _, presigned := h.initiateOutcome(t, alice, "track.flac", sha, int64(len(correctBody))); instant {
			t.Fatal("未验证的 sha256 不得让任何人秒传")
		} else if !h.local && presigned == 0 {
			t.Fatal("非秒传必须签发直传地址")
		}
		// 别人：未完成的同一 sha256 由上传者本人接手，跨用户处置需要 storage.asset.moderate。
		code, resp := h.do(bob, http.MethodPost, "/api/storage/upload/initiate",
			`{"file_name":"track.flac","file_size":27,"sha256_hash":"`+sha+`"}`)
		if code != 409 || !strings.Contains(resp, "upload_in_progress") {
			t.Fatalf("他人重传未完成的 sha256 应 409 upload_in_progress，实际 %d（%s）", code, resp)
		}
		// 失败上传不得把该 sha256 的键占住（S3 模式由服务端在校验失败后清掉）。
		if h.s3 != nil && h.s3.hasObject(got.ObjectKey) {
			t.Fatal("校验失败的对象不得留在内容寻址键上")
		}

		// 正确内容仍然可发布：失败上传留下的 pending 资产就是该 sha256 的占位行
		// （内容寻址的唯一索引仍在），由**上传者本人**重传正确内容即可收尾——
		// 换成另一个人会被 assets_sha256 唯一索引挡住，那是"占位行归谁"的取舍，
		// 不是本用例要固定的行为。
		token := alice
		retryID := h.initiate(t, token, "track.flac", sha, int64(len(correctBody)), 1)
		if code, resp := h.put(t, token, retryID, correctBody); code != 200 {
			t.Fatalf("重传正确内容失败: %d（%s）", code, resp)
		}
		if code, resp := h.complete(token, retryID); code != 200 {
			t.Fatalf("重传后 complete 应 200，实际 %d（%s）", code, resp)
		}
		published := h.asset(t, retryID)
		if published.Status != "complete" || !published.HashVerified {
			t.Fatalf("重传后的完成态不符: status=%s hash_verified=%v", published.Status, published.HashVerified)
		}
		if _, err := h.byHash(t, sha); err != nil {
			t.Fatalf("正确内容发布后应可被查重命中: %v", err)
		}
	})
}

// 超限对象的显式失败（预签名直传模式）：声明就超限时立即失败，
// 既不合并也不置完成，资产留在 pending 且不参与秒传；对象本身不进对象存储。
//
// "实际比声明大"的对象会在回读校验前先撞上 size_mismatch（见下一条用例）——
// 那同样是显式失败、同样不置完成，不会静默跳过校验。
func TestUploadVerificationLimitFailsExplicitly(t *testing.T) {
	h := newUploadHarnessWith(t, true, 1) // 1MiB 上限
	token := h.token("11111111-1111-1111-1111-111111111111")

	small := []byte("small-body")
	sha := sha256HexOf(small)
	assetID := h.initiate(t, token, "declared-big.iso", sha, 2<<20, 1)
	code, resp := h.complete(token, assetID)
	if code != 413 || !strings.Contains(resp, "hash_verify_too_large") {
		t.Fatalf("声明超限应 413 hash_verify_too_large，实际 %d（%s）", code, resp)
	}
	got := h.asset(t, assetID)
	if got.Status == "complete" || got.HashVerified {
		t.Fatalf("超限对象不得置为完成: status=%s hash_verified=%v", got.Status, got.HashVerified)
	}
	if _, err := h.byHash(t, sha); err == nil {
		t.Fatal("超限未完成的资产不得被秒传查重命中")
	}
	if h.s3.hasObject(got.ObjectKey) {
		t.Fatalf("声明超限时对象不该被写进对象存储（key=%s keys=%v）", got.ObjectKey, h.s3.keys())
	}
	// 上限本身的口径由 objects 包的 TestVerifyHashRefusesOversizedObject /
	// TestVerifyHashTimesOut 固定（那里能拿到真实超限对象）；这里只固定"不置完成、不查重命中"。
}

// 实际大小与声明不一致（直传路径上没有服务端逐字节校验）时，先撞上 size_mismatch：
// 同样是显式失败，资产保持未完成，不会被"跳过校验但置完成"。
func TestUploadSizeMismatchStaysUnpublished(t *testing.T) {
	h := newUploadHarness(t, true)
	token := h.token("11111111-1111-1111-1111-111111111111")

	content := []byte("actual-body-is-longer-than-declared")
	assetID := h.initiate(t, token, "liar.bin", sha256HexOf(content), 10, 1)
	if code, resp := h.put(t, token, assetID, content); code != 200 {
		t.Fatalf("直传应被对象存储收下: %d（%s）", code, resp)
	}
	code, resp := h.complete(token, assetID)
	if code != 409 || !strings.Contains(resp, "size_mismatch") {
		t.Fatalf("大小不符应 409 size_mismatch，实际 %d（%s）", code, resp)
	}
	got := h.asset(t, assetID)
	if got.Status == "complete" || got.HashVerified {
		t.Fatalf("大小不符的资产不得置为完成: status=%s hash_verified=%v", got.Status, got.HashVerified)
	}
}

// verify-hash 的高成本分支要求登录：它读回整份对象并写回校验结论，不是公开只读接口。
func TestVerifyHashByAssetRequiresLogin(t *testing.T) {
	h := newUploadHarness(t, false)
	token := h.token("11111111-1111-1111-1111-111111111111")
	assetID := h.initiate(t, token, "track.flac", sha256HexOf(correctBody), int64(len(correctBody)), 1)
	if code, resp := h.put(t, token, assetID, correctBody); code != 200 {
		t.Fatalf("上传失败: %d（%s）", code, resp)
	}
	if code, resp := h.complete(token, assetID); code != 200 {
		t.Fatalf("complete 失败: %d（%s）", code, resp)
	}

	body, _ := json.Marshal(map[string]any{"asset_id": assetID})
	// 401 的错误码要与鉴权中间件同口径：客户端按码分支，同一语义不能出现两个码。
	if code, resp := h.do("", http.MethodPost, "/api/storage/verify-hash", string(body)); code != 401 {
		t.Fatalf("匿名按 asset_id 校验应 401，实际 %d（%s）", code, resp)
	} else if !strings.Contains(resp, `"error":"authentication_required"`) {
		t.Fatalf("匿名 401 的错误码应为 authentication_required，实际 %s", resp)
	}
	code, resp := h.do(token, http.MethodPost, "/api/storage/verify-hash", string(body))
	if code != 200 || !strings.Contains(resp, `"verified":true`) {
		t.Fatalf("上传者自校验应通过，实际 %d（%s）", code, resp)
	}
}

// fakeHarnessS3 是 handler 侧的最小假 S3 端点：建桶探测、单次 PUT、HEAD、GET、DELETE。
// 用例的 part_count 恒为 1（走单次 PUT + 预签名直传），分片合并已由 objects 包覆盖。
type fakeHarnessS3 struct {
	addr string
	srv  *httptest.Server
	mu   sync.Mutex
	objs map[string][]byte
}

func newFakeHarnessS3(t *testing.T) *fakeHarnessS3 {
	t.Helper()
	f := &fakeHarnessS3{objs: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	f.addr = strings.TrimPrefix(f.srv.URL, "http://")
	return f
}

func (f *fakeHarnessS3) objectURL(key string) string {
	return f.srv.URL + "/metafusion-test/" + key
}

func (f *fakeHarnessS3) hasObject(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objs[key]
	return ok
}

func (f *fakeHarnessS3) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []string{}
	for k := range f.objs {
		out = append(out, k)
	}
	return out
}

func (f *fakeHarnessS3) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/")
	key := ""
	if i := strings.Index(path, "/"); i >= 0 {
		key = path[i+1:]
	}
	switch {
	case r.Method == http.MethodHead && key == "":
		w.WriteHeader(http.StatusOK) // 桶已存在，服务不再建桶
	case r.Method == http.MethodPut && key == "":
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet && key == "":
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte("<LocationConstraint></LocationConstraint>"))
	case r.Method == http.MethodPut:
		f.objs[key] = decodeChunked(readBody(r))
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodDelete:
		delete(f.objs, key)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodHead:
		body, ok := f.objs[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet:
		body, ok := f.objs[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func readBody(r *http.Request) []byte {
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil
	}
	return body
}

// decodeChunked 还原 aws-chunked 编码（签名直传时 minio 客户端会这样发）：
// 真实 S3 自行解码，假端点必须自己处理，否则存下来的是带签名头的原始字节。
func decodeChunked(body []byte) []byte {
	if !strings.Contains(string(body), "chunk-signature=") {
		return body
	}
	out := []byte{}
	rest := body
	for {
		i := strings.Index(string(rest), "\r\n")
		if i < 0 {
			return out
		}
		header := string(rest[:i])
		if j := strings.Index(header, ";"); j >= 0 {
			header = header[:j]
		}
		size, err := strconv.ParseInt(strings.TrimSpace(header), 16, 64)
		if err != nil {
			return append(out, rest...)
		}
		rest = rest[i+2:]
		if size == 0 {
			return out
		}
		if int64(len(rest)) < size {
			return append(out, rest...)
		}
		out = append(out, rest[:size]...)
		rest = rest[size:]
		if strings.HasPrefix(string(rest), "\r\n") {
			rest = rest[2:]
		}
	}
}
