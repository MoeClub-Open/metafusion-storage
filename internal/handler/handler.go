// Package handler 暴露存储服务的 HTTP 契约：/api/storage/*。
// 契约以 metafusion-docs 的 docs/api-storage.md 为准（内容寻址 + 预签名直传 + 绑定用途），
// 绑定只描述"文件是谁的什么用途"，收录位置仍留在目录侧的 locator。
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/MoeclubM/metafusion-storage/internal/audit"
	"github.com/MoeclubM/metafusion-storage/internal/auth"
	"github.com/MoeclubM/metafusion-storage/internal/catalog"
	"github.com/MoeclubM/metafusion-storage/internal/config"
	"github.com/MoeclubM/metafusion-storage/internal/objects"
	"github.com/MoeclubM/metafusion-storage/internal/store"
	"github.com/MoeclubM/metafusion-storage/internal/upstream"
)

var (
	shaPattern  = regexp.MustCompile("^[0-9a-f]{64}$")
	codePattern = regexp.MustCompile("^[a-z][a-z0-9_]{0,31}$")
)

const defaultRole = "master_archive"

type Handler struct {
	db       *store.Store
	objects  *objects.Store
	catalog  *catalog.Client
	verifier *auth.Verifier
	cfg      config.Config
	log      *log.Logger
	// audit 是审计留痕记录器；nil 表示未接线（离线单测只挂路由、不写库）。见 UseAudit。
	audit *audit.Recorder
}

func New(db *store.Store, objs *objects.Store, cat *catalog.Client, verifier *auth.Verifier, cfg config.Config) *Handler {
	return &Handler{db: db, objects: objs, catalog: cat, verifier: verifier, cfg: cfg, log: log.Default()}
}

// Register 挂载契约路由。读接口允许匿名（可见性由目录实体决定）；
// 创建/登记自己的资产要 storage.asset.upload，其余写接口只要求登录（见 requireUpload）。
func (h *Handler) Register(r *gin.Engine) {
	h.registerCatalogLifecycle(r)
	v := h.verifier
	api := r.Group("/api/storage")
	// 审计中间件挂在 api 组上、在路由注册之前（契约 §3）：只有注册表里登记了动作码的写路由才留痕。
	//
	// 顺序是硬要求：组级中间件排在路由级中间件之前，因此审计总是先于身份中间件（v.Required / v.Middleware）
	// 进入请求链——身份中间件 abort 掉的写请求（伪造/失效 PAT、账号服务不可达、无凭据）因此也留痕。
	// 把审计挪到任何路由级中间件之后都会让这类请求静默消失（见 audit_postgres_test.go 的
	// TestAuditLogAgainstPostgresAuthRejectedWrites）。
	api.Use(h.auditMiddleware())
	api.Use(v.Middleware())
	api.Use(apiRateLimiter())
	{
		api.POST("/upload/initiate", v.Required(), h.requireUpload(), h.initiateUpload)
		api.POST("/upload/complete", v.Required(), h.requireUpload(), h.completeUpload)
		api.PUT("/upload/stream/:assetId", v.Required(), h.requireUpload(), h.streamUpload)
		api.POST("/bind", v.Required(), h.requireUpload(), h.bind)
		api.POST("/assets/:id/block", v.Required(), h.blockAsset)
		api.POST("/assets/:id/unblock", v.Required(), h.unblockAsset)
		// 解绑只能删自己的绑定（handler 内按上传者判定），不需要额外权限码。
		api.DELETE("/bindings/:id", v.Required(), h.unbind)
		api.POST("/verify-hash", v.Middleware(), h.verifyHash)
		api.GET("/assets/:id", v.Middleware(), h.getAsset)
		api.GET("/assets", v.Required(), h.listAssets)
		api.GET("/bindings", v.Required(), h.listBindings)
		api.GET("/moderation/blocked", v.Required(), h.listBlockedAssets)
		api.GET("/assets/:id/content", v.Middleware(), h.assetContent)
		api.GET("/entities/:id/files", v.Middleware(), h.listEntityFiles)
		api.GET("/download/:assetId", v.Middleware(), h.download)
		api.GET("/stats", v.Required(), h.stats)
	}
}

func fail(c *gin.Context, status int, code string) {
	// 审计中间件据此写 result=failure + error_code：错误码必须与响应体 error 字段一致（契约 §1）。
	audit.Fail(c, code)
	c.JSON(status, gin.H{"error": code})
}

