package relayctl

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/jobs"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// JobRevoke 是 relay.revoke（设计 §4.8）：撤销写 Redis 重试 3 次仍失败时兜底，把这枚令牌的撤销再写一次。
// PostgreSQL 里已有这条撤销，所以作业只管 Redis；重放幂等。
const JobRevoke = "relay.revoke"

// JobKick 是 relay.kick：踢人命令发布失败时兜底，把同一条命令再发一次（命令幂等）。
const JobKick = "relay.kick"

// MaxAttempts 是 relay.revoke / relay.kick 的 max_attempts：实际上不设上限（退避封顶 1 小时，约 40 天），
// 撤销与踢人是安全动作，不能因为 Redis 持续故障一小时就判失败；Redis 恢复后下一次重试即送达，另有定时重放（Sync）兜底。
const MaxAttempts = 1000

// retryDelay 是兜底作业的首次到期延后：给正常路径留出时间先做完（成功后会取消作业）。
const retryDelay = 2 * time.Second

type kickPayload struct {
	SourceID      string `json:"sourceId"`
	Subject       string `json:"subject,omitempty"`
	SubjectPrefix string `json:"subjectPrefix,omitempty"`
	DeviceID      string `json:"deviceId,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

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
	runner.Handle(JobKick, func(ctx context.Context, job saas.Job) error {
		var payload kickPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil || payload.SourceID == "" {
			return errors.New("作业载荷不合格式")
		}
		return redis.Kick(ctx, KickFilter{SourceID: payload.SourceID, Subject: payload.Subject, SubjectPrefix: payload.SubjectPrefix, DeviceID: payload.DeviceID}, payload.Reason)
	})
}

// enqueueRevoke 在事务里为一枚令牌插 relay.revoke 作业（按 jti 去重）：与撤销落盘同一个事务，所以 PostgreSQL 有这条撤销
// 就一定有兜底作业；Redis 写成功后由调用方取消，失败则由作业运行器退避重试。
func enqueueRevoke(ctx context.Context, tx saas.Tx, tenantID, jti string, expMs, nowMs int64) error {
	payload, _ := json.Marshal(revokePayload{JTI: jti, ExpMs: expMs})
	_, _, err := tx.Jobs().Enqueue(ctx, tenantID, saas.NewJob{Kind: JobRevoke, Payload: payload, RunAtMs: nowMs + retryDelay.Milliseconds(), DedupeKey: jti, MaxAttempts: MaxAttempts}, nowMs)
	return err
}

// cancelRevokeJobs 取消这些令牌的兜底作业（Redis 已经写成功）。取消失败无碍：作业重放是幂等的。
func cancelRevokeJobs(ctx context.Context, store saas.Store, tenantID string, jtis []string) {
	if len(jtis) == 0 {
		return
	}
	_ = store.Tx(context.WithoutCancel(ctx), func(tx saas.Tx) error {
		for _, jti := range jtis {
			if err := tx.Jobs().Cancel(ctx, tenantID, JobRevoke, jti); err != nil {
				return err
			}
		}
		return nil
	})
}

// enqueueKick 把发布失败的踢人命令排成 relay.kick 作业（按 源|subject|设备 去重）。
func enqueueKick(ctx context.Context, store saas.Store, tenantID string, filter KickFilter, reason string, nowMs int64) error {
	payload, _ := json.Marshal(kickPayload{SourceID: filter.SourceID, Subject: filter.Subject, SubjectPrefix: filter.SubjectPrefix, DeviceID: filter.DeviceID, Reason: reason})
	key := filter.SourceID + "|" + filter.Subject + "|" + filter.SubjectPrefix + "|" + filter.DeviceID
	return store.Tx(context.WithoutCancel(ctx), func(tx saas.Tx) error {
		_, _, err := tx.Jobs().Enqueue(ctx, tenantID, saas.NewJob{Kind: JobKick, Payload: payload, RunAtMs: nowMs + retryDelay.Milliseconds(), DedupeKey: key, MaxAttempts: MaxAttempts}, nowMs)
		return err
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
			_, _, err := tx.Jobs().Enqueue(detached, tenantID, saas.NewJob{Kind: JobRevoke, Payload: payload, RunAtMs: nowMs, DedupeKey: jti, MaxAttempts: MaxAttempts}, nowMs)
			return err
		})
		if err != nil && log != nil {
			log(err)
		}
	}
}
