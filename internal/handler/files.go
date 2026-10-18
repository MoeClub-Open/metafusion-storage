package handler

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/MoeclubM/metafusion-storage/internal/auth"
	"github.com/MoeclubM/metafusion-storage/internal/catalog"
	"github.com/MoeclubM/metafusion-storage/internal/objects"
	"github.com/MoeclubM/metafusion-storage/internal/store"
	"github.com/MoeclubM/metafusion-storage/internal/upstream"
)

// maxBindingProbe 限制一次读取鉴权最多问目录服务几次：绑定是"文件属于谁"的少量元数据，
// 异常多的绑定不值得为一次下载打穿目录服务。
const maxBindingProbe = 20

// contentCacheSeconds 是内联分发的浏览器缓存时长。可见性按**请求**判定（绑定目标是否可见），
// 因此响应只能进私有缓存：共享缓存会把一次成功鉴权的响应复用给无权者。
const contentCacheSeconds = 300

// readable 是文件读取的唯一判定：上传者本人或持 storage.asset.moderate 的审核者直通，
// 其余人只要**任一**绑定目标可见即可读（直通档位同 canManageAsset，替换拆分前的 role == admin）。
// 与下载、预览、哈希校验共用同一判定，避免同一份文件在不同接口上口径不同。
//
// 第二个返回值只在"问不到目录服务"时非 nil：调用方必须回 503，不能折成"不可读"。
// 查库失败仍按不可读处理（与拆分前一致，属本地依赖，现场有日志）；而"目录结论不可知"
// 必须对调用方可观测，否则上游抖动会表现成 404。
func (h *Handler) readable(c *gin.Context, asset store.Asset) (bool, error) {
	if err := h.db.CheckAssetActive(c.Request.Context(), asset.ID); err != nil {
		if errors.Is(err, store.ErrAssetReclaiming) || errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	p := auth.Current(c)
	if canManageAsset(p, asset.UploaderID) {
		return true, nil
	}
	// 禁发位：目录公开不等于文件可分发。blocked 只拦分发——上传者本人与审核者
	// 仍由上一行直通（处置与自查），其余人即使绑定实体可见也不可读。
	if asset.Blocked {
		return false, nil
	}
	if asset.Status != "complete" {
		return false, nil
	}
	bindings, err := h.db.BindingsForAsset(c.Request.Context(), asset.ID)
	if err != nil {
		return false, nil
	}
	if len(bindings) > maxBindingProbe {
		bindings = bindings[:maxBindingProbe]
	}
	bearer, cookie := h.credentials(c)
	for _, b := range bindings {
		_, err := h.catalog.Visible(c.Request.Context(), b.TargetEntityID, bearer, cookie)
		if err == nil {
			return true, nil
		}
		if errors.Is(err, catalog.ErrNotVisible) {
			// 这一条绑定不可见（含合并后查无此实体）：继续看下一条——只要有一个绑定
			// 目标可见，这份文件就可读。
			continue
		}
		// 问不到目录服务：不能当成"这条绑定不可见"继续往下试，否则目录服务一抖
		// 就表现成"这份文件不存在"。
		return false, err
	}
	return false, nil
}

// denyUnreadable 把 readable 的结论写成响应，返回 true 表示请求已处理完（调用方直接 return）。
// 上游不可用 → 503 + upstream_unavailable（可观测的依赖故障）；
// 不可读 → 仍是原来的 404 not_found（不区分"无权限"与"不存在"，避免泄露他人上传的存在性）。
func (h *Handler) denyUnreadable(c *gin.Context, asset store.Asset) bool {
	ok, err := h.readable(c, asset)
	if err == nil && ok {
		return false
	}
	if err != nil {
		h.log.Printf("storage: asset %s 读取鉴权问不到目录服务，回 503 %s: %v",
			asset.ID, upstream.CodeUpstreamUnavailable, err)
		fail(c, http.StatusServiceUnavailable, upstream.CodeUpstreamUnavailable)
		return true
	}
	fail(c, http.StatusNotFound, "not_found")
	return true
}

func (h *Handler) getAsset(c *gin.Context) {
	if !validID(c.Param("id")) {
		fail(c, 404, "not_found")
		return
	}
	ctx := c.Request.Context()
	asset, err := h.db.Asset(ctx, c.Param("id"))
	if errors.Is(err, store.ErrNotFound) {
		fail(c, 404, "not_found")
		return
	}
	if err != nil {
		fail(c, 500, "module_error")
		return
	}
	if h.denyUnreadable(c, asset) {
		return
	}
	bindings, err := h.db.BindingsForAsset(ctx, asset.ID)
	if err != nil {
		fail(c, 500, "module_error")
		return
	}
	c.JSON(200, gin.H{"asset": asset, "bindings": bindings})
}

// objectReadFailed 把"读不到对象"翻成对外结论，并留下可排查的日志。
//
// 键上根本没有对象（数据缺失）与对象存储/网络故障是两件事：前者重试、扩容、换端点
// 都不会让内容出现，后者才值得重试。以前两种都回 503 storage_unavailable，
// 于是"这批文件早就不在了"在监控里长得和"对象存储挂了"一模一样，也解释不了
// 为什么同一资源的元数据接口照常 200（元数据在库里，不碰对象存储）。
func (h *Handler) objectReadFailed(c *gin.Context, asset store.Asset, err error) {
	if objects.IsObjectMissing(err) {
		h.log.Printf("storage: asset %s 的对象不在对象存储里 key=%s: %v", asset.ID, asset.ObjectKey, err)
		fail(c, 404, "object_missing")
		return
	}
	h.log.Printf("storage: asset %s 读对象失败 key=%s: %v", asset.ID, asset.ObjectKey, err)
	fail(c, 503, "storage_unavailable")
}

// assetContent 按请求鉴权后**原样**把对象内容发给调用者：不转码、不裁剪、不改一个字节。
//
// 与 download 的分工是"稳定地址"与"一次性取件"：download 在对象存储模式下只回一个预签名地址，
// 它的有效期受 STORAGE_PRESIGN_TTL_MINUTES 限制，签名 Host 又是对象存储端点（未配置
// STORAGE_S3_PUBLIC_ENDPOINT 时浏览器根本不可达）；目录里 pictures[].url 这类需要长期可引用、
// 且要能被 <img> 直接加载的地址，只能由本服务每次重新鉴权后转发原档。
// 可见性判定与 download 完全同一处（readable），不可读一律 404，不区分"无权限"与"不存在"。
//
// 响应头按"能被浏览器反复加载"来配（见 contentCacheHeaders）：类型取资产登记的 mime（缺失才按
// 扩展名/内容嗅探兜底，不给一律 octet-stream）、位图走 inline 而不是 attachment（后者会让 <img>
// 变成下载）、Cache-Control 只进私有缓存、ETag 用内容摘要并支持 If-None-Match → 304。
func (h *Handler) assetContent(c *gin.Context) {
	if !validID(c.Param("id")) {
		fail(c, 404, "not_found")
		return
	}
	ctx := c.Request.Context()
	asset, err := h.db.Asset(ctx, c.Param("id"))
	if errors.Is(err, store.ErrNotFound) {
		fail(c, 404, "not_found")
		return
	}
	if err != nil {
		fail(c, 500, "module_error")
		return
	}
	if h.denyUnreadable(c, asset) {
		return
	}
	if asset.Status != "complete" {
		fail(c, 404, "not_found")
		return
	}
	h.serveAssetContent(c, asset, true)
}

func (h *Handler) serveAssetContent(c *gin.Context, asset store.Asset, cacheable bool) {
	ctx := c.Request.Context()
	etag := contentCacheHeaders(c, asset)
	if !cacheable {
		c.Header("Cache-Control", "private, no-store")
	}
	// 条件请求命中就到此为止：封面会被反复加载（列表滚回来、详情页重进、<img> 重排），
	// 不接 304 的话每一次加载都要把原档从对象存储整份读一遍再发一遍字节。
	//
	// 位置是硬要求：可见性判定（denyUnreadable）已经在上面做完，304 不是第二条放行路径——
	// 一份失去可见绑定的资产在这里照样 404，哪怕客户端手上的摘要与内容完全对得上。
	// 客户端缓存里那份副本最多活 max-age 秒（见 contentCacheHeaders）：这是选择"按请求鉴权
	// + 私有短缓存"时就接受的窗口，与本轮加不加 ETag 无关（不加的话副本照样存在，只是每次都回源）。
	if etagMatches(c.GetHeader("If-None-Match"), etag) {
		c.Status(http.StatusNotModified)
		return
	}
	obj, size, err := h.objects.Open(ctx, asset.ObjectKey)
	if err != nil {
		h.objectReadFailed(c, asset, err)
		return
	}
	defer obj.Close()
	// 类型与 disposition 都由服务端判定，不看调用方声明的用途：
	//  1. 判定必须落在**最终下发的类型**上（登记值可能被扩展名或内容嗅探覆盖，见 inlineMime），
	//     否则"声明 application/octet-stream、内容是 HTML"就能绕过白名单；
	//  2. 白名单内（位图/音视频/纯文本/PDF 等）才 inline，其余一律 attachment —— 包括
	//     text/html、image/svg+xml、application/xhtml+xml 这些会被当文档渲染的类型（审计 S-3）；
	//  3. nosniff 对所有响应都发：白名单内的 text/plain 也不许浏览器嗅探成 HTML。
	mimeType := inlineMime(asset, obj)
	c.Header("Content-Type", mimeType)
	c.Header("X-Content-Type-Options", "nosniff")
	if inlineAllowed(mimeType) {
		c.Header("Content-Disposition", "inline")
	} else {
		// 附件分支保留真实类型：下载到本地后系统仍能正确识别，收口靠 disposition + nosniff。
		c.Header("Content-Disposition", contentDisposition(asset.FileName))
	}
	// 交给 ServeContent：Range 与 If-Modified-Since 由它处理，大文件不用整份读进内存。
	http.ServeContent(c.Writer, c.Request, asset.FileName, time.Time{}, obj)
	_ = size
}

// contentCacheHeaders 写内容端点的缓存与条件请求头。
//
// 必须在**打开对象之前**就设好：命中 If-None-Match 时响应只有头没有正文，
// 而 RFC 9110 §9.4.1 要求 304 带上它在 200 里也会发的那几项（Cache-Control、ETag…），
// 否则缓存里的副本失去再验证依据。
func contentCacheHeaders(c *gin.Context, asset store.Asset) string {
	// 只进私有缓存：可见性是按请求问目录服务判的，共享缓存（CDN/反代）会把一次
	// 成功鉴权的响应复用给无权者，等于把可见性烤死在缓存里。max-age 给一个短窗口，
	// 让同一页面里的反复加载不必每次回到服务端。
	c.Header("Cache-Control", "private, max-age="+strconv.Itoa(contentCacheSeconds))
	etag := assetETag(asset)
	c.Header("ETag", etag)
	return etag
}

// assetETag 给出这份内容的实体标签。
//
// 内容寻址让摘要本身就是版本号：一份资产的字节由它的 sha256 唯一确定
// （assets_sha256 上有唯一索引），换内容等于换资产，因此不需要额外的版本列，
// 也不必回答“什么时候该让标签变”。
func assetETag(asset store.Asset) string {
	sum := strings.TrimSpace(asset.SHA256)
	if sum == "" {
		// 库里 sha256 是 NOT NULL，走到这里只可能是行被外部改坏；退化到资产 id 至少保证
		// 标签非空且语法合法（ETag: "" 不是合法的 entity-tag）。
		sum = asset.ID
	}
	return `"` + sum + `"`
}

// etagMatches 判定条件请求的 If-None-Match 是否命中：支持 `*`、逗号多值与弱标签（W/）。
// 实体标签按 RFC 9110 §8.8.3 是不透明串，比对区分大小写；只有 `W/` 前缀在比较时去掉。
//
// 与 readable 同一条判定的另一面：命中与否只在**放行已经成立之后**才看，
// 所以这里宽松最多让一次回源省下来，严格不会多拦住谁——权限从来不靠标签比对收口。
func etagMatches(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}
	want := strings.Trim(etag, `"`)
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		candidate = strings.TrimPrefix(candidate, "w/")
		if strings.Trim(candidate, `"`) == want {
			return true
		}
	}
	return false
}