// maxJSONBody 是 JSON 写接口的请求体上限。必须自己封顶：网关的 client_max_body_size 是 1G，
// 应用层不设限就等于把"一个请求能让服务占多少内存"交给调用方决定。
const maxJSONBody = 2 << 20

// body 是 JSON 写接口统一的请求解析：2MB 上限 + 拒绝未知字段。
//
// 与流式上传（streamUpload）分成两档是刻意的：那里收到的就是文件本身，上限按
// STORAGE_MAX_UPLOAD_MB 单独声明；这四个端点只收元数据，2MB 足够。
// 未知字段一律拒绝而不是静默忽略：字段名拼错的请求不会再"看起来成功了"，
// 与目录服务、互动服务的写接口同一口径。
func body(c *gin.Context, v any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxJSONBody)
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		fail(c, 400, "invalid_payload")
		return false
	}
	return true
}

// requireUpload 要求登录且持有 storage.asset.upload：上传（创建自己的资产、完成直传）
// 与绑定用途属于"创建与登记自己的东西"，与 storage.asset.moderate（处置他人资产）分开收口。
//
// 为什么现在才收口：本服务此前是"登录即可上传"，因为账号服务播种的系统组里没有任何 storage.* 码，
// 直接按码收口会让存量实例只剩管理员能上传。本批次账号侧已把该码播进 member 组并回填存量实例，
// 两侧同批上线后收口不改变既有行为（成员照常上传），管理员之后可以按需移除该码来收紧。
//
// 401 由 v.Required() 先给出（未登录），这里只把"已登录但缺码"判成 403 forbidden。
func (h *Handler) requireUpload() gin.HandlerFunc {
	return func(c *gin.Context) {
		if p := auth.Current(c); p == nil || !p.Can(auth.PermissionAssetUpload) {
			fail(c, http.StatusForbidden, "forbidden")
			c.Abort()
			return
		}
		c.Next()
	}
}

// validID 报告路径或载荷里的 id 是否是合法 uuid。存储侧的所有 id 都是 uuid 主键，
// 非法字面量必须按"不存在"处理：直接送进 uuid 列只会拿到 pq 的解析错误，
// 再被兜成 500 module_error（契约里没有这个状态码，线上可复现）。
func validID(id string) bool {
	_, err := uuid.Parse(id)
	return err == nil
}

func (h *Handler) credentials(c *gin.Context) (string, string) {
	bearer := ""
	if authz := strings.TrimSpace(c.GetHeader("Authorization")); len(authz) > 7 && strings.EqualFold(authz[:7], "bearer ") {
		bearer = strings.TrimSpace(authz[7:])
	}
	cookie := ""
	if ck, err := c.Request.Cookie("mf_session"); err == nil {
		cookie = ck.Value
	}
	return bearer, cookie
}

// visibleEntity 通过目录服务确认实体可见，并返回权威 kind。
// 存储侧不缓存目录结论，也不直连目录库；可见性规则只有 catalog 一处实现。
//
// 三种终局，第三种以前被折进第二种：目录说不可见 → 404 not_found；
// 问不到目录服务（超时/连接失败/5xx/熔断打开）→ 503 upstream_unavailable，并**当场写响应**。
// 折在一起会让上游抖一下就等于"这个实体不存在"——文件从列表里静默消失，运维看不到故障。
// 因此本函数负责写失败响应，调用方只需 if !ok { return }。
func (h *Handler) visibleEntity(c *gin.Context, entityID string) (string, bool) {
	_, kind, ok := h.canonicalEntity(c, entityID)
	return kind, ok
}

func (h *Handler) canonicalEntity(c *gin.Context, entityID string) (string, string, bool) {
	if _, err := uuid.Parse(entityID); err != nil {
		fail(c, http.StatusNotFound, "not_found")
		return "", "", false
	}
	bearer, cookie := h.credentials(c)
	identity, err := h.catalog.Identity(c.Request.Context(), entityID, bearer, cookie)
	if err == nil {
		return identity.CanonicalID, identity.Entity.Kind, true
	}
	if errors.Is(err, catalog.ErrNotVisible) {
		fail(c, http.StatusNotFound, "not_found")
		return "", "", false
	}
	h.log.Printf("storage: 实体 %s 可见性问不到目录服务，回 503 %s: %v", entityID, upstream.CodeUpstreamUnavailable, err)
	fail(c, http.StatusServiceUnavailable, upstream.CodeUpstreamUnavailable)
	return "", "", false
}

