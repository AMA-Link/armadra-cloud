package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"

	"github.com/AMA-Link/armadra-cloud/go/internal/identity"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// 语义与内存参照、PostgreSQL 实现一致，同一套合同测试守住：
//   - Update 是一个 BEGIN IMMEDIATE 事务：读行 → 在副本上跑 fn → 变了才写（并给租户修订加一）；
//     fn 返回错误就回滚，什么也不写；
//   - 读方法答新对象；列表按创建顺序（rowid）。

// updateRow 是一行的原子读-改-写：fn 拿到副本（或 nil），改了就按原主键写回。
func updateRow[R any](
	ctx context.Context, d *DB, tenantID string,
	read func(q *safeConn) (*R, error),
	write func(q *safeConn, draft R) error,
	fn func(draft *R) error,
) error {
	return d.write(ctx, func(q *safeConn) error {
		row, err := read(q)
		if err != nil {
			return err
		}
		if row == nil {
			return fn(nil)
		}
		before := store.Marshal(row)
		if err := fn(row); err != nil {
			return err
		}
		if bytes.Equal(before, store.Marshal(row)) {
			return nil
		}
		if err := write(q, *row); err != nil {
			return err
		}
		return bumpRevision(ctx, q, tenantID)
	})
}

// one 把「没有这一行」翻成 nil。
func one[R any](row *R, err error) (*R, error) {
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return row, err
}

type ports struct{ d *DB }

// Ports 是控制核全部存储端口。
func (d *DB) Ports() *store.Ports {
	p := ports{d}
	return &store.Ports{
		Accounts:      accounts(p),
		Devices:       devices(p),
		Sources:       sources(p),
		Registrations: registrations(p),
		Links:         links(p),
		Tenants:       tenants(p),
		Traffic:       trafficStore(p),
	}
}

// ------------------------------------------------------------ 账号

type accounts ports

func (a accounts) selectOne(ctx context.Context, q *safeConn, where string, args ...any) (*store.AccountRecord, error) {
	return one(toAccount(q.QueryRowContext(ctx, "SELECT "+accountColumns+" FROM accounts WHERE "+where, args...)))
}

func (a accounts) FindByName(ctx context.Context, tenantID, name string) (out *store.AccountRecord, err error) {
	err = a.d.read(ctx, func(q *safeConn) (err error) {
		out, err = a.selectOne(ctx, q, "tenant_id = ? AND name = ?", tenantID, name)
		return err
	})
	return out, err
}

func (a accounts) Get(ctx context.Context, tenantID, accountID string) (out *store.AccountRecord, err error) {
	err = a.d.read(ctx, func(q *safeConn) (err error) {
		out, err = a.selectOne(ctx, q, "tenant_id = ? AND account_id = ?", tenantID, accountID)
		return err
	})
	return out, err
}

func (a accounts) Update(ctx context.Context, tenantID, accountID string, fn func(draft *store.AccountRecord) error) error {
	return updateRow(ctx, a.d, tenantID,
		func(q *safeConn) (*store.AccountRecord, error) {
			return a.selectOne(ctx, q, "tenant_id = ? AND account_id = ?", tenantID, accountID)
		},
		func(q *safeConn, draft store.AccountRecord) error {
			return writeAccount(ctx, q, tenantID, accountID, draft)
		},
		fn)
}

func (a accounts) ResetPassword(ctx context.Context, tenantID, accountID string, password identity.PasswordRecord, nowMs int64, keepDeviceID string) (found bool, err error) {
	err = a.d.write(ctx, func(q *safeConn) error {
		row, err := a.selectOne(ctx, q, "tenant_id = ? AND account_id = ?", tenantID, accountID)
		if err != nil || row == nil {
			return err
		}
		row.Password, row.UpdatedAtMs, row.Failures, row.LockedUntilMs = password, nowMs, 0, 0
		if err := writeAccount(ctx, q, tenantID, accountID, *row); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx,
			"UPDATE devices SET revoked_at_ms = ? WHERE tenant_id = ? AND subject = ? AND revoked_at_ms IS NULL AND device_id <> ?",
			nowMs, tenantID, accountID, keepDeviceID); err != nil {
			return err
		}
		found = true
		return bumpRevision(ctx, q, tenantID)
	})
	return found && err == nil, err
}