// inlineMime 决定内联响应的内容类型：优先资产登记的 mime（上传时的 mime_type），
// 缺失或退化成 application/octet-stream 时按扩展名、再按内容嗅探补齐。
// 类型不对不会让请求失败，只会让浏览器按"用途不符"拒绝渲染（<img> 拿到 text/plain 就是破图），
// 因此这条兜底必须存在，而不是把 application/octet-stream 直接发出去。
func inlineMime(asset store.Asset, obj io.ReadSeeker) string {
	if m := strings.TrimSpace(asset.MimeType); m != "" && !strings.EqualFold(m, "application/octet-stream") {
		return m
	}
	if ext := path.Ext(asset.FileName); ext != "" {
		if m := mime.TypeByExtension(ext); m != "" {
			return m
		}
	}
	head := make([]byte, 512)
	n, _ := obj.Read(head)
	// 嗅探读走了开头，必须回到起点：剩下的内容由 ServeContent 从当前位置继续发。
	if _, err := obj.Seek(0, io.SeekStart); err != nil {
		return "application/octet-stream"
	}
	if n <= 0 {
		return "application/octet-stream"
	}
	return http.DetectContentType(head[:n])
}

// listEntityFiles 是"这个介质/轨道/表达上挂了哪些文件"的入口。
// 实体可见性由目录服务判定，绑定与文件元数据由存储服务提供。
//
// X01 读聚合：合并只广播事件、不改写别人表里的引用，绑定仍挂在旧 ID 上，
// 因此按 {请求 ID + 存活身份 + 目录别名全集} 聚合（catalog.AliasSet，与互动侧同一口径），
// 并按绑定 ID 去重。目录 identity 已返回正向链与反向全集：A→C、B→C、C→D 后从 D
// 一次读全 A/B/C/D 上的绑定。
func (h *Handler) listEntityFiles(c *gin.Context) {
	entityID := c.Param("id")
	kind, ok := h.visibleEntity(c, entityID)
	if !ok {
		return
	}
	ids := []string{entityID}
	bearer, cookie := h.credentials(c)
	if ident, err := h.catalog.Identity(c.Request.Context(), entityID, bearer, cookie); err == nil {
		// 全集展开：从旧 ID 读带正向链，从存活身份读带反向全集（目录 identity 已收齐去重）；
		// 别名空时退化为单 ID。非法 canonical 不进查询。
		if ident.CanonicalID != "" && validID(ident.CanonicalID) {
			ids = catalog.AliasSet(ident.CanonicalID, append([]string{entityID}, ident.Aliases...)...)
		}
	} else if !errors.Is(err, catalog.ErrNotVisible) {
		// 可见性已确认（visibleEntity 通过），这里问不到的是身份解析：不能回空列表——
		// 那是“这个实体没有文件”的业务结论，会把依赖故障说成事实。
		h.log.Printf("storage: 实体 %s 身份解析问不到目录服务，回 503 %s: %v", entityID, upstream.CodeUpstreamUnavailable, err)
		fail(c, http.StatusServiceUnavailable, upstream.CodeUpstreamUnavailable)
		return
	}
	files, err := h.db.BindingsForEntityIDs(c.Request.Context(), ids)
	if err != nil {
		fail(c, 500, "module_error")
		return
	}
	// 被禁发的文件不对无权者透露存在性（与 readable 同口径，见 governance.go 三态门禁）。
	files = filterBlocked(files, auth.Current(c))
	c.JSON(200, gin.H{"target_entity_id": entityID, "target_kind": kind, "files": dedupeFileBindings(files)})
}