func normalizeRole(role string) string {
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "" {
		return defaultRole
	}
	if !codePattern.MatchString(role) {
		return ""
	}
	return role
}

type initiateRequest struct {
	FileName         string `json:"file_name"`
	FileSize         int64  `json:"file_size"`
	SHA256Hash       string `json:"sha256_hash"`
	MimeType         string `json:"mime_type"`
	PartCount        int    `json:"part_count"`
	TargetEntityID   string `json:"target_entity_id"`
	TargetEntityType string `json:"target_entity_type"`
	BindingRole      string `json:"binding_role"`
}

type initiateResponse struct {
	IsInstantUpload bool           `json:"is_instant_upload"`
	AssetID         string         `json:"asset_id"`
	ObjectKey       string         `json:"object_key"`
	UploadID        string         `json:"upload_id,omitempty"`
	PresignedURLs   []string       `json:"presigned_urls,omitempty"`
	DirectUploadURL string         `json:"direct_upload_url,omitempty"`
	PartSizeHint    int64          `json:"part_size_hint,omitempty"`
	ExpiresAt       time.Time      `json:"expires_at"`
	Asset           *store.Asset   `json:"asset,omitempty"`
	Binding         *store.Binding `json:"binding,omitempty"`
}

// initiateUpload 是直传第一步：命中 sha256 即秒传，否则签发直传地址；
// 同一 sha256 的未完成上传由上传者本人续传，避免两个客户端互相覆盖。
func (h *Handler) initiateUpload(c *gin.Context) {
	var in initiateRequest
	if !body(c, &in) {
		return
	}
	in.FileName = strings.TrimSpace(in.FileName)
	in.SHA256Hash = strings.ToLower(strings.TrimSpace(in.SHA256Hash))
	if in.FileName == "" || len(in.FileName) > 255 || !shaPattern.MatchString(in.SHA256Hash) || in.FileSize < 0 {
		fail(c, 400, "invalid_payload")
		return
	}
	if in.PartCount <= 0 {
		in.PartCount = 1
	}
	if in.PartCount > h.cfg.MaxPartCount {
		fail(c, 400, "too_many_parts")
		return
	}
	role := normalizeRole(in.BindingRole)
	if role == "" {
		fail(c, 400, "invalid_binding_role")
		return
	}
	if role == "cover_image" && in.TargetEntityID != "" {
		fail(c, http.StatusConflict, "catalog_managed_binding")
		return
	}
	if in.MimeType == "" {
		in.MimeType = "application/octet-stream"
	}
	targetKind := ""
	if in.TargetEntityID != "" {
		canonical, kind, ok := h.canonicalEntity(c, in.TargetEntityID)
		if !ok {
			return
		}
		targetKind = kind
		in.TargetEntityID = canonical
		if in.TargetEntityType != "" && !strings.EqualFold(in.TargetEntityType, kind) {
			// 类型以目录服务的权威值为准，声明不符直接拒绝，避免绑定写错维度。
			fail(c, 400, "invalid_target_type")
			return
		}
	}

	p := auth.Current(c)
	ctx := c.Request.Context()

	// 命中分两种，判据必须分开：
	//  1. **已验证**的资产才能秒传（complete 态由 CompleteAsset 保证 hash_verified=true）；
	//  2. 未验证的未完成资产（上传中、或回读校验失败过）不能秒传，但必须按
	//     "同一 sha256 的未完成上传由上传者本人续传"的既有口径继续处理——
	//     校验失败的上传者重传正确内容才能收尾。若这里直接当"不存在"去新建资产，
	//     就会撞上 assets_sha256 唯一索引（那行 pending 占位行还在），
	//     结果是这个 sha256 被一次失败上传永久占死，谁也传不了。
	asset, err := h.db.VerifiedAssetByHash(ctx, in.SHA256Hash)
	if errors.Is(err, store.ErrNotFound) {
		asset, err = h.db.AssetByHash(ctx, in.SHA256Hash)
	}
	switch {
	case err == nil && asset.Status == "complete":
		// S02 复用门禁（与 verify-hash/下载/独立 bind 同一口径，字节去重与授权分开）：
		// 先查可读性，无权命中统一 404（不返他人元数据，上游故障则 503），摘要不做授权凭证；
		// 再查再绑定权限，新增绑定需所有者/审核（与 bind 一致），可读仅允许秒传元数据复用。
		if h.denyUnreadable(c, asset) {
			return
		}
		if err := h.db.CheckAssetActive(ctx, asset.ID); err != nil {
			h.failStorageWrite(c, err)
			return
		}
		if in.TargetEntityID != "" && !canManageAsset(p, asset.UploaderID) {
			fail(c, 403, "forbidden")
			return
		}
		resp := initiateResponse{IsInstantUpload: true, AssetID: asset.ID, ObjectKey: asset.ObjectKey, Asset: &asset}
		h.auditAsset(c, asset, map[string]any{"is_instant_upload": true})
		if in.TargetEntityID != "" {
			b, berr := h.createBinding(ctx, asset.ID, in.TargetEntityID, targetKind, role, p.ID)
			if berr != nil {
				h.failStorageWrite(c, berr)
				return
			}
			resp.Binding = &b
			h.auditAsset(c, asset, map[string]any{"binding_id": b.ID, "binding_role": role, "target_entity_id": b.TargetEntityID})
		}
		c.JSON(200, resp)
		return
	case err == nil:
		// 续传/覆盖他人尚未完成的上传是跨用户处置，用 storage.asset.moderate
		// （拆分前这里是 role == admin：语义相同，只是改成认权限组）。
		if asset.UploaderID != p.ID && !p.Can(auth.PermissionAssetModerate) {
			fail(c, 409, "upload_in_progress")
			return
		}
		// Renew before signing addresses: a durable deletion gate must not
		// hand out new upload credentials for bytes already being reclaimed.
		if err := h.db.SetUploadExpiry(ctx, asset.ID, time.Now().Add(h.cfg.UploadLease())); err != nil {
			h.failStorageWrite(c, err)
			return
		}
		resp, rerr := h.presign(ctx, asset, in.PartCount, false)
		if rerr != nil {
			if errors.Is(rerr, store.ErrAssetReclaiming) {
				h.failStorageWrite(c, rerr)
				return
			}
			fail(c, 503, "storage_unavailable")
			return
		}
		// 续传即"我还活着"：刷新租约，避免传得慢的上传被清理任务收走。
		// 续传/覆盖他人未完成的上传是跨用户处置：摘要里点出这次到底是不是替别人收尾。
		h.auditAsset(c, asset, map[string]any{"resumed": true, "part_count": in.PartCount, "cross_user": asset.UploaderID != p.ID})
		c.JSON(200, resp)
		return
	case !errors.Is(err, store.ErrNotFound):
		fail(c, 500, "module_error")
		return
	}

	// 配额只拦"新建占位"：续传是把已占的预算传完，不在此判定。
	// 预留与占位同事务（CreatePendingAsset 内锁序固定、锁内重查），并发初始化不再互相超发。
	lease := time.Now().Add(h.cfg.UploadLease())
	asset = store.Asset{
		ID:              uuid.NewString(),
		SHA256:          in.SHA256Hash,
		DeclaredSize:    in.FileSize,
		MimeType:        in.MimeType,
		FileName:        in.FileName,
		ObjectKey:       h.objects.KeyFor(in.SHA256Hash, in.FileName),
		Status:          "pending",
		UploaderID:      p.ID,
		UploadExpiresAt: &lease,
	}
	if quotasEnabled(h.cfg) {
		if err = h.db.CreatePendingAsset(ctx, asset, quotaLimits(h.cfg)); err != nil {
			switch {
			case errors.Is(err, store.ErrQuotaExceeded):
				fail(c, http.StatusRequestEntityTooLarge, "quota_exceeded")
			case errors.Is(err, store.ErrTooManyUploads):
				fail(c, http.StatusTooManyRequests, "too_many_uploads")
			default:
				fail(c, 500, "module_error")
			}
			return
		}
	} else if err = h.db.CreateAsset(ctx, asset); err != nil {
		fail(c, 500, "module_error")
		return
	}
	resp, rerr := h.presign(ctx, asset, in.PartCount, true)
	if rerr != nil {
		fail(c, 503, "storage_unavailable")
		return
	}
	h.auditAsset(c, asset, map[string]any{"created": true, "part_count": in.PartCount})
	// 新建资产时若同时给了目标实体，绑定在直传完成前后都成立：
	// 绑定只是"这份内容属于谁"，不依赖上传是否已落定。
	if in.TargetEntityID != "" {
		b, berr := h.createBinding(ctx, asset.ID, in.TargetEntityID, targetKind, role, p.ID)
		if berr != nil {
			h.failStorageWrite(c, berr)
			return
		}
		resp.Binding = &b
		h.auditAsset(c, asset, map[string]any{"binding_id": b.ID, "binding_role": role, "target_entity_id": b.TargetEntityID})
	}
	c.JSON(200, resp)
}

