package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	// 空导入只为注册 postgres 驱动：漏掉它时 sql.Open("postgres", …) 会直接报 unknown driver，
	// 而编译与静态检查都发现不了。driver_test.go 专门守住这条。
	_ "github.com/lib/pq"
)

// ErrNotFound 表示目标记录不存在（调用方据此回 404，不区分权限与不存在）。
var ErrNotFound = errors.New("not_found")

// ErrAssetUnverified 表示尝试把一个 hash_verified=false 的资产置为完成。
// 内容寻址的前提是"键上的内容确实等于该 sha256"：完成态只能由服务端回读校验
// 通过（VerifyHash）或流式上传边收边算（PutStream）产生，没有第三条路。
var ErrAssetUnverified = errors.New("asset_unverified")

// ErrAssetReclaiming 表示资产已进入持久登记的字节删除阶段，不能再次绑定或激活。
var ErrAssetReclaiming = errors.New("asset_reclaiming")

// 表结构在 migrations/ 下（000001_init.up.sql），由 Init → Migrate 在启动时应用：
// 不再内联 DDL，避免结构与迁移文件各存一份、改一处漏一处。

// Asset 是一份物理文件的内容寻址记录：身份是 sha256，不含任何目录语义。
type Asset struct {
	ID                string `json:"id"`
	SHA256            string `json:"sha256"`
	SizeBytes         int64  `json:"size_bytes"`
	DeclaredSize      int64  `json:"declared_size"`
	MimeType          string `json:"mime_type"`
	FileName          string `json:"file_name"`
	ObjectKey         string `json:"object_key"`
	Status            string `json:"status"`
	MultipartUploadID string `json:"multipart_upload_id,omitempty"`
	HashVerified      bool   `json:"hash_verified"`
	UploaderID        string `json:"uploader_id"`
	// UploadExpiresAt 是上传租约到期时间：过期未完成的 pending 由后台清理任务回收。
	// 存量 NULL 按 created_at + 默认 TTL 兜底（见 lifecycle.go 的回收查询），迁移不回填策略值。
	UploadExpiresAt *time.Time `json:"upload_expires_at,omitempty"`
	// ReclaimToken/ReclaimClaimedAt 是清理认领的持有标记：空表示无人认领。
	// 只允许认领持有者删行；续租/完成/绑定会清掉它（主动方胜出），超时未落定可被接管。
	ReclaimToken     string     `json:"-"`
	ReclaimClaimedAt *time.Time `json:"-"`
	// Blocked 是独立于 status 的禁发标记：status 只回答"内容是否验过"，
	// Blocked 只回答"是否允许分发"（目录公开 ≠ 文件可分发）。解禁后原状态仍在。
	Blocked       bool       `json:"blocked"`
	BlockedReason string     `json:"blocked_reason,omitempty"`
	BlockedAt     *time.Time `json:"blocked_at,omitempty"`
	// FailReason 只在服务端回读校验失败时写入，供运维与上传者定位；不参与任何判定。
	FailReason  string     `json:"fail_reason,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// Binding 是"文件 → 目录实体"的挂载：用途用 binding_role 表达（track_audio、disc_image…），
// 位置/区间属于目录侧的 locator，不在这里重复。
type Binding struct {
	ID             string    `json:"id"`
	AssetID        string    `json:"asset_id"`
	TargetEntityID string    `json:"target_entity_id"`
	TargetKind     string    `json:"target_kind"`
	BindingRole    string    `json:"binding_role"`
	CreatedBy      string    `json:"created_by"`
	CreatedAt      time.Time `json:"created_at"`
}

// FileBinding 是列表视图：绑定 + 它指向的文件元数据。
type FileBinding struct {
	Binding
	Asset Asset `json:"asset"`
}

type Store struct{ db *sql.DB }

func Open(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(32)
	db.SetMaxIdleConns(16)
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// DB 暴露底层连接，供健康检查使用。
func (s *Store) DB() *sql.DB { return s.db }

// Init 应用尚未记账的迁移（DDL 见 migrations/，账本 storage.schema_migrations）。
// 保持"启动即可用"的既有行为：迁移文件本身幂等，老实例重复启动不会改结构。
func (s *Store) Init(ctx context.Context) error {
	_, err := s.Migrate(ctx)
	return err
}

const assetCols = "id,sha256,size_bytes,declared_size,mime_type,file_name,object_key,status,multipart_upload_id,hash_verified,fail_reason,uploader_id,upload_expires_at,blocked,blocked_reason,blocked_at,created_at,completed_at,reclaim_token,reclaim_claimed_at"

func scanAsset(row interface{ Scan(...any) error }) (Asset, error) {
	var a Asset
	err := row.Scan(&a.ID, &a.SHA256, &a.SizeBytes, &a.DeclaredSize, &a.MimeType, &a.FileName, &a.ObjectKey, &a.Status, &a.MultipartUploadID, &a.HashVerified, &a.FailReason, &a.UploaderID, &a.UploadExpiresAt, &a.Blocked, &a.BlockedReason, &a.BlockedAt, &a.CreatedAt, &a.CompletedAt, &a.ReclaimToken, &a.ReclaimClaimedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Asset{}, ErrNotFound
	}
	return a, err
}

func (s *Store) Asset(ctx context.Context, id string) (Asset, error) {
	return scanAsset(s.db.QueryRowContext(ctx, "SELECT "+assetCols+" FROM storage.assets WHERE id=$1", id))
}

func (s *Store) AssetByHash(ctx context.Context, sha256 string) (Asset, error) {
	return scanAsset(s.db.QueryRowContext(ctx, "SELECT "+assetCols+" FROM storage.assets WHERE sha256=$1", sha256))
}

// VerifiedAssetByHash 只返回**服务端已验过内容**的资产，供秒传/查重使用。
//
// 查重采信的是"这个 sha256 的内容已经在库里"，而唯一能证明这一点的字段是 hash_verified：
// 预签名直传路径 initiate 时只能采信客户端声明的摘要，若在这里不过滤，
// 一份等长的错内容就能把这个 sha256 占住，之后所有秒传都会命中错内容。
// 未验证的命中一律当作不存在，调用方继续走正常上传。
func (s *Store) VerifiedAssetByHash(ctx context.Context, sha256 string) (Asset, error) {
	return scanAsset(s.db.QueryRowContext(ctx, "SELECT "+assetCols+" FROM storage.assets WHERE sha256=$1 AND hash_verified AND NOT reclaim_deleting", sha256))
}

// CreateAsset 登记一份上传占位：调用方（initiate）同时给出 upload_expires_at 租约，
// NULL 表示存量/未知租约，回收查询按 created_at + 默认 TTL 兜底。
func (s *Store) CreateAsset(ctx context.Context, a Asset) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lockObjectKeyTx(ctx, tx, a.ObjectKey); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO storage.assets(id,sha256,size_bytes,declared_size,mime_type,file_name,object_key,status,multipart_upload_id,uploader_id,upload_expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)",
		a.ID, a.SHA256, a.SizeBytes, a.DeclaredSize, a.MimeType, a.FileName, a.ObjectKey, a.Status, a.MultipartUploadID, a.UploaderID, a.UploadExpiresAt)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// SetUploadSession 记录分片上传会话，便于中断后续传复用同一个 uploadID。
func (s *Store) SetUploadSession(ctx context.Context, id, uploadID string) error {
	res, err := s.db.ExecContext(ctx, "UPDATE storage.assets SET multipart_upload_id=$2 WHERE id=$1 AND NOT reclaim_deleting", id, uploadID)
	return s.checkReclaimMutation(ctx, id, res, err)
}

// CompleteAsset 落定文件大小与完成状态；size 一律覆盖——两条上传路径都在落定前
// 算过真实字节数（直传按回读校验、流式按写入计数），不存在"为 0 就不覆盖"的分支。
//
// 只认 hash_verified=true 的行：完成态就是"可被秒传复用"的公开态，未验证的资产
// 不能被任何调用方（包括将来新增的上传路径）推到这个状态上。
func (s *Store) CompleteAsset(ctx context.Context, id string, size int64) error {
	res, err := s.db.ExecContext(ctx, "UPDATE storage.assets SET status='complete', size_bytes=$2, completed_at=now(), reclaim_token='', reclaim_claimed_at=NULL WHERE id=$1 AND hash_verified AND NOT reclaim_deleting", id, size)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if err := s.checkReclaimMutation(ctx, id, res, nil); err != nil {
			return err
		}
		return ErrAssetUnverified
	}
	return nil
}

// MarkHashVerified 记录服务端实际读回字节重算的摘要与声明一致，并落定大小。
func (s *Store) MarkHashVerified(ctx context.Context, id string, size int64) error {
	return s.markHash(ctx, id, true, size, "")
}

// MarkHashMismatch 记录服务端回读结果的摘要与声明不符：资产保持 pending（因此不参与
// 秒传），大小不覆盖——declared_size 是客户端声明值，size_bytes 在未验证前只作参考，
// 覆盖它会让统计与排查都失去基准。失败原因写入对象本身，授权实例可直接查到。
func (s *Store) MarkHashMismatch(ctx context.Context, id, reason string) error {
	return s.markHash(ctx, id, false, 0, reason)
}

// markHash 用一次事务合并两条改动：hash_verified=false 必然伴随 status='pending'。
// 拆成两条独立语句时，中间态（已置为 pending/complete 但摘要字段未落）会在并发
// 查重里被看见，而查重的判据正是这两个字段。
func (s *Store) markHash(ctx context.Context, id string, verified bool, size int64, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "UPDATE storage.assets SET hash_verified=$2, reclaim_token='', reclaim_claimed_at=NULL WHERE id=$1 AND NOT reclaim_deleting", id, verified)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var deleting bool
		if err = tx.QueryRowContext(ctx, "SELECT reclaim_deleting FROM storage.assets WHERE id=$1", id).Scan(&deleting); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if deleting {
			return ErrAssetReclaiming
		}
		return nil
	}
	if verified {
		_, err = tx.ExecContext(ctx, "UPDATE storage.assets SET size_bytes=$2 WHERE id=$1", id, size)
	} else {
		_, err = tx.ExecContext(ctx, "UPDATE storage.assets SET status='pending', fail_reason=$2 WHERE id=$1", id, reason)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Bind(ctx context.Context, b Binding) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = checkCatalogBindingTarget(ctx, tx, &b); err != nil {
		return err
	}
	var deleting bool
	if err = tx.QueryRowContext(ctx, "SELECT reclaim_deleting FROM storage.assets WHERE id=$1 FOR UPDATE", b.AssetID).Scan(&deleting); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if deleting {
		return ErrAssetReclaiming
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO storage.bindings(id,asset_id,target_entity_id,target_kind,binding_role,created_by)
		VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(asset_id,target_entity_id,binding_role)
		DO UPDATE SET confirmed_at=now()`,
		b.ID, b.AssetID, b.TargetEntityID, b.TargetKind, b.BindingRole, b.CreatedBy); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE storage.assets SET reclaim_token='', reclaim_claimed_at=NULL WHERE id=$1", b.AssetID); err != nil {
		return err
	}
	return tx.Commit()
}

