package postgres

import (
	"bytes"
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/AMA-Link/armadra-cloud/go/internal/identity"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// 语义与内存参照、SQLite 实现一致，同一套合同测试守住：写都是一个事务——先锁租户行（同一租户的写
// 串行）→ 读行 → 在副本上跑 fn → 变了才写（并给租户修订加一）；fn 返回错误就回滚；
// 读方法答新对象；列表按创建顺序（seq）。

// one 把「没有这一行」翻成 nil。
func one[R any](row *R, err error) (*R, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return row, err
}

// updateRow 是一行的原子读-改-写：fn 拿到副本（或 nil），改了就按原主键写回。
func updateRow[R any](
	ctx context.Context, write txWriter, tenantID string,
	read func(tx pgx.Tx) (*R, error),
	put func(tx pgx.Tx, draft R) error,
	fn func(draft *R) error,
) error {
	return write(ctx, tenantID, func(tx pgx.Tx) (bool, error) {
		row, err := read(tx)
		if err != nil {
			return false, err
		}
		if row == nil {
			return false, fn(nil)
		}
		before := store.Marshal(row)
		if err := fn(row); err != nil {
			return false, err
		}
		if bytes.Equal(before, store.Marshal(row)) {
			return false, nil
		}
		return true, put(tx, *row)
	})
}

// list 读多行。
func list[R any](ctx context.Context, q querier, scan func(scanner) (R, error), query string, args ...any) ([]R, error) {
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []R
	for rows.Next() {
		row, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func deref[R any](scan func(scanner) (*R, error)) func(scanner) (R, error) {
	return func(s scanner) (R, error) {
		row, err := scan(s)
		if err != nil {
			var zero R
			return zero, err
		}
		return *row, nil
	}
}

// forUpdate 是读-改-写路径读行时的锁子句：updateRow 的读、Put、Insert 的失效设备清理加它，并发的读-改-写
// 按行排队，不丢更新（读已提交下不加锁会让两个事务读到同一份旧行、各写各的）。Get / List 这类普通读不加：
// 否则读也隐式拿行锁，既挡住别人的写，又绕过 saas.Tx 的加锁顺序检查。
const forUpdate = " FOR UPDATE"

// txWriter 跑一个租户的写：独立模式是 DB.write（自己开事务、锁租户行、改了就加修订号），
// 绑定模式（TxPorts）是直接在调用方的事务里跑 fn。
type txWriter func(ctx context.Context, tenantID string, fn func(tx pgx.Tx) (bool, error)) error

// ports 是端口的公共底座。tx 为 nil 时每个方法自己开事务（个人中转与 serve 之外的调用）；
// tx 非 nil 时（TxPorts）全部读写都在这个事务里，不锁 tenants 行、不加修订号，设备清理按账号（SaaS 实现方案 §4.11）。
type ports struct {
	d  *DB
	tx pgx.Tx
	// rowLock 为真（SaaSPorts）：每次写自己开事务，但不锁 tenants 行、不加修订号；读-改-写靠行锁。
	rowLock bool
}

// q 是读用的连接：绑定模式读事务自己（看得到本事务的写）。
func (p ports) q() querier {
	if p.tx != nil {
		return p.tx
	}
	return p.d.pool
}

// write 是写用的事务入口。
func (p ports) write(ctx context.Context, tenantID string, fn func(tx pgx.Tx) (bool, error)) error {
	if p.tx != nil {
		_, err := fn(p.tx)
		return err
	}
	if p.rowLock {
		return p.d.tx(ctx, pgx.TxOptions{}, func(tx pgx.Tx) error {
			_, err := fn(tx)
			return err
		})
	}
	return p.d.write(ctx, tenantID, fn)
}

// Ports 是控制核全部存储端口。
func (d *DB) Ports() *store.Ports { return portsOf(ports{d: d}) }

// SaaSPorts 是 SaaS 控制面平时用的核心端口（不在 SaaS 事务里时）：每次写一个独立事务，读-改-写靠行锁，
// 不锁 tenants 行、不加租户修订号（否则所有登录、刷新、最近使用落盘都在租户行上排队，还会让各实例的
// 签名钥监视白白重读），设备清理只看这个账号。在 SaaS 事务（saas.Tx）的闭包里不能用它，用 tx.Ports()。
func (d *DB) SaaSPorts() *store.Ports { return portsOf(ports{d: d, rowLock: true}) }

func portsOf(p ports) *store.Ports {
	return &store.Ports{
		Accounts:      accounts{p},
		Devices:       devices{p},
		Sources:       sources{p},
		Registrations: registrations{p},
		Links:         links{p},
		Tenants:       tenants{p},
		Traffic:       trafficStore(p),
	}
}

// ------------------------------------------------------------ 账号

type accounts struct{ ports }

func (a accounts) selectOne(ctx context.Context, q querier, lock, where string, args ...any) (*store.AccountRecord, error) {
	return one(toAccount(q.QueryRow(ctx, "SELECT "+accountColumns+" FROM accounts WHERE "+where+lock, args...)))
}

func (a accounts) FindByName(ctx context.Context, tenantID, name string) (*store.AccountRecord, error) {
	return a.selectOne(ctx, a.q(), "", "tenant_id = $1 AND name = $2", tenantID, name)
}

func (a accounts) Get(ctx context.Context, tenantID, accountID string) (*store.AccountRecord, error) {
	return a.selectOne(ctx, a.q(), "", "tenant_id = $1 AND account_id = $2", tenantID, accountID)
}

func (a accounts) Update(ctx context.Context, tenantID, accountID string, fn func(draft *store.AccountRecord) error) error {
	return updateRow(ctx, a.write, tenantID,
		func(tx pgx.Tx) (*store.AccountRecord, error) {
			return a.selectOne(ctx, tx, forUpdate, "tenant_id = $1 AND account_id = $2", tenantID, accountID)
		},
		func(tx pgx.Tx, draft store.AccountRecord) error {
			return writeAccount(ctx, tx, tenantID, accountID, draft)
		},
		fn)
}

func (a accounts) ResetPassword(ctx context.Context, tenantID, accountID string, password identity.PasswordRecord, nowMs int64, keepDeviceID string) (found bool, err error) {
	err = a.write(ctx, tenantID, func(tx pgx.Tx) (bool, error) {
		row, err := a.selectOne(ctx, tx, forUpdate, "tenant_id = $1 AND account_id = $2", tenantID, accountID)
		if err != nil || row == nil {
			return false, err
		}
		row.Password, row.UpdatedAtMs, row.Failures, row.LockedUntilMs = password, nowMs, 0, 0
		if err := writeAccount(ctx, tx, tenantID, accountID, *row); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE devices SET revoked_at_ms = $1
			 WHERE tenant_id = $2 AND subject = $3 AND revoked_at_ms IS NULL AND device_id <> $4`,
			nowMs, tenantID, accountID, keepDeviceID); err != nil {
			return false, err
		}
		found = true
		return true, nil
	})
	return found && err == nil, err
}

// ------------------------------------------------------------ 设备

type devices struct{ ports }

func (v devices) selectOne(ctx context.Context, q querier, lock, tenantID, deviceID string) (*store.DeviceRecord, error) {
	return one(toDevice(q.QueryRow(ctx,
		"SELECT "+deviceColumns+" FROM devices WHERE tenant_id = $1 AND device_id = $2"+lock, tenantID, deviceID)))
}

func (v devices) Get(ctx context.Context, tenantID, deviceID string) (*store.DeviceRecord, error) {
	return v.selectOne(ctx, v.q(), "", tenantID, deviceID)
}

func (v devices) ListBySubject(ctx context.Context, tenantID, subject string) ([]store.DeviceRecord, error) {
	return list(ctx, v.q(), deref(toDevice),
		"SELECT "+deviceColumns+" FROM devices WHERE tenant_id = $1 AND subject = $2 ORDER BY seq", tenantID, subject)
}

func (v devices) Insert(ctx context.Context, tenantID string, device store.DeviceRecord, nowMs int64) error {
	return v.write(ctx, tenantID, func(tx pgx.Tx) (bool, error) {
		// 顺手清掉早已失效的设备行，表不无限长（判定与内存参照同一个函数；只看已撤销或已过期的）。
		// 绑定模式（SaaS）只清这个账号的：不扫整个租户（租户里有海量账号的设备）。
		query := "SELECT " + deviceColumns + ` FROM devices
			 WHERE tenant_id = $1 AND (revoked_at_ms IS NOT NULL OR expires_at_ms <= $2)`
		args := []any{tenantID, nowMs}
		if v.tx != nil || v.rowLock {
			query += " AND subject = $3"
			args = append(args, device.Subject)
		}
		candidates, err := list(ctx, tx, deref(toDevice), query+forUpdate, args...)
		if err != nil {
			return false, err
		}
		for _, existing := range candidates {
			if store.IsStaleDevice(existing, nowMs) {
				if _, err := tx.Exec(ctx, "DELETE FROM devices WHERE tenant_id = $1 AND device_id = $2",
					tenantID, existing.DeviceID); err != nil {
					return false, err
				}
			}
		}
		return true, insertDevice(ctx, tx, tenantID, device)
	})
}

func (v devices) Update(ctx context.Context, tenantID, deviceID string, fn func(draft *store.DeviceRecord) error) error {
	return updateRow(ctx, v.write, tenantID,
		func(tx pgx.Tx) (*store.DeviceRecord, error) {
			return v.selectOne(ctx, tx, forUpdate, tenantID, deviceID)
		},
		func(tx pgx.Tx, draft store.DeviceRecord) error {
			return writeDevice(ctx, tx, tenantID, deviceID, draft)
		},
		fn)
}

func (v devices) Touch(ctx context.Context, tenantID string, seen map[string]int64) error {
	if len(seen) == 0 {
		return nil
	}
	return v.write(ctx, tenantID, func(tx pgx.Tx) (bool, error) {
		var changed int64
		for deviceID, atMs := range seen {
			tag, err := tx.Exec(ctx,
				`UPDATE devices SET last_seen_at_ms = $1
				 WHERE tenant_id = $2 AND device_id = $3 AND COALESCE(last_seen_at_ms, 0) < $1`,
				atMs, tenantID, deviceID)
			if err != nil {
				return false, err
			}
			changed += tag.RowsAffected()
		}
		return changed > 0, nil
	})
}

// ------------------------------------------------------------ 源

type sources struct{ ports }

func (s sources) selectOne(ctx context.Context, q querier, lock, tenantID, sourceID string) (*store.SourceRecord, error) {
	return one(toSource(q.QueryRow(ctx,
		"SELECT "+sourceColumns+" FROM sources WHERE tenant_id = $1 AND source_id = $2"+lock, tenantID, sourceID)))
}

func (s sources) Get(ctx context.Context, tenantID, sourceID string) (*store.SourceRecord, error) {
	return s.selectOne(ctx, s.q(), "", tenantID, sourceID)
}

func (s sources) List(ctx context.Context, tenantID string) ([]store.SourceRecord, error) {
	return list(ctx, s.q(), deref(toSource),
		"SELECT "+sourceColumns+" FROM sources WHERE tenant_id = $1 ORDER BY seq", tenantID)
}

func (s sources) Put(ctx context.Context, tenantID, sourceID string, fn func(existing *store.SourceRecord) (store.SourceRecord, error)) (out store.SourceRecord, err error) {
	err = s.write(ctx, tenantID, func(tx pgx.Tx) (bool, error) {
		existing, err := s.selectOne(ctx, tx, forUpdate, tenantID, sourceID)
		if err != nil {
			return false, err
		}
		record, err := fn(existing)
		if err != nil {
			return false, err
		}
		// 替换 = 删掉旧行再追加（与内存参照一样排到最后）。
		if _, err := tx.Exec(ctx, "DELETE FROM sources WHERE tenant_id = $1 AND source_id = $2", tenantID, sourceID); err != nil {
			return false, err
		}
		out = record
		return true, insertSource(ctx, tx, tenantID, record)
	})
	return out, err
}

func (s sources) Update(ctx context.Context, tenantID, sourceID string, fn func(draft *store.SourceRecord) error) error {
	return updateRow(ctx, s.write, tenantID,
		func(tx pgx.Tx) (*store.SourceRecord, error) {
			return s.selectOne(ctx, tx, forUpdate, tenantID, sourceID)
		},
		func(tx pgx.Tx, draft store.SourceRecord) error {
			return writeSource(ctx, tx, tenantID, sourceID, draft)
		},
		fn)
}

// ------------------------------------------------------------ 注册令牌

type registrations struct{ ports }

func (r registrations) Add(ctx context.Context, tenantID string, token store.RegistrationToken, nowMs int64, max int) (added bool, err error) {
	err = r.write(ctx, tenantID, func(tx pgx.Tx) (bool, error) {
		var live int
		if err := tx.QueryRow(ctx,
			"SELECT count(*)::int FROM registration_tokens WHERE tenant_id = $1 AND expires_at_ms > $2",
			tenantID, nowMs).Scan(&live); err != nil {
			return false, err
		}
		if live >= max {
			return false, nil
		}
		if _, err := tx.Exec(ctx,
			"DELETE FROM registration_tokens WHERE tenant_id = $1 AND expires_at_ms <= $2", tenantID, nowMs); err != nil {
			return false, err
		}
		added = true
		return true, insertRegistration(ctx, tx, tenantID, token)
	})
	return added && err == nil, err
}

func (r registrations) Consume(ctx context.Context, tenantID, tokenHash string, nowMs int64) (accountID string, ok bool, err error) {
	err = r.write(ctx, tenantID, func(tx pgx.Tx) (bool, error) {
		// 逐条定时安全地比较（与内存参照一样不按哈希直接查）。这里不加 FOR UPDATE：否则会锁住租户下全部
		// 未过期的注册令牌，把所有主机登记串行化；单次消耗靠下面 DELETE 的 RowsAffected()==1（并发的另一个
		// 事务先删了，这一条的 DELETE 等它提交后影响 0 行，当作已用）。
		rows, err := list(ctx, tx, toRegistration,
			"SELECT "+registrationColumns+" FROM registration_tokens WHERE tenant_id = $1 AND expires_at_ms > $2 ORDER BY seq",
			tenantID, nowMs)
		if err != nil {
			return false, err
		}
		for _, row := range rows {
			if !store.SameHex(row.TokenHash, tokenHash) {
				continue
			}
			tag, err := tx.Exec(ctx,
				"DELETE FROM registration_tokens WHERE tenant_id = $1 AND token_hash = $2", tenantID, row.TokenHash)
			if err != nil {
				return false, err
			}
			// 令牌已被别人消耗（并发）：当作已用，不再认。
			if tag.RowsAffected() != 1 {
				continue
			}
			accountID, ok = row.AccountID, true
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return "", false, err
	}
	return accountID, ok, nil
}

// ------------------------------------------------------------ 链接

type links struct{ ports }

func (l links) selectOne(ctx context.Context, q querier, lock, tenantID, linkID string) (*store.LinkRecord, error) {
	return one(toLink(q.QueryRow(ctx,
		"SELECT "+linkColumns+" FROM links WHERE tenant_id = $1 AND link_id = $2"+lock, tenantID, linkID)))
}

func (l links) Get(ctx context.Context, tenantID, linkID string) (*store.LinkRecord, error) {
	return l.selectOne(ctx, l.q(), "", tenantID, linkID)
}

func (l links) List(ctx context.Context, tenantID, sourceID string) ([]store.LinkRecord, error) {
	if sourceID == "" {
		return list(ctx, l.q(), deref(toLink),
			"SELECT "+linkColumns+" FROM links WHERE tenant_id = $1 ORDER BY seq", tenantID)
	}
	return list(ctx, l.q(), deref(toLink),
		"SELECT "+linkColumns+" FROM links WHERE tenant_id = $1 AND source_id = $2 ORDER BY seq", tenantID, sourceID)
}

func (l links) Insert(ctx context.Context, tenantID string, link store.LinkRecord) error {
	return l.write(ctx, tenantID, func(tx pgx.Tx) (bool, error) {
		return true, insertLink(ctx, tx, tenantID, link)
	})
}

func (l links) Update(ctx context.Context, tenantID, linkID string, fn func(draft *store.LinkRecord) error) error {
	return updateRow(ctx, l.write, tenantID,
		func(tx pgx.Tx) (*store.LinkRecord, error) { return l.selectOne(ctx, tx, forUpdate, tenantID, linkID) },
		func(tx pgx.Tx, draft store.LinkRecord) error { return writeLink(ctx, tx, tenantID, linkID, draft) },
		fn)
}

func (l links) Revoke(ctx context.Context, tenantID, linkID string, nowMs int64) (sourceID string, ok bool, err error) {
	err = l.write(ctx, tenantID, func(tx pgx.Tx) (bool, error) {
		err := tx.QueryRow(ctx,
			"SELECT source_id FROM links WHERE tenant_id = $1 AND link_id = $2 AND revoked_at_ms IS NULL",
			tenantID, linkID).Scan(&sourceID)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx,
			"UPDATE links SET revoked_at_ms = $1 WHERE tenant_id = $2 AND link_id = $3", nowMs, tenantID, linkID); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE devices SET revoked_at_ms = $1
			 WHERE tenant_id = $2 AND revoked_at_ms IS NULL AND starts_with(subject, $3)`,
			nowMs, tenantID, store.GuestPrefix(linkID)); err != nil {
			return false, err
		}
		ok = true
		return true, nil
	})
	if err != nil || !ok {
		return "", false, err
	}
	return sourceID, true, nil
}

// ------------------------------------------------------------ 租户

type tenants struct{ ports }

func (t tenants) Get(ctx context.Context, tenantID string) (store.TenantSettings, error) {
	return readTenantSettings(ctx, t.q(), tenantID)
}

func readTenantSettings(ctx context.Context, q querier, tenantID string) (out store.TenantSettings, err error) {
	var origins string
	err = q.QueryRow(ctx,
		"SELECT issuer, allow_guests, trusted_origins_json FROM tenants WHERE tenant_id = $1", tenantID).
		Scan(&out.Issuer, &out.AllowGuests, &origins)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, store.ErrNoTenant
	}
	if err != nil {
		return out, err
	}
	if err := decodeJSON(origins, &out.TrustedOrigins); err != nil {
		return out, err
	}
	out.TrustedOrigins = store.NonNil(out.TrustedOrigins)
	return out, nil
}
