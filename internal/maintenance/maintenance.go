// Package maintenance 是存储服务的后台任务入口：上传治理的回收与一致性核查。
//
// 以 storage-server worker 子命令运行（见 cmd/server/main.go），默认跑一次就退出，
// 由 cron/systemd timer 按周期触发——worker 只是同一个可执行程序的另一种模式，
// 不是新的业务服务（生产目标 §6：worker 归各域）。
//
// 两条铁律：清理绝不动被绑定的资产；删字节前持久登记删除阶段。
// 任何一步的"不知道"（读失败、键对不上、缺失比例异常）都只上报不动手。
package maintenance

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/MoeclubM/metafusion-storage/internal/config"
	"github.com/MoeclubM/metafusion-storage/internal/objects"
	"github.com/MoeclubM/metafusion-storage/internal/store"
)

// cleanupBatch / reconcileBatch 是单次 worker 的处理上限：后台批处理按批推进，
// 不是把全表倒进内存。量大时多跑几次，自然收敛。
const (
	cleanupBatch   = 1000
	reconcileBatch = 500
	reportListCap  = 100
	missingAbortN  = 5
)

// CleanupReport 是单次回收的结果：只计数与抽样键，全量明细看日志。
type CleanupReport struct {
	Candidates           int
	UnboundRowsRemoved   int
	Reclaimed            int
	SharedRowsRemoved    int
	ObjectAlreadyMissing int
	SkippedBound         int
	SkippedKeyMismatch   int
	SkippedRevoked       int
	ObjectErrors         int
	RemovedKeys          []string
	MismatchedKeys       []string
}

// RunCleanup 回收过期 pending 与超过失绑保留期的 complete。
// Store 在锁内复核状态、绑定、租约/保留期与共享引用，先持久登记删除阶段，
// 再持对象键锁和资产行锁执行最多 15 秒的对象操作与删行。
// 对象操作失败保留行供重试；删除阶段的行不能再次绑定/激活，崩溃亦不能恢复成缺字节的活跃资产。
func RunCleanup(ctx context.Context, db *store.Store, objs *objects.Store, cfg config.Config) (CleanupReport, error) {
	var rep CleanupReport
	now := time.Now()
	staleBefore := now.Add(-store.ReclaimClaimTimeout)
	candidates, err := db.ReclaimCandidates(ctx, now, staleBefore, cleanupBatch)
	if err != nil {
		return rep, err
	}
	unboundBefore := now.Add(-cfg.UnboundRetention())
	unbound, err := db.UnboundCandidates(ctx, unboundBefore, cleanupBatch)
	if err != nil {
		return rep, err
	}
	candidates = append(candidates, unbound...)
	rep.Candidates = len(candidates)
	for _, a := range candidates {
		token := uuid.NewString()
		if a.Status == "pending" {
			claimed, ok, err := db.TryClaimReclaim(ctx, a.ID, now, token, staleBefore)
			if err != nil {
				rep.ObjectErrors++
				continue
			}
			if !ok {
				rep.SkippedRevoked++
				continue
			}
			a = claimed
		}
		// outcome 只在事务提交成功后计入报告。
		outcome := ""
		remove := func(objectCtx context.Context, current store.Asset, shared bool) error {
			if shared {
				outcome = "shared"
				return nil
			}
			exists, err := objs.Exists(objectCtx, current.ObjectKey)
			if err != nil {
				return err
			}
			if !exists {
				outcome = "missing"
				return nil
			}
			if err := objs.Remove(objectCtx, current.ObjectKey); err != nil {
				return err
			}
			outcome = "removed"
			return nil
		}
		var deleted bool
		if a.Status == "complete" {
			deleted, err = db.ReclaimUnbound(ctx, a.ID, unboundBefore, remove)
		} else {
			deleted, err = db.ReclaimClaimedAsset(ctx, a.ID, token, now, remove)
			if err != nil || !deleted {
				_ = db.ReleaseReclaimClaim(ctx, a.ID, token)
			}
		}
		if errors.Is(err, store.ErrReclaimKeyMismatch) {
			rep.SkippedKeyMismatch++
			if len(rep.MismatchedKeys) < reportListCap {
				rep.MismatchedKeys = append(rep.MismatchedKeys, a.ObjectKey)
			}
			continue
		}
		if err != nil {
			rep.ObjectErrors++
			continue
		}
		if !deleted {
			rep.SkippedRevoked++
			continue
		}
		if a.Status == "complete" {
			rep.UnboundRowsRemoved++
		}
		switch outcome {
		case "shared":
			rep.SharedRowsRemoved++
		case "missing":
			rep.ObjectAlreadyMissing++
		case "removed":
			rep.Reclaimed++
			if len(rep.RemovedKeys) < reportListCap {
				rep.RemovedKeys = append(rep.RemovedKeys, a.ObjectKey)
			}
		}
	}
	return rep, nil
}