// checkReclaimMutation 区分删除阶段拒绝与资产已被回收，不能把空 UPDATE 当续租成功。
func (s *Store) checkReclaimMutation(ctx context.Context, id string, res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n > 0 {
		return err
	}
	var deleting bool
	err = s.db.QueryRowContext(ctx, "SELECT reclaim_deleting FROM storage.assets WHERE id=$1", id).Scan(&deleting)
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

func (s *Store) Binding(ctx context.Context, id string) (Binding, error) {
	var b Binding
	err := s.db.QueryRowContext(ctx, "SELECT id,asset_id,target_entity_id,target_kind,binding_role,created_by,created_at FROM storage.bindings WHERE id=$1", id).
		Scan(&b.ID, &b.AssetID, &b.TargetEntityID, &b.TargetKind, &b.BindingRole, &b.CreatedBy, &b.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Binding{}, ErrNotFound
	}
	return b, err
}

func (s *Store) Unbind(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM storage.bindings WHERE id=$1", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// BindingsForEntity 列出挂在某个实体上的全部文件；调用方需先确认实体对请求者可见。
func (s *Store) BindingsForEntity(ctx context.Context, entityID string) ([]FileBinding, error) {
	return s.BindingsForEntityIDs(ctx, []string{entityID})
}

// BindingsForEntityIDs 按一批实体 ID 列出挂载文件（X01 读聚合：请求 ID + 存活身份，
// 含历史别名；调用方需先确认请求实体对请求者可见）。
// 空集合直接返回空（不拼非法 SQL）；上限 500 与单 ID 口径一致，多 ID 共享该上限。
func (s *Store) BindingsForEntityIDs(ctx context.Context, entityIDs []string) ([]FileBinding, error) {
	ids := []string{}
	seen := map[string]bool{}
	for _, id := range entityIDs {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return []FileBinding{}, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "$" + strconv.Itoa(i+1)
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, `SELECT b.id,b.asset_id,b.target_entity_id,b.target_kind,b.binding_role,b.created_by,b.created_at,
		a.id,a.sha256,a.size_bytes,a.declared_size,a.mime_type,a.file_name,a.object_key,a.status,a.multipart_upload_id,a.hash_verified,a.fail_reason,a.uploader_id,a.upload_expires_at,a.blocked,a.blocked_reason,a.blocked_at,a.created_at,a.completed_at,a.reclaim_token,a.reclaim_claimed_at
		FROM storage.bindings b JOIN storage.assets a ON a.id=b.asset_id
		WHERE b.target_entity_id IN (`+strings.Join(placeholders, ",")+`) ORDER BY b.created_at DESC LIMIT 500`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FileBinding{}
	for rows.Next() {
		var f FileBinding
		if err = rows.Scan(&f.ID, &f.AssetID, &f.TargetEntityID, &f.TargetKind, &f.BindingRole, &f.CreatedBy, &f.CreatedAt,
			&f.Asset.ID, &f.Asset.SHA256, &f.Asset.SizeBytes, &f.Asset.DeclaredSize, &f.Asset.MimeType, &f.Asset.FileName, &f.Asset.ObjectKey,
			&f.Asset.Status, &f.Asset.MultipartUploadID, &f.Asset.HashVerified, &f.Asset.FailReason, &f.Asset.UploaderID, &f.Asset.UploadExpiresAt, &f.Asset.Blocked, &f.Asset.BlockedReason, &f.Asset.BlockedAt, &f.Asset.CreatedAt, &f.Asset.CompletedAt, &f.Asset.ReclaimToken, &f.Asset.ReclaimClaimedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// BindingsForAsset 用于读取鉴权：任一绑定目标可见即可读。
func (s *Store) BindingsForAsset(ctx context.Context, assetID string) ([]Binding, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id,asset_id,target_entity_id,target_kind,binding_role,created_by,created_at FROM storage.bindings WHERE asset_id=$1", assetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Binding{}
	for rows.Next() {
		var b Binding
		if err = rows.Scan(&b.ID, &b.AssetID, &b.TargetEntityID, &b.TargetKind, &b.BindingRole, &b.CreatedBy, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Stats 供运营后台查看容量占用。
func (s *Store) Stats(ctx context.Context) (assets int64, bytes int64, err error) {
	err = s.db.QueryRowContext(ctx, "SELECT count(*),COALESCE(sum(size_bytes),0) FROM storage.assets WHERE status='complete'").Scan(&assets, &bytes)
	return assets, bytes, err
}
