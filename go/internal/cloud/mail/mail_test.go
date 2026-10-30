package mail_test

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/jobs"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/mail"
	"github.com/AMA-Link/armadra-cloud/go/internal/identity"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/pgtest"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/postgres"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

const (
	tenant    = "cloud"
	appOrigin = "https://app.example.test"
)

type clock struct{ at atomic.Int64 }

func (c *clock) Now() time.Time          { return time.UnixMilli(c.at.Load()) }
func (c *clock) Advance(d time.Duration) { c.at.Add(d.Milliseconds()) }

type env struct {
	t      *testing.T
	ctx    context.Context
	db     *postgres.DB
	store  saas.Store
	clock  *clock
	sender *mail.MemorySender
	mail   *mail.Service
	runner *jobs.Runner
}

func open(t *testing.T) *env { return openWith(t, nil, 0) }

// openWith 可换发信方与没配 SMTP 时的积压上限（sender 为 nil 用内存发信方）。
func openWith(t *testing.T, custom mail.Sender, backlog int) *env {
	t.Helper()
	url, schema := pgtest.Schema(t)
	ctx := context.Background()
	db, err := postgres.Open(ctx, postgres.Options{URL: url, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.CreateTenant(ctx, tenant, store.Tenant{TenantSettings: store.TenantSettings{Issuer: appOrigin, TrustedOrigins: []string{}}}); err != nil {
		t.Fatal(err)
	}
	clk := &clock{}
	clk.at.Store(time.Now().Truncate(time.Second).UnixMilli())
	e := &env{t: t, ctx: ctx, db: db, store: db.SaaS(), clock: clk, sender: &mail.MemorySender{}}
	var sender mail.Sender = e.sender
	if custom != nil {
		sender = custom
	}
	e.mail, err = mail.New(mail.Options{Store: e.store, TenantID: tenant, Sender: sender, AppOrigin: appOrigin, Now: clk.Now, BacklogCap: backlog})
	if err != nil {
		t.Fatal(err)
	}
	e.runner = jobs.New(jobs.Options{Store: e.store, TenantID: tenant, Workers: 2, Now: clk.Now, Poll: 20 * time.Millisecond})
	e.mail.Register(e.runner)
	e.runner.Start()
	t.Cleanup(func() { e.runner.Stop(5 * time.Second) })
	return e
}

func (e *env) account(id, email string) {
	e.t.Helper()
	nowMs := e.clock.Now().UnixMilli()
	hashed, err := identity.HashPassword("plover-quartz-lantern-41")
	if err != nil {
		e.t.Fatal(err)
	}
	err = e.store.Tx(e.ctx, func(tx saas.Tx) error {
		return tx.Accounts().Create(e.ctx, tenant, saas.Account{
			AccountRecord: store.AccountRecord{AccountID: id, Name: id, Password: hashed, CreatedAtMs: nowMs, UpdatedAtMs: nowMs},
			Email:         email, DisplayName: "Ann", Locale: "en", Status: saas.AccountActive,
		})
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) enqueue(m mail.Mail) {
	e.t.Helper()
	if err := e.store.Tx(e.ctx, func(tx saas.Tx) error { return e.mail.Enqueue(e.ctx, tx, m) }); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) waitMessages(n int) []mail.Message {
	e.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if got := e.sender.Messages(); len(got) >= n {
			return got
		}
	}
	e.t.Fatalf("等不到第 %d 封邮件", n)
	return nil
}

func (e *env) eventually(what string, ok func() bool) {
	e.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	e.t.Fatalf("超时：%s", what)
}

func (e *env) count(sql string, args ...any) (n int) {
	e.t.Helper()
	if err := e.db.Pool().QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return
}

// metric 取 RenderMetrics 里某个计数器的值（没有这一行答 -1）。
func (e *env) metric(name string) int {
	e.t.Helper()
	for _, line := range strings.Split(e.mail.RenderMetrics(), "\n") {
		if value, ok := strings.CutPrefix(line, name+" "); ok {
			n, err := strconv.Atoi(value)
			if err != nil {
				e.t.Fatalf("%s 的值：%q", name, value)
			}
			return n
		}
	}
	return -1
}

// expectMetrics 断言四个发信计数器。
func (e *env) expectMetrics(sent, failures, abandoned, dropped int) {
	e.t.Helper()
	for name, want := range map[string]int{
		"armadra_cloud_mail_sent_total": sent, "armadra_cloud_mail_send_failures_total": failures,
		"armadra_cloud_mail_abandoned_total": abandoned, "armadra_cloud_mail_backlog_dropped_total": dropped,
	} {
		if got := e.metric(name); got != want {
			e.t.Fatalf("%s：%d，要 %d\n%s", name, got, want, e.mail.RenderMetrics())
		}
	}
}

func fragment(t *testing.T, m mail.Message, path string) string {
	t.Helper()
	u, err := url.Parse(strings.Fields(m.Text[strings.Index(m.Text, "http"):])[0])
	if err != nil || u.Path != path || u.Fragment == "" || u.RawQuery != "" || u.Scheme+"://"+u.Host != appOrigin {
		t.Fatalf("链接要是 %s%s#令牌：%q", appOrigin, path, m.Text)
	}
	return u.Fragment
}

func TestVerifyEmailTokenIsHashedAndOnlyInFragment(t *testing.T) {
	e := open(t)
	e.account("a1", "ann@example.test")
	e.enqueue(mail.Mail{Template: mail.VerifyEmail, Locale: mail.LocaleEN, To: "ann@example.test", AccountID: "a1", Params: mail.Params{DisplayName: "Ann"}})
	message := e.waitMessages(1)[0]
	if message.Subject != "Verify your email" || message.To != "ann@example.test" || !strings.HasSuffix(message.MessageID, "@app.example.test") {
		t.Fatalf("邮件：%+v", message)
	}
	token := fragment(t, message, "/verify")
	record, err := e.store.Read().EmailToken(e.ctx, tenant, mail.HashToken(token))
	if err != nil || record == nil || record.AccountID != "a1" || record.Purpose != saas.EmailPurposeVerify || record.Email != "ann@example.test" {
		t.Fatalf("库里的令牌行：%+v %v", record, err)
	}
	if got := record.ExpiresAtMs - record.CreatedAtMs; got != mail.VerifyTTL.Milliseconds() {
		t.Fatalf("验证令牌 24 小时：%d", got)
	}
	// 库里的任何一张表都没有令牌明文。
	for _, table := range []string{"email_tokens", "jobs", "outbox_emails"} {
		var text *string
		if err := e.db.Pool().QueryRow(e.ctx, `SELECT string_agg(t::text, E'\n') FROM `+table+` t`).Scan(&text); err != nil {
			t.Fatal(err)
		}
		if text != nil && strings.Contains(*text, token) {
			t.Fatalf("%s 里出现了令牌明文", table)
		}
	}
	e.eventually("发件箱记已发", func() bool {
		return e.count("SELECT count(*) FROM outbox_emails WHERE sent_at_ms IS NOT NULL") == 1
	})
}

func TestTokenTTLsPerPurpose(t *testing.T) {
	e := open(t)
	e.account("a1", "ann@example.test")
	e.enqueue(mail.Mail{Template: mail.PasswordReset, Locale: mail.LocaleZH, To: "ann@example.test", AccountID: "a1"})
	e.enqueue(mail.Mail{Template: mail.EmailChange, Locale: mail.LocaleZH, To: "new@example.test", AccountID: "a1"})
	e.waitMessages(2)
	ttl := func(purpose string) (ms int64) {
		if err := e.db.Pool().QueryRow(e.ctx, "SELECT expires_at_ms - created_at_ms FROM email_tokens WHERE purpose = $1", purpose).Scan(&ms); err != nil {
			t.Fatal(err)
		}
		return
	}
	if ttl("reset") != (30*time.Minute).Milliseconds() || ttl("change") != (24*time.Hour).Milliseconds() {
		t.Fatalf("重置 30 分钟、改邮箱 24 小时：%d %d", ttl("reset"), ttl("change"))
	}
	for _, m := range e.sender.Messages() {
		if m.To == "new@example.test" {
			fragment(t, m, "/verify")
		} else {
			fragment(t, m, "/reset")
		}
	}
}

func TestSecondTokenMailInvalidatesTheFirst(t *testing.T) {
	e := open(t)
	e.account("a1", "ann@example.test")
	for range 2 {
		e.enqueue(mail.Mail{Template: mail.PasswordReset, Locale: mail.LocaleEN, To: "ann@example.test", AccountID: "a1"})
	}
	messages := e.waitMessages(2)
	first, second := fragment(t, messages[0], "/reset"), fragment(t, messages[1], "/reset")
	live := func(token string) bool {
		record, err := e.store.Read().EmailToken(e.ctx, tenant, mail.HashToken(token))
		return err == nil && record != nil && record.ConsumedAtMs == nil
	}
	// 两封并发发出时哪封后生成不确定，但同账号同目的任何时刻只有一枚有效。
	if live(first) == live(second) || e.count("SELECT count(*) FROM email_tokens WHERE consumed_at_ms IS NULL") != 1 {
		t.Fatalf("只有最后一枚有效：%v %v", live(first), live(second))
	}
}

func TestRetryUsesFreshTokenAndNothingIsSentTwice(t *testing.T) {
	e := open(t)
	e.account("a1", "ann@example.test")
	e.sender.FailNext(1)
	e.enqueue(mail.Mail{Template: mail.VerifyEmail, Locale: mail.LocaleEN, To: "ann@example.test", AccountID: "a1"})
	e.eventually("第一次失败", func() bool {
		return e.count("SELECT count(*) FROM jobs WHERE last_error IS NOT NULL AND done_at_ms IS NULL") == 1
	})
	var firstHash string
	if err := e.db.Pool().QueryRow(e.ctx, "SELECT token_hash FROM email_tokens").Scan(&firstHash); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(31 * time.Second)
	message := e.waitMessages(1)[0]
	if mail.HashToken(fragment(t, message, "/verify")) == firstHash {
		t.Fatal("重试要换新令牌")
	}
	if e.count("SELECT count(*) FROM email_tokens WHERE consumed_at_ms IS NULL") != 1 {
		t.Fatal("旧令牌作废")
	}

	// 作业完成没记上被重放：已发的不再发。
	e.eventually("完成", func() bool {
		return e.count("SELECT count(*) FROM jobs WHERE kind = 'mail.send' AND done_at_ms IS NOT NULL") == 1
	})
	if _, err := e.db.Pool().Exec(e.ctx, "UPDATE jobs SET done_at_ms = NULL WHERE kind = 'mail.send'"); err != nil {
		t.Fatal(err)
	}
	e.eventually("重放后再次完成", func() bool {
		return e.count("SELECT count(*) FROM jobs WHERE kind = 'mail.send' AND done_at_ms IS NOT NULL") == 1
	})
	if n := len(e.sender.Messages()); n != 1 {
		t.Fatalf("重放不重发：%d", n)
	}
	// 指标：失败一次、发出一封；重放不重计。
	e.expectMetrics(1, 1, 0, 0)
}

func TestGivingUpMarksTheOutboxFailed(t *testing.T) {
	e := open(t)
	e.account("a1", "ann@example.test")
	e.sender.FailNext(100)
	e.enqueue(mail.Mail{Template: mail.PasswordChanged, Locale: mail.LocaleEN, To: "ann@example.test", Params: mail.Params{DisplayName: "Ann"}})
	if _, err := e.db.Pool().Exec(e.ctx, "UPDATE jobs SET max_attempts = 2"); err != nil {
		t.Fatal(err)
	}
	e.eventually("第一次失败", func() bool {
		return e.count("SELECT count(*) FROM jobs WHERE attempts = 1 AND last_error IS NOT NULL") == 1
	})
	e.clock.Advance(31 * time.Second)
	e.eventually("用尽重试后发件箱记失败", func() bool {
		return e.count("SELECT count(*) FROM outbox_emails WHERE failed_at_ms IS NOT NULL") == 1 &&
			e.count("SELECT count(*) FROM jobs WHERE failed_at_ms IS NOT NULL") == 1
	})
	if len(e.sender.Messages()) != 0 {
		t.Fatal("一封都没发出")
	}
	// 指标：两次尝试都失败，一封用尽重试。
	e.expectMetrics(0, 2, 1, 0)
	if text := e.mail.RenderMetrics(); strings.Contains(text, "ann@") || strings.Contains(text, mail.PasswordChanged) {
		t.Fatalf("指标里不带地址与模板：%s", text)
	}
}

func TestDecoyAndUndeliverableAreDroppedWithoutSending(t *testing.T) {
	e := open(t)
	e.account("a1", "ann@example.test")
	// 占位发件（找回口令对没注册的邮箱）：丢弃，不留地址。
	e.enqueue(mail.Mail{Template: mail.PasswordReset, Locale: mail.LocaleZH, To: "nobody@example.test", Params: mail.Params{Decoy: true}})
	// 验证邮件发信时账号已经验证过：没有意义，丢掉。
	e.enqueue(mail.Mail{Template: mail.VerifyEmail, Locale: mail.LocaleZH, To: "ann@example.test", AccountID: "a1"})
	if _, err := e.db.Pool().Exec(e.ctx, "UPDATE accounts SET email_verified_at_ms = 1 WHERE account_id = 'a1'"); err != nil {
		t.Fatal(err)
	}
	e.eventually("两封都处理完", func() bool {
		return e.count("SELECT count(*) FROM jobs WHERE kind = 'mail.send' AND done_at_ms IS NOT NULL") == 2
	})
	if len(e.sender.Messages()) != 0 || e.count("SELECT count(*) FROM outbox_emails") != 0 {
		t.Fatalf("不发信也不留发件箱：%d", len(e.sender.Messages()))
	}
}

func TestTokenTemplateNeedsAnAccount(t *testing.T) {
	e := open(t)
	err := e.store.Tx(e.ctx, func(tx saas.Tx) error {
		return e.mail.Enqueue(e.ctx, tx, mail.Mail{Template: mail.VerifyEmail, Locale: mail.LocaleZH, To: "ann@example.test"})
	})
	if err == nil {
		t.Fatal("带令牌的模板必须给账号")
	}
}

func TestEnqueueRollsBackWithTheBusinessTransaction(t *testing.T) {
	e := open(t)
	e.account("a1", "ann@example.test")
	stop := context.Canceled
	err := e.store.Tx(e.ctx, func(tx saas.Tx) error {
		if err := e.mail.Enqueue(e.ctx, tx, mail.Mail{Template: mail.PasswordChanged, Locale: mail.LocaleEN, To: "ann@example.test"}); err != nil {
			return err
		}
		return stop
	})
	if err != stop {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if e.count("SELECT count(*) FROM outbox_emails")+e.count("SELECT count(*) FROM jobs WHERE kind = 'mail.send'") != 0 || len(e.sender.Messages()) != 0 {
		t.Fatal("回滚的事务不留发件箱与作业，也不发信")
	}
}

func TestGarbageCollectionJobs(t *testing.T) {
	e := open(t)
	e.account("a1", "ann@example.test")
	e.enqueue(mail.Mail{Template: mail.VerifyEmail, Locale: mail.LocaleEN, To: "ann@example.test", AccountID: "a1"})
	e.waitMessages(1)
	// 作业的完成也要等记进库再推时钟：完成时刻晚于推时钟会被记成推过之后，「完成满 7 天」就差一截。
	e.eventually("已发", func() bool {
		return e.count("SELECT count(*) FROM outbox_emails WHERE sent_at_ms IS NOT NULL") == 1 &&
			e.count("SELECT count(*) FROM jobs WHERE kind = 'mail.send' AND done_at_ms IS NOT NULL") == 1
	})
	// 令牌 24 小时后过期，过期再满 24 小时才清。
	e.clock.Advance(25 * time.Hour)
	time.Sleep(1500 * time.Millisecond)
	if e.count("SELECT count(*) FROM email_tokens") != 1 {
		t.Fatal("刚过期的令牌还要留着（过期的与已用的要能区分）")
	}
	e.clock.Advance(24 * time.Hour)
	e.eventually("过期满 24 小时的令牌被清", func() bool { return e.count("SELECT count(*) FROM email_tokens") == 0 })
	if e.count("SELECT count(*) FROM outbox_emails") != 1 {
		t.Fatal("发件箱满 30 天才清")
	}
	e.clock.Advance(6 * 24 * time.Hour)
	e.eventually("完成满 7 天的作业被清", func() bool { return e.count("SELECT count(*) FROM jobs WHERE kind = 'mail.send'") == 0 })
	e.clock.Advance(23 * 24 * time.Hour)
	e.eventually("发出满 30 天的发件箱被清", func() bool { return e.count("SELECT count(*) FROM outbox_emails") == 0 })
}

func TestOutboxParamsHoldNoToken(t *testing.T) {
	e := open(t)
	e.account("a1", "ann@example.test")
	e.enqueue(mail.Mail{Template: mail.VerifyEmail, Locale: mail.LocaleEN, To: "ann@example.test", AccountID: "a1", Params: mail.Params{DisplayName: "Ann"}})
	var params, payload json.RawMessage
	if err := e.db.Pool().QueryRow(e.ctx, "SELECT o.params, j.payload FROM outbox_emails o JOIN jobs j ON j.job_id = o.job_id").Scan(&params, &payload); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	_ = json.Unmarshal(payload, &decoded)
	if len(decoded) != 2 || decoded["accountId"] != "a1" || decoded["emailId"] == "" {
		t.Fatalf("作业载荷只有标识：%s", payload)
	}
	if strings.Contains(string(params), "token") || strings.Contains(string(params), "http") {
		t.Fatalf("发件箱参数里没有令牌与链接：%s", params)
	}
}

func TestDecoyRowStoresNoAddress(t *testing.T) {
	e := open(t)
	e.sender.FailNext(0)
	// 占位行在作业跑之前就暂停作业，直接看入队时落库的内容。
	e.runner.Stop(5 * time.Second)
	e.enqueue(mail.Mail{Template: mail.PasswordReset, Locale: mail.LocaleZH, To: "probed@example.test", Params: mail.Params{Decoy: true}})
	var to string
	if err := e.db.Pool().QueryRow(e.ctx, "SELECT to_email FROM outbox_emails").Scan(&to); err != nil || to != "" {
		t.Fatalf("占位行不存被试探的地址：%q %v", to, err)
	}
}

func TestRetryDoesNotReuseMessageID(t *testing.T) {
	e := open(t)
	e.account("a1", "ann@example.test")
	e.sender.FailNext(1)
	e.enqueue(mail.Mail{Template: mail.VerifyEmail, Locale: mail.LocaleEN, To: "ann@example.test", AccountID: "a1"})
	// 等失败记进库（按当时的时钟排到 30 秒后）再推时钟：只看发件方被调用过，运行器可能还没记失败，
	// 推过的时钟会被记成重试的起点，重试就排到推完之后再 30 秒，永远等不到。
	e.eventually("第一次失败", func() bool {
		return len(e.sender.Attempts()) == 1 &&
			e.count("SELECT count(*) FROM jobs WHERE last_error IS NOT NULL AND locked_by IS NULL AND done_at_ms IS NULL") == 1
	})
	e.clock.Advance(31 * time.Second)
	e.waitMessages(1)
	attempts := e.sender.Attempts()
	if len(attempts) != 2 || attempts[0].MessageID == attempts[1].MessageID ||
		!strings.HasSuffix(attempts[0].MessageID, "@app.example.test") || !strings.Contains(attempts[1].MessageID, ".2@") {
		t.Fatalf("每次尝试一个 Message-ID：%+v", attempts)
	}
}

func TestTokenMailCarriesNoDisplayNameAndDeviceNameIsSanitized(t *testing.T) {
	e := open(t)
	e.account("a1", "ann@example.test")
	phish := "点击 https://evil.example 领奖"
	e.enqueue(mail.Mail{Template: mail.VerifyEmail, Locale: mail.LocaleZH, To: "ann@example.test", AccountID: "a1", Params: mail.Params{DisplayName: phish}})
	e.enqueue(mail.Mail{Template: mail.NewDevice, Locale: mail.LocaleZH, To: "ann@example.test",
		Params: mail.Params{DisplayName: "Ann", DeviceName: "办公室 http://evil.example/x " + strings.Repeat("长", 100), Platform: "desktop"}})
	for _, m := range e.waitMessages(2) {
		if strings.Contains(m.Text, "evil.example") || strings.Contains(m.HTML, "evil.example") || strings.Contains(m.Text, "领奖") {
			t.Fatalf("邮件里不该出现注册者填的文字或网址：%s", m.Text)
		}
		if strings.Contains(m.Subject, "Ann") {
			t.Fatal("主题不带名字")
		}
	}
}

func TestWithoutSMTPJobsAreDeferredBacklogIsCappedAndNoTokenIsMinted(t *testing.T) {
	e := openWith(t, mail.Disabled(), 3)
	e.account("a1", "ann@example.test")
	for range 5 {
		e.enqueue(mail.Mail{Template: mail.VerifyEmail, Locale: mail.LocaleEN, To: "ann@example.test", AccountID: "a1"})
		time.Sleep(5 * time.Millisecond) // created_at_ms 区分先后
		e.clock.Advance(time.Millisecond)
	}
	e.eventually("积压只留最新的 3 封", func() bool {
		return e.count("SELECT count(*) FROM outbox_emails") == 3 && e.mail.Dropped() == 2
	})
	e.eventually("作业延后、不计尝试", func() bool {
		return e.count("SELECT count(*) FROM jobs WHERE kind = 'mail.send' AND attempts = 0 AND locked_by IS NULL AND last_error IS NULL AND run_at_ms > $1",
			e.clock.Now().UnixMilli()+int64(time.Minute/time.Millisecond)) == 3
	})
	if e.count("SELECT count(*) FROM email_tokens") != 0 {
		t.Fatal("没配 SMTP 不生成令牌")
	}
	if e.count("SELECT count(*) FROM jobs WHERE kind = 'mail.send'") != 3 {
		t.Fatal("被丢掉的邮件连同作业一起删")
	}
	// 没配 SMTP 不算发信失败，只计丢掉的积压。
	e.expectMetrics(0, 0, 0, 2)
}

// 排队期间账号邮箱换了：寄往旧地址的找回口令邮件丢掉，不发。
func TestResetMailToAnAddressNoLongerTheAccountsIsDropped(t *testing.T) {
	e := open(t)
	e.runner.Stop(5 * time.Second)
	e.account("a1", "ann@example.test")
	e.enqueue(mail.Mail{Template: mail.PasswordReset, Locale: mail.LocaleEN, To: "ann@example.test", AccountID: "a1"})
	if _, err := e.db.Pool().Exec(e.ctx, "UPDATE accounts SET email = 'moved@example.test' WHERE account_id = 'a1'"); err != nil {
		t.Fatal(err)
	}
	runner := jobs.New(jobs.Options{Store: e.store, TenantID: tenant, Workers: 1, Now: e.clock.Now, Poll: 20 * time.Millisecond})
	e.mail.Register(runner)
	runner.Start()
	t.Cleanup(func() { runner.Stop(5 * time.Second) })
	e.eventually("处理完", func() bool {
		return e.count("SELECT count(*) FROM jobs WHERE kind = 'mail.send' AND done_at_ms IS NOT NULL") == 1
	})
	if len(e.sender.Messages()) != 0 || e.count("SELECT count(*) FROM email_tokens") != 0 {
		t.Fatal("不发信也不生成令牌")
	}
}
