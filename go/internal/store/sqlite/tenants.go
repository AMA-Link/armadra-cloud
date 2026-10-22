package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// 租户行与整租户的导入 / 导出（personal 的 migrate、status、doctor 用）。快照里各表按创建顺序
// （rowid），导入按数组顺序写，导出再导入行序不变。

const tenantColumns = "issuer, allow_guests, trusted_origins_json, rate_limits_json, trusted_proxy_json, display_name, revision"

func toTenant(row scanner) (*store.Tenant, error) {
	var (
		t                      store.Tenant
		origins                string
		limits, proxy, display sql.NullString
	)
	if err := row.Scan(&t.Issuer, &t.AllowGuests, &origins, &limits, &proxy, &display, &t.Revision); err != nil {
		return nil, err
	}
	if err := decodeJSON(origins, &t.TrustedOrigins); err != nil {
		return nil, err
	}
	t.TrustedOrigins = store.NonNil(t.TrustedOrigins)
	if limits.Valid {
		t.RateLimits = []byte(limits.String)
	}
	if proxy.Valid {
		t.TrustedProxy = []byte(proxy.String)
	}
	if display.Valid {
		v := display.String
		t.DisplayName = &v
	}
	return &t, nil
}

func rawJSON(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

func displayName(name *string) any {
	if name == nil {
		return nil
	}
	return *name
}

func readTenant(ctx context.Context, q *sql.Conn, tenantID string) (*store.Tenant, error) {
	return one(toTenant(q.QueryRowContext(ctx, "SELECT "+tenantColumns+" FROM tenants WHERE tenant_id = ?", tenantID)))
}

func createTenant(ctx context.Context, q *sql.Conn, tenantID string, t store.Tenant) error {
	_, err := q.ExecContext(ctx,
		`INSERT INTO tenants (tenant_id, issuer, allow_guests, trusted_origins_json, rate_limits_json,
		   trusted_proxy_json, display_name, revision) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		tenantID, t.Issuer, t.AllowGuests, encodeJSON(store.NonNil(t.TrustedOrigins)), rawJSON(t.RateLimits),
		rawJSON(t.TrustedProxy), displayName(t.DisplayName), t.Revision)
	return err
}

// ListTenants 按创建顺序列出租户 id。
func (d *DB) ListTenants(ctx context.Context) (out []string, err error) {
	err = d.read(ctx, func(q *sql.Conn) error {
		rows, err := q.QueryContext(ctx, "SELECT tenant_id FROM tenants ORDER BY rowid")
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out = append(out, id)
		}
		return rows.Err()
	})
	return out, err
}

// ReadTenant 读租户行；没有答 nil。
func (d *DB) ReadTenant(ctx context.Context, tenantID string) (out *store.Tenant, err error) {
	err = d.read(ctx, func(q *sql.Conn) (err error) {
		out, err = readTenant(ctx, q, tenantID)
		return err
	})
	return out, err
}

// Revision 是租户当前的修订号。
func (d *DB) Revision(ctx context.Context, tenantID string) (revision int64, ok bool, err error) {
	err = d.read(ctx, func(q *sql.Conn) error {
		err := q.QueryRowContext(ctx, "SELECT revision FROM tenants WHERE tenant_id = ?", tenantID).Scan(&revision)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		ok = err == nil
		return err
	})
	return revision, ok, err
}

// CreateTenant 建租户（已有同 id 的会因主键冲突失败）。
func (d *DB) CreateTenant(ctx context.Context, tenantID string, tenant store.Tenant) error {
	return d.write(ctx, func(q *sql.Conn) error { return createTenant(ctx, q, tenantID, tenant) })
}

// UpdateTenant 是租户设置的原子读-改-写；没变不写。不改修订号以外的列（revision 由写入加一）。
func (d *DB) UpdateTenant(ctx context.Context, tenantID string, fn func(draft *store.Tenant) error) error {
	return d.write(ctx, func(q *sql.Conn) error {
		current, err := readTenant(ctx, q, tenantID)
		if err != nil {
			return err
		}
		if current == nil {
			return store.ErrNoTenant
		}
		current.Revision = 0
		before := store.Marshal(current)
		if err := fn(current); err != nil {
			return err
		}
		if bytes.Equal(before, store.Marshal(current)) {
			return nil
		}
		if _, err := q.ExecContext(ctx,
			`UPDATE tenants SET issuer = ?, allow_guests = ?, trusted_origins_json = ?,
			   rate_limits_json = ?, trusted_proxy_json = ?, display_name = ?
			 WHERE tenant_id = ?`,
			current.Issuer, current.AllowGuests, encodeJSON(store.NonNil(current.TrustedOrigins)),
			rawJSON(current.RateLimits), rawJSON(current.TrustedProxy), displayName(current.DisplayName), tenantID); err != nil {
			return err
		}
		return bumpRevision(ctx, q, tenantID)
	})
}

// AddAccount 直接放一个账号（建租户与测试装配用）。
func (d *DB) AddAccount(ctx context.Context, tenantID string, account store.AccountRecord) error {
	return d.write(ctx, func(q *sql.Conn) error {
		if err := insertAccount(ctx, q, tenantID, account); err != nil {
			return err
		}
		return bumpRevision(ctx, q, tenantID)
	})
}

// ImportTenant 一个事务写入整个租户（租户必须还不存在，否则 exists）；修订号照快照。
func (d *DB) ImportTenant(ctx context.Context, tenantID string, snapshot store.TenantSnapshot) error {
	return d.write(ctx, func(q *sql.Conn) error {
		existing, err := readTenant(ctx, q, tenantID)
		if err != nil {
			return err
		}
		if existing != nil {
			return store.Errorf(store.KindExists, "租户已存在：%s", tenantID)
		}
		if err := createTenant(ctx, q, tenantID, snapshot.Tenant); err != nil {
			return err
		}
		for _, row := range snapshot.Accounts {
			if err := insertAccount(ctx, q, tenantID, row); err != nil {
				return err
			}
		}
		for _, row := range snapshot.SigningKeys {
			if err := insertSigningKey(ctx, q, tenantID, row); err != nil {
				return err
			}
		}
		for _, row := range snapshot.Sources {
			if err := insertSource(ctx, q, tenantID, row); err != nil {
				return err
			}
		}
		for _, row := range snapshot.Registrations {
			if err := insertRegistration(ctx, q, tenantID, row); err != nil {
				return err
			}
		}
		for _, row := range snapshot.Devices {
			if err := insertDevice(ctx, q, tenantID, row); err != nil {
				return err
			}
		}
		for _, row := range snapshot.Links {
			if err := insertLink(ctx, q, tenantID, row); err != nil {
				return err
			}
		}
		return nil
	})
}

// collect 把一个查询的所有行用 scan 读成切片。
func collect[R any](ctx context.Context, q *sql.Conn, query, tenantID string, scan func(scanner) (R, error)) ([]R, error) {
	rows, err := q.QueryContext(ctx, query, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
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

// ExportTenant 返回整个租户的一致快照（读在一个事务里，别的进程写不进中间）。
func (d *DB) ExportTenant(ctx context.Context, tenantID string) (snap store.TenantSnapshot, err error) {
	err = d.snapshot(ctx, func(q *sql.Conn) error {
		tenant, err := readTenant(ctx, q, tenantID)
		if err != nil {
			return err
		}
		if tenant == nil {
			return store.ErrNoTenant
		}
		snap.Tenant = *tenant
		deref := func(r *store.AccountRecord, err error) (store.AccountRecord, error) {
			if err != nil {
				return store.AccountRecord{}, err
			}
			return *r, nil
		}
		if snap.Accounts, err = collect(ctx, q, "SELECT "+accountColumns+" FROM accounts WHERE tenant_id = ? ORDER BY rowid", tenantID,
			func(s scanner) (store.AccountRecord, error) { return deref(toAccount(s)) }); err != nil {
			return err
		}
		if snap.SigningKeys, err = collect(ctx, q, "SELECT "+keyColumns+" FROM signing_keys WHERE tenant_id = ? ORDER BY rowid", tenantID,
			func(s scanner) (store.SigningKeyRecord, error) { return toSigningKey(s) }); err != nil {
			return err
		}
		if snap.Sources, err = collect(ctx, q, "SELECT "+sourceColumns+" FROM sources WHERE tenant_id = ? ORDER BY rowid", tenantID,
			func(s scanner) (store.SourceRecord, error) {
				r, err := toSource(s)
				if err != nil {
					return store.SourceRecord{}, err
				}
				return *r, nil
			}); err != nil {
			return err
		}
		if snap.Registrations, err = collect(ctx, q, "SELECT "+registrationColumns+" FROM registration_tokens WHERE tenant_id = ? ORDER BY rowid", tenantID,
			func(s scanner) (store.RegistrationToken, error) { return toRegistration(s) }); err != nil {
			return err
		}
		if snap.Devices, err = collect(ctx, q, "SELECT "+deviceColumns+" FROM devices WHERE tenant_id = ? ORDER BY rowid", tenantID,
			func(s scanner) (store.DeviceRecord, error) {
				r, err := toDevice(s)
				if err != nil {
					return store.DeviceRecord{}, err
				}
				return *r, nil
			}); err != nil {
			return err
		}
		snap.Links, err = collect(ctx, q, "SELECT "+linkColumns+" FROM links WHERE tenant_id = ? ORDER BY rowid", tenantID,
			func(s scanner) (store.LinkRecord, error) {
				r, err := toLink(s)
				if err != nil {
					return store.LinkRecord{}, err
				}
				return *r, nil
			})
		return err
	})
	return snap, err
}

// RevokedIDs 是已撤销的源与链接（serve 察觉到别的进程改了库时比对用）。
func (d *DB) RevokedIDs(ctx context.Context, tenantID string) (sources, links []string, err error) {
	err = d.read(ctx, func(q *sql.Conn) error {
		list := func(query string) ([]string, error) {
			return collect(ctx, q, query, tenantID, func(s scanner) (string, error) {
				var id string
				return id, s.Scan(&id)
			})
		}
		var err error
		if sources, err = list("SELECT source_id FROM sources WHERE tenant_id = ? AND revoked_at_ms IS NOT NULL"); err != nil {
			return err
		}
		links, err = list("SELECT link_id FROM links WHERE tenant_id = ? AND revoked_at_ms IS NOT NULL")
		return err
	})
	return sources, links, err
}
