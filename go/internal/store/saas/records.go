package saas

import (
	"encoding/json"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// 状态与范围的取值（迁移 0004）。
const (
	AccountActive   = "active"
	AccountDisabled = "disabled"
	ScopeOperator   = "operator"
)

// Account 是 SaaS 账号的完整行：核心的 AccountRecord（口令、锁定计数）加邮箱与资料。
// AccountRecord.Name 恒等于 AccountID（满足核心表的唯一索引，登录按邮箱不按 name）。
type Account struct {
	store.AccountRecord
	// Email 一律小写。
	Email             string
	EmailVerifiedAtMs *int64
	DisplayName       string
	Locale            string
	Status            string
	StatusReason      *string
	Scopes            []string
}

// HasScope 报告账号是否持有某个特殊范围（如 operator）。
func (a Account) HasScope(scope string) bool {
	for _, have := range a.Scopes {
		if have == scope {
			return true
		}
	}
	return false
}

// Membership 是账号所属的一个团队。
type Membership struct {
	TeamID string
	Slug   string
	Name   string
	Role   TeamRole
}

// Incident 是运营方的事故开关（platform_settings 键 incident，设计 §4.9）。
type Incident struct {
	AssertionsPaused     bool `json:"assertionsPaused"`
	RegistrationsPaused  bool `json:"registrationsPaused"`
	RejectNewConnections bool `json:"rejectNewConnections"`
}

// 邮件令牌的目的（迁移 0004 email_tokens.purpose）。
const (
	EmailPurposeVerify = "verify"
	EmailPurposeReset  = "reset"
	EmailPurposeChange = "change"
)

// EmailToken 是邮件里一次性令牌的库内记录：只有哈希，明文只在发出的邮件里。
type EmailToken struct {
	TokenHash string
	AccountID string
	Purpose   string
	// Email 是发往的地址；change 是新地址，确认时据此改邮箱。
	Email        string
	CreatedAtMs  int64
	ExpiresAtMs  int64
	ConsumedAtMs *int64
}

// NewJob 是入队的一个作业（设计 §6.1）。
type NewJob struct {
	Kind string
	// Payload 只有标识与参数，不含令牌、口令与内容。
	Payload json.RawMessage
	RunAtMs int64
	// DedupeKey：同 kind 下未完成的作业按它去重。
	DedupeKey string
	// SlotKey：定时作业的时间槽，完成后也不能再插。
	SlotKey string
}

// Job 是领到手的作业。
type Job struct {
	ID          int64
	Kind        string
	Payload     json.RawMessage
	Attempts    int
	MaxAttempts int
}

// OutboxEmail 是发件箱的一行（含收件地址，发出或失败满 30 天由 outbox.gc 删）。
type OutboxEmail struct {
	EmailID  string
	JobID    *int64
	Template string
	Locale   string
	To       string
	// Params 是渲染参数：标识与显示名，没有令牌。
	Params      json.RawMessage
	MessageID   string
	CreatedAtMs int64
	SentAtMs    *int64
	FailedAtMs  *int64
}

// Actor 的种类（audit_log.actor_kind）。
const (
	ActorAccount  = "account"
	ActorAPIToken = "api_token"
	ActorSystem   = "system"
	ActorOperator = "operator"
)

// AuditEntry 是审计与账号安全日志的一行：只记主体、动作、对象、时间、jti、IP 前缀与运营理由。
type AuditEntry struct {
	AtMs    int64
	EventID string
	// TeamID 是团队审计的团队；账号安全日志为空。
	TeamID string
	// AccountID 是安全日志的本人（action 以 security. 开头）。
	AccountID  string
	ActorKind  string
	ActorID    string
	Action     string
	TargetKind string
	TargetID   string
	SourceID   string
	JTI        string
	IPPrefix   string
	// DeviceName 是设备名快照。
	DeviceName string
	Reason     string
}
