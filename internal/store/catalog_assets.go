package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"
)

var ErrInvalidCatalogAsset = errors.New("invalid_catalog_asset")

// ActorIDs are authenticated authors fixed by catalog, never client claims.
// ModerateActorID is set only for the current caller with storage moderation.
type CatalogAssetReference struct {
	SourceWorkspaceID string   `json:"source_workspace_id,omitempty"`
	EntityID          string   `json:"entity_id"`
	AssetID           string   `json:"asset_id"`
	ActorIDs          []string `json:"actor_ids"`
	ModerateActorID   string   `json:"moderate_actor_id,omitempty"`
}

type CatalogAssetReferences struct {
	WorkspaceID string                  `json:"workspace_id"`
	Prepare     bool                    `json:"prepare"`
	Retain      bool                    `json:"retain"`
	References  []CatalogAssetReference `json:"references"`
}

// RetainCatalogAssets validates ownership and readiness under the same asset
// row lock as GC. A reference is private and never authorizes a public read.
// References are additive: immutable commits can still refer to prior covers.
func (s *Store) RetainCatalogAssets(ctx context.Context, in CatalogAssetReferences) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	refs := append([]CatalogAssetReference(nil), in.References...)
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].AssetID == refs[j].AssetID {
			return refs[i].EntityID < refs[j].EntityID
		}
		return refs[i].AssetID < refs[j].AssetID
	})
	for _, ref := range refs {
		var owner, status string
		var verified, blocked, deleting bool
		err = tx.QueryRowContext(ctx, `SELECT uploader_id::text,status,hash_verified,blocked,reclaim_deleting FROM storage.assets WHERE id=$1 FOR UPDATE`, ref.AssetID).Scan(&owner, &status, &verified, &blocked, &deleting)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInvalidCatalogAsset
		}
		if err != nil {
			return err
		}
		if deleting {
			return ErrAssetReclaiming
		}
		if status != "complete" || !verified || blocked {
			return ErrInvalidCatalogAsset
		}
		actor := ""
		if !in.Prepare && !in.Retain && len(ref.ActorIDs) == 1 && ref.ActorIDs[0] == owner {
			actor = owner
		}
		if !in.Prepare && !in.Retain && actor == "" && ref.ModerateActorID != "" {
			actor = ref.ModerateActorID
		}
		// Existing references prove an earlier authenticated save. Reusing one
		// inside the same workspace needs no impersonation of its first author.
		var retainedActor string
		err = tx.QueryRowContext(ctx, `SELECT actor_id::text FROM storage.catalog_asset_references WHERE workspace_id=$1 AND entity_id=$2 AND asset_id=$3 AND ($4 OR expires_at IS NULL OR expires_at>now())`, in.WorkspaceID, ref.EntityID, ref.AssetID, in.Retain).Scan(&retainedActor)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			actor = retainedActor
		}
		if actor == "" {
			// Only the authenticated catalog service can name a source copy;
			// catalog first verifies access to its frozen commit. The transfer
			// retains the same entity/asset identity and the original saver.
			if ref.SourceWorkspaceID != "" {
				err = tx.QueryRowContext(ctx, `SELECT actor_id::text FROM storage.catalog_asset_references WHERE workspace_id=$1 AND entity_id=$2 AND asset_id=$3`, ref.SourceWorkspaceID, ref.EntityID, ref.AssetID).Scan(&actor)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
			}
		}
		if actor == "" {
			// A checkout can retain an already-published cover on its own target;
			// this does not grant permission to reuse it on another target.
			err = tx.QueryRowContext(ctx, `SELECT created_by::text FROM storage.bindings WHERE asset_id=$1 AND target_entity_id=$2 AND binding_role='cover_image'`, ref.AssetID, ref.EntityID).Scan(&actor)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrInvalidCatalogAsset
			}
			if err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO storage.catalog_asset_references(workspace_id,entity_id,asset_id,actor_id,prepared,expires_at) VALUES($1,$2,$3,$4,$5,CASE WHEN $6 THEN NULL ELSE now()+interval '48 hours' END)
		 ON CONFLICT(workspace_id,entity_id,asset_id) DO UPDATE SET prepared=storage.catalog_asset_references.prepared OR EXCLUDED.prepared,
		 actor_id=CASE WHEN storage.catalog_asset_references.expires_at<=now() THEN EXCLUDED.actor_id ELSE storage.catalog_asset_references.actor_id END,
		 expires_at=CASE WHEN storage.catalog_asset_references.expires_at IS NULL OR $6 THEN NULL ELSE EXCLUDED.expires_at END`, in.WorkspaceID, ref.EntityID, ref.AssetID, actor, in.Prepare, in.Prepare || in.Retain); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type CatalogAssetReferenceCheck struct {
	WorkspaceID string `json:"workspace_id"`
	EntityID    string `json:"entity_id"`
	AssetID     string `json:"asset_id"`
	Referenced  bool   `json:"referenced"`
	Committed   bool   `json:"committed"`
}
type CatalogAssetReferenceChecker func(context.Context, []CatalogAssetReferenceCheck) ([]CatalogAssetReferenceCheck, error)

// Expiry requests confirmation, never permission to delete bytes. A stopped
// catalog worker cannot turn durable workspace history into abandoned uploads.
// Unknown upstream state keeps every reference protected. A compare-and-swap
// prevents a stale negative confirmation from removing a concurrent new save.
func (s *Store) SweepExpiredCatalogAssetReferences(ctx context.Context, check CatalogAssetReferenceChecker) error {
	rows, err := s.db.QueryContext(ctx, `SELECT workspace_id::text,entity_id::text,asset_id::text,expires_at FROM storage.catalog_asset_references WHERE expires_at<=now() ORDER BY expires_at,workspace_id,entity_id,asset_id LIMIT 1000`)
	if err != nil {
		return err
	}
	refs := []CatalogAssetReferenceCheck{}
	expiries := []time.Time{}
	for rows.Next() {
		var r CatalogAssetReferenceCheck
		var expires time.Time
		if err = rows.Scan(&r.WorkspaceID, &r.EntityID, &r.AssetID, &expires); err != nil {
			rows.Close()
			return err
		}
		refs = append(refs, r)
		expiries = append(expiries, expires)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(refs) == 0 {
		return err
	}
	if check == nil {
		return errors.New("catalog_reference_confirmation_required")
	}
	confirmed, err := check(ctx, refs)
	if err != nil {
		return err
	}
	if len(confirmed) != len(refs) {
		return errors.New("invalid_catalog_reference_confirmation")
	}
	for i, r := range confirmed {
		original := refs[i]
		if r.WorkspaceID != original.WorkspaceID || r.EntityID != original.EntityID || r.AssetID != original.AssetID || r.Committed && !r.Referenced {
			return errors.New("invalid_catalog_reference_confirmation")
		}
	}
	for i, r := range confirmed {
		if !r.Referenced {
			_, err = s.db.ExecContext(ctx, `DELETE FROM storage.catalog_asset_references WHERE workspace_id=$1 AND entity_id=$2 AND asset_id=$3 AND expires_at=$4`, r.WorkspaceID, r.EntityID, r.AssetID, expiries[i])
		} else {
			_, err = s.db.ExecContext(ctx, `UPDATE storage.catalog_asset_references SET expires_at=CASE WHEN $5 THEN NULL ELSE now()+interval '48 hours' END WHERE workspace_id=$1 AND entity_id=$2 AND asset_id=$3 AND expires_at=$4`, r.WorkspaceID, r.EntityID, r.AssetID, expiries[i], r.Committed)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
