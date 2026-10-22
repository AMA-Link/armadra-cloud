package postgres

import (
	"context"
	"errors"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// 签名钥：库是异步的而签发在热路径上，所以读缓存——第一次用时读一遍租户的 issuer 与签名钥，
// 之后由调用方在察觉修订号变了（别的进程 / 节点 rotate-key）时 Refresh；本实例的轮换与改租户
// 设置会让缓存失效。私钥以 PKCS#8 base64 存（不包裹），不进日志与响应。

type keyCache struct {
	d        *DB
	tenantID string

	mu     sync.Mutex
	loaded bool
	issuer string
	keys   []store.SigningKeyRecord
}

// Keys 是某个租户的签名钥（读缓存）。
func (d *DB) Keys(tenantID string) store.KeyStore { return d.keyCache(tenantID) }

// RefreshKeys 让某个租户的签名钥缓存重读（察觉到别的进程 / 节点改了之后调）。
func (d *DB) RefreshKeys(ctx context.Context, tenantID string) error {
	return d.keyCache(tenantID).refresh(ctx)
}

func (d *DB) keyCache(tenantID string) *keyCache {
	d.keysMu.Lock()
	defer d.keysMu.Unlock()
	cache, ok := d.keys[tenantID]
	if !ok {
		cache = &keyCache{d: d, tenantID: tenantID}
		d.keys[tenantID] = cache
	}
	return cache
}

func (d *DB) dropKeyCache(tenantID string) {
	d.keysMu.Lock()
	cache := d.keys[tenantID]
	d.keysMu.Unlock()
	if cache != nil {
		cache.mu.Lock()
		cache.loaded = false
		cache.mu.Unlock()
	}
}

func readSigningKeys(ctx context.Context, q querier, tenantID string) ([]store.SigningKeyRecord, error) {
	return list(ctx, q, func(s scanner) (store.SigningKeyRecord, error) { return toSigningKey(s) },
		"SELECT "+keyColumns+" FROM signing_keys WHERE tenant_id = $1 ORDER BY seq", tenantID)
}

func (c *keyCache) refresh(ctx context.Context) error {
	var issuer string
	err := c.d.pool.QueryRow(ctx, "SELECT issuer FROM tenants WHERE tenant_id = $1", c.tenantID).Scan(&issuer)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Errorf(store.KindNotFound, "没有这个租户：%s", c.tenantID)
	}
	if err != nil {
		return err
	}
	keys, err := readSigningKeys(ctx, c.d.pool, c.tenantID)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.issuer, c.keys, c.loaded = issuer, keys, true
	c.mu.Unlock()
	return nil
}

func (c *keyCache) ensure(ctx context.Context) error {
	c.mu.Lock()
	loaded := c.loaded
	c.mu.Unlock()
	if loaded {
		return nil
	}
	return c.refresh(ctx)
}

func (c *keyCache) Issuer(ctx context.Context) (string, error) {
	if err := c.ensure(ctx); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.issuer, nil
}

func (c *keyCache) SigningKeys(ctx context.Context) ([]store.SigningKeyRecord, error) {
	if err := c.ensure(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]store.SigningKeyRecord(nil), c.keys...), nil
}

// AddSigningKey 加一把钥（建租户、导入时用）。
func (d *DB) AddSigningKey(ctx context.Context, tenantID string, key store.SigningKeyRecord) error {
	err := d.write(ctx, tenantID, func(tx pgx.Tx) (bool, error) {
		return true, insertSigningKey(ctx, tx, tenantID, key)
	})
	d.dropKeyCache(tenantID)
	return err
}

// RotateKey 轮换（rotate-key）：追加新钥，其余未退役的定在一小时后退役，早已退役的删掉。答新 kid。
func (d *DB) RotateKey(ctx context.Context, tenantID string, nowMs int64) (kid string, err error) {
	err = d.write(ctx, tenantID, func(tx pgx.Tx) (bool, error) {
		before, err := readSigningKeys(ctx, tx, tenantID)
		if err != nil {
			return false, err
		}
		after, newKid, err := store.RotateSigningKeys(before, nowMs)
		if err != nil {
			return false, err
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
				_, err = tx.Exec(ctx, "DELETE FROM signing_keys WHERE tenant_id = $1 AND kid = $2", tenantID, key.Kid)
			case !sameInt(next.RetiredAtMs, key.RetiredAtMs):
				_, err = tx.Exec(ctx, "UPDATE signing_keys SET retired_at_ms = $1 WHERE tenant_id = $2 AND kid = $3",
					next.RetiredAtMs, tenantID, key.Kid)
			}
			if err != nil {
				return false, err
			}
		}
		for _, key := range after {
			if !known[key.Kid] {
				if err := insertSigningKey(ctx, tx, tenantID, key); err != nil {
					return false, err
				}
			}
		}
		kid = newKid
		return true, nil
	})
	d.dropKeyCache(tenantID)
	return kid, err
}

func sameInt(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
