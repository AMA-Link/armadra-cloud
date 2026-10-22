package jose

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/schema"
)

const (
	// DefaultJWKSTTL 是 JWKS 缓存的有效期。
	DefaultJWKSTTL = 10 * time.Minute
	// DefaultMinRefresh 是两次刷新之间的最短间隔：遇到未知 kid 也不会更勤。
	DefaultMinRefresh = 10 * time.Second
	fetchTimeout      = 10 * time.Second
	maxJWKSBytes      = 1 << 20
)

// JWKSOptions 配置缓存。
type JWKSOptions struct {
	// Fetch 取回 JWKS 的 JSON 文本。
	Fetch      func(ctx context.Context) ([]byte, error)
	TTL        time.Duration
	MinRefresh time.Duration
	// Now 供测试换时钟，缺省 time.Now。
	Now func() time.Time
}

type flight struct {
	done chan struct{}
	err  error
}

// JWKSCache 缓存签发者的公钥集，实现 Keys。
//
//   - 缓存 TTL 10 分钟，过期后下一次查钥时刷新；
//   - 遇到未知 kid 才刷新，且距上一次刷新不足 10 秒时不刷；
//   - 同一时刻只有一个刷新在飞，其余的等它；
//   - 刷新失败继续用旧钥。
type JWKSCache struct {
	fetch      func(ctx context.Context) ([]byte, error)
	ttl        time.Duration
	minRefresh time.Duration
	now        func() time.Time

	mu          sync.Mutex
	keys        map[string]ed25519.PublicKey
	loaded      bool
	loadedAt    time.Time
	attempted   bool
	lastAttempt time.Time
	lastErr     error
	inflight    *flight
}

// NewJWKSCache 建一个缓存，第一次查钥时才去取。
func NewJWKSCache(o JWKSOptions) *JWKSCache {
	c := &JWKSCache{fetch: o.Fetch, ttl: o.TTL, minRefresh: o.MinRefresh, now: o.Now}
	if c.ttl == 0 {
		c.ttl = DefaultJWKSTTL
	}
	if c.minRefresh == 0 {
		c.minRefresh = DefaultMinRefresh
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c
}

// Key 实现 Keys。
func (c *JWKSCache) Key(ctx context.Context, kid string) (ed25519.PublicKey, bool, error) {
	c.mu.Lock()
	now := c.now()
	fresh := c.loaded && now.Sub(c.loadedAt) < c.ttl
	if fresh {
		if key, ok := c.keys[kid]; ok {
			c.mu.Unlock()
			return key, true, nil
		}
	}
	// 过期了或遇到未知 kid：该不该刷新。
	if c.inflight == nil && c.attempted && now.Sub(c.lastAttempt) < c.minRefresh {
		key, ok := c.keys[kid]
		var err error
		if !c.loaded {
			err = c.lastErr
		}
		c.mu.Unlock()
		return key, ok, err
	}
	fl := c.inflight
	if fl == nil {
		fl = &flight{done: make(chan struct{})}
		c.inflight = fl
		c.attempted = true
		c.lastAttempt = now
		go c.refresh(fl)
	}
	c.mu.Unlock()

	select {
	case <-fl.done:
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
	c.mu.Lock()
	key, ok := c.keys[kid]
	loaded := c.loaded
	c.mu.Unlock()
	if ok {
		return key, true, nil
	}
	if fl.err != nil && !loaded {
		return nil, false, fl.err
	}
	return nil, false, nil
}

// refresh 在自己的 goroutine 里取，不受某个调用方取消的影响。
func (c *JWKSCache) refresh(fl *flight) {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	keys, err := c.load(ctx)

	c.mu.Lock()
	if err == nil {
		c.keys = keys
		c.loaded = true
		c.loadedAt = c.now()
		c.lastErr = nil
	} else {
		c.lastErr = err
	}
	c.inflight = nil
	fl.err = err
	c.mu.Unlock()
	close(fl.done)
}

func (c *JWKSCache) load(ctx context.Context) (map[string]ed25519.PublicKey, error) {
	data, err := c.fetch(ctx)
	if err != nil {
		return nil, err
	}
	return ParseJWKS(data)
}

// ParseJWKS 解一份 JWKS，返回 kid 到公钥的表；没有 kid 的钥跳过。
func ParseJWKS(data []byte) (map[string]ed25519.PublicKey, error) {
	var set gen.JWKSet
	if err := schema.Decode(gen.JWKSetSchema, data, &set); err != nil {
		return nil, fmt.Errorf("JWKS 不符合 schema：%w", err)
	}
	keys := make(map[string]ed25519.PublicKey, len(set.Keys))
	for _, jwk := range set.Keys {
		if jwk.Kid == nil || *jwk.Kid == "" {
			continue
		}
		raw, err := decodeB64(jwk.X)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("JWKS 里 %s 的 x 不是 32 字节", *jwk.Kid)
		}
		keys[*jwk.Kid] = ed25519.PublicKey(raw)
	}
	return keys, nil
}

// HTTPFetch 返回从 url 取 JWKS 的函数（GET，要求 200，正文最多 1 MiB）。
func HTTPFetch(client *http.Client, url string) func(ctx context.Context) ([]byte, error) {
	return func(ctx context.Context) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("取 JWKS 得到 HTTP %d", resp.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes+1))
		if err != nil {
			return nil, err
		}
		if len(body) > maxJWKSBytes {
			return nil, fmt.Errorf("JWKS 超过 %d 字节", maxJWKSBytes)
		}
		return body, nil
	}
}
