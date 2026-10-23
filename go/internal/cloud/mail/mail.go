// Package mail 是事务邮件：发件箱、模板、SMTP 适配器与 mail.send / outbox.gc 等作业（设计 §6.2、§6.3）。
//
// 业务事务里用 Service.Enqueue 插一行发件箱加一个 mail.send 作业，随提交送达。需要一次性令牌的模板
// （验证邮箱、改邮箱确认、找回口令）在发信时现生成令牌：作业载荷与发件箱里只有账号 id，mail.send 在自己的事务里
// 生成令牌、只把哈希写进 email_tokens（同目的的旧令牌作废）、提交后发信。明文令牌只存在于内存与发出的邮件里，
// 链接把它放在 # 片段中；重试会换新令牌，旧的随之作废。日志只记模板名、发件箱 id、结果与错误类名。
package mail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/jobs"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// 作业种类。
const (
	JobSend         = "mail.send"
	JobOutboxGC     = "outbox.gc"
	JobEmailTokenGC = "emailTokens.gc"
	JobJobsGC       = "jobs.gc"
)

// 令牌寿命与保留期（设计 §6.1、§6.3）。
const (
	VerifyTTL = 24 * time.Hour
	ChangeTTL = 24 * time.Hour
	ResetTTL  = 30 * time.Minute

	outboxRetention    = 30 * 24 * time.Hour
	tokenRetention     = 24 * time.Hour
	jobDoneRetention   = 7 * 24 * time.Hour
	jobFailedRetention = 30 * 24 * time.Hour
)

// tokenKinds 是带一次性令牌的模板：目的、寿命、链接路径。
var tokenKinds = map[string]struct {
	purpose string
	ttl     time.Duration
	path    string
}{
	VerifyEmail:   {saas.EmailPurposeVerify, VerifyTTL, "/verify"},
	EmailChange:   {saas.EmailPurposeChange, ChangeTTL, "/verify"},
	PasswordReset: {saas.EmailPurposeReset, ResetTTL, "/reset"},
}

// Params 是发件箱里的渲染参数：标识与显示名，没有令牌。
type Params struct {
	DisplayName string `json:"displayName,omitempty"`
	NewEmail    string `json:"newEmail,omitempty"`
	DeviceName  string `json:"deviceName,omitempty"`
	Platform    string `json:"platform,omitempty"`
	// Decoy：找回口令对没注册的邮箱插的占位行，与真发件走同样的库写入，作业里直接丢弃（防枚举，不留地址）。
	Decoy bool `json:"decoy,omitempty"`
}

// Mail 是要入队的一封邮件。
type Mail struct {
	Template string
	// Locale 取收件账号的 locale；没有账号的收件人取发起人的 locale。
	Locale string
	To     string
	// AccountID：带令牌的模板必填（令牌属于哪个账号）。
	AccountID string
	Params    Params
}

// Options 是邮件服务的装配。
type Options struct {
	Store     saas.Store
	TenantID  string
	Sender    Sender
	Templates *Templates
	// AppOrigin 是控制台来源：链接前缀，Message-ID 的主机部分。
	AppOrigin string
	Now       func() time.Time
	Log       *obs.Logger
}

// Service 是邮件服务。
type Service struct {
	o    Options
	host string
}