// presign 签发直传地址：分片上传建立（或复用）分片会话，本地对象模式返回服务端接收地址。
func (h *Handler) presign(ctx context.Context, asset store.Asset, partCount int, fresh bool) (initiateResponse, error) {
	resp := initiateResponse{AssetID: asset.ID, ObjectKey: asset.ObjectKey, ExpiresAt: time.Now().Add(h.cfg.PresignTTL)}
	if h.objects.Local() {
		resp.DirectUploadURL = "/api/storage/upload/stream/" + asset.ID
		return resp, nil
	}
	uploadID := asset.MultipartUploadID
	if partCount > 1 && (fresh || uploadID == "") {
		var err error
		uploadID, err = h.objects.NewParts(ctx, asset.ObjectKey, asset.MimeType, partCount)
		if err != nil {
			return resp, err
		}
		if err = h.db.SetUploadSession(ctx, asset.ID, uploadID); err != nil {
			return resp, err
		}
	}
	urls, err := h.objects.PresignParts(ctx, asset.ObjectKey, asset.MimeType, partCount, uploadID)
	if err != nil {
		return resp, err
	}
	resp.UploadID = uploadID
	resp.PresignedURLs = urls
	if partCount > 1 && asset.DeclaredSize > 0 {
		resp.PartSizeHint = (asset.DeclaredSize + int64(partCount) - 1) / int64(partCount)
	}
	return resp, nil
}

