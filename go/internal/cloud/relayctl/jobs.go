package relayctl

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/jobs"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// JobRevoke 是 relay.revoke（设计 §4.8）：撤销写 Redis 重试 3 次仍失败时兜底，把这枚令牌的撤销再写一次。
// PostgreSQL 里已有这条撤销，所以作业只管 Redis；重放幂等。
const JobRevoke = "relay.revoke"

type revokePayload struct {
	JTI   string `json:"jti"`
	ExpMs int64  `json:"expMs"`
}

// RegisterJobs 注册 relay.revoke。
func RegisterJobs(runner *jobs.Runner, redis *Redis) {
	runner.Handle(JobRevoke, func(ctx context.Context, job saas.Job) error {
		var payload revokePayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil || payload.JTI == "" {
			return errors.New("作业载荷不合格式")
		}
		return redis.ReplayRevoke(ctx, payload.JTI, payload.ExpMs)
	})
}

// RevokeFailureEnqueuer 返回 RedisOptions.RevokeFailed：把失败的撤销排成 relay.revoke 作业（按 jti 去重）。
func RevokeFailureEnqueuer(store saas.Store, tenantID string, now Clock, log func(error)) func(ctx context.Context, jti string, expMs int64) {
	if now == nil {
		now = defaultClock
	}
	return func(ctx context.Context, jti string, expMs int64) {
		payload, _ := json.Marshal(revokePayload{JTI: jti, ExpMs: expMs})
		nowMs := now().UnixMilli()
		// 调用方的 ctx 可能已取消（请求结束）：作业入库用独立的上下文，别因此丢掉兜底。
		detached := context.WithoutCancel(ctx)
		err := store.Tx(detached, func(tx saas.Tx) error {
			_, _, err := tx.Jobs().Enqueue(detached, tenantID, saas.NewJob{Kind: JobRevoke, Payload: payload, RunAtMs: nowMs, DedupeKey: jti}, nowMs)
			return err
		})
		if err != nil && log != nil {
			log(err)
		}
	}
}
