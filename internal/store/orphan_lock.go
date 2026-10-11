package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// QuarantineOrphan shares the object-key lock with asset creation and reclaim.
// The final reference check and bounded object move are one protected operation:
// a just-created asset can never lose its bytes to a stale orphan scan.
func (s *Store) QuarantineOrphan(ctx context.Context, key string, move func(context.Context) error) (bool, error) {
	if move == nil {
		return false, errors.New("orphan quarantine callback required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := lockObjectKeyTx(ctx, tx, key); err != nil {
		return false, err
	}
	var quarantined *time.Time
	err = tx.QueryRowContext(ctx, "SELECT quarantined_at FROM storage.orphaned_objects WHERE object_key=$1 FOR UPDATE", key).Scan(&quarantined)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if quarantined != nil {
		return false, nil
	}
	var referenced bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM storage.assets WHERE object_key=$1)", key).Scan(&referenced); err != nil {
		return false, err
	}
	if referenced {
		if _, err := tx.ExecContext(ctx, "DELETE FROM storage.orphaned_objects WHERE object_key=$1", key); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	objectCtx, cancel := context.WithTimeout(ctx, ReclaimObjectTimeout)
	err = move(objectCtx)
	cancel()
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE storage.orphaned_objects SET quarantined_at=now() WHERE object_key=$1", key); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
