package saas

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
)

// TOTPCredential 是运营账号的两步验证（account_credentials，kind = totp）：种子用主密钥包裹，
// LastStep 是最后一次用过的时间步（同一步不重用）。
type TOTPCredential struct {
	CredentialID string
	SecretWrap   string
	LastStep     *int64
}

// IncidentPatch 是对事故开关的部分更新：非 nil 的项才改。
type IncidentPatch struct {
	AssertionsPaused     *bool
	RegistrationsPaused  *bool
	RejectNewConnections *bool
}

// AdminCursor 是运营列表分页的游标：(created_at_ms, id) 之后（按创建时间倒序）。
type AdminCursor struct {
	CreatedAtMs int64
	ID          string
}

type adminCursorJSON struct {
	C int64  `json:"c"`
	I string `json:"i"`
}

// Encode 是游标的线上编码（对客户端不透明）。
func (c AdminCursor) Encode() string {
	data, _ := json.Marshal(adminCursorJSON{C: c.CreatedAtMs, I: c.ID})
	return base64.RawURLEncoding.EncodeToString(data)
}

// DecodeAdminCursor 解游标；不合格式答错误。
func DecodeAdminCursor(text string) (*AdminCursor, error) {
	data, err := base64.RawURLEncoding.DecodeString(text)
	var c adminCursorJSON
	if err != nil || json.Unmarshal(data, &c) != nil || c.I == "" {
		return nil, errors.New("游标不合格式")
	}
	return &AdminCursor{CreatedAtMs: c.C, ID: c.I}, nil
}

// AdminQuery 是运营列表与搜索：Q 按邮箱 / 显示名 / 团队名 / slug 包含（ILIKE，仓储自己转义 % _ \）。
type AdminQuery struct {
	Q      string
	Status string
	After  *AdminCursor
	Limit  int
}

// AdminAccount 是运营看到的一个账号：只有元数据，没有任何内容与令牌。
type AdminAccount struct {
	AccountID     string
	Email         *string
	DisplayName   string
	EmailVerified bool
	Status        string
	Plan          string
	HostCount     int
	TeamCount     int
	CreatedAtMs   int64
	LastSeenAtMs  *int64
	// RelayBytesMonth 只在详情里填（本 UTC 月的 e2e 字节）。
	RelayBytesMonth int64
}

// AdminTeam 是运营看到的一个团队。
type AdminTeam struct {
	TeamID      string
	Slug        string
	Name        string
	Status      string
	Plan        string
	MemberCount int
	HostCount   int
	CreatedAtMs int64
	DeletedAtMs *int64
	// Seats（非 guest 成员）与 RelayBytesMonth 只在详情里填。
	Seats           int
	RelayBytesMonth int64
}

// AdminTenant 是搜索结果里账号与团队混排的一行。
type AdminTenant struct {
	Kind        string // "account" | "team"
	ID          string
	Name        string
	Plan        string
	HostCount   int
	Status      string // active | disabled | suspended
	CreatedAtMs int64
}

// OpsReader 是运营后台的只读查询（Reader 嵌入它）。每条都带 tenant_id；返回的都是元数据。
type OpsReader interface {
	AdminTenants(ctx context.Context, tenantID string, q AdminQuery) ([]AdminTenant, error)
	AdminAccounts(ctx context.Context, tenantID string, q AdminQuery) ([]AdminAccount, error)
	// AdminAccount 读一个账号的详情（含本月 e2e 字节，区间 [monthStartMs, monthEndMs)）；没有答 nil。
	AdminAccount(ctx context.Context, tenantID, accountID string, monthStartMs, monthEndMs int64) (*AdminAccount, error)
	AdminTeams(ctx context.Context, tenantID string, q AdminQuery) ([]AdminTeam, error)
	AdminTeam(ctx context.Context, tenantID, teamID string, monthStartMs, monthEndMs int64) (*AdminTeam, error)
	// OperatorActions 是运营对某个对象（target_kind / target_id）最近的动作，新的在前。
	OperatorActions(ctx context.Context, tenantID, targetKind, targetID string, limit int) ([]AuditEntry, error)
	// IncidentAt 读事故开关和最后一次改它的时刻；没写过时刻为 nil。
	IncidentAt(ctx context.Context, tenantID string) (Incident, *int64, error)
	// TOTP 读账号的两步验证；没有答 nil。
	TOTP(ctx context.Context, tenantID, accountID string) (*TOTPCredential, error)
}

// OpsRepo 是运营动作的写入。运营不是团队成员，所以这里收裸 id（调用方已核过运营身份）。
type OpsRepo interface {
	// SetTeamStatus 改团队状态；团队不存在答 false。
	SetTeamStatus(ctx context.Context, tenantID, teamID, status string, reason *string, nowMs int64) (found bool, err error)
	// TeamHostIDs 是团队未撤销的主机（升序）。
	TeamHostIDs(ctx context.Context, tenantID, teamID string) ([]string, error)
	// SetAccountStatus 改账号状态；账号不存在答 false。
	SetAccountStatus(ctx context.Context, tenantID, accountID, status string, reason *string, nowMs int64) (found bool, err error)
	// RevokeDevices 撤销账号的全部设备，答撤销了几台。
	RevokeDevices(ctx context.Context, tenantID, accountID string, nowMs int64) (int, error)
	// UpdateIncident 在行锁下读-改-写事故开关（platform_settings 键 incident；两位运营同时改不同的项不会互相覆盖），答改后的值。
	UpdateIncident(ctx context.Context, tenantID string, patch IncidentPatch, updatedBy string, nowMs int64) (Incident, error)
	// RevokeOperatorDevices 撤销账号的全部运营会话设备（scope = operator），答被撤销的设备 id。
	RevokeOperatorDevices(ctx context.Context, tenantID, accountID string, nowMs int64) ([]string, error)
	// AddScope 给账号加一个特殊范围（已有则不动），答账号存在与否。
	AddScope(ctx context.Context, tenantID, accountID, scope string, nowMs int64) (found bool, err error)
	// PutTOTP 登记（或换掉）账号的两步验证；种子已包裹。
	PutTOTP(ctx context.Context, tenantID, accountID, credentialID, secretWrap string, nowMs int64) error
	// AcceptTOTPStep 接受一个时间步：只有它大于已用过的最后一步才成功（并记下），防重放。
	AcceptTOTPStep(ctx context.Context, tenantID, accountID string, step, nowMs int64) (bool, error)
}
