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
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

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

	// backlogCap 是没配 SMTP 时发件箱里等着发的邮件上限，超出丢最旧的。
	backlogCap = 1000
	// notConfiguredRetry 是没配 SMTP 时作业延后多久再看。
	notConfiguredRetry = 5 * time.Minute

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
	// TeamName、InviterName、Role 是团队邀请的：团队名、邀请人显示名与被邀请的角色码。
	TeamName    string `json:"teamName,omitempty"`
	InviterName string `json:"inviterName,omitempty"`
	Role        string `json:"role,omitempty"`
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
	// InvitationID：团队邀请必填（发信时生成令牌、写进这条邀请）。
	InvitationID string
	Params       Params
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
	// BacklogCap 是没配 SMTP 时发件箱里等着发的邮件上限；缺省 1000。
	BacklogCap int
}

// Service 是邮件服务。
type Service struct {
	o    Options
	host string
	// dropped 是没配 SMTP 时因积压超限丢掉的邮件数（指标的替身，测试与日志用）。
	dropped atomic.Int64
}

// Dropped 是没配 SMTP 时因积压超限丢掉的邮件总数。
func (s *Service) Dropped() int64 { return s.dropped.Load() }

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
	if options.BacklogCap <= 0 {
		options.BacklogCap = backlogCap
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
	if mail.Template == TeamInvitation && mail.InvitationID == "" {
		return errors.New("团队邀请邮件必须给邀请 id")
	}
	if mail.Params.Decoy {
		mail.To = "" // 占位行不存被试探的地址
	}
	params, err := json.Marshal(mail.Params)
	if err != nil {
		return err
	}
	emailID := core.RandomToken(16)
	payload, err := json.Marshal(sendPayload{EmailID: emailID, AccountID: mail.AccountID, InvitationID: mail.InvitationID})
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
	EmailID      string `json:"emailId"`
	AccountID    string `json:"accountId,omitempty"`
	InvitationID string `json:"invitationId,omitempty"`
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
	if IsDisabled(s.o.Sender) {
		return s.deferUnconfigured(ctx)
	}
	var out *prepared
	err := s.o.Store.Tx(ctx, func(tx saas.Tx) error {
		out = nil
		return s.prepare(ctx, tx, payload, job.Attempts, &out)
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

func (s *Service) prepare(ctx context.Context, tx saas.Tx, payload sendPayload, attempt int, out **prepared) error {
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
	// 设备名与平台是客户端报的字符串：去网址特征并截短再渲染，不让陌生人借提醒邮件投放链接。
	data := Data{
		DisplayName: SanitizeLabel(params.DisplayName, labelMax), NewEmail: params.NewEmail,
		DeviceName: SanitizeLabel(params.DeviceName, labelMax), Platform: SanitizeLabel(params.Platform, labelMax),
		TeamName: SanitizeLabel(params.TeamName, labelMax), InviterName: SanitizeLabel(params.InviterName, labelMax),
		RoleLabel: RoleLabel(email.Locale, params.Role),
	}
	if email.Template == TeamInvitation {
		link, ok, err := s.inviteLink(ctx, tx, payload.InvitationID, email.To)
		if err != nil {
			return err
		}
		if !ok {
			return tx.Jobs().DeleteOutbox(ctx, tenant, email.EmailID)
		}
		data.Link = link
	}
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
		// 令牌邮件可能发给还没验证的地址：不带显示名（那是注册者自己填的任意文字）。
		data.DisplayName = ""
	}
	subject, text, html, err := s.o.Templates.Render(email.Locale, email.Template, data)
	if err != nil {
		return err
	}
	*out = &prepared{template: email.Template, message: Message{
		MessageID: attemptMessageID(email.MessageID, attempt), To: email.To, Subject: subject, Text: text, HTML: html,
	}}
	return nil
}

// inviteLink 在发信时给团队邀请生成令牌：只把哈希写进邀请行（旧令牌随之作废，重试会换新的），明文只进邮件的
// 链接片段。邀请已不是待处理（撤销、接受、过期）或收件地址对不上，这封信没有意义，答 ok = false。
func (s *Service) inviteLink(ctx context.Context, tx saas.Tx, invitationID, to string) (string, bool, error) {
	inv, err := tx.Invitations().Lock(ctx, s.o.TenantID, invitationID)
	if err != nil {
		return "", false, err
	}
	nowMs := s.nowMs()
	if inv == nil || !inv.Pending(nowMs) || inv.Email != strings.ToLower(to) {
		return "", false, nil
	}
	token := core.RandomToken(32)
	if err := tx.Invitations().SetToken(ctx, s.o.TenantID, invitationID, HashToken(token), nowMs); err != nil {
		return "", false, err
	}
	return s.o.AppOrigin + "/invite/" + invitationID + "#" + token, true, nil
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
	case saas.EmailPurposeReset:
		// 排队期间邮箱换了：旧地址不再是账号的邮箱，找回链接不能再寄给它。
		return account.Email == email.To
	}
	return true
}

// attemptMessageID 给 Message-ID 加上尝试序号：每次重试的令牌都是新的，内容不同，不复用同一个 id。
func attemptMessageID(id string, attempt int) string {
	local, host, ok := strings.Cut(id, "@")
	if !ok {
		return id
	}
	return local + "." + strconv.Itoa(attempt) + "@" + host
}

// deferUnconfigured 处理没配 SMTP 的情况：限制积压，作业延后再看，不生成令牌、不计尝试。
func (s *Service) deferUnconfigured(ctx context.Context) error {
	var dropped int
	err := s.o.Store.Tx(ctx, func(tx saas.Tx) (err error) {
		dropped, err = tx.Jobs().TrimOutbox(ctx, s.o.TenantID, s.o.BacklogCap)
		return err
	})
	if err != nil {
		return err
	}
	if dropped > 0 {
		s.dropped.Add(int64(dropped))
		s.o.Log.Warn("没配 SMTP，发件箱积压超限，丢掉最旧的邮件", obs.Operation("mail.backlog"), obs.Count(dropped))
	}
	return jobs.Defer(notConfiguredRetry)
}

const labelMax = 64

// urlLike 抓网址、邮箱和任何「域名形态」（一串字母数字，点，再两个以上字母），点两侧允许空白——
// 「evil . com」「evil。com」（全角点）这类混淆在 SanitizeLabel 里先归一再匹配。代价是「J. Smith」这类
// 带句点的名字也会被去掉，但这些标签只是展示用的，宁可多删。
var urlLike = regexp.MustCompile(`(?i)\S*://\S*|\bwww\.\S*|\S+@\S+|[\p{L}\p{N}-]+(?:\.[\p{L}\p{N}-]+)*\.[a-z]{2,}\S*`)

// dotLike 是常见的「点」的变体（全角句号、全角点、半角日文句号、中点、小圆点），归一成 ASCII 点。
var dotLike = strings.NewReplacer("。", ".", "．", ".", "｡", ".", "․", ".", "·", ".", "﹒", ".")

// spacedDot 是点两侧的空白（evil . com）。
var spacedDot = regexp.MustCompile(`\s*\.\s*`)

// SanitizeLabel 清理客户端或用户填的展示字符串（设备名、团队名、显示名）：去网址、邮箱、域名形态和控制字符，折叠空白，按码点截到 max。
func SanitizeLabel(s string, max int) string {
	// 归一后的字符串只用来判断；没有命中就保留原文（别把名字里的间隔号改成点）。
	if normalized := spacedDot.ReplaceAllString(dotLike.Replace(s), "."); urlLike.MatchString(normalized) {
		s = urlLike.ReplaceAllString(normalized, "")
	}
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '<' || r == '>' {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s
}
