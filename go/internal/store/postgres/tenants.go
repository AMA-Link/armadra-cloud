package postgres

import (
	"bytes"
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// 租户行与整租户的导入 / 导出（personal 的 migrate、status、doctor 用）。

const tenantColumns = "issuer, allow_guests, trusted_origins_json, rate_limits_json, trusted_proxy_json, display_name, revision"

func toTenant(row scanner) (*store.Tenant, error) {
	var (
		t                      store.Tenant
		origins                string
		limits, proxy, display *string
	)
	if err := row.Scan(&t.Issuer, &t.AllowGuests, &origins, &limits, &proxy, &display, &t.Revision); err != nil {
		return nil, err
	}
	if err := decodeJSON(origins, &t.TrustedOrigins); err != nil {
		return nil, err
	}
	t.TrustedOrigins = store.NonNil(t.TrustedOrigins)
	if limits != nil {
		t.RateLimits = []byte(*limits)
	}
	if proxy != nil {
		t.TrustedProxy = []byte(*proxy)
	}
	t.DisplayName = display
	return &t, nil
}

func rawJSON(raw []byte) *string {
	if len(raw) == 0 {
		return nil
	}
	text := string(raw)
	return &text
}

func readTenant(ctx context.Context, q querier, tenantID string) (*store.Tenant, error) {
	return one(toTenant(q.QueryRow(ctx, "SELECT "+tenantColumns+" FROM tenants WHERE tenant_id = $1", tenantID)))
}

func createTenant(ctx context.Context, q querier, tenantID string, t store.Tenant) error {
	_, err := q.Exec(ctx,
		`INSERT INTO tenants (tenant_id, issuer, allow_guests, trusted_origins_json, rate_limits_json,
		   trusted_proxy_json, display_name, revision) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		tenantID, t.Issuer, t.AllowGuests, encodeJSON(store.NonNil(t.TrustedOrigins)), rawJSON(t.RateLimits),
		rawJSON(t.TrustedProxy), t.DisplayName, t.Revision)
	return err
}

// ListTenants 按创建顺序列出租户 id。
func (d *DB) ListTenants(ctx context.Context) ([]string, error) {
	return list(ctx, d.pool, func(s scanner) (string, error) {
		var id string
		return id, s.Scan(&id)
	}, "SELECT tenant_id FROM tenants ORDER BY seq")
}

// ReadTenant 读租户行；没有答 nil。
func (d *DB) ReadTenant(ctx context.Context, tenantID string) (*store.Tenant, error) {
	return readTenant(ctx, d.pool, tenantID)
}

// Revision 是租户当前的修订号（个人中转每秒看一次，察觉别的进程 / 节点的写）。
func (d *DB) Revision(ctx context.Context, tenantID string) (revision int64, ok bool, err error) {
	err = d.pool.QueryRow(ctx, "SELECT revision FROM tenants WHERE tenant_id = $1", tenantID).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return revision, err == nil, err
}

// CreateTenant 建租户（已有同 id 的会因主键冲突失败）。
func (d *DB) CreateTenant(ctx context.Context, tenantID string, tenant store.Tenant) error {
	return d.tx(ctx, pgx.TxOptions{}, func(tx pgx.Tx) error { return createTenant(ctx, tx, tenantID, tenant) })
}

// UpdateTenant 是租户设置的原子读-改-写；没变不写。
func (d *DB) UpdateTenant(ctx context.Context, tenantID string, fn func(draft *store.Tenant) error) error {
	err := d.write(ctx, tenantID, func(tx pgx.Tx) (bool, error) {
		current, err := readTenant(ctx, tx, tenantID)
		if err != nil {
			return false, err
		}
		if current == nil {
			return false, store.ErrNoTenant
		}
		current.Revision = 0
		before := store.Marshal(current)
		if err := fn(current); err != nil {
			return false, err
		}
		if bytes.Equal(before, store.Marshal(current)) {
			return false, nil
		}
		_, err = tx.Exec(ctx,
			`UPDATE tenants SET issuer = $1, allow_guests = $2, trusted_origins_json = $3,
			   rate_limits_json = $4, trusted_proxy_json = $5, display_name = $6
			 WHERE tenant_id = $7`,
			current.Issuer, current.AllowGuests, encodeJSON(store.NonNil(current.TrustedOrigins)),
			rawJSON(current.RateLimits), rawJSON(current.TrustedProxy), current.DisplayName, tenantID)
		return true, err
	})
	d.dropKeyCache(tenantID)
	return err
}

// AddAccount 直接放一个账号（建租户与测试装配用）。
func (d *DB) AddAccount(ctx context.Context, tenantID string, account store.AccountRecord) error {
	return d.write(ctx, tenantID, func(tx pgx.Tx) (bool, error) {
		return true, insertAccount(ctx, tx, tenantID, account)
	})
}

// ImportTenant 一个事务写入整个租户（租户必须还不存在，否则 exists）；修订号照快照。
func (d *DB) ImportTenant(ctx context.Context, tenantID string, snapshot store.TenantSnapshot) error {
	return d.tx(ctx, pgx.TxOptions{}, func(tx pgx.Tx) error {
		existing, err := readTenant(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		if existing != nil {
			return store.Errorf(store.KindExists, "租户已存在：%s", tenantID)
		}
		if err := createTenant(ctx, tx, tenantID, snapshot.Tenant); err != nil {
			return err
		}
		for _, row := range snapshot.Accounts {
			if err := insertAccount(ctx, tx, tenantID, row); err != nil {
				return err
			}
		}
		for _, row := range snapshot.SigningKeys {
			if err := insertSigningKey(ctx, tx, tenantID, row); err != nil {
				return err
			}
		}
		for _, row := range snapshot.Sources {
			if err := insertSource(ctx, tx, tenantID, row); err != nil {
				return err
			}
		}
		for _, row := range snapshot.Registrations {
			if err := insertRegistration(ctx, tx, tenantID, row); err != nil {
				return err
			}
		}
		for _, row := range snapshot.Devices {
			if err := insertDevice(ctx, tx, tenantID, row); err != nil {
				return err
			}
		}
		for _, row := range snapshot.Links {
			if err := insertLink(ctx, tx, tenantID, row); err != nil {
				return err
			}
		}
		return nil
	})
}

// ExportTenant 返回整个租户的一致快照（读在一个 REPEATABLE READ 只读事务里，别的写不进中间）。
func (d *DB) ExportTenant(ctx context.Context, tenantID string) (snap store.TenantSnapshot, err error) {
	err = d.tx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		tenant, err := readTenant(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		if tenant == nil {
			return store.Errorf(store.KindNotFound, "没有这个租户：%s", tenantID)
		}
		snap.Tenant = *tenant
		if snap.Accounts, err = list(ctx, tx, deref(toAccount),
			"SELECT "+accountColumns+" FROM accounts WHERE tenant_id = $1 ORDER BY seq", tenantID); err != nil {
			return err
		}
		if snap.SigningKeys, err = readSigningKeys(ctx, tx, tenantID); err != nil {
			return err
		}
		if snap.Sources, err = list(ctx, tx, deref(toSource),
			"SELECT "+sourceColumns+" FROM sources WHERE tenant_id = $1 ORDER BY seq", tenantID); err != nil {
			return err
		}
		if snap.Registrations, err = list(ctx, tx, toRegistration,
			"SELECT "+registrationColumns+" FROM registration_tokens WHERE tenant_id = $1 ORDER BY seq", tenantID); err != nil {
			return err
		}
		if snap.Devices, err = list(ctx, tx, deref(toDevice),
			"SELECT "+deviceColumns+" FROM devices WHERE tenant_id = $1 ORDER BY seq", tenantID); err != nil {
			return err
		}
		snap.Links, err = list(ctx, tx, deref(toLink),
			"SELECT "+linkColumns+" FROM links WHERE tenant_id = $1 ORDER BY seq", tenantID)
		return err
	})
	return snap, err
}

// RevokedIDs 是已撤销的源与链接（个人中转察觉到外部改动时比对用）。
func (d *DB) RevokedIDs(ctx context.Context, tenantID string) (sources, links []string, err error) {
	id := func(s scanner) (string, error) {
		var v string
		return v, s.Scan(&v)
	}
	if sources, err = list(ctx, d.pool, id,
		"SELECT source_id FROM sources WHERE tenant_id = $1 AND revoked_at_ms IS NOT NULL", tenantID); err != nil {
		return nil, nil, err
	}
	links, err = list(ctx, d.pool, id,
		"SELECT link_id FROM links WHERE tenant_id = $1 AND revoked_at_ms IS NOT NULL", tenantID)
	return sources, links, err
}
