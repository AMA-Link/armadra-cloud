package audit

import (
	"context"
	"errors"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/jobs"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/quota"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// 作业种类（设计 §6.1）：都是每天一次的定时作业，同一天的时间槽只跑一次。
const (
	JobPartitions       = "audit.partitions"
	JobRetention        = "audit.retention"
	JobTrafficRetention = "traffic.retention"
)

const (
	// pruneBatch 是一条 DELETE 最多删的行数：分批删，不一次锁住大批行。
	pruneBatch = 5000
	// teamBatch 是一次取多少个有旧行的团队。
	teamBatch = 100
	// auditKeepMonths 是整月分区的保留月数：超过 13 个月的整月分区直接 DROP。
	auditKeepMonths = 13
)

// Partitions 建与删审计分区（postgres.DB 实现）。
type Partitions interface {
	EnsureAuditPartitions(ctx context.Context, now time.Time) ([]string, error)
	DropAuditPartitionsBefore(ctx context.Context, cutoff time.Time) ([]string, error)
}

// Register 把保留期与分区作业登记进运行器（每天一次）。
func (s *Service) Register(runner *jobs.Runner) {
	if s.partitions != nil {
		runner.Handle(JobPartitions, s.runPartitions)
		runner.Every(JobPartitions, jobs.Daily)
	}
	runner.Handle(JobRetention, s.runRetention)
	runner.Every(JobRetention, jobs.Daily)
	if s.traffic > 0 {
		runner.Handle(JobTrafficRetention, s.runTrafficRetention)
		runner.Every(JobTrafficRetention, jobs.Daily)
	}
}

// runPartitions 是 audit.partitions：补建当月与后两个月的分区（幂等；摘 / 挂兜底分区前设了 lock_timeout，拿不到锁就失败重试）。
func (s *Service) runPartitions(ctx context.Context, _ saas.Job) error {
	_, err := s.partitions.EnsureAuditPartitions(ctx, time.UnixMilli(s.nowMs()))
	return err
}

// runRetention 是 audit.retention：
//   - 团队审计按团队当前的计划删超期的行，一个团队一个团队地删，只动 team_id 相符的行；已清除的团队（teams 表里已没有）
//     审计行按设计保留，不按 free 的 7 天删，只随 13 个月的整月分区 DROP；
//   - 账号安全日志固定 90 天；
//   - 整月都超过 13 个月的分区直接 DROP。
//
// 可重放：每一步都是「删早于界线的行」。
func (s *Service) runRetention(ctx context.Context, _ saas.Job) error {
	now := time.UnixMilli(s.nowMs())
	var errs []error
	if err := s.pruneTeams(ctx, now); err != nil {
		errs = append(errs, err)
	}
	if err := s.loop(ctx, func(tx saas.Tx) (int, error) {
		return tx.Retention().PruneSecurityLog(ctx, s.tenant(), now.Add(-quota.SecurityLogRetention).UnixMilli(), pruneBatch)
	}); err != nil {
		errs = append(errs, err)
	}
	if s.partitions != nil {
		dropped, err := s.partitions.DropAuditPartitionsBefore(ctx, now.AddDate(0, -auditKeepMonths, 0))
		if err != nil {
			errs = append(errs, err)
		} else if len(dropped) > 0 {
			s.log.Info("已删除整月过期的审计分区", obs.Count(len(dropped)))
		}
	}
	return errors.Join(errs...)
}

// pruneTeams 逐团队删超期的审计行：先按最短的保留期找出有旧行的团队，再按各自的计划算界线。
func (s *Service) pruneTeams(ctx context.Context, now time.Time) error {
	scanBefore := now.Add(-quota.ShortestAuditRetention()).UnixMilli()
	after := ""
	for {
		var teams []saas.RetentionTeam
		err := s.store.Tx(ctx, func(tx saas.Tx) (err error) {
			teams, err = tx.Retention().TeamsWithAuditBefore(ctx, s.tenant(), scanBefore, after, teamBatch)
			return err
		})
		if err != nil {
			return err
		}
		for _, team := range teams {
			if err := s.pruneTeam(ctx, now, team); err != nil {
				return err
			}
		}
		if len(teams) < teamBatch {
			return nil
		}
		after = teams[len(teams)-1].TeamID
	}
}

func (s *Service) pruneTeam(ctx context.Context, now time.Time, team saas.RetentionTeam) error {
	before := now.Add(-quota.AuditRetention(team.Plan)).UnixMilli()
	return s.loop(ctx, func(tx saas.Tx) (int, error) {
		return tx.Retention().PruneTeamAudit(ctx, s.tenant(), team.TeamID, before, pruneBatch)
	})
}

// runTrafficRetention 是 traffic.retention：删早于 TRAFFIC_RETENTION_DAYS 的流量桶（节点侧的清理关掉）。
func (s *Service) runTrafficRetention(ctx context.Context, _ saas.Job) error {
	before := time.UnixMilli(s.nowMs()).Add(-s.traffic).UnixMilli()
	return s.loop(ctx, func(tx saas.Tx) (int, error) {
		return tx.Retention().PruneTraffic(ctx, s.tenant(), before, pruneBatch)
	})
}

// loop 一批一批删到删不动为止；租约丢了（ctx 取消）就停手。
func (s *Service) loop(ctx context.Context, prune func(tx saas.Tx) (int, error)) error {
	for {
		var n int
		if err := s.store.Tx(ctx, func(tx saas.Tx) (err error) {
			n, err = prune(tx)
			return err
		}); err != nil {
			return err
		}
		if n < pruneBatch {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}
