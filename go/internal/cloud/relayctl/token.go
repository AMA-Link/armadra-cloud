package relayctl

import (
	"context"

	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// RevokeToken 撤销一枚没有账号与主机的令牌（节点令牌，nodes revoke）：先写 PostgreSQL 的撤销表，再写 Redis
// （relay:revoked:<jti> + 广播，失败重试 3 次）。PostgreSQL 写成功而 Redis 失败时返回错误：撤销已落盘，
// serve 启动与订阅重新就绪时会重放进 Redis。
func RevokeToken(ctx context.Context, store saas.Store, redis *Redis, jti string, expMs int64) error {
	err := store.Tx(ctx, func(tx saas.Tx) error { return tx.Issued().RevokeToken(ctx, jti, expMs) })
	if err != nil {
		return err
	}
	return redis.revokeToken(ctx, jti, expMs)
}
