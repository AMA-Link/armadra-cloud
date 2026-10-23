package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// 作业队列、发件箱与审计的 PostgreSQL 实现（迁移 0008、0009；设计 §6.1）。

// NotifyChannel 是作业入队时 NOTIFY 的频道。
const NotifyChannel = "cloud_jobs"

const (
	retryBase = 30 * time.Second
	retryMax  = time.Hour
)

type jobRepo struct{ q querier }

func (r jobRepo) Enqueue(ctx context.Context, tenantID string, job saas.NewJob, nowMs int64) (int64, bool, error) {
	payload := job.Payload
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	var id int64
	err := r.q.QueryRow(ctx,
		`INSERT INTO jobs (tenant_id, kind, payload, dedupe_key, slot_key, run_at_ms, created_at_ms, max_attempts)
		 VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), $6, $7, COALESCE(NULLIF($8::integer, 0), 8))
		 ON CONFLICT DO NOTHING RETURNING job_id`,
		tenantID, job.Kind, payload, job.DedupeKey, job.SlotKey, job.RunAtMs, nowMs, job.MaxAttempts).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	// 随提交送达，回滚即不发。
	if _, err := r.q.Exec(ctx, "SELECT pg_notify($1, $2)", NotifyChannel, job.Kind); err != nil {
		return 0, false, err
	}
	return id, true, nil
}

func (r jobRepo) Cancel(ctx context.Context, tenantID, kind, dedupeKey string) error {
	_, err := r.q.Exec(ctx,
		`DELETE FROM jobs WHERE tenant_id = $1 AND kind = $2 AND dedupe_key = $3
		 AND done_at_ms IS NULL AND failed_at_ms IS NULL AND locked_by IS NULL`, tenantID, kind, dedupeKey)
	return err
}

