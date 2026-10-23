package postgres_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

func TestSaaSEmailTokenLifecycle(t *testing.T) {
	e := openSaaS(t)
	ctx, s := e.ctx, e.db.SaaS()
	e.account("a1")
	e.must("UPDATE accounts SET email = 'a1@example.com' WHERE account_id = 'a1'")

	tx := func(fn func(tx saas.Tx) error) {
		t.Helper()
		if err := s.Tx(ctx, fn); err != nil {
			t.Fatal(err)
		}
	}
	token := func(hash, purpose string, expires int64) saas.EmailToken {
		return saas.EmailToken{TokenHash: hash, AccountID: "a1", Purpose: purpose, Email: "A1@Example.com", CreatedAtMs: 100, ExpiresAtMs: expires}
	}
	tx(func(tx saas.Tx) error {
		for _, tk := range []saas.EmailToken{token("h-verify", "verify", 1000), token("h-reset", "reset", 1000), token("h-old", "verify", 50)} {
			if err := tx.Accounts().InsertEmailToken(ctx, saasTenant, tk); err != nil {
				return err
			}
		}
		return nil
	})
	got, err := s.Read().EmailToken(ctx, saasTenant, "h-verify")
	if err != nil || got == nil || got.Purpose != "verify" || got.Email != "a1@example.com" || got.ConsumedAtMs != nil {
		t.Fatalf("读令牌（邮箱转小写）：%+v %v", got, err)
	}
	if none, _ := s.Read().EmailToken(ctx, "default", "h-verify"); none != nil {
		t.Fatal("别的租户读不到")
	}

	// 只消耗一次；过期的、目的不符的消耗不了；消耗时一并答账号与地址。
	var first, second, expired, wrongPurpose *saas.EmailToken
	tx(func(tx saas.Tx) (err error) {
		wrongPurpose, err = tx.Accounts().ConsumeEmailToken(ctx, saasTenant, "h-reset", "verify", 500)
		if err != nil {
			return err
		}
		first, err = tx.Accounts().ConsumeEmailToken(ctx, saasTenant, "h-verify", "verify", 500)
		if err != nil {
			return err
		}
		second, err = tx.Accounts().ConsumeEmailToken(ctx, saasTenant, "h-verify", "verify", 501)
		if err != nil {
			return err
		}
		expired, err = tx.Accounts().ConsumeEmailToken(ctx, saasTenant, "h-old", "verify", 500)
		return err
	})
	if first == nil || first.AccountID != "a1" || first.Email != "a1@example.com" || first.Purpose != "verify" || second != nil || expired != nil {
		t.Fatalf("一次性与过期：%+v %v %v", first, second, expired)
	}
	if wrongPurpose != nil {
		t.Fatal("目的不符的令牌不能被消耗")
	}
	if still, _ := s.Read().EmailToken(ctx, saasTenant, "h-reset"); still.ConsumedAtMs != nil {
		t.Fatal("目的不符不改令牌")
	}

	// 作废同目的的未用令牌；别的目的不动。
	var n int
	tx(func(tx saas.Tx) (err error) {
		if _, err = tx.Accounts().InsertEmailToken(ctx, saasTenant, token("h-verify2", "verify", 1000)), nil; err != nil {
			return err
		}
		n, err = tx.Accounts().InvalidateEmailTokens(ctx, saasTenant, "a1", "verify", 600)
		return err
	})
	// h-old 也是未用的 verify：与 h-verify2 一起被作废（过期不影响作废）。
	if n != 2 {
		t.Fatalf("作废了几枚：%d", n)
	}
	reset, _ := s.Read().EmailToken(ctx, saasTenant, "h-reset")
	if reset.ConsumedAtMs != nil {
		t.Fatal("别的目的不该被作废")
	}

	// GC：过期早于 beforeMs 的删掉。
	tx(func(tx saas.Tx) (err error) {
		n, err = tx.Accounts().GCEmailTokens(ctx, saasTenant, 100)
		return err
	})
	if n != 1 || e.count("SELECT count(*) FROM email_tokens") != 3 {
		t.Fatalf("GC：%d", n)
	}

	// 验证与改邮箱。
	tx(func(tx saas.Tx) error { return tx.Accounts().SetEmailVerified(ctx, saasTenant, "a1", 700) })
	tx(func(tx saas.Tx) error { return tx.Accounts().SetEmailVerified(ctx, saasTenant, "a1", 900) })
	account, _ := s.Read().Account(ctx, saasTenant, "a1")
	if account.EmailVerifiedAtMs == nil || *account.EmailVerifiedAtMs != 700 {
		t.Fatalf("已验证的保持原时刻：%v", account.EmailVerifiedAtMs)
	}
	e.account("a2")
	e.must("UPDATE accounts SET email = 'a2@example.com' WHERE account_id = 'a2'")
	err = s.Tx(ctx, func(tx saas.Tx) error {
		return tx.Accounts().ChangeEmail(ctx, saasTenant, "a1", "A2@example.com", 1000)
	})
	if !isCode(err, saas.ErrEmailTaken) {
		t.Fatalf("新地址被占用：%v", err)
	}
	tx(func(tx saas.Tx) error {
		return tx.Accounts().ChangeEmail(ctx, saasTenant, "a1", "New@Example.com", 1000)
	})
	account, _ = s.Read().Account(ctx, saasTenant, "a1")
	if account.Email != "new@example.com" || *account.EmailVerifiedAtMs != 1000 {
		t.Fatalf("改邮箱：%+v", account)
	}
}

