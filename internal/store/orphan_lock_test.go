package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestOrphanQuarantineSerializesWithAssetCreationOnPostgres(t *testing.T) {
	s, base := reclaimTestStore(t)
	ctx, cancel := context.WithTimeout(base, 10*time.Second)
	defer cancel()
	a := reclaimAsset(time.Now().Add(time.Hour), uuid.NewString())
	if err := s.UpsertOrphanSeen(ctx, a.ObjectKey, "test"); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	moved := make(chan error, 1)
	go func() {
		_, err := s.QuarantineOrphan(ctx, a.ObjectKey, func(objectCtx context.Context) error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-objectCtx.Done():
				return objectCtx.Err()
			}
		})
		moved <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	started, created := make(chan struct{}), make(chan error, 1)
	go func() {
		close(started)
		created <- s.CreateAsset(ctx, a)
	}()
	<-started
	select {
	case err := <-created:
		t.Fatalf("asset registered while its bytes could still be moved: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	if err := <-moved; err != nil {
		t.Fatal(err)
	}
	if err := <-created; err != nil {
		t.Fatal(err)
	}
	// When registration wins first, a subsequent stale orphan receipt is
	// cleared without touching the referenced object at all.
	if err := s.UpsertOrphanSeen(ctx, a.ObjectKey, "stale second scan"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE storage.orphaned_objects SET quarantined_at=NULL WHERE object_key=$1", a.ObjectKey); err != nil {
		t.Fatal(err)
	}
	called := false
	ok, err := s.QuarantineOrphan(ctx, a.ObjectKey, func(context.Context) error { called = true; return nil })
	if err != nil || ok || called {
		t.Fatalf("a referenced key was quarantined: moved=%t callback=%t err=%v", ok, called, err)
	}
}
