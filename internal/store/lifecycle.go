package store

import (
	"context"
	"time"
)

// Usage 是配额判定的输入：complete 按服务端落定的真实字节计，pending 按客户端
// 声明大小计——占位即占预算，否则声明 1GB 的 pending 不占额度、落定时才超限。
type Usage struct {
	CompleteBytes int64
	PendingBytes  int64
	PendingCount  int64
}

// UserUsage 统计某上传者的容量与并发占用（pending 计数即并发上传数）。
func (s *Store) UserUsage(ctx context.Context, userID string) (Usage, error) {
	var u Usage
	err := s.db.QueryRowContext(ctx, "SELECT COALESCE(SUM(size_bytes) FILTER (WHERE status='complete'),0),"+
		"COALESCE(SUM(declared_size) FILTER (WHERE status='pending'),0),"+
		"COALESCE(SUM(CASE WHEN status='pending' THEN 1 ELSE 0 END),0)"+
		" FROM storage.assets WHERE uploader_id=$1", userID).Scan(&u.CompleteBytes, &u.PendingBytes, &u.PendingCount)
	return u, err
}

// SiteUsage 统计全站容量与并发占用，口径与 UserUsage 一致。
func (s *Store) SiteUsage(ctx context.Context) (Usage, error) {
	var u Usage
	err := s.db.QueryRowContext(ctx, "SELECT COALESCE(SUM(size_bytes) FILTER (WHERE status='complete'),0),"+
		"COALESCE(SUM(declared_size) FILTER (WHERE status='pending'),0),"+
		"COALESCE(SUM(CASE WHEN status='pending' THEN 1 ELSE 0 END),0)"+
		" FROM storage.assets").Scan(&u.CompleteBytes, &u.PendingBytes, &u.PendingCount)
	return u, err
}

// SetUploadExpiry 设置上传租约到期时间；续传复用会话时刷新它，避免"传得慢就被回收"。
// 尚未进入字节删除阶段时续租清掉认领标记；已经进入删除阶段时返回 ErrAssetReclaiming。
func (s *Store) SetUploadExpiry(ctx context.Context, id string, expiresAt time.Time) error {
	res, err := s.db.ExecContext(ctx, "UPDATE storage.assets SET upload_expires_at=$2, reclaim_token='', reclaim_claimed_at=NULL WHERE id=$1 AND NOT reclaim_deleting", id, expiresAt)
	return s.checkReclaimMutation(ctx, id, res, err)
}

