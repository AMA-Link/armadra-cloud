package relayctl

import (
	"context"

	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// RevokeToken 撤销一枚没有账号与主机的令牌（节点令牌，nodes revoke）：先写 PostgreSQL 的撤销表，再写 Redis
// （relay:revoked:<jti> + 广播，失败重试 3 次）。PostgreSQL 写成功而 Redis 失败时返回错误：撤销已落盘，
// 同一个事务里排了 relay.revoke 作业，serve 的作业运行器会重试，启动与订阅重新就绪时也会重放进 Redis。
func RevokeToken(ctx context.Context, store saas.Store, tenantID string, redis *Redis, jti string, expMs int64) error {
	nowMs := redis.nowMs()
	err := store.Tx(ctx, func(tx saas.Tx) error {
		if err := tx.Issued().RevokeToken(ctx, jti, expMs); err != nil {
			return err
		}
		// 同一个事务里排兜底作业：控制面的作业运行器会重试，不必等重启或重连。
		return enqueueRevoke(ctx, tx, tenantID, jti, expMs, nowMs)
	})
	if err != nil {
		return err
	}
	if err := redis.revokeToken(ctx, jti, expMs); err != nil {
		return err
	}
	cancelRevokeJobs(ctx, store, tenantID, []string{jti})
	return nil
}
