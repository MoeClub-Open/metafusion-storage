package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/MoeclubM/metafusion-storage/internal/audit"
	"github.com/MoeclubM/metafusion-storage/internal/auth"
	"github.com/MoeclubM/metafusion-storage/internal/config"
	"github.com/MoeclubM/metafusion-storage/internal/store"
)

// 三态门禁（生产目标 §6，契约见 README）：一份文件能不能分发出去，由三个正交状态共同决定——
//
//  1. 校验完成（assets.status）：只有 complete 才能分发；pending 是还没验过，
//     不是还没公开，两者不能互相推导。
//  2. 目录公开（catalog 可见性）：任一绑定目标实体对请求者可见。问不到目录服务是 503，
//     不是不可见——上游抖动不能表现为文件消失（见 readable/visibleEntity）。
//  3. 允许分发（assets.blocked）：独立禁发位。目录公开不等于文件可分发；blocked 只拦分发，
//     不删绑定、不改 status，解禁即恢复。
//
// readable 管能不能读到字节，filterBlocked 管列表里给不给看，block/unblock 是
// 审核者的处置入口。三处判据必须同源：blocked 永远只给 canManageAsset 的人放行。
func filterBlocked(in []store.FileBinding, p *auth.Principal) []store.FileBinding {
	out := make([]store.FileBinding, 0, len(in))
	for _, f := range in {
		if f.Asset.Blocked && !canManageAsset(p, f.Asset.UploaderID) {
			continue
		}
		out = append(out, f)
	}
	return out
}

// quotasEnabled 报告是否启用了任一预算：全零（默认）时 initiate 不查统计表，
// 行为与此前完全一致；运维先看 stats 与配额口径，再按需收紧。
func quotasEnabled(cfg config.Config) bool {
	return cfg.UserQuotaBytes() > 0 || cfg.SiteQuotaBytes() > 0 ||
		cfg.UserConcurrentUploads > 0 || cfg.SiteConcurrentUploads > 0
}

// quotaLimits 把配置折成存储层的预留输入：零值档不限制，调用方只在 quotasEnabled 时调存储原子方法。
func quotaLimits(cfg config.Config) store.QuotaLimits {
	return store.QuotaLimits{
		UserQuotaBytes: cfg.UserQuotaBytes(),
		SiteQuotaBytes: cfg.SiteQuotaBytes(),
		UserConcurrent: cfg.UserConcurrentUploads,
		SiteConcurrent: cfg.SiteConcurrentUploads,
	}
}

// quotaCheck 是配额的纯判定（便于离线固定口径）：并发看 pending 计数，容量看
// complete 真实字节 + pending 声明大小 + 本次声明。返回空串表示通过。
func quotaCheck(u, site store.Usage, fileSize int64, cfg config.Config) (string, int) {
	if cfg.UserConcurrentUploads > 0 && u.PendingCount >= int64(cfg.UserConcurrentUploads) {
		return "too_many_uploads", http.StatusTooManyRequests
	}
	if cfg.SiteConcurrentUploads > 0 && site.PendingCount >= int64(cfg.SiteConcurrentUploads) {
		return "too_many_uploads", http.StatusTooManyRequests
	}
	if q := cfg.UserQuotaBytes(); q > 0 && u.CompleteBytes+u.PendingBytes+fileSize > q {
		return "quota_exceeded", http.StatusRequestEntityTooLarge
	}
	if q := cfg.SiteQuotaBytes(); q > 0 && site.CompleteBytes+site.PendingBytes+fileSize > q {
		return "quota_exceeded", http.StatusRequestEntityTooLarge
	}
	return "", 0
}

// blockAsset 禁发：持 storage.asset.moderate 的审核者专用。只翻 blocked 位，
// 绑定与 status 原样保留——禁发是暂停分发，不是删除事实，误禁可逆。
func (h *Handler) blockAsset(c *gin.Context) {
	h.setBlocked(c, true)
}

// unblockAsset 解禁：复位禁发位，原 status 即恢复分发资格（仍需满足 readable 的另两态）。
func (h *Handler) unblockAsset(c *gin.Context) {
	h.setBlocked(c, false)
}

// listBlockedAssets 只向存储审核者提供禁发清单；普通上传者不能据此枚举资产。
func (h *Handler) listBlockedAssets(c *gin.Context) {
	p := auth.Current(c)
	if p == nil || !p.Can(auth.PermissionAssetModerate) {
		fail(c, http.StatusForbidden, "forbidden")
		return
	}
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "100"))
	if err != nil || limit < 1 || limit > 500 {
		fail(c, http.StatusBadRequest, "invalid_payload")
		return
	}
	offset, err := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if err != nil || offset < 0 {
		fail(c, http.StatusBadRequest, "invalid_payload")
		return
	}
	assets, err := h.db.ListBlocked(c.Request.Context(), limit, offset)
	if err != nil {
		fail(c, http.StatusInternalServerError, "module_error")
		return
	}
	c.JSON(http.StatusOK, gin.H{"assets": assets, "limit": limit, "offset": offset})
}

func (h *Handler) setBlocked(c *gin.Context, block bool) {
	if !validID(c.Param("id")) {
		fail(c, 404, "not_found")
		return
	}
	p := auth.Current(c)
	if p == nil || !p.Can(auth.PermissionAssetModerate) {
		fail(c, http.StatusForbidden, "forbidden")
		return
	}
	reason := ""
	if c.Request.ContentLength != 0 {
		// 无 tag 字段：encoding/json 本来就大小写不敏感匹配 reason，
		// 未知字段仍由 body 的 DisallowUnknownFields 拒绝。
		var in struct {
			Reason string
		}
		if !body(c, &in) {
			return
		}
		reason = strings.TrimSpace(in.Reason)
		if len(reason) > 280 {
			fail(c, 400, "invalid_payload")
			return
		}
	}
	ctx := c.Request.Context()
	asset, err := h.db.Asset(ctx, c.Param("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			fail(c, 404, "not_found")
			return
		}
		fail(c, 500, "module_error")
		return
	}
	// 处置对象在动作前快照：被拒的尝试同样因此带上 target（与 unbind 同一写法）。
	h.auditAsset(c, asset, nil)
	if block {
		if err := h.db.SetBlocked(ctx, asset.ID, reason); err != nil {
			h.failStorageWrite(c, err)
			return
		}
		now := time.Now()
		asset.Blocked = true
		asset.BlockedReason = reason
		asset.BlockedAt = &now
		audit.Describe(c, audit.Detail{TargetType: "asset", TargetID: asset.ID, Changes: map[string]any{
			"blocked":        map[string]any{"from": false, "to": true},
			"blocked_reason": reason,
		}})
	} else {
		was := asset.Blocked
		if err := h.db.ClearBlocked(ctx, asset.ID); err != nil {
			fail(c, 500, "module_error")
			return
		}
		asset.Blocked = false
		asset.BlockedReason = ""
		asset.BlockedAt = nil
		audit.Describe(c, audit.Detail{TargetType: "asset", TargetID: asset.ID, Changes: map[string]any{
			"blocked": map[string]any{"from": was, "to": false},
		}})
	}
	c.JSON(200, gin.H{"asset": asset})
}
