package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func catalogAssetInput(workspace, entity, asset string, actors ...string) CatalogAssetReferences {
	return CatalogAssetReferences{WorkspaceID: workspace, References: []CatalogAssetReference{{EntityID: entity, AssetID: asset, ActorIDs: actors}}}
}

func catalogAssetReferenceState(t *testing.T, s *Store, ctx context.Context, workspace, entity, asset string) (string, bool, *time.Time) {
	t.Helper()
	var actor string
	var prepared bool
	var expires *time.Time
	if err := s.db.QueryRowContext(ctx, `SELECT actor_id::text,prepared,expires_at FROM storage.catalog_asset_references WHERE workspace_id=$1 AND entity_id=$2 AND asset_id=$3`, workspace, entity, asset).Scan(&actor, &prepared, &expires); err != nil {
		t.Fatal(err)
	}
	return actor, prepared, expires
}

func TestCatalogAssetsOwnershipAndWorkspaceIsolationOnPostgres(t *testing.T) {
	s, ctx := reclaimTestStore(t)
	a := completeReclaimAsset(t, s, ctx)
	workspace, entity, other := uuid.NewString(), uuid.NewString(), uuid.NewString()
	in := catalogAssetInput(workspace, entity, a.ID, other)
	if err := s.RetainCatalogAssets(ctx, in); !errors.Is(err, ErrInvalidCatalogAsset) {
		t.Fatalf("another uploader's unbound asset accepted: %v", err)
	}
	in.References[0].ActorIDs = []string{a.UploaderID}
	if err := s.RetainCatalogAssets(ctx, in); err != nil {
		t.Fatal(err)
	}
	actor, prepared, expires := catalogAssetReferenceState(t, s, ctx, workspace, entity, a.ID)
	if actor != a.UploaderID || prepared || expires == nil || time.Until(*expires) < 47*time.Hour || time.Until(*expires) > 49*time.Hour {
		t.Fatalf("stage must preserve uploader and expire in 48h: actor=%s prepared=%t expires=%v", actor, prepared, expires)
	}
	// A collaborator can continue a valid workspace reference, without changing
	// its authenticated original asset author to the collaborator or merger.
	in.References[0].ActorIDs = []string{other}
	in.Retain = true
	if err := s.RetainCatalogAssets(ctx, in); err != nil {
		t.Fatal(err)
	}
	actor, prepared, expires = catalogAssetReferenceState(t, s, ctx, workspace, entity, a.ID)
	if actor != a.UploaderID || prepared || expires != nil {
		t.Fatalf("durable retention changed provenance or prepared publication: %s %t %v", actor, prepared, expires)
	}
	for _, changed := range []CatalogAssetReferences{
		catalogAssetInput(uuid.NewString(), entity, a.ID, other),
		catalogAssetInput(workspace, uuid.NewString(), a.ID, other),
	} {
		if err := s.RetainCatalogAssets(ctx, changed); !errors.Is(err, ErrInvalidCatalogAsset) {
			t.Fatalf("reference escaped workspace/target boundary: %v", err)
		}
	}
	in.Prepare, in.Retain = true, false
	if err := s.RetainCatalogAssets(ctx, in); err != nil {
		t.Fatal(err)
	}
	in.Prepare = false
	if err := s.RetainCatalogAssets(ctx, in); err != nil {
		t.Fatal(err)
	}
	actor, prepared, expires = catalogAssetReferenceState(t, s, ctx, workspace, entity, a.ID)
	if actor != a.UploaderID || !prepared || expires != nil {
		t.Fatalf("repeated staging downgraded prepared durable reference: %s %t %v", actor, prepared, expires)
	}
}

func TestCatalogAssetsExistingCoverOnlyProvesItsTargetOnPostgres(t *testing.T) {
	s, ctx := reclaimTestStore(t)
	a := completeReclaimAsset(t, s, ctx)
	entity, actor := uuid.NewString(), uuid.NewString()
	if err := s.Bind(ctx, Binding{ID: uuid.NewString(), AssetID: a.ID, TargetEntityID: entity, TargetKind: "work", BindingRole: "cover_image", CreatedBy: a.UploaderID}); err != nil {
		t.Fatal(err)
	}
	in := catalogAssetInput(uuid.NewString(), entity, a.ID, actor)
	if err := s.RetainCatalogAssets(ctx, in); err != nil {
		t.Fatalf("checkout of existing cover rejected: %v", err)
	}
	original, _, _ := catalogAssetReferenceState(t, s, ctx, in.WorkspaceID, entity, a.ID)
	if original != a.UploaderID {
		t.Fatalf("checkout replaced original binding author: %s", original)
	}
	in.References[0].EntityID = uuid.NewString()
	if err := s.RetainCatalogAssets(ctx, in); !errors.Is(err, ErrInvalidCatalogAsset) {
		t.Fatalf("public readability granted cross-target publication: %v", err)
	}
}