// dedupeFileBindings 按绑定 ID 去重：多 ID 聚合时同一绑定只出现一次。
// 同一资产挂在新旧两 ID 上的两条绑定是两行事实，不在此合并。
func dedupeFileBindings(in []store.FileBinding) []store.FileBinding {
	out := make([]store.FileBinding, 0, len(in))
	seen := map[string]bool{}
	for _, f := range in {
		if seen[f.ID] {
			continue
		}
		seen[f.ID] = true
		out = append(out, f)
	}
	return out
}

func (h *Handler) download(c *gin.Context) {
	if !validID(c.Param("assetId")) {
		fail(c, 404, "not_found")
		return
	}
	ctx := c.Request.Context()
	asset, err := h.db.Asset(ctx, c.Param("assetId"))
	if errors.Is(err, store.ErrNotFound) {
		fail(c, 404, "not_found")
		return
	}
	if err != nil {
		fail(c, 500, "module_error")
		return
	}
	// 不可读一律回 404：不区分"无权限"与"不存在"，避免泄露他人上传的存在性。
	// 与 content 同一处判定；两步分开写的原因见 assetContent（denyUnreadable 已经落过响应）。
	if h.denyUnreadable(c, asset) {
		return
	}
	if asset.Status != "complete" {
		fail(c, 404, "not_found")
		return
	}
	c.Header("Cache-Control", "private, no-store")
	if !h.objects.Local() {
		// 撤销窗口：签发出的 URL 在 PresignTTL 内（默认 120 分钟）持续有效，
		// 数据库改 blocked/解绑拦不住它——那是对象存储与签发方的直接约定。
		// 要求立即禁发的分发走短期签名（调小 STORAGE_PRESIGN_TTL_MINUTES）或鉴权代理
		// （/assets/:id/content 每次重新鉴权），不要宣称"已禁发"等于"已阻断一切访问"。
		url, expires, err := h.objects.PresignDownload(ctx, asset.ObjectKey, asset.FileName)
		if err != nil {
			fail(c, 503, "storage_unavailable")
			return
		}
		c.JSON(200, gin.H{
			"asset_id":     asset.ID,
			"file_name":    asset.FileName,
			"size_bytes":   asset.SizeBytes,
			"sha256":       asset.SHA256,
			"download_url": url,
			"expires_at":   expires,
		})
		return
	}
	// 本地对象模式：没有可签名对象存储，直接由本服务流式下发。
	obj, size, err := h.objects.Open(ctx, asset.ObjectKey)
	if err != nil {
		h.objectReadFailed(c, asset, err)
		return
	}
	defer obj.Close()
	c.Header("Content-Type", asset.MimeType)
	c.Header("Content-Disposition", contentDisposition(asset.FileName))
	// 下载口一直就是 attachment，这里补 nosniff：声明类型与实际内容不符时，
	// 别给浏览器任何"按内容重新判定"的机会（对象存储模式那侧由 S3 直发，加不了头）。
	c.Header("X-Content-Type-Options", "nosniff")
	http.ServeContent(c.Writer, c.Request, asset.FileName, time.Time{}, obj)
	_ = size
}

