package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MoeclubM/metafusion-storage/internal/store"
	"github.com/google/uuid"
)

// 自托管封面这条链路的存储侧承诺：目录里 pictures[].url 写
// https://<站点>/api/storage/assets/{id}/content，pictures[].asset_id 写同一个 uuid，
// 于是浏览器加载 <img src> 时打的正是这一条路由。下面几条用例固定它对外的形状：
// 绑到公开可见实体的 complete 资产匿名可得原字节与正确类型、位图不是 attachment、
// 缓存头只进私有缓存并支持条件请求，而"绑定不可见"和"内容还没验完"都不吐内容也不给 304。
//
// 需要真实测试库（STORAGE_TEST_DSN 未设置时整文件跳过）。可见性在这里由假目录服务回答
// 404 来制造（见 uploadHarness.visible）；真实目录服务对草稿/待审/隐藏条目怎么答复
// 不属本仓库能验证的范围，这里能验证的是"存储侧拿到不可见结论时怎么办"。
const coverEntity = "01a0a88d-cd52-745a-9266-b62aea6f8296"

// getContentIfNoneMatch 发一次带条件头的取内容请求：浏览器第二次加载同一张封面就是这个形状
// （带 If-None-Match，不带任何凭据）。
func (h *uploadHarness) getContentIfNoneMatch(token, assetID, etag string) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/storage/assets/"+assetID+"/content", nil)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.r.ServeHTTP(w, req)
	return w
}

// downloadRaw 用同一份资产打 download 口，用于比对两个读入口的口径。
func (h *uploadHarness) downloadRaw(token, assetID string) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/storage/download/"+assetID, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.r.ServeHTTP(w, req)
	return w
}

// uploadCover 传一份声明为 image/png 的位图、以 cover_image 绑到 coverEntity，返回资产 id 与原始字节。
// 走上传入口，再通过私有目录生命周期物化封面，因此 complete 与 hash_verified
// 都是服务端验过内容之后的结果，不是用例手写的状态。
func uploadCover(t *testing.T, h *uploadHarness, owner, entity string) (string, []byte) {
	t.Helper()
	body := onePixelPNGBytes(t)
	assetID := h.initiateWithMime(t, owner, "cover.png", sha256HexOf(body), int64(len(body)), "image/png")
	if code, resp := h.put(t, owner, assetID, body); code != 200 {
		t.Fatalf("上传返回 %d（%s）", code, resp)
	}
	h.bind(t, owner, assetID, entity)
	return assetID, body
}