func TestCatalogAssetsModeratorAndExpiredProvisionalProofOnPostgres(t *testing.T) {
	s, ctx := reclaimTestStore(t)
	a := completeReclaimAsset(t, s, ctx)
	workspace, entity, moderator := uuid.NewString(), uuid.NewString(), uuid.NewString()
	in := catalogAssetInput(workspace, entity, a.ID)
	in.References[0].ModerateActorID = moderator
	if err := s.RetainCatalogAssets(ctx, in); err != nil {
		t.Fatalf("authenticated moderator rejected: %v", err)
	}
	actor, _, _ := catalogAssetReferenceState(t, s, ctx, workspace, entity, a.ID)
	if actor != moderator {
		t.Fatalf("moderator's asset admission misattributed: %s", actor)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE storage.catalog_asset_references SET expires_at=now()-interval '1 second' WHERE workspace_id=$1`, workspace); err != nil {
		t.Fatal(err)
	}
	in.References[0].ModerateActorID = ""
	in.References[0].ActorIDs = []string{uuid.NewString()}
	in.Prepare = true
	if err := s.RetainCatalogAssets(ctx, in); !errors.Is(err, ErrInvalidCatalogAsset) {
		t.Fatalf("expired provisional proof authorized another actor: %v", err)
	}
}

func TestCatalogAssetsPrepareCannotGrantNewPrivateAssetsOnPostgres(t *testing.T) {
	s, ctx := reclaimTestStore(t)
	a := completeReclaimAsset(t, s, ctx)
	// A commit's author list can contain the uploader of an unrelated private
	// asset. Only authenticated staging for this exact scope grants its use.
	for _, actors := range [][]string{{a.UploaderID}, {uuid.NewString(), a.UploaderID}} {
		in := catalogAssetInput(uuid.NewString(), uuid.NewString(), a.ID, actors...)
		in.Prepare = true
		if err := s.RetainCatalogAssets(ctx, in); !errors.Is(err, ErrInvalidCatalogAsset) {
			t.Fatalf("commit author list granted unstaged private asset: %v", err)
		}
	}
}

func TestCatalogAssetsDurableCatalogConfirmationRenewsExpiredOwnerProofOnPostgres(t *testing.T) {
	s, ctx := reclaimTestStore(t)
	a := completeReclaimAsset(t, s, ctx)
	in := catalogAssetInput(uuid.NewString(), uuid.NewString(), a.ID, a.UploaderID)
	if err := s.RetainCatalogAssets(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE storage.catalog_asset_references SET expires_at=now()-interval '1 second' WHERE workspace_id=$1`, in.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	// The trusted catalog worker confirms the exact scope from its durable
	// workspace/commit facts after an outage; no user supplies this confirmation.
	in.Retain = true
	if err := s.RetainCatalogAssets(ctx, in); err != nil {
		t.Fatalf("persisted catalog fact could not confirm expired owner proof: %v", err)
	}
	actor, prepared, expires := catalogAssetReferenceState(t, s, ctx, in.WorkspaceID, in.References[0].EntityID, a.ID)
	if actor != a.UploaderID || prepared || expires != nil {
		t.Fatalf("catalog confirmation changed original author or published cover: %s %t %v", actor, prepared, expires)
	}
	in.Retain, in.Prepare = false, true
	if err := s.RetainCatalogAssets(ctx, in); err != nil {
		t.Fatalf("confirmed durable owner proof did not authorize merge: %v", err)
	}
}

func TestCatalogAssetsRejectUnavailableAndRollbackBatchOnPostgres(t *testing.T) {
	for _, state := range []string{"missing", "pending", "unverified", "blocked", "reclaiming"} {
		t.Run(state, func(t *testing.T) {
			s, ctx := reclaimTestStore(t)
			good, unavailable := completeReclaimAsset(t, s, ctx), completeReclaimAsset(t, s, ctx)
			if good.ID > unavailable.ID {
				good, unavailable = unavailable, good
			}
			query := map[string]string{
				"missing":    "DELETE FROM storage.assets WHERE id=$1",
				"pending":    "UPDATE storage.assets SET status='pending' WHERE id=$1",
				"unverified": "UPDATE storage.assets SET hash_verified=false WHERE id=$1",
				"blocked":    "UPDATE storage.assets SET blocked=true WHERE id=$1",
				"reclaiming": "UPDATE storage.assets SET reclaim_deleting=true WHERE id=$1",
			}[state]
			if _, err := s.db.ExecContext(ctx, query, unavailable.ID); err != nil {
				t.Fatal(err)
			}
			workspace, entity := uuid.NewString(), uuid.NewString()
			in := catalogAssetInput(workspace, entity, good.ID, good.UploaderID)
			in.References = append(in.References, CatalogAssetReference{EntityID: entity, AssetID: unavailable.ID, ActorIDs: []string{unavailable.UploaderID}})
			want := ErrInvalidCatalogAsset
			if state == "reclaiming" {
				want = ErrAssetReclaiming
			}
			if err := s.RetainCatalogAssets(ctx, in); !errors.Is(err, want) {
				t.Fatalf("unavailable %s asset accepted: %v", state, err)
			}
			var count int
			if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM storage.catalog_asset_references WHERE workspace_id=$1`, workspace).Scan(&count); err != nil || count != 0 {
				t.Fatalf("failed batch left partial references: count=%d err=%v", count, err)
			}
		})
	}
}

func TestCatalogAssetsCannotReactivateInterruptedReclamationOnPostgres(t *testing.T) {
	s, ctx := reclaimTestStore(t)
	a := completeReclaimAsset(t, s, ctx)
	agedUnbound(t, s, ctx, a.ID)
	interrupted := errors.New("object deletion interrupted")
	if deleted, err := s.ReclaimUnbound(ctx, a.ID, time.Now(), func(context.Context, Asset, bool) error { return interrupted }); deleted || !errors.Is(err, interrupted) {
		t.Fatalf("reclamation did not reach persistent deletion stage: %t %v", deleted, err)
	}
	in := catalogAssetInput(uuid.NewString(), uuid.NewString(), a.ID, a.UploaderID)
	in.Prepare = true
	if err := s.RetainCatalogAssets(ctx, in); !errors.Is(err, ErrAssetReclaiming) {
		t.Fatalf("private retention reactivated asset after irreversible deletion stage: %v", err)
	}
}

func TestCatalogAssetsProtectGarbageCollectionAtFinalCheckOnPostgres(t *testing.T) {
	s, ctx := reclaimTestStore(t)
	a := completeReclaimAsset(t, s, ctx)
	agedUnbound(t, s, ctx, a.ID)
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	if candidates, err := s.UnboundCandidates(ctx, cutoff, 100); err != nil || len(candidates) != 1 {
		t.Fatalf("unreferenced asset should initially be reclaimable: %+v %v", candidates, err)
	}
	in := catalogAssetInput(uuid.NewString(), uuid.NewString(), a.ID, a.UploaderID)
	if err := s.RetainCatalogAssets(ctx, in); err != nil {
		t.Fatal(err)
	}
	if candidates, err := s.UnboundCandidates(ctx, cutoff, 100); err != nil || len(candidates) != 0 {
		t.Fatalf("active private reference entered GC inventory: %+v %v", candidates, err)
	}
	called := false
	remove := func(context.Context, Asset, bool) error { called = true; return nil }
	if deleted, err := s.ReclaimUnbound(ctx, a.ID, cutoff, remove); err != nil || deleted || called {
		t.Fatalf("reference added after candidate scan failed final GC guard: deleted=%t called=%t err=%v", deleted, called, err)
	}
	// Expiry must ask catalog, not assume that a save failed. The catalog
	// service may have been stopped after its transaction committed.
	if _, err := s.db.ExecContext(ctx, `UPDATE storage.catalog_asset_references SET expires_at=now()-interval '1 second' WHERE workspace_id=$1`, in.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if candidates, err := s.UnboundCandidates(ctx, cutoff, 100); err != nil || len(candidates) != 0 {
		t.Fatalf("expired reference allowed deletion without catalog confirmation: %+v %v", candidates, err)
	}
	if deleted, err := s.ReclaimUnbound(ctx, a.ID, cutoff, remove); err != nil || deleted || called {
		t.Fatalf("final GC guard ignored unconfirmed expired reference: %t %t %v", deleted, called, err)
	}
	if err := s.SweepExpiredCatalogAssetReferences(ctx, func(_ context.Context, refs []CatalogAssetReferenceCheck) ([]CatalogAssetReferenceCheck, error) {
		return append([]CatalogAssetReferenceCheck(nil), refs...), nil
	}); err != nil {
		t.Fatal(err)
	}
	if candidates, err := s.UnboundCandidates(ctx, cutoff, 100); err != nil || len(candidates) != 1 {
		t.Fatalf("catalog-confirmed absent reference leaked bytes: %+v %v", candidates, err)
	}
	if deleted, err := s.ReclaimUnbound(ctx, a.ID, cutoff, remove); err != nil || !deleted || !called {
		t.Fatalf("confirmed absent reference blocked reclamation: deleted=%t called=%t err=%v", deleted, called, err)
	}
}

func TestCatalogAssetsSweepConfirmsCurrentAndCommittedRetentionOnPostgres(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "workspace", true: "commit"}[committed], func(t *testing.T) {
			s, ctx := reclaimTestStore(t)
			a := completeReclaimAsset(t, s, ctx)
			in := catalogAssetInput(uuid.NewString(), uuid.NewString(), a.ID, a.UploaderID)
			if err := s.RetainCatalogAssets(ctx, in); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx, `UPDATE storage.catalog_asset_references SET expires_at=now()-interval '1 day' WHERE workspace_id=$1`, in.WorkspaceID); err != nil {
				t.Fatal(err)
			}
			calls := 0
			if err := s.SweepExpiredCatalogAssetReferences(ctx, func(_ context.Context, refs []CatalogAssetReferenceCheck) ([]CatalogAssetReferenceCheck, error) {
				calls++
				if len(refs) != 1 || refs[0].WorkspaceID != in.WorkspaceID || refs[0].EntityID != in.References[0].EntityID || refs[0].AssetID != a.ID {
					t.Fatalf("confirmation changed private scope: %+v", refs)
				}
				confirmed := append([]CatalogAssetReferenceCheck(nil), refs...)
				confirmed[0].Referenced, confirmed[0].Committed = true, committed
				return confirmed, nil
			}); err != nil {
				t.Fatal(err)
			}
			actor, prepared, expires := catalogAssetReferenceState(t, s, ctx, in.WorkspaceID, in.References[0].EntityID, a.ID)
			if calls != 1 || actor != a.UploaderID || prepared || (committed && expires != nil) || (!committed && (expires == nil || time.Until(*expires) < 47*time.Hour)) {
				t.Fatalf("confirmation lost author/privacy/retention: calls=%d actor=%s prepared=%t expires=%v", calls, actor, prepared, expires)
			}
			if bindings, err := s.BindingsForAsset(ctx, a.ID); err != nil || len(bindings) != 0 {
				t.Fatalf("retention confirmation published private cover: %+v %v", bindings, err)
			}
		})
	}
}

func TestCatalogAssetsSweepUnknownAndMalformedConfirmationKeepsReferencesOnPostgres(t *testing.T) {
	for _, failure := range []string{"missing_checker", "upstream_error", "missing_result", "wrong_scope", "impossible_state"} {
		t.Run(failure, func(t *testing.T) {
			s, ctx := reclaimTestStore(t)
			a := completeReclaimAsset(t, s, ctx)
			in := catalogAssetInput(uuid.NewString(), uuid.NewString(), a.ID, a.UploaderID)
			if err := s.RetainCatalogAssets(ctx, in); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx, `UPDATE storage.catalog_asset_references SET expires_at=now()-interval '1 day' WHERE workspace_id=$1`, in.WorkspaceID); err != nil {
				t.Fatal(err)
			}
			_, _, before := catalogAssetReferenceState(t, s, ctx, in.WorkspaceID, in.References[0].EntityID, a.ID)
			var checker CatalogAssetReferenceChecker
			if failure != "missing_checker" {
				checker = func(_ context.Context, refs []CatalogAssetReferenceCheck) ([]CatalogAssetReferenceCheck, error) {
					if failure == "upstream_error" {
						return nil, errors.New("catalog unavailable")
					}
					confirmed := append([]CatalogAssetReferenceCheck(nil), refs...)
					switch failure {
					case "missing_result":
						return nil, nil
					case "wrong_scope":
						confirmed[0].WorkspaceID = uuid.NewString()
					case "impossible_state":
						confirmed[0].Referenced, confirmed[0].Committed = false, true
					}
					return confirmed, nil
				}
			}
			if err := s.SweepExpiredCatalogAssetReferences(ctx, checker); err == nil {
				t.Fatalf("unknown confirmation %s accepted as permission to delete", failure)
			}
			actor, prepared, after := catalogAssetReferenceState(t, s, ctx, in.WorkspaceID, in.References[0].EntityID, a.ID)
			if actor != a.UploaderID || prepared || before == nil || after == nil || !after.Equal(*before) {
				t.Fatalf("failed confirmation mutated retention: actor=%s prepared=%t before=%v after=%v", actor, prepared, before, after)
			}
		})
	}
}

func TestCatalogAssetsSweepStaleNegativeCannotDeleteConcurrentSaveOnPostgres(t *testing.T) {
	s, ctx := reclaimTestStore(t)
	a := completeReclaimAsset(t, s, ctx)
	in := catalogAssetInput(uuid.NewString(), uuid.NewString(), a.ID, a.UploaderID)
	if err := s.RetainCatalogAssets(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE storage.catalog_asset_references SET expires_at=now()-interval '1 day' WHERE workspace_id=$1`, in.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if err := s.SweepExpiredCatalogAssetReferences(ctx, func(_ context.Context, refs []CatalogAssetReferenceCheck) ([]CatalogAssetReferenceCheck, error) {
		// The catalog's negative snapshot predates this concurrent authenticated
		// save. Its CAS must not revoke the refreshed reservation afterwards.
		if err := s.RetainCatalogAssets(ctx, in); err != nil {
			return nil, err
		}
		return append([]CatalogAssetReferenceCheck(nil), refs...), nil
	}); err != nil {
		t.Fatal(err)
	}
	actor, _, expires := catalogAssetReferenceState(t, s, ctx, in.WorkspaceID, in.References[0].EntityID, a.ID)
	if actor != a.UploaderID || expires == nil || time.Until(*expires) < 47*time.Hour {
		t.Fatalf("stale negative confirmation removed concurrent save: actor=%s expires=%v", actor, expires)
	}
}

func TestCatalogAssetsDurableRetentionProtectsBytesWithoutPublishingOnPostgres(t *testing.T) {
	s, ctx := reclaimTestStore(t)
	a := completeReclaimAsset(t, s, ctx)
	agedUnbound(t, s, ctx, a.ID)
	in := catalogAssetInput(uuid.NewString(), uuid.NewString(), a.ID, a.UploaderID)
	if err := s.RetainCatalogAssets(ctx, in); err != nil {
		t.Fatal(err)
	}
	in.Retain = true
	if err := s.RetainCatalogAssets(ctx, in); err != nil {
		t.Fatal(err)
	}
	_, prepared, expires := catalogAssetReferenceState(t, s, ctx, in.WorkspaceID, in.References[0].EntityID, a.ID)
	if prepared || expires != nil {
		t.Fatalf("durable private retention must not authorize publication: %t %v", prepared, expires)
	}
	called := false
	if deleted, err := s.ReclaimUnbound(ctx, a.ID, time.Now(), func(context.Context, Asset, bool) error { called = true; return nil }); err != nil || deleted || called {
		t.Fatalf("durable history reference lost its bytes: %t %t %v", deleted, called, err)
	}
	if bindings, err := s.BindingsForAsset(ctx, a.ID); err != nil || len(bindings) != 0 {
		t.Fatalf("private history retention created public binding: %+v %v", bindings, err)
	}
}

func TestCatalogAssetsLifecycleRequiresPreparedTargetAndPreservesAuthorOnPostgres(t *testing.T) {
	s, ctx := reclaimTestStore(t)
	a := completeReclaimAsset(t, s, ctx)
	entity, workspace := uuid.NewString(), uuid.NewString()
	state := CatalogLifecycle{EntityID: entity, Kind: "work", State: "active", Version: 1, UpdatedAt: time.Now(), CoverAssetIDs: []string{a.ID}}
	if err := s.ApplyCatalogLifecycle(ctx, state); !errors.Is(err, ErrInvalidCatalogAsset) {
		t.Fatalf("unguarded lifecycle published private asset: %v", err)
	}
	in := catalogAssetInput(workspace, entity, a.ID, a.UploaderID)
	if err := s.RetainCatalogAssets(ctx, in); err != nil {
		t.Fatal(err)
	}
	in.Retain = true
	if err := s.RetainCatalogAssets(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyCatalogLifecycle(ctx, state); !errors.Is(err, ErrInvalidCatalogAsset) {
		t.Fatalf("private retained draft became public without prepare: %v", err)
	}
	in.Prepare = true
	if err := s.RetainCatalogAssets(ctx, in); err != nil {
		t.Fatal(err)
	}
	wrong := state
	wrong.EntityID = uuid.NewString()
	if err := s.ApplyCatalogLifecycle(ctx, wrong); !errors.Is(err, ErrInvalidCatalogAsset) {
		t.Fatalf("prepared reference authorized unrelated target: %v", err)
	}
	if err := s.ApplyCatalogLifecycle(ctx, state); err != nil {
		t.Fatal(err)
	}
	first, err := s.BindingByReference(ctx, a.ID, entity, "cover_image")
	if err != nil || first.CreatedBy != a.UploaderID || first.TargetKind != "work" {
		t.Fatalf("merged cover lost original author: %+v %v", first, err)
	}
	if err := s.ApplyCatalogLifecycle(ctx, state); err != nil {
		t.Fatalf("replayed lifecycle failed: %v", err)
	}
	second, err := s.BindingByReference(ctx, a.ID, entity, "cover_image")
	if err != nil || second.ID != first.ID || second.CreatedBy != first.CreatedBy || !second.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("replayed lifecycle replaced binding identity/provenance: before=%+v after=%+v err=%v", first, second, err)
	}
	bindings, err := s.BindingsForAsset(ctx, a.ID)
	if err != nil || len(bindings) != 1 {
		t.Fatalf("repeated lifecycle created duplicate bindings: %+v %v", bindings, err)
	}
}

func TestCatalogAssetsLifecycleRechecksReadinessAndRollsBackOnPostgres(t *testing.T) {
	for _, stateChange := range []string{"blocked", "unverified", "pending", "reclaiming", "unprepared"} {
		t.Run(stateChange, func(t *testing.T) {
			s, ctx := reclaimTestStore(t)
			good, bad := completeReclaimAsset(t, s, ctx), completeReclaimAsset(t, s, ctx)
			entity, workspace := uuid.NewString(), uuid.NewString()
			in := catalogAssetInput(workspace, entity, good.ID, good.UploaderID)
			in.References = append(in.References, CatalogAssetReference{EntityID: entity, AssetID: bad.ID, ActorIDs: []string{bad.UploaderID}})
			if err := s.RetainCatalogAssets(ctx, in); err != nil {
				t.Fatal(err)
			}
			in.Prepare = true
			if err := s.RetainCatalogAssets(ctx, in); err != nil {
				t.Fatal(err)
			}
			query := map[string]string{
				"blocked":    "UPDATE storage.assets SET blocked=true WHERE id=$1",
				"unverified": "UPDATE storage.assets SET hash_verified=false WHERE id=$1",
				"pending":    "UPDATE storage.assets SET status='pending' WHERE id=$1",
				"reclaiming": "UPDATE storage.assets SET reclaim_deleting=true WHERE id=$1",
				"unprepared": "UPDATE storage.catalog_asset_references SET prepared=false WHERE asset_id=$1",
			}[stateChange]
			if _, err := s.db.ExecContext(ctx, query, bad.ID); err != nil {
				t.Fatal(err)
			}
			state := CatalogLifecycle{EntityID: entity, Kind: "work", State: "active", Version: 1, UpdatedAt: time.Now(), CoverAssetIDs: []string{good.ID, bad.ID}}
			if err := s.ApplyCatalogLifecycle(ctx, state); !errors.Is(err, ErrInvalidCatalogAsset) {
				t.Fatalf("prepared proof bypassed current readiness %s: %v", stateChange, err)
			}
			if bindings, err := s.BindingsForEntity(ctx, entity); err != nil || len(bindings) != 0 {
				t.Fatalf("failed lifecycle published a partial cover set: %+v %v", bindings, err)
			}
			var stateCount int
			if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM storage.catalog_lifecycle_state WHERE entity_id=$1`, entity).Scan(&stateCount); err != nil || stateCount != 0 {
				t.Fatalf("failed lifecycle advanced reconciliation watermark: %d %v", stateCount, err)
			}
		})
	}
}