// createBinding 写一条"文件 → 实体"的绑定。
func (h *Handler) createBinding(ctx context.Context, assetID, entityID, kind, role, userID string) (store.Binding, error) {
	b := store.Binding{
		ID:             uuid.NewString(),
		AssetID:        assetID,
		TargetEntityID: entityID,
		TargetKind:     kind,
		BindingRole:    role,
		CreatedBy:      userID,
	}
	if err := h.db.Bind(ctx, b); err != nil {
		return store.Binding{}, err
	}
	return h.db.BindingByReference(ctx, assetID, entityID, role)
}

// canManageAsset 是纯函数：上传者本人，或持有 storage.asset.moderate 的审核者，
// 可完成/绑定/解绑。前者是所有权（自己的文件自己收尾），后者是审核权
// （代替拆分前的 role == admin，见 internal/auth/permission.go 的备注）。
func canManageAsset(p *auth.Principal, uploader string) bool {
	return p != nil && (p.ID == uploader || p.Can(auth.PermissionAssetModerate))
}

func (h *Handler) completeUpload(c *gin.Context) {
	var in struct {
		AssetID  string         `json:"asset_id"`
		UploadID string         `json:"upload_id"`
		Parts    []objects.Part `json:"parts"`
	}
	if !body(c, &in) || in.AssetID == "" {
		fail(c, 400, "invalid_payload")
		return
	}
	if !validID(in.AssetID) {
		fail(c, 404, "not_found")
		return
	}
	p := auth.Current(c)
	ctx := c.Request.Context()
	asset, err := h.db.Asset(ctx, in.AssetID)
	if errors.Is(err, store.ErrNotFound) {
		fail(c, 404, "not_found")
		return
	}
	if err != nil {
		fail(c, 500, "module_error")
		return
	}
	if !canManageAsset(p, asset.UploaderID) {
		fail(c, 403, "forbidden")
		return
	}
	// 被动对象与"变更前"摘要在这一步就确定：回读不符、超限这些失败路径同样要能追到是哪份资产。
	h.auditAsset(c, asset, nil)
	if err := h.db.SetUploadExpiry(ctx, asset.ID, time.Now().Add(h.cfg.UploadLease())); err != nil {
		h.failStorageWrite(c, err)
		return
	}
	if asset.Status == "complete" {
		h.auditAsset(c, asset, map[string]any{"already_complete": true})
		c.JSON(200, gin.H{"asset": asset, "already_complete": true})
		return
	}
	// 客户端声明的大小已经超上限时不必先合并：回读校验注定失败，
	// 提前失败也避免为一份不可能发布的资产在对象存储里留下碎片。
	if limit := h.objects.VerifyMaxBytes(); limit > 0 && asset.DeclaredSize > limit {
		fail(c, 413, "hash_verify_too_large")
		return
	}
	uploadID := in.UploadID
	if uploadID == "" {
		uploadID = asset.MultipartUploadID
	}
	size, err := h.objects.CompleteUpload(ctx, asset.ObjectKey, uploadID, in.Parts)
	if err != nil {
		fail(c, 400, "upload_incomplete")
		return
	}
	if h.objects.Local() {
		// 本地模式没有预签名直传：对象由 /upload/stream 边收边算摘要后落定，
		// complete 只做幂等确认（该路径已在 streamUpload 里验过摘要）。
		h.auditAsset(c, asset, map[string]any{"object_mode": "local", "confirmed": true})
		c.JSON(200, gin.H{"asset": asset})
		return
	}
	if asset.DeclaredSize > 0 && size != asset.DeclaredSize {
		_ = h.objects.AbortUpload(ctx, asset.ObjectKey, uploadID)
		fail(c, 409, "size_mismatch")
		return
	}
	// 未知大小（0 声明）按实际大小兑现：锁内重算预算，不足即明确拒绝，资产保持 pending。
	if asset.DeclaredSize == 0 && quotasEnabled(h.cfg) {
		if cerr := h.db.CheckCompleteCapacity(ctx, asset.UploaderID, asset.DeclaredSize, size, quotaLimits(h.cfg)); cerr != nil {
			if errors.Is(cerr, store.ErrQuotaExceeded) {
				_ = h.objects.AbortUpload(ctx, asset.ObjectKey, uploadID)
				_ = h.objects.Remove(ctx, asset.ObjectKey)
				_ = h.db.MarkHashMismatch(ctx, asset.ID, "quota_exceeded")
				fail(c, http.StatusRequestEntityTooLarge, "quota_exceeded")
				return
			}
			fail(c, 500, "module_error")
			return
		}
	}
	// 预签名路径的内容从未经过服务端：落定前必须整份回读、重算 sha256 再比对。
	// 大小一致不足以证明内容一致——等长的另一份内容完全可能挂在同一个 sha256 键上。
	started := time.Now()
	digest, read, verr := h.objects.VerifyHash(ctx, asset.ObjectKey, asset.SHA256)
	if verr != nil {
		// 校验没通过就没有"完成"可谈：资产留在 pending（既不参与秒传也不可下载），
		// 摘要不符的原始对象也一并中止/删除，不给内容寻址键留污染对象。
		reason, status := verifyError(verr)
		h.log.Printf("storage: asset %s 回读校验失败 code=%s size=%d read=%d elapsed=%s err=%v",
			asset.ID, reason, size, read, time.Since(started), verr)
		if !errors.Is(verr, objects.ErrVerifyTooLarge) {
			_ = h.objects.AbortUpload(ctx, asset.ObjectKey, uploadID)
		}
		_ = h.objects.Remove(ctx, asset.ObjectKey)
		if merr := h.db.MarkHashMismatch(ctx, asset.ID, reason); merr != nil {
			h.log.Printf("storage: asset %s 记录校验失败原因出错: %v", asset.ID, merr)
		}
		fail(c, status, reason)
		return
	}
	if err = h.db.MarkHashVerified(ctx, asset.ID, read); err != nil {
		h.failStorageWrite(c, err)
		return
	}
	if err = h.db.CompleteAsset(ctx, asset.ID, read); err != nil {
		// 到这里 hash_verified 已经为真，ErrAssetUnverified 只会在数据被外部改坏时出现。
		h.failStorageWrite(c, err)
		return
	}
	before := asset.Status
	asset.Status = "complete"
	asset.SizeBytes = read
	asset.HashVerified = true
	h.log.Printf("storage: asset %s 回读校验通过 sha256=%s size=%d elapsed=%s", asset.ID, digest, read, time.Since(started))
	audit.Describe(c, audit.Detail{TargetType: "asset", TargetID: asset.ID,
		Changes: assetTransition(before, asset, map[string]any{"verified_size_bytes": read})})
	c.JSON(200, gin.H{"asset": asset})
}