// 匿名 <img> 直链：200 + 原字节 + 能渲染的类型 + inline + 私有短缓存 + 可复用的 ETag。
func TestSelfHostedCoverIsDirectlyLoadable(t *testing.T) {
	h := newUploadHarness(t, false)
	owner := h.token("11111111-1111-1111-1111-111111111111")
	assetID, body := uploadCover(t, h, owner, coverEntity)

	w := h.getContent("", assetID)
	if w.Code != 200 {
		t.Fatalf("绑定到可见实体的封面匿名应 200，实际 %d（%s）", w.Code, w.Body.String())
	}
	if !bytes.Equal(w.Body.Bytes(), body) {
		t.Fatalf("发出的字节与上传的不一致：%d vs %d", w.Body.Len(), len(body))
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type = %q，期望按资产声明的 mime 发（一律 octet-stream 会让 <img> 破图）", ct)
	}
	// attachment 会让浏览器把图片当下载处理，<img> 直接不显示：位图必须留在 inline 分支。
	if cd := w.Header().Get("Content-Disposition"); cd != "inline" {
		t.Fatalf("Content-Disposition = %q，期望 inline", cd)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "private, max-age=300" {
		t.Fatalf("Cache-Control = %q，期望只进私有缓存（可见性按请求判定，共享缓存会把结论烤死）", cc)
	}
	// ETag 用内容摘要：目录里的引用是长期地址，客户端靠它免掉重复回源。
	if etag, want := w.Header().Get("ETag"), `"`+sha256HexOf(body)+`"`; etag != want {
		t.Fatalf("ETag = %q，期望 %q", etag, want)
	}
	// 同一份内容、同一个判定：download 口对同一请求者也必须通（口径不分叉）。
	if dl := h.downloadRaw("", assetID); dl.Code != 200 {
		t.Fatalf("同一份可见资产 download 应 200，实际 %d（%s）", dl.Code, dl.Body.String())
	}
}

// 条件请求：第二次加载不该再传一遍正文，但也不能不带缓存头。
func TestSelfHostedCoverConditionalRequest(t *testing.T) {
	h := newUploadHarness(t, false)
	owner := h.token("11111111-1111-1111-1111-111111111111")
	assetID, body := uploadCover(t, h, owner, coverEntity)

	first := h.getContent("", assetID)
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("200 响应必须带 ETag，否则反复加载的封面每次都整份回源")
	}

	hit := h.getContentIfNoneMatch("", assetID, etag)
	if hit.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match 命中应 304，实际 %d（%s）", hit.Code, hit.Body.String())
	}
	if hit.Body.Len() != 0 {
		t.Fatalf("304 不得带正文，实际 %d 字节", hit.Body.Len())
	}
	// 304 仍要带上它在 200 里也会发的那几项，缓存副本才留得住再验证依据（RFC 9110 §9.4.1）。
	if got := hit.Header().Get("ETag"); got != etag {
		t.Fatalf("304 的 ETag = %q，期望回显 %q", got, etag)
	}
	if got := hit.Header().Get("Cache-Control"); got != "private, max-age=300" {
		t.Fatalf("304 的 Cache-Control = %q，期望 private, max-age=300", got)
	}
	// 反过来：表示"这次的正文长这样"的头不出现在 304 上。
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Disposition"} {
		if got := hit.Header().Get(name); got != "" {
			t.Errorf("304 不应带 %s，实际 %q", name, got)
		}
	}

	// 弱标签、多值列表与 * 都是客户端可能发出来的形状，都算命中。
	for _, form := range []string{"W/" + etag, "  " + etag + "  ", `"deadbeef", ` + etag, "*"} {
		if got := h.getContentIfNoneMatch("", assetID, form); got.Code != http.StatusNotModified {
			t.Fatalf("If-None-Match: %s 应 304，实际 %d", form, got.Code)
		}
	}
	// 标签对不上（客户端缓存的是另一份内容）必须回到完整分发，不能吐 304。
	stale := h.getContentIfNoneMatch("", assetID, `"0000000000000000000000000000000000000000000000000000000000000000"`)
	if stale.Code != 200 || !bytes.Equal(stale.Body.Bytes(), body) {
		t.Fatalf("标签不符应回 200 与完整内容，实际 %d / %d 字节", stale.Code, stale.Body.Len())
	}
}

