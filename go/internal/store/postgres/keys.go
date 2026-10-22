package postgres

import (
	"context"
	"errors"
	"sync"
	"time"

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
	// ticket 是重读的版本号：每次重读开始时取一个新号，读完只有号不小于 applied 才写回，
	// 慢的旧读不会盖掉后发的新读（或本实例轮换后的失效）。
	ticket, applied uint64
	lastRefresh     time.Time
}

// 查不到 kid 时最短隔多久重读一次。
const refreshOnMissGap = 5 * time.Second

// Keys 是某个租户的签名钥（读缓存）。
func (d *DB) Keys(tenantID string) store.KeyStore { return d.keyCache(tenantID) }

// RefreshKeys 让某个租户的签名钥缓存重读（察觉到别的进程 / 节点改了之后调；
// 装配方的存储监视在租户修订号变化时调它，见 store.KeyCacheRefresher）。重读带版本号：
// 并发的旧读不会盖掉后发的新读。
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
		// 失效之前发出的重读读到的是旧数据，写回要被拒。
		cache.ticket++
		cache.applied = cache.ticket
		cache.mu.Unlock()
	}
}

func readSigningKeys(ctx context.Context, q querier, tenantID string) ([]store.SigningKeyRecord, error) {
	return list(ctx, q, func(s scanner) (store.SigningKeyRecord, error) { return toSigningKey(s) },
		"SELECT "+keyColumns+" FROM signing_keys WHERE tenant_id = $1 ORDER BY seq", tenantID)
}

// RefreshOnMiss 实现 store.KeyRefresher：验签查不到 kid 时限频重读。
func (c *keyCache) RefreshOnMiss(ctx context.Context) error {
	c.mu.Lock()
	if c.loaded && time.Since(c.lastRefresh) < c.d.refreshGap {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()
	return c.refresh(ctx)
}

// begin 为一次重读取版本号。
func (c *keyCache) begin() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ticket++
	return c.ticket
}

// apply 把版本号为 ticket 的读结果写回；比已生效的版本旧就丢弃，答是否写回了。
func (c *keyCache) apply(ticket uint64, issuer string, keys []store.SigningKeyRecord) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastRefresh = time.Now()
	if ticket < c.applied {
		return false // 后发的读（或本实例的失效）已经生效，这份是旧的
	}
	c.applied = ticket
	c.issuer, c.keys, c.loaded = issuer, keys, true
	return true
}

func (c *keyCache) refresh(ctx context.Context) error {
	ticket := c.begin()
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
	c.apply(ticket, issuer, keys)
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