func (r jobRepo) InsertOutbox(ctx context.Context, tenantID string, e saas.OutboxEmail) error {
	_, err := r.q.Exec(ctx,
		`INSERT INTO outbox_emails (tenant_id, email_id, job_id, template, locale, to_email, params, message_id, created_at_ms)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		tenantID, e.EmailID, e.JobID, e.Template, e.Locale, e.To, e.Params, e.MessageID, e.CreatedAtMs)
	return err
}

func (r jobRepo) Outbox(ctx context.Context, tenantID, emailID string) (*saas.OutboxEmail, error) {
	var e saas.OutboxEmail
	err := r.q.QueryRow(ctx,
		`SELECT email_id, job_id, template, locale, to_email, params, message_id, created_at_ms, sent_at_ms, failed_at_ms
		 FROM outbox_emails WHERE tenant_id = $1 AND email_id = $2 FOR UPDATE`, tenantID, emailID).
		Scan(&e.EmailID, &e.JobID, &e.Template, &e.Locale, &e.To, &e.Params, &e.MessageID, &e.CreatedAtMs, &e.SentAtMs, &e.FailedAtMs)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &e, err
}

func (r jobRepo) MarkOutboxSent(ctx context.Context, tenantID, emailID string, nowMs int64) error {
	_, err := r.q.Exec(ctx, "UPDATE outbox_emails SET sent_at_ms = $1 WHERE tenant_id = $2 AND email_id = $3", nowMs, tenantID, emailID)
	return err
}

func (r jobRepo) MarkOutboxFailed(ctx context.Context, tenantID, emailID string, nowMs int64) error {
	_, err := r.q.Exec(ctx, "UPDATE outbox_emails SET failed_at_ms = $1 WHERE tenant_id = $2 AND email_id = $3", nowMs, tenantID, emailID)
	return err
}

func (r jobRepo) DeleteOutbox(ctx context.Context, tenantID, emailID string) error {
	_, err := r.q.Exec(ctx, "DELETE FROM outbox_emails WHERE tenant_id = $1 AND email_id = $2", tenantID, emailID)
	return err
}

func (r jobRepo) CancelUnsentMail(ctx context.Context, tenantID, accountID, template string) (int, error) {
	tag, err := r.q.Exec(ctx,
		`DELETE FROM outbox_emails o USING jobs j
		 WHERE o.tenant_id = $1 AND j.tenant_id = $1 AND o.job_id = j.job_id AND j.kind = 'mail.send'
		   AND j.payload->>'accountId' = $2 AND o.template = $3 AND o.sent_at_ms IS NULL AND o.failed_at_ms IS NULL`,
		tenantID, accountID, template)
	return int(tag.RowsAffected()), err
}

// TrimOutbox 不按账号加锁：只在没配 SMTP 时由 mail.send 调用，与别的事务抢发件箱行时若死锁，
// saas.Store.Tx 回滚后重跑（最多 3 次），作业本身也会被重试，可以接受。
func (r jobRepo) TrimOutbox(ctx context.Context, tenantID string, keep int) (int, error) {
	rows, err := r.q.Query(ctx,
		`DELETE FROM outbox_emails WHERE tenant_id = $1 AND email_id IN (
		   SELECT email_id FROM outbox_emails WHERE tenant_id = $1 AND sent_at_ms IS NULL AND failed_at_ms IS NULL
		   ORDER BY created_at_ms DESC, email_id DESC OFFSET $2)
		 RETURNING job_id`, tenantID, keep)
	if err != nil {
		return 0, err
	}
	var jobIDs []int64
	for rows.Next() {
		var id *int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		if id != nil {
			jobIDs = append(jobIDs, *id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	dropped := len(jobIDs)
	if dropped > 0 {
		if _, err := r.q.Exec(ctx,
			"DELETE FROM jobs WHERE tenant_id = $1 AND job_id = ANY($2) AND done_at_ms IS NULL AND locked_by IS NULL", tenantID, jobIDs); err != nil {
			return 0, err
		}
	}
	return dropped, nil
}

func (r jobRepo) GCOutbox(ctx context.Context, tenantID string, beforeMs int64) (int, error) {
	tag, err := r.q.Exec(ctx,
		`DELETE FROM outbox_emails WHERE tenant_id = $1
		 AND (sent_at_ms < $2 OR failed_at_ms < $2)`, tenantID, beforeMs)
	return int(tag.RowsAffected()), err
}

func (r jobRepo) GCJobs(ctx context.Context, tenantID string, doneBeforeMs, failedBeforeMs int64) (int, error) {
	tag, err := r.q.Exec(ctx,
		`DELETE FROM jobs WHERE tenant_id = $1 AND (done_at_ms < $2 OR failed_at_ms < $3)`,
		tenantID, doneBeforeMs, failedBeforeMs)
	return int(tag.RowsAffected()), err
}

type auditRepo struct{ q querier }

func (r auditRepo) Append(ctx context.Context, tenantID string, e saas.AuditEntry) error {
	_, err := r.q.Exec(ctx,
		`INSERT INTO audit_log (at_ms, event_id, tenant_id, team_id, account_id, actor_kind, actor_id, action,
		   target_kind, target_id, source_id, jti, ip_prefix, device_name, reason)
		 VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), $6, NULLIF($7, ''), $8, NULLIF($9, ''), NULLIF($10, ''),
		   NULLIF($11, ''), NULLIF($12, ''), NULLIF($13, ''), NULLIF($14, ''), NULLIF($15, ''))`,
		e.AtMs, e.EventID, tenantID, e.TeamID, e.AccountID, e.ActorKind, e.ActorID, e.Action,
		e.TargetKind, e.TargetID, e.SourceID, e.JTI, e.IPPrefix, e.DeviceName, e.Reason)
	return err
}

func (r auditRepo) SeenLogin(ctx context.Context, tenantID, accountID, deviceName, ipPrefix string, sinceMs int64) (bool, error) {
	var seen bool
	err := r.q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM audit_log WHERE tenant_id = $1 AND account_id = $2
		   AND action IN ('security.login', 'security.register') AND at_ms >= $3
		   AND device_name = $4 AND ip_prefix = $5)`,
		tenantID, accountID, sinceMs, deviceName, ipPrefix).Scan(&seen)
	return seen, err
}

// queue 是作业队列的非事务操作，直接走连接池。
type queue struct{ d *DB }

func (q queue) Claim(ctx context.Context, tenantID, worker string, kinds []string, nowMs, leaseMs int64, limit int) ([]saas.Job, error) {
	return list(ctx, q.d.pool, func(s scanner) (saas.Job, error) {
		var j saas.Job
		err := s.Scan(&j.ID, &j.Kind, &j.Payload, &j.Attempts, &j.MaxAttempts)
		return j, err
	}, `UPDATE jobs SET locked_by = $1, locked_until_ms = $2::bigint + $3::bigint, attempts = attempts + 1
	    WHERE job_id IN (
	      SELECT job_id FROM jobs
	      WHERE tenant_id = $4 AND done_at_ms IS NULL AND failed_at_ms IS NULL AND run_at_ms <= $2
	        AND (locked_until_ms IS NULL OR locked_until_ms < $2)
	        AND attempts < max_attempts AND kind = ANY($6::text[])
	      ORDER BY run_at_ms LIMIT $5 FOR UPDATE SKIP LOCKED)
	    RETURNING job_id, kind, payload, attempts, max_attempts`,
		worker, nowMs, leaseMs, tenantID, limit, kinds)
}

func (q queue) Release(ctx context.Context, tenantID string, jobID int64, worker string, retryAtMs int64) error {
	_, err := q.d.pool.Exec(ctx,
		`UPDATE jobs SET locked_by = NULL, locked_until_ms = NULL, attempts = GREATEST(attempts - 1, 0),
		   run_at_ms = GREATEST(run_at_ms, $4::bigint)
		 WHERE tenant_id = $1 AND job_id = $2 AND locked_by = $3 AND done_at_ms IS NULL AND failed_at_ms IS NULL`,
		tenantID, jobID, worker, retryAtMs)
	return err
}

