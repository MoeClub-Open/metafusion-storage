package maintenance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/MoeclubM/metafusion-storage/internal/store"
)

func (f *cleanupFixture) completeUnbound(a store.Asset) {
	f.t.Helper()
	if err := f.db.MarkHashVerified(f.ctx, a.ID, 10); err != nil {
		f.t.Fatal(err)
	}
	if err := f.db.CompleteAsset(f.ctx, a.ID, 10); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.db.DB().ExecContext(f.ctx, "UPDATE storage.assets SET unbound_since=now()-interval '8 days' WHERE id=$1", a.ID); err != nil {
		f.t.Fatal(err)
	}
}

func TestCleanupReclaimsCompleteAfterRetention(t *testing.T) {
	f := newCleanupFixture(t)
	a := f.expiredPending("complete-retention", true)
	f.completeUnbound(a)
	rep, err := RunCleanup(f.ctx, f.db, f.objs, f.cfg)
	if err != nil || rep.UnboundRowsRemoved != 1 || rep.Reclaimed != 1 {
		t.Fatalf("超过保留期的失绑complete应回收: %+v err=%v", rep, err)
	}
	if _, err := f.db.Asset(f.ctx, a.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("资产行应回收: %v", err)
	}
	if exists, err := f.objs.Exists(f.ctx, a.ObjectKey); err != nil || exists {
		t.Fatalf("独占对象字节应回收: exists=%v err=%v", exists, err)
	}
}

func TestCleanupCompleteKeepsSharedBytes(t *testing.T) {
	f := newCleanupFixture(t)
	a := f.expiredPending("shared-complete", true)
	f.completeUnbound(a)
	other := f.expiredPending("shared-live-reference", false)
	if _, err := f.db.DB().ExecContext(f.ctx, "UPDATE storage.assets SET object_key=$2,upload_expires_at=$3 WHERE id=$1", other.ID, a.ObjectKey, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	rep, err := RunCleanup(f.ctx, f.db, f.objs, f.cfg)
	if err != nil || rep.UnboundRowsRemoved != 1 || rep.SharedRowsRemoved != 1 || rep.Reclaimed != 0 {
		t.Fatalf("共享资产只删失绑行: %+v err=%v", rep, err)
	}
	f.mustGet(other.ID)
	if exists, err := f.objs.Exists(f.ctx, a.ObjectKey); err != nil || !exists {
		t.Fatalf("共享对象字节必须保留: exists=%v err=%v", exists, err)
	}
}

func TestCleanupCompleteFailureRetainsGateAndRetries(t *testing.T) {
	f := newCleanupFixture(t)
	a := f.expiredPending("complete-failure", false)
	f.completeUnbound(a)
	objectPath := filepath.Join(f.root, filepath.FromSlash(a.ObjectKey))
	if err := os.MkdirAll(objectPath, 0o700); err != nil {
		t.Fatal(err)
	}
	junk := filepath.Join(objectPath, "junk")
	if err := os.WriteFile(junk, []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := RunCleanup(f.ctx, f.db, f.objs, f.cfg)
	if err != nil || rep.ObjectErrors != 1 || rep.UnboundRowsRemoved != 0 {
		t.Fatalf("删除失败必须保留行可重试: %+v err=%v", rep, err)
	}
	f.mustGet(a.ID)
	if err := f.db.Bind(f.ctx, store.Binding{ID: uuid.NewString(), AssetID: a.ID, TargetEntityID: uuid.NewString(), CreatedBy: a.UploaderID}); !errors.Is(err, store.ErrAssetReclaiming) {
		t.Fatalf("删除阶段不能重新绑定: %v", err)
	}
	if err := os.Remove(junk); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(objectPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(objectPath, []byte("repaired object"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err = RunCleanup(f.ctx, f.db, f.objs, f.cfg)
	if err != nil || rep.UnboundRowsRemoved != 1 || rep.Reclaimed != 1 {
		t.Fatalf("后续worker应重试成功: %+v err=%v", rep, err)
	}
}

// 对象已删除，但资产 DELETE 的提交失败：不可重新绑定缺失字节的行，下一 worker 收尾。
func TestCleanupCompleteCommitFailureKeepsDeletionGate(t *testing.T) {
	f := newCleanupFixture(t)
	a := f.expiredPending("complete-commit-failure", true)
	f.completeUnbound(a)
	ddl := `CREATE OR REPLACE FUNCTION storage.test_reclaim_commit_failure() RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN RAISE EXCEPTION 'test deferred delete failure'; END $$;
	CREATE CONSTRAINT TRIGGER test_reclaim_commit_failure AFTER DELETE ON storage.assets
	DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION storage.test_reclaim_commit_failure();`
	if _, err := f.db.DB().ExecContext(f.ctx, ddl); err != nil {
		t.Fatal(err)
	}
	clear := func() error {
		_, err := f.db.DB().ExecContext(context.Background(), `DROP TRIGGER IF EXISTS test_reclaim_commit_failure ON storage.assets;
		DROP FUNCTION IF EXISTS storage.test_reclaim_commit_failure();`)
		return err
	}
	t.Cleanup(func() { _ = clear() })
	rep, err := RunCleanup(f.ctx, f.db, f.objs, f.cfg)
	if err != nil || rep.ObjectErrors != 1 || rep.UnboundRowsRemoved != 0 {
		t.Fatalf("提交失败不得报告已回收行: %+v err=%v", rep, err)
	}
	f.mustGet(a.ID)
	if exists, err := f.objs.Exists(f.ctx, a.ObjectKey); err != nil || exists {
		t.Fatalf("模拟应在对象删除后才提交失败: exists=%v err=%v", exists, err)
	}
	if err := f.db.Bind(f.ctx, store.Binding{ID: uuid.NewString(), AssetID: a.ID, TargetEntityID: uuid.NewString(), CreatedBy: a.UploaderID}); !errors.Is(err, store.ErrAssetReclaiming) {
		t.Fatalf("缺失对象的删除阶段资产不能重新绑定: %v", err)
	}
	if err := f.db.SetBlocked(f.ctx, a.ID, "stale_reconcile_snapshot"); !errors.Is(err, store.ErrAssetReclaiming) {
		t.Fatalf("旧对账快照不能把删除阶段行禁发而阻碍重试: %v", err)
	}
	reconcile, err := RunReconcile(f.ctx, f.db, f.objs, f.cfg)
	if err != nil || reconcile.MarkedBlocked != 0 || reconcile.CheckedComplete != 0 {
		t.Fatalf("对账不能把删除阶段资产禁发而阻碍回收重试: %+v err=%v", reconcile, err)
	}
	if err := clear(); err != nil {
		t.Fatal(err)
	}
	rep, err = RunCleanup(f.ctx, f.db, f.objs, f.cfg)
	if err != nil || rep.UnboundRowsRemoved != 1 || rep.ObjectAlreadyMissing != 1 {
		t.Fatalf("提交故障恢复后应按缺失对象收尾: %+v err=%v", rep, err)
	}
}
