package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MoeclubM/metafusion-storage/internal/store"
	"github.com/google/uuid"
)

// 1×1 透明 PNG：用例要验的是"发出去的就是原字节"，用真图片头让内容嗅探有东西可嗅。
const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func onePixelPNGBytes(t *testing.T) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(onePixelPNG)
	if err != nil {
		t.Fatalf("解码用例图片失败: %v", err)
	}
	return raw
}

// getContent 发一次不带身份的取内容请求（浏览器 <img> 就是这个形状），返回响应以便查头。
func (h *uploadHarness) getContent(token, assetID string) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/storage/assets/"+assetID+"/content", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.r.ServeHTTP(w, req)
	return w
}

// bind materializes an approved catalog cover through the private lifecycle.
func (h *uploadHarness) bind(t *testing.T, token, assetID, entityID string) {
	t.Helper()
	ctx := context.Background()
	asset, err := h.db.Asset(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	in := store.CatalogAssetReferences{WorkspaceID: uuid.NewString(), References: []store.CatalogAssetReference{{EntityID: entityID, AssetID: assetID, ActorIDs: []string{asset.UploaderID}}}}
	if err = h.db.RetainCatalogAssets(ctx, in); err != nil {
		t.Fatal(err)
	}
	in.Prepare = true
	if err = h.db.RetainCatalogAssets(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err = h.db.ApplyCatalogLifecycle(ctx, store.CatalogLifecycle{EntityID: entityID, Kind: "work", State: "active", Version: 1, UpdatedAt: time.Now(), CoverAssetIDs: []string{assetID}}); err != nil {
		t.Fatal(err)
	}
}

// 目录里的图片引用要一条**稳定、可公开加载**的地址：download 在对象存储模式下只回预签名地址
// （会过期，且签名 Host 是对象存储端点，未配 STORAGE_S3_PUBLIC_ENDPOINT 时浏览器不可达）。
// assets/{id}/content 补这条：按请求鉴权、原样内联分发，绑定到可见实体后匿名可读。
// 只跑本地对象模式：内容怎么进对象存储不是这条路由的判定点（两种模式都是同一份 objects.Open）。
func TestAssetContentServesOriginalBytesInline(t *testing.T) {
	h := newUploadHarness(t, false)
	owner := h.token("11111111-1111-1111-1111-111111111111")
	body := onePixelPNGBytes(t)
	sha := sha256HexOf(body)

	assetID := h.initiate(t, owner, "cover.png", sha, int64(len(body)), 1)
	if code, resp := h.put(t, owner, assetID, body); code != 200 {
		t.Fatalf("上传返回 %d（%s）", code, resp)
	}

	// 未绑定 + 匿名：不可读，按不存在返回——否则匿名者能靠 uuid 探测他人上传。
	if w := h.getContent("", assetID); w.Code != 404 {
		t.Fatalf("未绑定的资产匿名取内容应 404，实际 %d（%s）", w.Code, w.Body.String())
	}
	// 上传者本人直通。
	if w := h.getContent(owner, assetID); w.Code != 200 {
		t.Fatalf("上传者取内容应 200，实际 %d（%s）", w.Code, w.Body.String())
	}

	h.bind(t, owner, assetID, "01a0a88d-cd52-745a-9266-b62aea6f8296")
	w := h.getContent("", assetID)
	if w.Code != 200 {
		t.Fatalf("绑定到可见实体后匿名应 200，实际 %d（%s）", w.Code, w.Body.String())
	}
	if got := w.Body.Bytes(); !bytes.Equal(got, body) {
		t.Fatalf("分发的字节与上传的不一致：%d 字节 vs %d 字节", len(got), len(body))
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type = %q，期望 image/png（类型不对浏览器会按用途不符拒渲染）", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); cd != "inline" {
		t.Fatalf("Content-Disposition = %q，期望 inline", cd)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "private, max-age=300" {
		t.Fatalf("Cache-Control = %q，期望私有缓存（可见性按请求判定，不能进共享缓存）", cc)
	}
}

// 非法 uuid 与未完成态都不该被兜成 500 或被读出来。
func TestAssetContentRejectsBadTargets(t *testing.T) {
	h := newUploadHarness(t, false)
	owner := h.token("11111111-1111-1111-1111-111111111111")
	body := onePixelPNGBytes(t)
	sha := sha256HexOf(body)
	assetID := h.initiate(t, owner, "cover.png", sha, int64(len(body)), 1)

	if w := h.getContent(owner, "not-a-uuid"); w.Code != 404 {
		t.Fatalf("非法 uuid 应 404，实际 %d", w.Code)
	}
	// 还没上传（pending）的资产不得被分发。
	if w := h.getContent(owner, assetID); w.Code != 404 {
		t.Fatalf("pending 资产应 404，实际 %d（%s）", w.Code, w.Body.String())
	}
}

// 内容类型兜底：登记值优先；退化成 octet-stream 时按扩展名、再按内容嗅探补齐。
// 直接把 application/octet-stream 发出去不会报错，只会让 <img> 破图。
func TestInlineMimeFallsBackToExtensionAndSniffing(t *testing.T) {
	png := onePixelPNGBytes(t)
	cases := []struct {
		name  string
		asset store.Asset
		body  []byte
		want  string
	}{
		{"登记值优先", store.Asset{MimeType: "image/webp", FileName: "cover.png"}, png, "image/webp"},
		{"扩展名兜底", store.Asset{MimeType: "application/octet-stream", FileName: "cover.webp"}, png, "image/webp"},
		{"内容嗅探兜底", store.Asset{MimeType: "application/octet-stream", FileName: "blob"}, png, "image/png"},
		{"空条目不给伪类型", store.Asset{MimeType: "application/octet-stream", FileName: "balob"}, nil, "application/octet-stream"},
	}
	for _, tc := range cases {
		reader := bytes.NewReader(tc.body)
		got := inlineMime(tc.asset, reader)
		if got != tc.want {
			t.Fatalf("%s: inlineMime = %q，期望 %q", tc.name, got, tc.want)
		}
		// 嗅探读走的字节必须还回去，否则分发出去的内容会少掉开头 512 字节。
		rest, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("%s: 读回剩余内容失败: %v", tc.name, err)
		}
		if !bytes.Equal(rest, tc.body) {
			t.Fatalf("%s: 嗅探没有把读取位置还原（剩 %d 字节，原 %d 字节）", tc.name, len(rest), len(tc.body))
		}
	}
}