// verifyError 把回读校验的失败原因翻成对外错误码与状态码（纯函数，便于离线固定口径）：
// 摘要不符是客户端内容错了（409），超限与超时是这份对象太大/太慢（413、408）。
// 其它读取失败不是"内容错"，按 503 处理，避免把对象存储故障说成上传者的问题。
func verifyError(err error) (code string, status int) {
	switch {
	case errors.Is(err, objects.ErrHashMismatch):
		return "hash_mismatch", 409
	case errors.Is(err, objects.ErrVerifyTooLarge):
		return "hash_verify_too_large", 413
	case errors.Is(err, objects.ErrVerifyTimeout):
		return "verify_timeout", 408
	default:
		return "storage_unavailable", 503
	}
}

// streamUpload 是服务端接收路径：本地对象模式的主要上传方式，
// 同时也可作为预签名不可用时的兜底。落盘前流式计算 sha256 并与声明比对。
func (h *Handler) streamUpload(c *gin.Context) {
	if !validID(c.Param("assetId")) {
		fail(c, 404, "not_found")
		return
	}
	p := auth.Current(c)
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
	if !canManageAsset(p, asset.UploaderID) {
		fail(c, 403, "forbidden")
		return
	}
	h.auditAsset(c, asset, nil)
	if err := h.db.SetUploadExpiry(ctx, asset.ID, time.Now().Add(h.cfg.UploadLease())); err != nil {
		h.failStorageWrite(c, err)
		return
	}
	// 单列一档上限而不是复用 JSON 写接口的 2MB：这里收到的是原始文件字节，
	// 大小由 STORAGE_MAX_UPLOAD_MB（默认 0 = 不限制，大文件优先）决定，
	// 声明的大小在 initiate 与落定阶段另有校验。网关的 client_max_body_size 是 1G，
	// 因此未配置该变量时本服务不额外收口。
	if h.cfg.MaxUploadMB > 0 {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, int64(h.cfg.MaxUploadMB)<<20)
	}
	// PutStream 在发布对象之前就比对声明摘要：不符时对象根本没进对象存储，
	// 因此这里只把 ErrHashMismatch 翻成 409，不需要再补一次"读完才比对"的清理。
	size, _, err := h.objects.PutStream(ctx, asset.ObjectKey, c.Request.Body, asset.SHA256)
	if errors.Is(err, objects.ErrHashMismatch) {
		fail(c, 409, "hash_mismatch")
		return
	}
	if err != nil {
		fail(c, 400, "upload_failed")
		return
	}
	// 流式落定同样按声明兑现：声明为正必须与实际一致，否则刚写入的对象立即清除。
	if asset.DeclaredSize > 0 && size != asset.DeclaredSize {
		_ = h.objects.Remove(ctx, asset.ObjectKey)
		fail(c, 409, "size_mismatch")
		return
	}
	// 未知大小（0 声明）按实际大小兑现：锁内重算预算，不足即明确拒绝并清除刚写入的对象。
	if asset.DeclaredSize == 0 && quotasEnabled(h.cfg) {
		if cerr := h.db.CheckCompleteCapacity(ctx, asset.UploaderID, asset.DeclaredSize, size, quotaLimits(h.cfg)); cerr != nil {
			_ = h.objects.Remove(ctx, asset.ObjectKey)
			if errors.Is(cerr, store.ErrQuotaExceeded) {
				_ = h.db.MarkHashMismatch(ctx, asset.ID, "quota_exceeded")
				fail(c, http.StatusRequestEntityTooLarge, "quota_exceeded")
				return
			}
			fail(c, 500, "module_error")
			return
		}
	}
	if err := h.db.MarkHashVerified(ctx, asset.ID, size); err != nil {
		h.failStorageWrite(c, err)
		return
	}
	if err := h.db.CompleteAsset(ctx, asset.ID, size); err != nil {
		h.failStorageWrite(c, err)
		return
	}
	before := asset.Status
	asset.Status = "complete"
	asset.SizeBytes = size
	asset.HashVerified = true
	// 只记元数据：请求体是文件本身（可能上百 MB），审计绝不读它、也不放它的任何片段。
	audit.Describe(c, audit.Detail{TargetType: "asset", TargetID: asset.ID,
		Changes: assetTransition(before, asset, nil)})
	c.JSON(200, gin.H{"asset": asset})
}

