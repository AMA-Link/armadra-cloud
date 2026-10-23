package teams

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/audit"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/jobs"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// 作业种类（设计 §6.1）。
const (
	// JobPurge 清除一个团队（run_at = purge_at_ms，dedupe = teamId）。
	JobPurge = "team.purge"
	// JobPurgeDue 每小时扫一遍已到清除时刻却没有作业的团队，补插 JobPurge（作业被去重挡掉或丢了的兜底）。
	JobPurgeDue = "team.purgeDue"

	// purgeBatch 是一次扫描最多补插的团队数。
	purgeBatch = 100
)

type purgePayload struct {
	TeamID string `json:"teamId"`
}

// enqueuePurge 插清除作业：dedupe_key = teamId，同一团队只有一个未完成的清除作业。
func enqueuePurge(ctx context.Context, tx saas.Tx, tenantID, teamID string, runAtMs, nowMs int64) error {
	payload, err := json.Marshal(purgePayload{TeamID: teamID})
	if err != nil {
		return err
	}
	_, _, err = tx.Jobs().Enqueue(ctx, tenantID, saas.NewJob{Kind: JobPurge, Payload: payload, RunAtMs: runAtMs, DedupeKey: teamID}, nowMs)
	return err
}

// Register 把清除作业的处理器登记进运行器。
func (s *Service) Register(runner *jobs.Runner) {
	runner.Handle(JobPurge, s.purge)
	runner.Handle(JobPurgeDue, s.purgeDue)
	runner.Every(JobPurgeDue, jobs.Hourly)
}

// purge 是 team.purge：恢复期已满才动手（先锁团队行再读，恢复与清除互斥）；撤销团队主机、删授予、邀请、
// 链接、待信任、注册令牌、source_spaces、entitlements，最后删团队行（成员级联）；审计行保留。可重放：团队
// 已经清走就直接结束。
func (s *Service) purge(ctx context.Context, job saas.Job) error {
	var payload purgePayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil || payload.TeamID == "" {
		return errors.New("作业载荷不合格式")
	}
	return s.store.Tx(ctx, func(tx saas.Tx) error {
		found, err := tx.Teams().LockTeam(ctx, s.tenant(), payload.TeamID)
		if err != nil || !found {
			return err
		}
		nowMs := s.nowMs()
		purged, sources, err := tx.Teams().Purge(ctx, s.tenant(), payload.TeamID, nowMs)
		if err != nil || !purged {
			return err
		}
		s.afterRevokeHosts(tx, sources, "team_purged", true)
		teamID := payload.TeamID
		tx.AfterCommit(func(ctx context.Context) {
			if err := s.relay.ClearTeamQuota(ctx, teamID); err != nil {
				s.log.Warn("删团队并发额度失败", obs.Operation("teams.quota"), obs.Error(err))
			}
		})
		return audit.Write(ctx, tx, s.tenant(), nowMs, saas.AuditEntry{
			TeamID: teamID, ActorKind: saas.ActorSystem, Action: ActionPurge, TargetKind: "team", TargetID: teamID,
		})
	})
}

// purgeDue 是 team.purgeDue：给已到清除时刻的团队补插 team.purge。
func (s *Service) purgeDue(ctx context.Context, _ saas.Job) error {
	nowMs := s.nowMs()
	due, err := s.store.Read().DueTeams(ctx, s.tenant(), nowMs, purgeBatch)
	if err != nil {
		return err
	}
	return s.store.Tx(ctx, func(tx saas.Tx) error {
		for _, teamID := range due {
			if err := enqueuePurge(ctx, tx, s.tenant(), teamID, nowMs, nowMs); err != nil {
				return err
			}
		}
		return nil
	})
}
