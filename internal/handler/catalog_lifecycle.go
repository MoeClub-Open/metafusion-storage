package handler

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/MoeclubM/metafusion-storage/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// This endpoint is registered outside /api/storage and must remain inaccessible
// through the public gateway. An empty configured secret never authenticates.
func (h *Handler) registerCatalogLifecycle(r *gin.Engine) {
	if strings.TrimSpace(h.cfg.CatalogLifecycleSecret) == "" {
		return
	}
	r.POST("/internal/catalog/lifecycle", h.catalogLifecycle)
	r.GET("/internal/catalog/binding-targets", h.catalogBindingTargets)
	r.POST("/internal/catalog/assets/prepare", h.catalogAssetsPrepare)
	r.GET("/internal/catalog/workspaces/:workspaceID/entities/:entityID/assets/:assetID/content", h.catalogWorkspaceAssetContent)
}

func (h *Handler) catalogAssetsPrepare(c *gin.Context) {
	if !h.authorizeCatalogLifecycle(c) {
		return
	}
	var in store.CatalogAssetReferences
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxJSONBody)
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		fail(c, 400, "invalid_payload")
		return
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		fail(c, 400, "invalid_payload")
		return
	}
	if !validID(in.WorkspaceID) || len(in.References) == 0 || len(in.References) > 10000 {
		fail(c, 400, "invalid_payload")
		return
	}
	seen := map[string]bool{}
	for _, ref := range in.References {
		key := ref.EntityID + ":" + ref.AssetID
		if !validID(ref.EntityID) || !validID(ref.AssetID) || seen[key] || len(ref.ActorIDs) > 100 || (ref.ModerateActorID != "" && !validID(ref.ModerateActorID)) {
			fail(c, 400, "invalid_payload")
			return
		}
		seen[key] = true
		for _, actor := range ref.ActorIDs {
			if !validID(actor) {
				fail(c, 400, "invalid_payload")
				return
			}
		}
	}
	if err := h.db.RetainCatalogAssets(c.Request.Context(), in); err != nil {
		if errors.Is(err, store.ErrInvalidCatalogAsset) {
			fail(c, 400, "invalid_catalog_asset")
			return
		}
		h.failStorageWrite(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) authorizeCatalogLifecycle(c *gin.Context) bool {
	configured := strings.TrimSpace(h.cfg.CatalogLifecycleSecret)
	want := sha256.Sum256([]byte("Bearer " + configured))
	got := sha256.Sum256([]byte(c.GetHeader("Authorization")))
	if configured == "" || subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_service_credential"})
		return false
	}
	return true
}

func (h *Handler) catalogLifecycle(c *gin.Context) {
	if !h.authorizeCatalogLifecycle(c) {
		return
	}
	var state store.CatalogLifecycle
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxJSONBody)
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&state); err != nil || !validCatalogLifecycle(state) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_payload"})
		return
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_payload"})
		return
	}
	if err := h.db.ApplyCatalogLifecycle(c.Request.Context(), state); err != nil {
		h.log.Printf("storage: catalog lifecycle reconciliation failed for entity %s: %v", state.EntityID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "module_error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) catalogBindingTargets(c *gin.Context) {
	if !h.authorizeCatalogLifecycle(c) {
		return
	}
	after := c.Query("after")
	cursor, cursorErr := uuid.Parse(after)
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "100"))
	if (after != "" && (cursorErr != nil || cursor.String() != after)) || err != nil || limit <= 0 || limit > 100 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_payload"})
		return
	}
	ids, err := h.db.CatalogBindingTargets(c.Request.Context(), after, limit)
	if err != nil {
		h.log.Printf("storage: catalog binding inventory failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "module_error"})
		return
	}
	next := ""
	if len(ids) == limit {
		next = ids[len(ids)-1]
	}
	c.JSON(http.StatusOK, gin.H{"entity_ids": ids, "next_cursor": next})
}

func validCatalogLifecycle(s store.CatalogLifecycle) bool {
	if !validID(s.EntityID) || s.Version <= 0 || s.UpdatedAt.IsZero() || len(s.Aliases) > 10000 || len(s.CoverAssetIDs) > 10000 {
		return false
	}
	switch s.State {
	case "deleted":
	case "active":
		if !codePattern.MatchString(s.Kind) {
			return false
		}
	default:
		return false
	}
	seen := map[string]bool{s.EntityID: true}
	for _, id := range s.Aliases {
		if !validID(id) || seen[id] || s.AliasVersions[id] <= 0 {
			return false
		}
		seen[id] = true
	}
	if len(s.AliasVersions) != len(s.Aliases) {
		return false
	}
	for _, id := range s.CoverAssetIDs {
		if !validID(id) {
			return false
		}
	}
	return true
}

func (h *Handler) failStorageWrite(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrAssetReclaiming):
		fail(c, http.StatusConflict, "asset_reclaiming")
	case errors.Is(err, store.ErrInvalidBindingTarget):
		fail(c, http.StatusConflict, "binding_target_changed")
	case errors.Is(err, store.ErrNotFound):
		fail(c, http.StatusNotFound, "not_found")
	default:
		fail(c, http.StatusInternalServerError, "module_error")
	}
}
