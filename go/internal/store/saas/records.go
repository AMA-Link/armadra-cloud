package saas

import "github.com/AMA-Link/armadra-cloud/go/internal/store"

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
