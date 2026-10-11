package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func completeReclaimAsset(t *testing.T, s *Store, ctx context.Context) Asset {
	t.Helper()
	a := reclaimAsset(time.Now().Add(-time.Hour), "11111111-1111-1111-1111-111111111111")
	if err := s.CreateAsset(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkHashVerified(ctx, a.ID, 10); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteAsset(ctx, a.ID, 10); err != nil {
		t.Fatal(err)
	}
	a.Status = "complete"
	return a
}

func unboundAt(t *testing.T, s *Store, ctx context.Context, id string) *time.Time {
	t.Helper()
	var since *time.Time
	if err := s.db.QueryRowContext(ctx, "SELECT unbound_since FROM storage.assets WHERE id=$1", id).Scan(&since); err != nil {
		t.Fatal(err)
	}
	return since
}

func agedUnbound(t *testing.T, s *Store, ctx context.Context, id string) {
	t.Helper()
	if _, err := s.db.ExecContext(ctx, "UPDATE storage.assets SET unbound_since=now()-interval '8 days' WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
}

func reclaimBinding(a Asset) Binding {
	return Binding{ID: uuid.NewString(), AssetID: a.ID, TargetEntityID: uuid.NewString(), TargetKind: "track", BindingRole: "track_audio", CreatedBy: a.UploaderID}
}

func TestUnboundClockTracksCompletionAndLastBinding(t *testing.T) {
	s, ctx := reclaimTestStore(t)
	a := completeReclaimAsset(t, s, ctx)
	if since := unboundAt(t, s, ctx, a.ID); since == nil || time.Since(*since) > time.Minute {
		t.Fatalf("完成但未绑定应开始保留期: %v", since)
	}
	agedUnbound(t, s, ctx, a.ID)
	b1, b2 := reclaimBinding(a), reclaimBinding(a)
	for _, b := range []Binding{b1, b2} {
		if err := s.Bind(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	if since := unboundAt(t, s, ctx, a.ID); since != nil {
		t.Fatalf("重新绑定应取消计时: %v", since)
	}
	if err := s.Unbind(ctx, b1.ID); err != nil {
		t.Fatal(err)
	}
	if since := unboundAt(t, s, ctx, a.ID); since != nil {
		t.Fatalf("尚有一个绑定不得开始计时: %v", since)
	}
	// bulk DELETE 同样必须落定最后解绑的时刻，不能沿用最初完成时间。
	if _, err := s.db.ExecContext(ctx, "DELETE FROM storage.bindings WHERE asset_id=$1", a.ID); err != nil {
		t.Fatal(err)
	}
	if since := unboundAt(t, s, ctx, a.ID); since == nil || time.Since(*since) > time.Minute {
		t.Fatalf("最后解绑应重新起算: %v", since)
	}
	if rows, err := s.UnboundCandidates(ctx, time.Now().Add(-7*24*time.Hour), 100); err != nil || len(rows) != 0 {
		t.Fatalf("重新计时后保留期未到: %+v err=%v", rows, err)
	}
}

func TestUnboundClockTracksBindingAssetMove(t *testing.T) {
	s, ctx := reclaimTestStore(t)
	a, other := completeReclaimAsset(t, s, ctx), completeReclaimAsset(t, s, ctx)
	b := reclaimBinding(a)
	if err := s.Bind(ctx, b); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE storage.bindings SET asset_id=$2 WHERE id=$1", b.ID, other.ID); err != nil {
		t.Fatal(err)
	}
	if unboundAt(t, s, ctx, a.ID) == nil || unboundAt(t, s, ctx, other.ID) != nil {
		t.Fatal("绑定迁移应让原资产开始计时、新资产取消计时")
	}
}

func TestUnboundMigrationBackfillStartsConservatively(t *testing.T) {
	s, ctx := reclaimTestStore(t)
	a := completeReclaimAsset(t, s, ctx)
	if _, err := s.db.ExecContext(ctx, "UPDATE storage.assets SET unbound_since=NULL, completed_at=now()-interval '1 year' WHERE id=$1", a.ID); err != nil {
		t.Fatal(err)
	}
	body, err := migrationFS.ReadFile("migrations/000006_unbound_retention.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, string(body)); err != nil {
		t.Fatal(err)
	}
	if since := unboundAt(t, s, ctx, a.ID); since == nil || time.Since(*since) > time.Minute {
		t.Fatalf("历史完成时间不能导致迁移后立即回收: %v", since)
	}
}

func TestReclaimUnboundRechecksBindingsAndSharedObject(t *testing.T) {
	s, ctx := reclaimTestStore(t)
	a := completeReclaimAsset(t, s, ctx)
	agedUnbound(t, s, ctx, a.ID)
	if rows, err := s.UnboundCandidates(ctx, time.Now().Add(-7*24*time.Hour), 100); err != nil || len(rows) != 1 {
		t.Fatalf("保留期已到应列为候选: %+v err=%v", rows, err)
	}
	b := reclaimBinding(a)
	if err := s.Bind(ctx, b); err != nil {
		t.Fatal(err)
	}
	called := false
	deleted, err := s.ReclaimUnbound(ctx, a.ID, time.Now(), func(context.Context, Asset, bool) error { called = true; return nil })
	if err != nil || deleted || called {
		t.Fatalf("候选后重绑必须保留字节和行: deleted=%v called=%v err=%v", deleted, called, err)
	}
	if err = s.Unbind(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	agedUnbound(t, s, ctx, a.ID)
	other := reclaimAsset(time.Now().Add(time.Hour), a.UploaderID)
	other.ObjectKey = a.ObjectKey
	if err = s.CreateAsset(ctx, other); err != nil {
		t.Fatal(err)
	}
	deleted, err = s.ReclaimUnbound(ctx, a.ID, time.Now(), func(_ context.Context, current Asset, shared bool) error {
		if !shared || current.ID != a.ID {
			t.Fatalf("有另一资产行共享字节时只能删本行: shared=%v asset=%+v", shared, current)
		}
		return nil
	})
	if err != nil || !deleted {
		t.Fatalf("失绑共享资产应仅删行: deleted=%v err=%v", deleted, err)
	}
	if _, err = s.Asset(ctx, other.ID); err != nil {
		t.Fatalf("共享方必须保留: %v", err)
	}
}

func TestReclaimDeletionGateSurvivesFailureAndConcurrentBind(t *testing.T) {
	for _, status := range []string{"complete", "pending"} {
		t.Run(status, func(t *testing.T) {
			s, ctx := reclaimTestStore(t)
			a := completeReclaimAsset(t, s, ctx)
			agedUnbound(t, s, ctx, a.ID)
			token := "pending-cleanup"
			if status == "pending" {
				if err := s.MarkHashMismatch(ctx, a.ID, "retry"); err != nil {
					t.Fatal(err)
				}
				if _, ok, err := s.TryClaimReclaim(ctx, a.ID, time.Now(), token, time.Now().Add(-ReclaimClaimTimeout)); err != nil || !ok {
					t.Fatalf("claim: ok=%v err=%v", ok, err)
				}
			}
			reclaim := func(fn func(context.Context, Asset, bool) error) (bool, error) {
				if status == "pending" {
					return s.ReclaimClaimedAsset(ctx, a.ID, token, time.Now(), fn)
				}
				return s.ReclaimUnbound(ctx, a.ID, time.Now(), fn)
			}
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			failure := errors.New("object removed then transaction interrupted")
			go func() {
				_, err := reclaim(func(objectCtx context.Context, _ Asset, _ bool) error {
					deadline, ok := objectCtx.Deadline()
					if !ok || time.Until(deadline) > ReclaimObjectTimeout {
						return errors.New("object timeout missing")
					}
					close(entered)
					<-release
					return failure
				})
				done <- err
			}()
			select {
			case <-entered:
			case err := <-done:
				t.Fatalf("未进入对象阶段: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("回收未进入对象阶段")
			}
			bindingDone := make(chan error, 1)
			go func() { bindingDone <- s.Bind(ctx, reclaimBinding(a)) }()
			select {
			case err := <-bindingDone:
				close(release)
				t.Fatalf("对象操作期间绑定必须等待资产行锁: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			close(release)
			if err := <-done; !errors.Is(err, failure) {
				t.Fatalf("模拟对象删除后的失败未保留: %v", err)
			}
			if err := <-bindingDone; !errors.Is(err, ErrAssetReclaiming) {
				t.Fatalf("失败后不能绑定可能缺字节的资产: %v", err)
			}
			for name, op := range map[string]func() error{
				"renew":    func() error { return s.SetUploadExpiry(ctx, a.ID, time.Now().Add(time.Hour)) },
				"session":  func() error { return s.SetUploadSession(ctx, a.ID, "new") },
				"verify":   func() error { return s.MarkHashVerified(ctx, a.ID, 10) },
				"complete": func() error { return s.CompleteAsset(ctx, a.ID, 10) },
			} {
				if err := op(); !errors.Is(err, ErrAssetReclaiming) {
					t.Errorf("%s 必须拒绝删除阶段资产: %v", name, err)
				}
			}
			deleted, err := reclaim(func(context.Context, Asset, bool) error { return nil })
			if err != nil || !deleted {
				t.Fatalf("失败后的 worker 应能重试收尾: deleted=%v err=%v", deleted, err)
			}
			if _, err := s.Asset(ctx, a.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("重试完成后行应消失: %v", err)
			}
		})
	}
}
