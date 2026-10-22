package sqlite

import (
	"context"
	"database/sql"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// 签名钥：每次都读库（表只有几行），别的进程 rotate-key 之后立即可见；轮换在一个写事务里。
// 私钥以 PKCS#8 base64 存、不包裹，靠库文件 0600 保护，不进日志与响应。

type keys struct {
	d        *DB
	tenantID string
}

// Keys 是某个租户的签名钥。
func (d *DB) Keys(tenantID string) store.KeyStore { return keys{d, tenantID} }

func readSigningKeys(ctx context.Context, q *sql.Conn, tenantID string) ([]store.SigningKeyRecord, error) {
	return collect(ctx, q, "SELECT "+keyColumns+" FROM signing_keys WHERE tenant_id = ? ORDER BY rowid", tenantID,
		func(s scanner) (store.SigningKeyRecord, error) { return toSigningKey(s) })
}

func (k keys) Issuer(ctx context.Context) (issuer string, err error) {
	err = k.d.read(ctx, func(q *sql.Conn) error {
		err := q.QueryRowContext(ctx, "SELECT issuer FROM tenants WHERE tenant_id = ?", k.tenantID).Scan(&issuer)
		if err == sql.ErrNoRows {
			return store.ErrNoTenant
		}
		return err
	})
	return issuer, err
}

func (k keys) SigningKeys(ctx context.Context) (out []store.SigningKeyRecord, err error) {
	err = k.d.read(ctx, func(q *sql.Conn) (err error) {
		out, err = readSigningKeys(ctx, q, k.tenantID)
		return err
	})
	return out, err
}

// AddSigningKey 加一把钥（建租户、导入时用）。
func (d *DB) AddSigningKey(ctx context.Context, tenantID string, key store.SigningKeyRecord) error {
	return d.write(ctx, func(q *sql.Conn) error {
		if err := insertSigningKey(ctx, q, tenantID, key); err != nil {
			return err
		}
		return bumpRevision(ctx, q, tenantID)
	})
}

// RotateKey 轮换（rotate-key）：追加新钥，其余未退役的定在一小时后退役，早已退役的删掉。答新 kid。
func (d *DB) RotateKey(ctx context.Context, tenantID string, nowMs int64) (kid string, err error) {
	err = d.write(ctx, func(q *sql.Conn) error {
		before, err := readSigningKeys(ctx, q, tenantID)
		if err != nil {
			return err
		}
		after, newKid, err := store.RotateSigningKeys(before, nowMs)
		if err != nil {
			return err
		}
		kept := map[string]store.SigningKeyRecord{}
		for _, key := range after {
			kept[key.Kid] = key
		}
		known := map[string]bool{}
		for _, key := range before {
			known[key.Kid] = true
			next, ok := kept[key.Kid]
			switch {
			case !ok:
				_, err = q.ExecContext(ctx, "DELETE FROM signing_keys WHERE tenant_id = ? AND kid = ?", tenantID, key.Kid)
			case !sameInt(next.RetiredAtMs, key.RetiredAtMs):
				_, err = q.ExecContext(ctx, "UPDATE signing_keys SET retired_at_ms = ? WHERE tenant_id = ? AND kid = ?",
					nullable(next.RetiredAtMs), tenantID, key.Kid)
			}
			if err != nil {
				return err
			}
		}
		for _, key := range after {
			if !known[key.Kid] {
				if err := insertSigningKey(ctx, q, tenantID, key); err != nil {
					return err
				}
			}
		}
		kid = newKid
		return bumpRevision(ctx, q, tenantID)
	})
	return kid, err
}

func sameInt(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