// contentDisposition 生成下载响应头。文件名是上传者提供的任意字符串，
// 直接拼进 header 会让名字里的引号改写 disposition 的其它参数（非 ASCII 名
// 也是一串裸字节）；交给 mime.FormatMediaType 编码，必要时走 RFC 2231 的
// filename*。对象存储模式那一侧由 objects.mimeDisposition 负责，口径一致。
func contentDisposition(name string) string {
	if name == "" {
		return "attachment"
	}
	if v := mime.FormatMediaType("attachment", map[string]string{"filename": name}); v != "" {
		return v
	}
	return "attachment"
}

// verifyHash 两种用法：只给 sha256 是秒传探测（返回是否已存在），
// 给 asset_id 则读回对象重算摘要，与声明的 sha256 比对。
//
// 探测（sha 分支）允许匿名：它只回答"库里有没有这份内容"，判定与 initiate 同一口径
// （hash_verified=true），未验证的命中按不存在返回。
// 按 asset_id 校验必须登录：它要整份回读对象（成本随对象大小增长，且受同一套上限约束）
// 并写回校验结论，是"上传者/审核者维护自己资产"的动作，不是公开只读接口。
func (h *Handler) verifyHash(c *gin.Context) {
	var in struct {
		AssetID    string `json:"asset_id"`
		SHA256Hash string `json:"sha256_hash"`
	}
	if !body(c, &in) {
		return
	}
	ctx := c.Request.Context()
	if sha := strings.ToLower(strings.TrimSpace(in.SHA256Hash)); sha != "" {
		if !shaPattern.MatchString(sha) {
			fail(c, 400, "invalid_payload")
			return
		}
		// 秒传探测与 initiate 同一条判据：只有服务端验过内容的资产才算"已有这份内容"。
		// 未验证的命中按不存在返回，客户端会走正常上传，不会白等一份错内容。
		asset, err := h.db.VerifiedAssetByHash(ctx, sha)
		if err != nil {
			// 无权读取的内容一律当作不存在。
			c.JSON(200, gin.H{"exists": false})
			return
		}
		ok, uerr := h.readable(c, asset)
		if uerr != nil {
			// 问不到目录服务：不能回 exists:false——那是"这份内容不存在"的业务结论，
			// 会把依赖故障说成事实。客户端按 503 重试即可（探测失败不影响内容安全）。
			h.log.Printf("storage: 秒传探测问不到目录服务，回 503 %s: %v", upstream.CodeUpstreamUnavailable, uerr)
			fail(c, http.StatusServiceUnavailable, upstream.CodeUpstreamUnavailable)
			return
		}
		if !ok {
			// 无权读取的内容一律当作不存在。
			c.JSON(200, gin.H{"exists": false})
			return
		}
		c.JSON(200, gin.H{"exists": true, "asset_id": asset.ID, "status": asset.Status, "size_bytes": asset.SizeBytes})
		return
	}
	if in.AssetID == "" {
		fail(c, 400, "invalid_payload")
		return
	}
	// 字面量非法即格式错误，先于鉴权回 404（与其余按 id 查库的入口一致，
	// 也保住契约里"非法 id 不得被兜成 500"的口径）；合法 id 才要求登录。
	if !validID(in.AssetID) {
		fail(c, 404, "not_found")
		return
	}
	if auth.Current(c) == nil {
		// 错误码与鉴权中间件（internal/auth 的 Middleware）同口径：客户端按码分支，
		// 同一语义出现两个码会让前端只认其中一个。
		fail(c, 401, "authentication_required")
		return
	}
	asset, err := h.db.Asset(ctx, in.AssetID)
	if errors.Is(err, store.ErrNotFound) {
		fail(c, 404, "not_found")
		return
	}
	if err != nil {
		fail(c, 500, "module_error")
		return
	}
	if h.denyUnreadable(c, asset) {
		return
	}
	digest, size, verr := h.objects.VerifyHash(ctx, asset.ObjectKey, "")
	if verr != nil {
		// 与 complete 同一套上限：超限/超时都是显式失败，不返回半份摘要。
		code, status := verifyError(verr)
		fail(c, status, code)
		return
	}
	verified := digest == asset.SHA256
	if !verified {
		// 回读发现键上的内容与声明不符：只把**尚未发布**的资产钉在失败态（不参与秒传），
		// 已经 complete 的资产不在这里降级——降级属于"内容坏了"的处置决定，
		// 不该由一个匿名可触发的接口顺手改掉线上可读资产的状态，只留日志供审计跟进。
		if asset.Status != "complete" {
			_ = h.db.MarkHashMismatch(ctx, asset.ID, "hash_mismatch")
		}
		h.log.Printf("storage: asset %s 回读摘要与声明不符 declared=%s actual=%s status=%s",
			asset.ID, asset.SHA256, digest, asset.Status)
	}
	c.JSON(200, gin.H{
		"asset_id":        asset.ID,
		"sha256":          digest,
		"declared_sha256": asset.SHA256,
		"verified":        verified,
		"size_bytes":      size,
	})
}

func (h *Handler) stats(c *gin.Context) {
	// 全局容量是运营数据，跨所有上传者：用 storage.asset.moderate，
	// 与拆分前「仅管理员可看」一致（不是任何人都能从令牌拿到的公开统计）。
	if !auth.Current(c).Can(auth.PermissionAssetModerate) {
		fail(c, 403, "forbidden")
		return
	}
	assets, bytes, err := h.db.Stats(c.Request.Context())
	if err != nil {
		fail(c, 500, "module_error")
		return
	}
	pending, blocked, err := h.db.StatusCounts(c.Request.Context())
	if err != nil {
		fail(c, 500, "module_error")
		return
	}
	c.JSON(200, gin.H{"assets": assets, "bytes": bytes, "pending": pending, "blocked": blocked})
}