func (q queue) SweepExhausted(ctx context.Context, tenantID string, nowMs int64) (int, error) {
	tag, err := q.d.pool.Exec(ctx,
		`UPDATE jobs SET failed_at_ms = $1, last_error = 'lease_expired', locked_by = NULL, locked_until_ms = NULL
		 WHERE tenant_id = $2 AND done_at_ms IS NULL AND failed_at_ms IS NULL AND attempts >= max_attempts
		   AND (locked_until_ms IS NULL OR locked_until_ms < $1)`,
		nowMs, tenantID)
	return int(tag.RowsAffected()), err
}

func (q queue) Extend(ctx context.Context, tenantID string, jobID int64, worker string, nowMs, leaseMs int64) (bool, error) {
	tag, err := q.d.pool.Exec(ctx,
		`UPDATE jobs SET locked_until_ms = $1::bigint + $2::bigint
		 WHERE tenant_id = $3 AND job_id = $4 AND locked_by = $5 AND done_at_ms IS NULL AND failed_at_ms IS NULL`,
		nowMs, leaseMs, tenantID, jobID, worker)
	return err == nil && tag.RowsAffected() == 1, err
}

func (q queue) Complete(ctx context.Context, tenantID string, jobID int64, worker string, nowMs int64) (bool, error) {
	tag, err := q.d.pool.Exec(ctx,
		`UPDATE jobs SET done_at_ms = $1, locked_by = NULL, locked_until_ms = NULL
		 WHERE tenant_id = $2 AND job_id = $3 AND locked_by = $4 AND done_at_ms IS NULL AND failed_at_ms IS NULL`,
		nowMs, tenantID, jobID, worker)
	return err == nil && tag.RowsAffected() == 1, err
}

// backoff 是第 attempts 次失败后的等待：30 秒 × 2^(attempts-1)，上限 1 小时。
func backoff(attempts int) time.Duration {
	wait := retryBase
	for i := 1; i < attempts && wait < retryMax; i++ {
		wait *= 2
	}
	return min(wait, retryMax)
}

func (q queue) Fail(ctx context.Context, tenantID string, job saas.Job, worker string, nowMs int64, lastError string) (bool, error) {
	if job.Attempts >= job.MaxAttempts {
		tag, err := q.d.pool.Exec(ctx,
			`UPDATE jobs SET failed_at_ms = $1, last_error = $2, locked_by = NULL, locked_until_ms = NULL
			 WHERE tenant_id = $3 AND job_id = $4 AND locked_by = $5 AND done_at_ms IS NULL AND failed_at_ms IS NULL`,
			nowMs, lastError, tenantID, job.ID, worker)
		return err == nil && tag.RowsAffected() == 1, err
	}
	_, err := q.d.pool.Exec(ctx,
		`UPDATE jobs SET run_at_ms = $1, last_error = $2, locked_by = NULL, locked_until_ms = NULL
		 WHERE tenant_id = $3 AND job_id = $4 AND locked_by = $5 AND done_at_ms IS NULL AND failed_at_ms IS NULL`,
		nowMs+backoff(job.Attempts).Milliseconds(), lastError, tenantID, job.ID, worker)
	return false, err
}

// Listen 占一条专用连接 LISTEN cloud_jobs；断了就退避重连，每次连上也调一次 wake（可能漏了通知）。
func (q queue) Listen(ctx context.Context, wake func()) {
	delay := time.Second
	for ctx.Err() == nil {
		if q.listenOnce(ctx, wake) {
			delay = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, 30*time.Second)
	}
}

// listenOnce 答是否至少连上过一次。退出时先 UNLISTEN * 再放回池；做不到（连接已坏）就关掉这条连接，
// 不让一条还在监听的连接回到池里被别的查询借走。
func (q queue) listenOnce(ctx context.Context, wake func()) (connected bool) {
	conn, err := q.d.pool.Acquire(ctx)
	if err != nil {
		return false
	}
	clean := false
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if clean {
			if _, err := conn.Exec(cleanup, "UNLISTEN *"); err == nil {
				conn.Release()
				return
			}
		}
		_ = conn.Hijack().Close(cleanup)
	}()
	if _, err := conn.Exec(ctx, "LISTEN "+NotifyChannel); err != nil {
		return false
	}
	connected = true
	wake()
	for {
		if _, err := conn.Conn().WaitForNotification(ctx); err != nil {
			clean = ctx.Err() != nil // 只是被叫停：连接还好，UNLISTEN 后放回；其它错误一律关掉
			return connected
		}
		wake()
	}
}
