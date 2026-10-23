package relayctl

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/quota"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// ErrSyncing 是重放进行中时 /ready 的答复（设计 §7.2：重放完成前不对外答就绪）。
var ErrSyncing = errors.New("正在把撤销、签发索引与团队额度重放进 Redis")

// Sync 把 PostgreSQL 里不能丢的状态重写进 Redis（设计 §7.2）：
//
//   - 撤销表：未到期的 relay_revoked_tokens 全部 SET … PXAT … NX；
//   - 签发索引：cloud_issued_tokens（WHERE tenant_id = $1 AND exp_ms > now）重建 cloud:issued:*；
//   - 团队额度：按 quota.Plans 加各团队的 overrides 把全部 relay:quota:team:* 重写一遍。
//
// serve 启动时与 Redis 订阅每次重新就绪时（Plane 的就绪回调）各跑一次；全部幂等，所以 Redis 被清空、
// 滚动重启、AOF 丢了最后一秒都靠它恢复。重放期间 Ready 答 ErrSyncing。
type Sync struct {
	Store  saas.Store
	Tenant string
	Redis  *Redis
	Now    Clock
	Log    *obs.Logger

	syncing atomic.Int32
}

func (s *Sync) nowMs() int64 {
	if s.Now == nil {
		return defaultClock().UnixMilli()
	}
	return s.Now().UnixMilli()
}

// Ready 是 /ready 的探针：重放进行中答 ErrSyncing。
func (s *Sync) Ready(context.Context) error {
	if s.syncing.Load() > 0 {
		return ErrSyncing
	}
	return nil
}

// Run 重放一次；任何一步失败都报错（作为 Plane 的就绪回调时，这次订阅就绪作废、退避重连后再来）。
func (s *Sync) Run(ctx context.Context) error {
	s.syncing.Add(1)
	defer s.syncing.Add(-1)
	nowMs := s.nowMs()
	var (
		revoked []Revoked
		issued  []Issued
		quotas  = TeamQuotas{}
	)
	err := s.Store.Tx(ctx, func(tx saas.Tx) error {
		revoked, issued = nil, nil
		rows, err := tx.Issued().Revoked(ctx, nowMs)
		if err != nil {
			return err
		}
		for _, row := range rows {
			revoked = append(revoked, Revoked{JTI: row.JTI, ExpMs: row.ExpMs})
		}
		active, err := tx.Issued().Active(ctx, s.Tenant, nowMs)
		if err != nil {
			return err
		}
		for _, row := range active {
			issued = append(issued, Issued{Subject: row.AccountID, SourceID: row.SourceID, DeviceID: row.DeviceID, JTI: row.JTI, ExpMs: row.ExpMs})
		}
		teams, err := tx.TeamEntitlements(ctx, s.Tenant)
		if err != nil {
			return err
		}
		for _, team := range teams {
			quotas[team.TeamID] = Quota{Concurrent: quota.ForTeam(team.Plan, team.Overrides).Concurrent}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := s.Redis.Restore(ctx, revoked, issued); err != nil {
		return err
	}
	if err := s.Redis.WriteQuotas(ctx, quotas); err != nil {
		return err
	}
	if s.Log != nil {
		s.Log.Info("已把撤销、签发索引与团队额度重放进 Redis",
			obs.Operation("relayctl.sync"), obs.Count(len(revoked)+len(issued)+len(quotas)))
	}
	return nil
}