func isCode(err error, target *saas.Error) bool {
	e, ok := err.(*saas.Error)
	return ok && e.Code == target.Code
}

func TestSaaSOutboxAndJobGC(t *testing.T) {
	e := openSaaS(t)
	ctx, s := e.ctx, e.db.SaaS()
	var jobID int64
	if err := s.Tx(ctx, func(tx saas.Tx) error {
		var err error
		jobID, _, err = tx.Jobs().Enqueue(ctx, saasTenant, saas.NewJob{Kind: "mail.send", RunAtMs: 1}, 1)
		if err != nil {
			return err
		}
		for _, id := range []string{"m1", "m2", "m3"} {
			if err := tx.Jobs().InsertOutbox(ctx, saasTenant, saas.OutboxEmail{
				EmailID: id, JobID: &jobID, Template: "verify_email", Locale: "zh-CN", To: strings.ToLower("To-" + id + "@Example.com"),
				Params: json.RawMessage(`{"displayName":"Ann"}`), MessageID: id + "@app.test", CreatedAtMs: 10,
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx saas.Tx) error {
		if err := tx.Jobs().MarkOutboxSent(ctx, saasTenant, "m1", 100); err != nil {
			return err
		}
		if err := tx.Jobs().MarkOutboxFailed(ctx, saasTenant, "m2", 200); err != nil {
			return err
		}
		got, err := tx.Jobs().Outbox(ctx, saasTenant, "m1")
		if err != nil || got == nil || got.SentAtMs == nil || *got.SentAtMs != 100 || string(got.Params) != `{"displayName": "Ann"}` || got.To != "to-m1@example.com" {
			t.Errorf("读发件箱：%+v %v", got, err)
		}
		if none, _ := tx.Jobs().Outbox(ctx, saasTenant, "nope"); none != nil {
			t.Error("不存在答 nil")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// outbox.gc：发出或失败早于界线的删；没发的不动。
	var n int
	if err := s.Tx(ctx, func(tx saas.Tx) (err error) {
		n, err = tx.Jobs().GCOutbox(ctx, saasTenant, 150)
		return err
	}); err != nil || n != 1 {
		t.Fatalf("GCOutbox：%d %v", n, err)
	}
	if err := s.Tx(ctx, func(tx saas.Tx) (err error) {
		n, err = tx.Jobs().GCOutbox(ctx, saasTenant, 300)
		return err
	}); err != nil || n != 1 || e.count("SELECT count(*) FROM outbox_emails") != 1 {
		t.Fatalf("GCOutbox 第二次：%d %v", n, err)
	}

	// jobs.gc：完成满界线的作业删掉，发件箱的 job_id 置空。
	e.must("UPDATE jobs SET done_at_ms = 100 WHERE job_id = $1", jobID)
	if err := s.Tx(ctx, func(tx saas.Tx) (err error) {
		n, err = tx.Jobs().GCJobs(ctx, saasTenant, 50, 50)
		return err
	}); err != nil || n != 0 {
		t.Fatalf("未到界线不删：%d %v", n, err)
	}
	if err := s.Tx(ctx, func(tx saas.Tx) (err error) {
		n, err = tx.Jobs().GCJobs(ctx, saasTenant, 150, 50)
		return err
	}); err != nil || n != 1 {
		t.Fatalf("GCJobs：%d %v", n, err)
	}
	if e.count("SELECT count(*) FROM outbox_emails WHERE job_id IS NULL") != 1 {
		t.Fatal("删作业后发件箱的 job_id 置空")
	}
}

func TestSaaSAuditAppend(t *testing.T) {
	e := openSaaS(t)
	ctx, s := e.ctx, e.db.SaaS()
	if _, err := e.db.EnsureAuditPartitions(ctx, timeAt(2026, 10, 23)); err != nil {
		t.Fatal(err)
	}
	at := timeAt(2026, 10, 23).UnixMilli()
	if err := s.Tx(ctx, func(tx saas.Tx) error {
		return tx.Audit().Append(ctx, saasTenant, saas.AuditEntry{
			AtMs: at, EventID: "ev1", AccountID: "a1", ActorKind: saas.ActorAccount, ActorID: "a1", Action: "security.login",
			IPPrefix: "10.1.2.0/24", DeviceName: "笔记本", SourceID: "src1",
		})
	}); err != nil {
		t.Fatal(err)
	}
	var team, device, source *string
	if err := e.pool.QueryRow(ctx, "SELECT team_id, device_name, source_id FROM audit_log WHERE event_id = 'ev1'").Scan(&team, &device, &source); err != nil {
		t.Fatal(err)
	}
	if team != nil || device == nil || *device != "笔记本" || source == nil || *source != "src1" {
		t.Fatalf("空串写成 NULL，设备名快照与 source_id 落库：%v %v %v", team, device, source)
	}
	// 设备名超过 128 字符被库拒绝（audit 包写入前会截断）。
	err := s.Tx(ctx, func(tx saas.Tx) error {
		return tx.Audit().Append(ctx, saasTenant, saas.AuditEntry{AtMs: at, EventID: "ev2", ActorKind: saas.ActorSystem, Action: "x", DeviceName: strings.Repeat("a", 129)})
	})
	if err == nil {
		t.Fatal("设备名过长应拒绝")
	}
	// 没有对应月份分区的时刻落进兜底分区，不报错（启动与每日作业负责补分区）。
	if err := s.Tx(ctx, func(tx saas.Tx) error {
		return tx.Audit().Append(ctx, saasTenant, saas.AuditEntry{AtMs: timeAt(2030, 1, 1).UnixMilli(), EventID: "ev3", ActorKind: saas.ActorSystem, Action: "x"})
	}); err != nil {
		t.Fatal(err)
	}
}

func timeAt(year, month, day int) time.Time {
	return time.Date(year, time.Month(month), day, 12, 0, 0, 0, time.UTC)
}

func TestSaaSCancelUnsentMailAndSeenLogin(t *testing.T) {
	e := openSaaS(t)
	ctx, s := e.ctx, e.db.SaaS()
	if _, err := e.db.EnsureAuditPartitions(ctx, timeAt(2026, 10, 23)); err != nil {
		t.Fatal(err)
	}
	enqueue := func(tx saas.Tx, id, account, template string) {
		t.Helper()
		jobID, _, err := tx.Jobs().Enqueue(ctx, saasTenant, saas.NewJob{Kind: "mail.send", Payload: json.RawMessage(`{"emailId":"` + id + `","accountId":"` + account + `"}`), RunAtMs: 1}, 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Jobs().InsertOutbox(ctx, saasTenant, saas.OutboxEmail{
			EmailID: id, JobID: &jobID, Template: template, Locale: "en", To: id + "@example.com",
			Params: json.RawMessage(`{}`), MessageID: id + "@app.test", CreatedAtMs: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := s.Tx(ctx, func(tx saas.Tx) error {
		enqueue(tx, "c1", "a1", "email_change")
		enqueue(tx, "c2", "a1", "password_reset")
		enqueue(tx, "c3", "a2", "email_change")
		enqueue(tx, "c4", "a1", "email_change")
		if err := tx.Jobs().MarkOutboxSent(ctx, saasTenant, "c4", 5); err != nil {
			return err
		}
		var err error
		n, err = tx.Jobs().CancelUnsentMail(ctx, saasTenant, "a1", "email_change")
		return err
	}); err != nil || n != 1 {
		t.Fatalf("只删该账号该模板还没发的：%d %v", n, err)
	}
	if e.count("SELECT count(*) FROM outbox_emails") != 3 {
		t.Fatal("别的账号、别的模板、已发的都留着")
	}

	at := timeAt(2026, 10, 23).UnixMilli()
	if err := s.Tx(ctx, func(tx saas.Tx) error {
		return tx.Audit().Append(ctx, saasTenant, saas.AuditEntry{AtMs: at, EventID: "l1", AccountID: "a1", ActorKind: saas.ActorAccount, ActorID: "a1", Action: "security.login", DeviceName: "笔记本", IPPrefix: "10.1.2.0/24"})
	}); err != nil {
		t.Fatal(err)
	}
	seen := func(name, prefix string, since int64) bool {
		t.Helper()
		var got bool
		if err := s.Tx(ctx, func(tx saas.Tx) (err error) {
			got, err = tx.Audit().SeenLogin(ctx, saasTenant, "a1", name, prefix, since)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if !seen("笔记本", "10.1.2.0/24", at-1000) || seen("笔记本", "10.1.3.0/24", at-1000) || seen("台式机", "10.1.2.0/24", at-1000) || seen("笔记本", "10.1.2.0/24", at+1) {
		t.Fatal("SeenLogin 要同名、同前缀、在窗口内")
	}
}
