package store

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCatalogLifecycleMigrationsReplayOnPostgres(t *testing.T) {
	s, ctx := reclaimTestStore(t)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for attempt := 0; attempt < 2; attempt++ {
		for _, name := range []string{
			"000006_unbound_retention.up.sql", "000007_catalog_lifecycle.up.sql", "000008_binding_confirmation.up.sql", "000009_catalog_review_state.up.sql",
		} {
			body, err := migrationFS.ReadFile("migrations/" + name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(ctx, string(body)); err != nil {
				t.Fatalf("migration %s replay %d: %v", name, attempt, err)
			}
		}
	}
	var indexExists bool
	if err := tx.QueryRowContext(ctx, "SELECT to_regclass('storage.catalog_lifecycle_canonical') IS NOT NULL").Scan(&indexExists); err != nil || !indexExists {
		t.Fatalf("canonical lifecycle index absent after migrations: exists=%t err=%v", indexExists, err)
	}
}

func TestCatalogLifecyclePreservesMergeDraftAndSharedAssetsOnPostgres(t *testing.T) {
	s, ctx := reclaimTestStore(t)
	source, canonical, other := uuid.NewString(), uuid.NewString(), uuid.NewString()
	uploader := uuid.NewString()
	assets := make([]Asset, 5)
	for i := range assets {
		assets[i] = reclaimAsset(time.Now().Add(time.Hour), uploader)
		if err := s.CreateAsset(ctx, assets[i]); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkHashVerified(ctx, assets[i].ID, 10); err != nil {
			t.Fatal(err)
		}
		if err := s.CompleteAsset(ctx, assets[i].ID, 10); err != nil {
			t.Fatal(err)
		}
	}
	bind := func(asset int, entity, role string) string {
		t.Helper()
		b := Binding{ID: uuid.NewString(), AssetID: assets[asset].ID, TargetEntityID: entity,
			TargetKind: "work", BindingRole: role, CreatedBy: uploader}
		if err := s.Bind(ctx, b); err != nil {
			t.Fatal(err)
		}
		return b.ID
	}
	bind(0, source, "cover_image")
	bind(1, canonical, "cover_image")
	bind(2, canonical, "cover_image") // an obsolete picture
	bind(0, canonical, "cover_image") // duplicate on merge
	bind(3, source, "master_archive")
	bind(3, other, "master_archive") // shared asset must survive canonical deletion
	fresh := bind(4, canonical, "cover_image")
	cutoff := time.Now().UTC()
	// A repeated Bind retains identity/creator but refreshes its confirmation,
	// so a previous catalog snapshot cannot withdraw a new picture upload.
	if err := s.Bind(ctx, Binding{ID: uuid.NewString(), AssetID: assets[4].ID, TargetEntityID: canonical,
		TargetKind: "work", BindingRole: "cover_image", CreatedBy: uploader}); err != nil {
		t.Fatal(err)
	}
	confirmed, err := s.BindingByReference(ctx, assets[4].ID, canonical, "cover_image")
	if err != nil || confirmed.ID != fresh {
		t.Fatalf("rebind replaced the original binding identity: %+v %v", confirmed, err)
	}
	state := CatalogLifecycle{EntityID: canonical, Kind: "work", Status: "active", Version: 3,
		UpdatedAt: cutoff, Aliases: []string{source}, AliasVersions: map[string]int64{source: 2},
		CoverAssetIDs: []string{assets[0].ID, assets[1].ID}}
	for i := 0; i < 2; i++ {
		if err := s.ApplyCatalogLifecycle(ctx, state); err != nil {
			t.Fatal(err)
		}
	}
	files, err := s.BindingsForEntity(ctx, canonical)
	if err != nil || len(files) != 4 {
		t.Fatalf("merge/replay should retain source cover, target cover, archive and fresh pre-Save binding: %d %v", len(files), err)
	}
	for _, file := range files {
		if file.AssetID == assets[2].ID {
			t.Fatal("obsolete picture binding was retained")
		}
	}
	if files, err := s.BindingsForEntity(ctx, source); err != nil || len(files) != 0 {
		t.Fatalf("old source bindings were not migrated: %d %v", len(files), err)
	}
	page, err := s.CatalogBindingTargets(ctx, "", 1)
	if err != nil || len(page) != 1 {
		t.Fatalf("binding inventory first keyset page: %v %v", page, err)
	}
	rest, err := s.CatalogBindingTargets(ctx, page[0], 100)
	if err != nil || len(rest) != 1 || rest[0] <= page[0] {
		t.Fatalf("binding inventory must deduplicate roles and advance its cursor: %v %v", rest, err)
	}
	oldTarget := state
	oldTarget.Aliases, oldTarget.AliasVersions = nil, nil
	oldTarget.CoverAssetIDs = []string{assets[1].ID}
	if err := s.ApplyCatalogLifecycle(ctx, oldTarget); err != nil {
		t.Fatal(err)
	}
	if files, _ := s.BindingsForEntity(ctx, canonical); len(files) != 4 {
		t.Fatal("equal-version pre-merge target snapshot revoked migrated covers")
	}
	if err := s.Bind(ctx, Binding{ID: uuid.NewString(), AssetID: assets[0].ID, TargetEntityID: source,
		TargetKind: "work", BindingRole: "cover_image", CreatedBy: uploader}); !errors.Is(err, ErrInvalidBindingTarget) {
		t.Fatalf("in-flight stale binding should conflict after merge: %v", err)
	}
	state.Status, state.Version = "active", 4
	if err := s.ApplyCatalogLifecycle(ctx, state); err != nil {
		t.Fatal(err)
	}
	if files, err := s.BindingsForEntity(ctx, canonical); err != nil || len(files) != 4 {
		t.Fatalf("unpublish withdrew editable draft bindings: %d %v", len(files), err)
	}
	// A delayed pre-merge source snapshot cannot undo migration or remove the
	// source cover now attached to canonical identity.
	stale := CatalogLifecycle{EntityID: source, Status: "deleted", Version: 1, UpdatedAt: cutoff}
	if err := s.ApplyCatalogLifecycle(ctx, stale); err != nil {
		t.Fatal(err)
	}
	if files, _ := s.BindingsForEntity(ctx, canonical); len(files) != 4 {
		t.Fatal("stale source event withdrew canonical files")
	}
	state.Status, state.Version = "deleted", 5
	if err := s.ApplyCatalogLifecycle(ctx, state); err != nil {
		t.Fatal(err)
	}
	if files, _ := s.BindingsForEntity(ctx, canonical); len(files) != 0 {
		t.Fatal("canonical deletion left entity bindings")
	}
	if files, _ := s.BindingsForEntity(ctx, other); len(files) != 1 {
		t.Fatal("canonical deletion withdrew an unrelated shared reference")
	}
	var since *time.Time
	if err := s.db.QueryRowContext(ctx, "SELECT unbound_since FROM storage.assets WHERE id=$1", assets[0].ID).Scan(&since); err != nil || since == nil {
		t.Fatalf("last reference removal did not start retention: %v", err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT unbound_since FROM storage.assets WHERE id=$1", assets[3].ID).Scan(&since); err != nil || since != nil {
		t.Fatalf("shared referenced asset must not enter retention: %v", err)
	}
	if err := s.Bind(ctx, Binding{ID: uuid.NewString(), AssetID: assets[0].ID, TargetEntityID: canonical,
		TargetKind: "work", BindingRole: "cover_image", CreatedBy: uploader}); !errors.Is(err, ErrInvalidBindingTarget) {
		t.Fatalf("in-flight stale binding should conflict after deletion: %v", err)
	}
}
