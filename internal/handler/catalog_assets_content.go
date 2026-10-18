package handler

import (
	"errors"
	"net/http"

	"github.com/MoeclubM/metafusion-storage/internal/store"
)

import "github.com/gin-gonic/gin"

// Catalog checks the current workspace/PR viewer before proxying this private
// exact reference. The service credential alone never permits arbitrary IDs.
func (h *Handler) catalogWorkspaceAssetContent(c *gin.Context) {
	if !h.authorizeCatalogLifecycle(c) {
		return
	}
	wid, eid, aid := c.Param("workspaceID"), c.Param("entityID"), c.Param("assetID")
	if !validID(wid) || !validID(eid) || !validID(aid) {
		fail(c, http.StatusNotFound, "not_found")
		return
	}
	ctx := c.Request.Context()
	asset, err := h.db.Asset(ctx, aid)
	if errors.Is(err, store.ErrNotFound) {
		fail(c, 404, "not_found")
		return
	}
	if err != nil {
		fail(c, 500, "module_error")
		return
	}
	if asset.Status != "complete" || !asset.HashVerified || asset.Blocked {
		fail(c, 404, "not_found")
		return
	}
	if err = h.db.CheckAssetActive(ctx, aid); err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrAssetReclaiming) {
			fail(c, 404, "not_found")
		} else {
			fail(c, 500, "module_error")
		}
		return
	}
	var exists bool
	if err = h.db.DB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM storage.catalog_asset_references WHERE workspace_id=$1 AND entity_id=$2 AND asset_id=$3)
	 OR EXISTS(SELECT 1 FROM storage.bindings WHERE target_entity_id=$2 AND asset_id=$3 AND binding_role='cover_image')`, wid, eid, aid).Scan(&exists); err != nil {
		fail(c, 500, "module_error")
		return
	}
	if !exists {
		fail(c, 404, "not_found")
		return
	}
	h.serveAssetContent(c, asset, false)
}