// UnboundCandidates 列出已完成、未禁发且最后解绑超过保留期的资产。
// 从未绑定的 complete 在完成时开始计时，存量在 000006 迁移时开始计时。
func (s *Store) UnboundCandidates(ctx context.Context, before time.Time, limit int) ([]Asset, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+assetCols+" FROM storage.assets a"+
		" WHERE a.status='complete' AND NOT a.blocked AND a.unbound_since <= $1"+
		" AND NOT EXISTS (SELECT 1 FROM storage.bindings b WHERE b.asset_id=a.id)"+
		" AND NOT EXISTS (SELECT 1 FROM storage.catalog_asset_references r WHERE r.asset_id=a.id)"+
		" ORDER BY a.unbound_since, a.id LIMIT $2", before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Asset{}
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ReclaimCandidates 列出可回收的过期 pending：无绑定、非禁发、租约已过期、无有效认领持有。
// 只做候选列举：逐行处置前必须经 TryClaimReclaim 原子认领，认领失败即他人已接管或行已转活跃。
func (s *Store) ReclaimCandidates(ctx context.Context, now time.Time, staleBefore time.Time, limit int) ([]Asset, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+assetCols+" FROM storage.assets a"+
		" WHERE a.status='pending' AND NOT a.blocked"+
		" AND NOT EXISTS (SELECT 1 FROM storage.bindings b WHERE b.asset_id=a.id)"+
		" AND a.upload_expires_at <= $1"+
		" AND (a.reclaim_token='' OR a.reclaim_claimed_at IS NULL OR a.reclaim_claimed_at <= $2)"+
		" ORDER BY a.created_at ASC LIMIT $3", now, staleBefore, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Asset{}
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// HasBindings 报告资产是否仍被任何实体引用：删除前的最后一道门，
// 候选查询与实际删除之间可能有人刚绑上，不能只信候选时的快照。
func (s *Store) HasBindings(ctx context.Context, assetID string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM storage.bindings WHERE asset_id=$1)", assetID).Scan(&exists)
	return exists, err
}

// DeleteAsset 删除资产行（绑定经外键级联一起走）。对象字节的删除由调用方在确认
// 无共享引用后再做，两步的顺序与条件见 maintenance，Store 层不跨界删对象。
func (s *Store) DeleteAsset(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM storage.assets WHERE id=$1", id)
	return err
}

// ObjectKeyRefCount 报告还有多少资产行引用同一个对象键（含调用方自己）。
// 内容寻址去重意味着"无绑定"不等于"无引用"：秒传复用的 complete 行可能正指着
// 同一个键，清掉其中一行就删字节会把另一行的内容一起删掉。
func (s *Store) ObjectKeyRefCount(ctx context.Context, objectKey string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM storage.assets WHERE object_key=$1", objectKey).Scan(&n)
	return n, err
}

// SetBlocked 禁发：只翻 blocked 位，不动 status——解禁后原状态仍在，
// 不需要"解禁恢复 complete/pending"的分支。分发门禁见 handler.readable。
func (s *Store) SetBlocked(ctx context.Context, id, reason string) error {
	res, err := s.db.ExecContext(ctx, "UPDATE storage.assets SET blocked=true, blocked_reason=$2, blocked_at=now() WHERE id=$1 AND NOT reclaim_deleting", id, reason)
	return s.checkReclaimMutation(ctx, id, res, err)
}

// ClearBlocked 解禁：复位标记与留痕字段，status 原样保留。
func (s *Store) ClearBlocked(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE storage.assets SET blocked=false, blocked_reason='', blocked_at=NULL WHERE id=$1", id)
	return err
}

// ListBlocked 按禁发时间倒序列出被禁发资产，供运营分页盘点。
func (s *Store) ListBlocked(ctx context.Context, limit, offset int) ([]Asset, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+assetCols+" FROM storage.assets WHERE blocked ORDER BY blocked_at DESC, id DESC LIMIT $1 OFFSET $2", limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Asset{}
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// StatusCounts 报告运营口径：未完成数与被禁发数（/stats 的配额与禁发盘点输入）。
func (s *Store) StatusCounts(ctx context.Context) (pending, blocked int64, err error) {
	err = s.db.QueryRowContext(ctx, "SELECT COALESCE(SUM(CASE WHEN status='pending' THEN 1 ELSE 0 END),0), COALESCE(SUM(CASE WHEN blocked THEN 1 ELSE 0 END),0) FROM storage.assets").Scan(&pending, &blocked)
	return pending, blocked, err
}

// CompleteKey 是对账页的最小行：只带定位键，不带元数据。
type CompleteKey struct {
	ID        string
	ObjectKey string
	SHA256    string
}

// ListCompleteKeys 分页列出已完成资产的定位键（id 排序，前翻页），供对账任务
// 逐批核对"库里说有，对象存储里真有"。limit 上限 1000，对账是后台批处理不是在线接口。
func (s *Store) ListCompleteKeys(ctx context.Context, limit, offset int) ([]CompleteKey, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id, object_key, sha256 FROM storage.assets"+
		" WHERE status='complete' AND NOT reclaim_deleting ORDER BY id ASC LIMIT $1 OFFSET $2", limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CompleteKey{}
	for rows.Next() {
		var k CompleteKey
		if err := rows.Scan(&k.ID, &k.ObjectKey, &k.SHA256); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// IsKeyReferenced 报告对象键是否仍被任何资产行引用：孤儿判定的"库侧"一半，
// 另一半是对象存储侧的实际键列表（见 maintenance，双向核对缺一半就不能动手）。
func (s *Store) IsKeyReferenced(ctx context.Context, objectKey string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM storage.assets WHERE object_key=$1)", objectKey).Scan(&exists)
	return exists, err
}

// UpsertOrphanSeen 记录一次孤儿发现：键已在台账则只刷新备注（first_seen_at 保持首次发现，
// 保留期从它起算，反复发现不能顺延——否则保留期永远不到）。
func (s *Store) UpsertOrphanSeen(ctx context.Context, objectKey, note string) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO storage.orphaned_objects(object_key, note) VALUES($1,$2)"+
		" ON CONFLICT(object_key) DO UPDATE SET note=EXCLUDED.note", objectKey, note)
	return err
}

// MarkOrphanQuarantined 记录孤儿已隔离（搬进 quarantine/ 前缀）：只写台账，搬运本身由调用方做，
// 两步都成功才算隔离完成——先写台账再搬运，搬运失败时台账仍显示未隔离可重试。
func (s *Store) MarkOrphanQuarantined(ctx context.Context, objectKey string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE storage.orphaned_objects SET quarantined_at=now() WHERE object_key=$1", objectKey)
	return err
}

// ListOrphansDue 列出"首次发现已超过保留期、且尚未隔离"的孤儿（按发现时间排序）。
func (s *Store) ListOrphansDue(ctx context.Context, before time.Time, limit int) ([]string, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, "SELECT object_key FROM storage.orphaned_objects"+
		" WHERE quarantined_at IS NULL AND first_seen_at <= $1 ORDER BY first_seen_at ASC LIMIT $2", before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ClearOrphan 删除孤儿台账行：运维确认字节已处置（或键重新被引用）后的收尾，worker 不主动调它。
func (s *Store) ClearOrphan(ctx context.Context, objectKey string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM storage.orphaned_objects WHERE object_key=$1", objectKey)
	return err
}