// 条件请求不是第二条放行路径：可见性先问，问不过就是 404，与 download 同一个码同一个体。
func TestSelfHostedCoverVisibilityNotBypassedByETag(t *testing.T) {
	h := newUploadHarness(t, false)
	owner := h.token("11111111-1111-1111-1111-111111111111")
	assetID, _ := uploadCover(t, h, owner, coverEntity)
	etag := h.getContent("", assetID).Header().Get("ETag")
	if etag == "" {
		t.Fatal("前置条件不成立：首次 200 应带 ETag")
	}

	// 任一绑定目标可见即可读：把第一条绑定转成不可见，第二条仍然公开。
	second := uuid.NewString()
	h.bind(t, owner, assetID, second)
	h.visible = func(id string) bool { return id != coverEntity }
	visible := h.getContentIfNoneMatch("", assetID, etag)
	if visible.Code != http.StatusNotModified {
		t.Fatalf("仍有可见绑定时条件请求应 304，实际 %d（%s）", visible.Code, visible.Body.String())
	}

	// 目录侧把剩下的那条绑定也不再对匿名者公开（改私有、撤公开、合并后查无此实体）。
	h.visible = func(string) bool { return false }
	w := h.getContentIfNoneMatch("", assetID, etag)
	dl := h.downloadRaw("", assetID)
	if w.Code != http.StatusNotFound {
		t.Fatalf("全部绑定不可见时条件请求应 404，实际 %d（%s）", w.Code, w.Body.String())
	}
	// 不可读的响应里不能有内容指纹，否则探测者能靠它比对"这份内容换没换"。
	if got := w.Header().Get("ETag"); got != "" {
		t.Fatalf("404 不应带 ETag，实际 %q", got)
	}
	// 两个读入口的对外结论必须一致：都是 404 not_found，不区分"无权限"与"不存在"，
	// 否则别人能靠"download 说没有、content 说有"探测出这里挂着一份文件。
	if dl.Code != w.Code || dl.Body.String() != w.Body.String() {
		t.Fatalf("content 与 download 口径分叉：%d %s vs %d %s",
			w.Code, w.Body.String(), dl.Code, dl.Body.String())
	}
	assertDeniedBody(t, w, "not_found")
}

// assertDeniedBody 固定被拒响应的形状：一个可解析的 {"error":"<机器码>"}。
// denyUnreadable 已经自己落过响应，调用方再 fail 一次会给同一请求追加第二个 JSON 体
// （按码分支的 bot 直接解析失败），所以"恰好一个错误体"本身就是判据。
func assertDeniedBody(t *testing.T, w *httptest.ResponseRecorder, wantErr string) {
	t.Helper()
	var got struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("错误体不是一个可解析的 JSON 对象: %v（%s）", err, w.Body.String())
	}
	if got.Error != wantErr {
		t.Fatalf("错误码 = %q，期望 %q", got.Error, wantErr)
	}
}

// pending（内容还没验完）不出内容：即使绑在可见实体上、即使匿名者猜对了 ETag。
// 这条同时守住"稳定地址不会吐出半成品"——目录侧引用一个刚 initiate 的 uuid 时，
// 页面拿到的是 404（破图），而不是一份未经验证的字节的 200 或 304。
func TestSelfHostedCoverServesNothingWhilePending(t *testing.T) {
	h := newUploadHarness(t, false)
	owner := h.token("11111111-1111-1111-1111-111111111111")
	body := onePixelPNGBytes(t)
	assetID := h.initiate(t, owner, "cover.png", sha256HexOf(body), int64(len(body)), 1)
	// Public writes and catalog prepare reject pending covers. Construct a
	// storage-only inconsistent reference to exercise the read-side defense too.
	if err := h.db.Bind(t.Context(), store.Binding{ID: uuid.NewString(), AssetID: assetID, TargetEntityID: coverEntity, TargetKind: "work", BindingRole: "cover_image", CreatedBy: "11111111-1111-1111-1111-111111111111"}); err != nil {
		t.Fatal(err)
	}
	if got := h.asset(t, assetID); got.Status == "complete" {
		t.Fatalf("前置条件不成立：这条资产应当还没落定（status=%s）", got.Status)
	}

	for _, tc := range []struct{ name, token, ifNoneMatch string }{
		{"匿名", "", ""},
		{"匿名带任意条件头", "", `"` + sha256HexOf(body) + `"`},
		{"上传者本人", owner, ""},
		{"本人带正确摘要作条件头", owner, `"` + sha256HexOf(body) + `"`},
	} {
		w := h.getContentIfNoneMatch(tc.token, assetID, tc.ifNoneMatch)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s: pending 资产应 404，实际 %d（%s）", tc.name, w.Code, w.Body.String())
		}
		assertDeniedBody(t, w, "not_found")
		if got := w.Header().Get("ETag"); got != "" {
			t.Fatalf("%s: pending 资产的 404 不应带 ETag，实际 %q", tc.name, got)
		}
	}
}
