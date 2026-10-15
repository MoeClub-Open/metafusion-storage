package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/lib/pq"
)

// This lock serializes reference reconciliation with new bindings. It is
// distinct from migration/quota/object locks and is never held for object I/O.
const catalogLifecycleLockID = int64(740222)

var ErrInvalidBindingTarget = errors.New("binding_target_changed")

// CheckAssetActive keeps deletion-stage assets out of reads and instant-upload
// responses, including when a previous object operation/commit was interrupted.
func (s *Store) CheckAssetActive(ctx context.Context, id string) error {
	var deleting bool
	err := s.db.QueryRowContext(ctx, "SELECT reclaim_deleting FROM storage.assets WHERE id=$1", id).Scan(&deleting)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if deleting {
		return ErrAssetReclaiming
	}
	return nil
}

type CatalogLifecycle struct {
	EntityID      string           `json:"entity_id"`
	Kind          string           `json:"kind"`
	State         string           `json:"state"`
	Version       int64            `json:"version"`
	UpdatedAt     time.Time        `json:"updated_at"`
	Aliases       []string         `json:"aliases"`
	AliasVersions map[string]int64 `json:"alias_versions"`
	CoverAssetIDs []string         `json:"cover_asset_ids"`
}

// BindingByReference returns the persisted identity after an idempotent Bind,
// whose proposed ID may differ from the already-existing binding's ID.
func (s *Store) BindingByReference(ctx context.Context, assetID, entityID, role string) (Binding, error) {
	var b Binding
	err := s.db.QueryRowContext(ctx, `SELECT id,asset_id,target_entity_id,target_kind,binding_role,created_by,created_at
		FROM storage.bindings WHERE asset_id=$1 AND target_entity_id=$2 AND binding_role=$3`, assetID, entityID, role).
		Scan(&b.ID, &b.AssetID, &b.TargetEntityID, &b.TargetKind, &b.BindingRole, &b.CreatedBy, &b.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Binding{}, ErrNotFound
	}
	return b, err
}

func (s *Store) CatalogBindingTargets(ctx context.Context, after string, limit int) ([]string, error) {
	var cursor any
	if after != "" {
		cursor = after
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT target_entity_id::text FROM storage.bindings
		WHERE ($1::uuid IS NULL OR target_entity_id>$1::uuid) ORDER BY target_entity_id::text LIMIT $2`, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func checkCatalogBindingTarget(ctx context.Context, tx *sql.Tx, b *Binding) error {
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", catalogLifecycleLockID); err != nil {
		return err
	}
	var stateCode string
	err := tx.QueryRowContext(ctx, "SELECT state FROM storage.catalog_lifecycle_state WHERE entity_id=$1", b.TargetEntityID).Scan(&stateCode)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if stateCode == "merged" || stateCode == "deleted" {
		// The HTTP write path resolves canonical identity first. A transition
		// between that read and this write is a retryable conflict, not a stale
		// binding recreated after the lifecycle consumer has already cleaned it.
		return ErrInvalidBindingTarget
	}
	return nil
}

// ApplyCatalogLifecycle migrates all roles across a merge, withdraws all roles
// for deletion, and reconciles only cover_image on live/draft entities. Asset
// unbound timestamps are maintained by storage-owned triggers in migration 6.
func (s *Store) ApplyCatalogLifecycle(ctx context.Context, state CatalogLifecycle) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", catalogLifecycleLockID); err != nil {
		return err
	}
	ids := append([]string{state.EntityID}, state.Aliases...)
	versions := map[string]int64{state.EntityID: state.Version}
	for _, alias := range state.Aliases {
		versions[alias] = state.AliasVersions[alias]
	}
	rows, err := tx.QueryContext(ctx, "SELECT entity_id::text,version FROM storage.catalog_lifecycle_state WHERE entity_id=ANY($1::uuid[])", pq.Array(ids))
	if err != nil {
		return err
	}
	stale := false
	for rows.Next() {
		var id string
		var version int64
		if err := rows.Scan(&id, &version); err != nil {
			rows.Close()
			return err
		}
		if version > versions[id] {
			stale = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if stale {
		return nil
	}
	// Adding a merge alias does not necessarily increment the target version.
	// An equal-version snapshot with an older, shorter alias set must not revoke
	// migrated source covers. Alias membership is monotonic for one canonical
	// identity; canonical migration includes the whole old set in its new set.
	known := map[string]bool{}
	for _, id := range state.Aliases {
		known[id] = true
	}
	rows, err = tx.QueryContext(ctx, `SELECT entity_id::text FROM storage.catalog_lifecycle_state
		WHERE canonical_id=$1 AND entity_id<>$1`, state.EntityID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var alias string
		if err := rows.Scan(&alias); err != nil {
			rows.Close()
			return err
		}
		if !known[alias] {
			stale = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if stale {
		return nil
	}
	// Lock assets in deterministic order, sharing Bind/GC's row boundary. A
	// last-reference deletion cannot race completed-asset reclamation.
	rows, err = tx.QueryContext(ctx, `SELECT a.id FROM storage.assets a
		WHERE EXISTS(SELECT 1 FROM storage.bindings b WHERE b.asset_id=a.id AND b.target_entity_id=ANY($1::uuid[]))
		ORDER BY a.id FOR UPDATE OF a`, pq.Array(ids))
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if state.State == "deleted" {
		_, err = tx.ExecContext(ctx, "DELETE FROM storage.bindings WHERE target_entity_id=ANY($1::uuid[])", pq.Array(ids))
	} else {
		// Keep an existing canonical binding on collision; otherwise preserve
		// one source binding (including its original ID, creator and timestamp).
		_, err = tx.ExecContext(ctx, `DELETE FROM storage.bindings b WHERE b.target_entity_id=ANY($1::uuid[])
			AND EXISTS(SELECT 1 FROM storage.bindings keep WHERE keep.asset_id=b.asset_id AND keep.binding_role=b.binding_role
			AND (keep.target_entity_id=$2::uuid OR (keep.target_entity_id=ANY($1::uuid[]) AND keep.id::text<b.id::text)))`,
			pq.Array(state.Aliases), state.EntityID)
		if err == nil {
			_, err = tx.ExecContext(ctx, "UPDATE storage.bindings SET target_entity_id=$2,target_kind=$3 WHERE target_entity_id=ANY($1::uuid[])",
				pq.Array(state.Aliases), state.EntityID, state.Kind)
		}
		if err == nil {
			// Upload binds before catalog Save. A fresh binding created after
			// the snapshot must survive until the matching Save can reference it.
			_, err = tx.ExecContext(ctx, `DELETE FROM storage.bindings WHERE target_entity_id=$1 AND binding_role='cover_image'
				AND NOT(asset_id=ANY($2::uuid[])) AND confirmed_at <= $3`, state.EntityID, pq.Array(state.CoverAssetIDs), state.UpdatedAt)
		}
	}
	if err != nil {
		return err
	}
	for _, id := range ids {
		stateCode, canonical := state.State, state.EntityID
		if id != state.EntityID && stateCode != "deleted" {
			stateCode = "merged"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO storage.catalog_lifecycle_state(entity_id,version,state,canonical_id,entity_updated_at)
			VALUES($1,$2,$3,$4,$5) ON CONFLICT(entity_id) DO UPDATE SET version=EXCLUDED.version,state=EXCLUDED.state,
			canonical_id=EXCLUDED.canonical_id,entity_updated_at=EXCLUDED.entity_updated_at,applied_at=now()`,
			id, versions[id], stateCode, canonical, state.UpdatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
