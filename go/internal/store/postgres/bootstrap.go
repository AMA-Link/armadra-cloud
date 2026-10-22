package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// 启动期的两件事（SaaS 实现方案 §2.1）：租户引导与审计分区。都在 advisory lock 下做，多个实例同时
// 启动时只有一个真正写，其余等它提交后看到结果。

const (
	// bootstrapLock 是租户引导的 advisory lock 键：「CLBS」。
	bootstrapLock = 0x434c4253
	// auditLock 是审计分区维护的 advisory lock 键：「AUDT」，建分区的函数文档要求调用方持有它。
	auditLock = 0x41554454
	// auditMonthsAhead 是启动时同步建出的分区数：当月与后两个月。
	auditMonthsAhead = 3
	// auditLockTimeout 是摘 / 挂兜底分区要父表排他锁时最多等多久。
	auditLockTimeout = "5s"
)

// BootstrapTenant 保证存储租户存在：不存在就建（issuer 取参数）并生成第一把签名钥（要主密钥包裹）；
// 存在而 issuer 与参数不同 → KindUnknown 拒绝，绝不改写已有租户的 issuer。答是否新建。
func (d *DB) BootstrapTenant(ctx context.Context, tenantID, issuer string, nowMs int64) (created bool, err error) {
	if d.wrapper == nil {
		return false, store.Errorf(store.KindUnknown, "租户引导要主密钥：签名私钥必须包裹后才落库")
	}
	err = d.tx(ctx, pgx.TxOptions{}, func(tx pgx.Tx) error {
		created = false
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(bootstrapLock)); err != nil {
			return err
		}
		existing, err := readTenant(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		if existing != nil {
			if existing.Issuer != issuer {
				return store.Errorf(store.KindUnknown, "issuer 与库里的不一致，拒绝启动：库里是 %s，配置是 %s", existing.Issuer, issuer)
			}
			keys, err := d.readSigningKeys(ctx, tx, tenantID)
			if err != nil {
				return err
			}
			if len(keys) == 0 {
				return store.Errorf(store.KindCorrupt, "租户没有签名钥，拒绝启动")
			}
			return nil
		}
		tenant := store.Tenant{TenantSettings: store.TenantSettings{Issuer: issuer, AllowGuests: true, TrustedOrigins: []string{}}}
		if err := createTenant(ctx, tx, tenantID, tenant); err != nil {
			return err
		}
		key, err := store.GenerateSigningKey("k1", nowMs)
		if err != nil {
			return err
		}
		if err := d.insertSigningKey(ctx, tx, tenantID, key); err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil {
		return false, err
	}
	d.dropKeyCache(tenantID)
	return created, nil
}

// AuditPartitionName 是某个 UTC 月的审计分区名：audit_log_YYYY_MM。
func AuditPartitionName(month time.Time) string {
	return fmt.Sprintf("audit_log_%04d_%02d", month.Year(), int(month.Month()))
}

// EnsureAuditPartitions 同步建 now 所在月与后两个月的审计分区（幂等）。失败（含等父表锁超时）答错误，
// 调用方拒绝启动。答确保存在的分区名。
func (d *DB) EnsureAuditPartitions(ctx context.Context, now time.Time) ([]string, error) {
	var names []string
	err := d.tx(ctx, pgx.TxOptions{}, func(tx pgx.Tx) error {
		names = names[:0]
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(auditLock)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '"+auditLockTimeout+"'"); err != nil {
			return err
		}
		first := time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
		for i := 0; i < auditMonthsAhead; i++ {
			start := first.AddDate(0, i, 0)
			end := start.AddDate(0, 1, 0)
			name := AuditPartitionName(start)
			if _, err := tx.Exec(ctx, "SELECT audit_ensure_partition($1, $2, $3)", start.UnixMilli(), end.UnixMilli(), name); err != nil {
				return err
			}
			names = append(names, name)
		}
		return nil
	})
	if err != nil {
		var pgErr interface{ SQLState() string }
		if errors.As(err, &pgErr) {
			return nil, store.Errorf(store.KindCorrupt, "审计分区建不出来：%s", pgErr.SQLState())
		}
		return nil, err
	}
	return append([]string(nil), names...), nil
}
