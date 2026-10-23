package saas

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/AMA-Link/armadra-cloud/go/internal/jose"
)

// 共享 JWKS（Go 设计 §3.6）：缓存在 <前缀>jwks:<sha256(issuer)>，所有节点共用，同一时刻只有拿到锁的
// 节点回源刷新；控制面 keys rotate 后可以直接覆盖这个键。各节点自己还有一层 jose.JWKSCache
// （10 分钟、未知 kid 10 秒最多刷一次），未知 kid 时才会读到这里。
const (
	// jwksShared 是共享副本的有效期：轮换后最迟这么久所有节点都回源一次。
	jwksShared = 60 * time.Second
	// jwksLock 是回源锁的有效期（回源超时之内）。
	jwksLock = 15 * time.Second
	// jwksWait 是没拿到锁的节点等别人写回的时长，过了就自己回源。
	jwksWait = 2 * time.Second
)

// JWKSKey 是共享副本的键：<前缀>jwks:<issuer 的 SHA-256 hex>。
func JWKSKey(prefix, issuer string) string {
	sum := sha256.Sum256([]byte(issuer))
	return prefix + "jwks:" + hex.EncodeToString(sum[:])
}

// sharedJWKS 返回 control 的 JWKS 取法：先读共享副本，没有就抢锁回源并写回。
// 共享副本坏了（不是合法的 JWKS）当作没有。Redis 出错时直接回源（验令牌不依赖 Redis 可用）。
func sharedJWKS(client goredis.UniversalClient, prefix, issuer string, origin func(context.Context) ([]byte, error)) func(context.Context) ([]byte, error) {
	key := JWKSKey(prefix, issuer)
	read := func(ctx context.Context) ([]byte, bool) {
		b, err := client.Get(ctx, key).Bytes()
		if err != nil {
			return nil, false
		}
		if _, err := jose.ParseJWKS(b); err != nil {
			return nil, false
		}
		return b, true
	}
	return func(ctx context.Context) ([]byte, error) {
		if b, ok := read(ctx); ok {
			return b, nil
		}
		locked, err := client.SetNX(ctx, key+":lock", "1", jwksLock).Result()
		if err == nil && !locked {
			deadline := time.Now().Add(jwksWait)
			for time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(100 * time.Millisecond):
				}
				if b, ok := read(ctx); ok {
					return b, nil
				}
			}
		}
		body, ferr := origin(ctx)
		if ferr != nil {
			return nil, ferr
		}
		if _, perr := jose.ParseJWKS(body); perr != nil {
			return nil, errors.New("签发者的 JWKS 不合格式")
		}
		_ = client.Set(ctx, key, body, jwksShared).Err()
		if locked {
			_ = client.Del(ctx, key+":lock").Err()
		}
		return body, nil
	}
}