func (h *Handler) bind(c *gin.Context) {
	var in struct {
		AssetID          string `json:"asset_id"`
		TargetEntityID   string `json:"target_entity_id"`
		TargetEntityType string `json:"target_entity_type"`
		BindingRole      string `json:"binding_role"`
	}
	if !body(c, &in) || in.AssetID == "" || in.TargetEntityID == "" {
		fail(c, 400, "invalid_payload")
		return
	}
	role := normalizeRole(in.BindingRole)
	if role == "" {
		fail(c, 400, "invalid_binding_role")
		return
	}
	if role == "cover_image" {
		fail(c, http.StatusConflict, "catalog_managed_binding")
		return
	}
	p := auth.Current(c)
	ctx := c.Request.Context()
	asset, err := h.db.Asset(ctx, in.AssetID)
	if errors.Is(err, store.ErrNotFound) {
		fail(c, 404, "not_found")
		return
	}
	if err != nil {
		fail(c, 500, "module_error")
		return
	}
	// S02 绑定门禁（与秒传复用同一口径）：先查可读性，不存在与无权统一 404 不探存在性；
	// 再查所有者/审核，可读仅允许读，新增绑定仍需所有者/审核，避免摘要预言扩大访问。
	if h.denyUnreadable(c, asset) {
		return
	}
	if !canManageAsset(p, asset.UploaderID) {
		fail(c, 403, "forbidden")
		return
	}
	h.auditAsset(c, asset, nil)
	canonical, kind, ok := h.canonicalEntity(c, in.TargetEntityID)
	if !ok {
		return
	}
	if in.TargetEntityType != "" && !strings.EqualFold(in.TargetEntityType, kind) {
		fail(c, 400, "invalid_target_type")
		return
	}
	b, err := h.createBinding(ctx, asset.ID, canonical, kind, role, p.ID)
	if err != nil {
		h.failStorageWrite(c, err)
		return
	}
	audit.Describe(c, audit.Detail{TargetType: "binding", TargetID: b.ID, Changes: map[string]any{
		"asset_id":         asset.ID,
		"sha256":           asset.SHA256,
		"target_entity_id": b.TargetEntityID,
		"target_kind":      b.TargetKind,
		"binding_role":     b.BindingRole,
	}})
	c.JSON(200, gin.H{"binding": b})
}