// New 建邮件服务；Templates 缺省用随包的。
func New(options Options) (*Service, error) {
	if options.Templates == nil {
		t, err := LoadTemplates()
		if err != nil {
			return nil, err
		}
		options.Templates = t
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Log == nil {
		options.Log = obs.Nop()
	}
	u, err := url.Parse(options.AppOrigin)
	if err != nil || u.Host == "" {
		return nil, errors.New("AppOrigin 不合法")
	}
	return &Service{o: options, host: u.Host}, nil
}

func (s *Service) nowMs() int64 { return s.o.Now().UnixMilli() }

// Enqueue 在业务事务里插发件箱一行和一个 mail.send 作业（同事务 NOTIFY，回滚即不发）。
func (s *Service) Enqueue(ctx context.Context, tx saas.Tx, mail Mail) error {
	if _, token := tokenKinds[mail.Template]; token && mail.AccountID == "" && !mail.Params.Decoy {
		return errors.New("带令牌的模板必须给账号")
	}
	params, err := json.Marshal(mail.Params)
	if err != nil {
		return err
	}
	emailID := core.RandomToken(16)
	payload, err := json.Marshal(sendPayload{EmailID: emailID, AccountID: mail.AccountID})
	if err != nil {
		return err
	}
	nowMs := s.nowMs()
	jobID, _, err := tx.Jobs().Enqueue(ctx, s.o.TenantID, saas.NewJob{Kind: JobSend, Payload: payload, RunAtMs: nowMs}, nowMs)
	if err != nil {
		return err
	}
	return tx.Jobs().InsertOutbox(ctx, s.o.TenantID, saas.OutboxEmail{
		EmailID: emailID, JobID: &jobID, Template: mail.Template, Locale: normalizeLocale(mail.Locale),
		To: mail.To, Params: params, MessageID: emailID + "@" + s.host, CreatedAtMs: nowMs,
	})
}

// normalizeLocale 把任意语言标记收成库里允许的两个值。
func normalizeLocale(locale string) string {
	if locale == LocaleEN {
		return LocaleEN
	}
	return LocaleZH
}

type sendPayload struct {
	EmailID   string `json:"emailId"`
	AccountID string `json:"accountId,omitempty"`
}

// HashToken 是库里存的令牌哈希：SHA-256 的十六进制。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Register 把本服务的作业处理器与定时作业登记进运行器。
func (s *Service) Register(runner *jobs.Runner) {
	runner.Handle(JobSend, s.send)
	runner.Handle(JobOutboxGC, s.gc(JobOutboxGC))
	runner.Handle(JobEmailTokenGC, s.gc(JobEmailTokenGC))
	runner.Handle(JobJobsGC, s.gc(JobJobsGC))
	runner.Every(JobOutboxGC, jobs.Daily)
	runner.Every(JobJobsGC, jobs.Daily)
	runner.Every(JobEmailTokenGC, jobs.Hourly)
}

// gc 是三个清理作业的处理器：删的都是已经没用的行，重放无害。
func (s *Service) gc(kind string) jobs.Handler {
	return func(ctx context.Context, _ saas.Job) error {
		now := s.o.Now()
		return s.o.Store.Tx(ctx, func(tx saas.Tx) error {
			var (
				n   int
				err error
			)
			switch kind {
			case JobOutboxGC:
				n, err = tx.Jobs().GCOutbox(ctx, s.o.TenantID, now.Add(-outboxRetention).UnixMilli())
			case JobEmailTokenGC:
				n, err = tx.Accounts().GCEmailTokens(ctx, s.o.TenantID, now.Add(-tokenRetention).UnixMilli())
			default:
				n, err = tx.Jobs().GCJobs(ctx, s.o.TenantID, now.Add(-jobDoneRetention).UnixMilli(), now.Add(-jobFailedRetention).UnixMilli())
			}
			if err == nil && n > 0 {
				s.o.Log.Info("已清理过期的行", obs.Kind(kind), obs.Count(n))
			}
			return err
		})
	}
}

// prepared 是 mail.send 第一段事务的产物。
type prepared struct {
	message  Message
	template string
}

// send 是 mail.send：事务里读发件箱、（需要时）生成令牌写哈希并渲染；提交后发信；再记已发。
// 可重放：已发的直接结束；发信失败抛错由作业重试（换新令牌，旧的作废）。
func (s *Service) send(ctx context.Context, job saas.Job) error {
	var payload sendPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil || payload.EmailID == "" {
		return errors.New("作业载荷不合格式")
	}
	var out *prepared
	err := s.o.Store.Tx(ctx, func(tx saas.Tx) error {
		out = nil
		return s.prepare(ctx, tx, payload, &out)
	})
	if err != nil || out == nil {
		return err
	}
	if err := s.o.Sender.Send(ctx, out.message); err != nil {
		if job.Attempts >= job.MaxAttempts {
			// 最后一次也失败：发件箱记失败，30 天后清走。
			_ = s.o.Store.Tx(context.WithoutCancel(ctx), func(tx saas.Tx) error {
				return tx.Jobs().MarkOutboxFailed(ctx, s.o.TenantID, payload.EmailID, s.nowMs())
			})
		}
		s.o.Log.Warn("邮件没发出", obs.Template(out.template), obs.EmailID(payload.EmailID), obs.Result("failed"), obs.Error(err))
		return err
	}
	s.o.Log.Info("邮件已发出", obs.Template(out.template), obs.EmailID(payload.EmailID), obs.Result("sent"))
	return s.o.Store.Tx(ctx, func(tx saas.Tx) error {
		return tx.Jobs().MarkOutboxSent(ctx, s.o.TenantID, payload.EmailID, s.nowMs())
	})
}

func (s *Service) prepare(ctx context.Context, tx saas.Tx, payload sendPayload, out **prepared) error {
	tenant := s.o.TenantID
	// 统一加锁顺序：账号行在发件箱行之前；同一账号同目的的两封令牌邮件因此串行，只有最后一枚有效。
	if payload.AccountID != "" {
		if _, err := tx.Accounts().LockAccount(ctx, tenant, payload.AccountID); err != nil {
			return err
		}
	}
	email, err := tx.Jobs().Outbox(ctx, tenant, payload.EmailID)
	if err != nil {
		return err
	}
	if email == nil || email.SentAtMs != nil || email.FailedAtMs != nil {
		return nil // 已发、已判失败或已清走：幂等地结束
	}
	var params Params
	if err := json.Unmarshal(email.Params, &params); err != nil {
		return errors.New("发件箱参数不合格式")
	}
	if params.Decoy {
		return tx.Jobs().DeleteOutbox(ctx, tenant, email.EmailID)
	}
	data := Data{DisplayName: params.DisplayName, NewEmail: params.NewEmail, DeviceName: params.DeviceName, Platform: params.Platform}
	if kind, ok := tokenKinds[email.Template]; ok {
		account, err := tx.Account(ctx, tenant, payload.AccountID)
		if err != nil {
			return err
		}
		if !deliverable(email, account, kind.purpose) {
			return tx.Jobs().DeleteOutbox(ctx, tenant, email.EmailID)
		}
		token := core.RandomToken(32)
		nowMs := s.nowMs()
		if _, err := tx.Accounts().InvalidateEmailTokens(ctx, tenant, account.AccountID, kind.purpose, nowMs); err != nil {
			return err
		}
		if err := tx.Accounts().InsertEmailToken(ctx, tenant, saas.EmailToken{
			TokenHash: HashToken(token), AccountID: account.AccountID, Purpose: kind.purpose, Email: email.To,
			CreatedAtMs: nowMs, ExpiresAtMs: nowMs + kind.ttl.Milliseconds(),
		}); err != nil {
			return err
		}
		data.Link = s.o.AppOrigin + kind.path + "#" + token
		if data.DisplayName == "" {
			data.DisplayName = account.DisplayName
		}
	}
	subject, text, html, err := s.o.Templates.Render(email.Locale, email.Template, data)
	if err != nil {
		return err
	}
	*out = &prepared{template: email.Template, message: Message{
		MessageID: email.MessageID, To: email.To, Subject: subject, Text: text, HTML: html,
	}}
	return nil
}

// deliverable：令牌邮件发信时账号还要满足条件，否则这封信没有意义，丢掉。
func deliverable(email *saas.OutboxEmail, account *saas.Account, purpose string) bool {
	if account == nil || account.Status != saas.AccountActive {
		return false
	}
	switch purpose {
	case saas.EmailPurposeVerify:
		return account.EmailVerifiedAtMs == nil && account.Email == email.To
	case saas.EmailPurposeChange:
		return account.Email != email.To
	}
	return true
}