// ------------------------------------------------------------ 设备

type devices ports

func (v devices) selectOne(ctx context.Context, q *safeConn, tenantID, deviceID string) (*store.DeviceRecord, error) {
	return one(toDevice(q.QueryRowContext(ctx,
		"SELECT "+deviceColumns+" FROM devices WHERE tenant_id = ? AND device_id = ?", tenantID, deviceID)))
}

func (v devices) Get(ctx context.Context, tenantID, deviceID string) (out *store.DeviceRecord, err error) {
	err = v.d.read(ctx, func(q *safeConn) (err error) {
		out, err = v.selectOne(ctx, q, tenantID, deviceID)
		return err
	})
	return out, err
}

func (v devices) ListBySubject(ctx context.Context, tenantID, subject string) (out []store.DeviceRecord, err error) {
	err = v.d.read(ctx, func(q *safeConn) error {
		rows, err := q.QueryContext(ctx,
			"SELECT "+deviceColumns+" FROM devices WHERE tenant_id = ? AND subject = ? ORDER BY rowid", tenantID, subject)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			row, err := toDevice(rows)
			if err != nil {
				return err
			}
			out = append(out, *row)
		}
		return rows.Err()
	})
	return out, err
}

func (v devices) Insert(ctx context.Context, tenantID string, device store.DeviceRecord, nowMs int64) error {
	return v.d.write(ctx, func(q *safeConn) error {
		// 顺手清掉早已失效的设备行，表不无限长（判定与内存参照同一个函数）。
		rows, err := q.QueryContext(ctx, "SELECT "+deviceColumns+" FROM devices WHERE tenant_id = ?", tenantID)
		if err != nil {
			return err
		}
		var stale []string
		for rows.Next() {
			row, err := toDevice(rows)
			if err != nil {
				_ = rows.Close()
				return err
			}
			if store.IsStaleDevice(*row, nowMs) {
				stale = append(stale, row.DeviceID)
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		for _, id := range stale {
			if _, err := q.ExecContext(ctx, "DELETE FROM devices WHERE tenant_id = ? AND device_id = ?", tenantID, id); err != nil {
				return err
			}
		}
		if err := insertDevice(ctx, q, tenantID, device); err != nil {
			return err
		}
		return bumpRevision(ctx, q, tenantID)
	})
}

func (v devices) Update(ctx context.Context, tenantID, deviceID string, fn func(draft *store.DeviceRecord) error) error {
	return updateRow(ctx, v.d, tenantID,
		func(q *safeConn) (*store.DeviceRecord, error) { return v.selectOne(ctx, q, tenantID, deviceID) },
		func(q *safeConn, draft store.DeviceRecord) error {
			return writeDevice(ctx, q, tenantID, deviceID, draft)
		},
		fn)
}

func (v devices) Touch(ctx context.Context, tenantID string, seen map[string]int64) error {
	if len(seen) == 0 {
		return nil
	}
	return v.d.write(ctx, func(q *safeConn) error {
		var changed int64
		for deviceID, atMs := range seen {
			res, err := q.ExecContext(ctx,
				`UPDATE devices SET last_seen_at_ms = ?
				 WHERE tenant_id = ? AND device_id = ? AND COALESCE(last_seen_at_ms, 0) < ?`,
				atMs, tenantID, deviceID, atMs)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			changed += n
		}
		if changed > 0 {
			return bumpRevision(ctx, q, tenantID)
		}
		return nil
	})
}

// ------------------------------------------------------------ 源

type sources ports

func (s sources) selectOne(ctx context.Context, q *safeConn, tenantID, sourceID string) (*store.SourceRecord, error) {
	return one(toSource(q.QueryRowContext(ctx,
		"SELECT "+sourceColumns+" FROM sources WHERE tenant_id = ? AND source_id = ?", tenantID, sourceID)))
}

func (s sources) Get(ctx context.Context, tenantID, sourceID string) (out *store.SourceRecord, err error) {
	err = s.d.read(ctx, func(q *safeConn) (err error) {
		out, err = s.selectOne(ctx, q, tenantID, sourceID)
		return err
	})
	return out, err
}

func (s sources) List(ctx context.Context, tenantID string) (out []store.SourceRecord, err error) {
	err = s.d.read(ctx, func(q *safeConn) error {
		rows, err := q.QueryContext(ctx,
			"SELECT "+sourceColumns+" FROM sources WHERE tenant_id = ? ORDER BY rowid", tenantID)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			row, err := toSource(rows)
			if err != nil {
				return err
			}
			out = append(out, *row)
		}
		return rows.Err()
	})
	return out, err
}

func (s sources) Put(ctx context.Context, tenantID, sourceID string, fn func(existing *store.SourceRecord) (store.SourceRecord, error)) (out store.SourceRecord, err error) {
	err = s.d.write(ctx, func(q *safeConn) error {
		existing, err := s.selectOne(ctx, q, tenantID, sourceID)
		if err != nil {
			return err
		}
		record, err := fn(existing)
		if err != nil {
			return err
		}
		// 替换 = 删掉旧行再追加（与内存参照一样排到最后）。
		if _, err := q.ExecContext(ctx, "DELETE FROM sources WHERE tenant_id = ? AND source_id = ?", tenantID, sourceID); err != nil {
			return err
		}
		if err := insertSource(ctx, q, tenantID, record); err != nil {
			return err
		}
		out = record
		return bumpRevision(ctx, q, tenantID)
	})
	return out, err
}

func (s sources) Update(ctx context.Context, tenantID, sourceID string, fn func(draft *store.SourceRecord) error) error {
	return updateRow(ctx, s.d, tenantID,
		func(q *safeConn) (*store.SourceRecord, error) { return s.selectOne(ctx, q, tenantID, sourceID) },
		func(q *safeConn, draft store.SourceRecord) error {
			return writeSource(ctx, q, tenantID, sourceID, draft)
		},
		fn)
}

// ------------------------------------------------------------ 注册令牌

type registrations ports

func (r registrations) Add(ctx context.Context, tenantID string, token store.RegistrationToken, nowMs int64, max int) (added bool, err error) {
	err = r.d.write(ctx, func(q *safeConn) error {
		var live int
		if err := q.QueryRowContext(ctx,
			"SELECT count(*) FROM registration_tokens WHERE tenant_id = ? AND expires_at_ms > ?",
			tenantID, nowMs).Scan(&live); err != nil {
			return err
		}
		if live >= max {
			return nil
		}
		if _, err := q.ExecContext(ctx,
			"DELETE FROM registration_tokens WHERE tenant_id = ? AND expires_at_ms <= ?", tenantID, nowMs); err != nil {
			return err
		}
		if err := insertRegistration(ctx, q, tenantID, token); err != nil {
			return err
		}
		added = true
		return bumpRevision(ctx, q, tenantID)
	})
	return added && err == nil, err
}

func (r registrations) Consume(ctx context.Context, tenantID, tokenHash string, nowMs int64) (accountID string, ok bool, err error) {
	err = r.d.write(ctx, func(q *safeConn) error {
		// 逐条定时安全地比较（与内存参照一样不按哈希直接查）。
		rows, err := q.QueryContext(ctx,
			"SELECT "+registrationColumns+" FROM registration_tokens WHERE tenant_id = ? AND expires_at_ms > ? ORDER BY rowid",
			tenantID, nowMs)
		if err != nil {
			return err
		}
		var hit *store.RegistrationToken
		for rows.Next() {
			row, err := toRegistration(rows)
			if err != nil {
				_ = rows.Close()
				return err
			}
			if hit == nil && store.SameHex(row.TokenHash, tokenHash) {
				copied := row
				hit = &copied
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil || hit == nil {
			return err
		}
		if _, err := q.ExecContext(ctx,
			"DELETE FROM registration_tokens WHERE tenant_id = ? AND token_hash = ?", tenantID, hit.TokenHash); err != nil {
			return err
		}
		accountID, ok = hit.AccountID, true
		return bumpRevision(ctx, q, tenantID)
	})
	if err != nil {
		return "", false, err
	}
	return accountID, ok, nil
}

// ------------------------------------------------------------ 链接

type links ports

func (l links) selectOne(ctx context.Context, q *safeConn, tenantID, linkID string) (*store.LinkRecord, error) {
	return one(toLink(q.QueryRowContext(ctx,
		"SELECT "+linkColumns+" FROM links WHERE tenant_id = ? AND link_id = ?", tenantID, linkID)))
}

func (l links) Get(ctx context.Context, tenantID, linkID string) (out *store.LinkRecord, err error) {
	err = l.d.read(ctx, func(q *safeConn) (err error) {
		out, err = l.selectOne(ctx, q, tenantID, linkID)
		return err
	})
	return out, err
}

func (l links) List(ctx context.Context, tenantID, sourceID string) (out []store.LinkRecord, err error) {
	err = l.d.read(ctx, func(q *safeConn) error {
		query, args := "SELECT "+linkColumns+" FROM links WHERE tenant_id = ? ORDER BY rowid", []any{tenantID}
		if sourceID != "" {
			query = "SELECT " + linkColumns + " FROM links WHERE tenant_id = ? AND source_id = ? ORDER BY rowid"
			args = append(args, sourceID)
		}
		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			row, err := toLink(rows)
			if err != nil {
				return err
			}
			out = append(out, *row)
		}
		return rows.Err()
	})
	return out, err
}

func (l links) Insert(ctx context.Context, tenantID string, link store.LinkRecord) error {
	return l.d.write(ctx, func(q *safeConn) error {
		if err := insertLink(ctx, q, tenantID, link); err != nil {
			return err
		}
		return bumpRevision(ctx, q, tenantID)
	})
}

func (l links) Update(ctx context.Context, tenantID, linkID string, fn func(draft *store.LinkRecord) error) error {
	return updateRow(ctx, l.d, tenantID,
		func(q *safeConn) (*store.LinkRecord, error) { return l.selectOne(ctx, q, tenantID, linkID) },
		func(q *safeConn, draft store.LinkRecord) error { return writeLink(ctx, q, tenantID, linkID, draft) },
		fn)
}

func (l links) Revoke(ctx context.Context, tenantID, linkID string, nowMs int64) (sourceID string, ok bool, err error) {
	err = l.d.write(ctx, func(q *safeConn) error {
		err := q.QueryRowContext(ctx,
			"SELECT source_id FROM links WHERE tenant_id = ? AND link_id = ? AND revoked_at_ms IS NULL",
			tenantID, linkID).Scan(&sourceID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx,
			"UPDATE links SET revoked_at_ms = ? WHERE tenant_id = ? AND link_id = ?", nowMs, tenantID, linkID); err != nil {
			return err
		}
		prefix := store.GuestPrefix(linkID)
		if _, err := q.ExecContext(ctx,
			`UPDATE devices SET revoked_at_ms = ?
			 WHERE tenant_id = ? AND revoked_at_ms IS NULL AND substr(subject, 1, ?) = ?`,
			nowMs, tenantID, len([]rune(prefix)), prefix); err != nil {
			return err
		}
		ok = true
		return bumpRevision(ctx, q, tenantID)
	})
	if err != nil || !ok {
		return "", false, err
	}
	return sourceID, true, nil
}

// ------------------------------------------------------------ 租户

type tenants ports

func (t tenants) Get(ctx context.Context, tenantID string) (out store.TenantSettings, err error) {
	err = t.d.read(ctx, func(q *safeConn) error {
		var origins string
		err := q.QueryRowContext(ctx,
			"SELECT issuer, allow_guests, trusted_origins_json FROM tenants WHERE tenant_id = ?", tenantID).
			Scan(&out.Issuer, &out.AllowGuests, &origins)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNoTenant
		}
		if err != nil {
			return err
		}
		if err := decodeJSON(origins, &out.TrustedOrigins); err != nil {
			return err
		}
		out.TrustedOrigins = store.NonNil(out.TrustedOrigins)
		return nil
	})
	return out, err
}