// ReconcileReport 是单次对账的结果。
type ReconcileReport struct {
	CheckedComplete    int
	MissingAssetIDs    []string
	MarkedBlocked      int
	MarkingAborted     bool
	ReadErrors         int
	OrphansSeen        int
	OrphansQuarantined int
	QuarantineErrors   int
}

// RunReconcile 双向核对：库→对象（complete 行是否真有字节）与对象→库（无引用的键记孤儿台账）。
// 缺失的 complete 行先禁发标记（隔离），不解绑不删行；孤儿先搬隔离区，不删字节。
// 熔断：读错存在、或缺失比例异常（缺失多且占比过半）时只上报不标记——那更像
// 对象存储整体不可用或桶配错，而不是"这几份内容丢了"，批量禁发会把故障放大成事故。
func RunReconcile(ctx context.Context, db *store.Store, objs *objects.Store, cfg config.Config) (ReconcileReport, error) {
	var rep ReconcileReport
	missing := []store.CompleteKey{}
	for offset := 0; ; offset += reconcileBatch {
		page, err := db.ListCompleteKeys(ctx, reconcileBatch, offset)
		if err != nil {
			return rep, err
		}
		for _, k := range page {
			rep.CheckedComplete++
			exists, err := objs.Exists(ctx, k.ObjectKey)
			if err != nil {
				rep.ReadErrors++
				continue
			}
			if !exists {
				missing = append(missing, k)
				if len(rep.MissingAssetIDs) < reportListCap {
					rep.MissingAssetIDs = append(rep.MissingAssetIDs, k.ID)
				}
			}
		}
		if len(page) < reconcileBatch {
			break
		}
	}
	if shouldMarkMissing(len(missing), rep.CheckedComplete, rep.ReadErrors) {
		for _, k := range missing {
			// 隔离标记：禁发位只拦分发，绑定与元数据原样保留，字节恢复后解禁即回。
			if err := db.SetBlocked(ctx, k.ID, "object_missing"); err != nil {
				rep.ReadErrors++
				continue
			}
			rep.MarkedBlocked++
		}
	} else if len(missing) > 0 {
		rep.MarkingAborted = true
	}
	if err := scanOrphans(ctx, db, objs, cfg, &rep); err != nil {
		return rep, err
	}
	return rep, nil
}

// scanOrphans 做对象→库一半：列出对象键，无引用的记台账；超过保留期仍无引用的搬隔离区。
func scanOrphans(ctx context.Context, db *store.Store, objs *objects.Store, cfg config.Config, rep *ReconcileReport) error {
	keys, err := objs.ListKeys(ctx, "objects/", 0)
	if err != nil {
		return err
	}
	for _, key := range keys {
		ref, err := db.IsKeyReferenced(ctx, key)
		if err != nil {
			rep.ReadErrors++
			continue
		}
		if ref {
			continue
		}
		if err := db.UpsertOrphanSeen(ctx, key, "reconcile"); err != nil {
			rep.ReadErrors++
			continue
		}
		rep.OrphansSeen++
	}
	due, err := db.ListOrphansDue(ctx, time.Now().Add(-cfg.OrphanRetention()), reconcileBatch)
	if err != nil {
		return err
	}
	for _, key := range due {
		moved, err := db.QuarantineOrphan(ctx, key, func(objectCtx context.Context) error {
			_, err := objs.Quarantine(objectCtx, key)
			return err
		})
		if err != nil {
			rep.QuarantineErrors++
			continue
		}
		if moved {
			rep.OrphansQuarantined++
		}
	}
	return nil
}

// pendingExpired 与回收 SQL 一致：缺租约的行不可回收。
func pendingExpired(expiresAt *time.Time, now time.Time) bool {
	return expiresAt != nil && !expiresAt.After(now)
}

// keyMatchesSHA 是双向核对的键检查：对象键必须落在声明 sha 的前缀下。
// 文件名后缀可变（同一内容重传文件名不同），前缀是唯一的身份断言。
func keyMatchesSHA(objectKey, sha256hex string) bool {
	if objectKey == "" || sha256hex == "" {
		return false
	}
	return strings.HasPrefix(objectKey, objects.KeyPrefixFor(sha256hex))
}

// shouldMarkMissing 决定是否把缺失对象标记为禁发（纯函数，便于单测固定口径）：
// 有读错、零检查、缺失多且占比过半时一律不标记——只上报，等人工看。
func shouldMarkMissing(missing, checked, readErrors int) bool {
	if missing == 0 || checked == 0 {
		return false
	}
	if readErrors > 0 {
		return false
	}
	if missing >= missingAbortN && float64(missing)/float64(checked) > 0.5 {
		return false
	}
	return true
}