func (h *Handler) unbind(c *gin.Context) {
	if !validID(c.Param("id")) {
		fail(c, 404, "not_found")
		return
	}
	p := auth.Current(c)
	ctx := c.Request.Context()
	b, err := h.db.Binding(ctx, c.Param("id"))
	if errors.Is(err, store.ErrNotFound) {
		fail(c, 404, "not_found")
		return
	}
	if err != nil {
		fail(c, 500, "module_error")
		return
	}
	asset, err := h.db.Asset(ctx, b.AssetID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		fail(c, 500, "module_error")
		return
	}
	// 绑定创建者、文件上传者或管理员可以解绑：绑定是"关系"，纠错不应被上传者身份卡住。
	if !canManageAsset(p, asset.UploaderID) && b.CreatedBy != p.ID {
		fail(c, 403, "forbidden")
		return
	}
	if b.BindingRole == "cover_image" {
		fail(c, http.StatusConflict, "catalog_managed_binding")
		return
	}
	// 快照在删除前取：审计要能回答"删掉的是哪一条关系"；被拒的尝试同样因此带上 target。
	audit.Describe(c, audit.Detail{TargetType: "binding", TargetID: b.ID, Changes: map[string]any{
		"asset_id":         b.AssetID,
		"target_entity_id": b.TargetEntityID,
		"target_kind":      b.TargetKind,
		"binding_role":     b.BindingRole,
		"created_by":       b.CreatedBy,
	}})
	if err = h.db.Unbind(ctx, b.ID); err != nil {
		fail(c, 500, "module_error")
		return
	}
	audit.Describe(c, audit.Detail{TargetType: "binding", TargetID: b.ID, Changes: map[string]any{"removed": true}})
	c.JSON(200, gin.H{"ok": true})
}
