// Package store 是控制核的存储端口与记录类型。
//
// 端口对应 packages/control-core/src/ports.ts：每个方法的第一个业务参数是 tenantID
// （个人中转恒为 DefaultTenant），Update 类方法是原子的读-改-写——fn 在一份副本上执行，
// 返回错误则什么也不写，没改任何东西也不写。实现在 sqlite/ 与 postgres/，statefile/ 只读导入
// state.json。记录的 JSON 形状与 TypeScript 一侧逐字段相同（camelCase、字段顺序一致），
// 两种实现读写的库可以交替给 TypeScript 版打开。
package store

import (
	"context"
	"encoding/json"

	"github.com/AMA-Link/armadra-cloud/go/internal/identity"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// DefaultTenant 是个人中转唯一的租户。
const DefaultTenant = "default"

// AccountRecord 是账号。AccountID 是不透明的 sub（个人中转 acct:<账号名>，SaaS 是 uuid）。
type AccountRecord struct {
	AccountID       string                  `json:"accountId"`
	Name            string                  `json:"name"`
	Password        identity.PasswordRecord `json:"password"`
	CreatedAtMs     int64                   `json:"createdAtMs"`
	UpdatedAtMs     int64                   `json:"updatedAtMs"`
	Failures        int64                   `json:"failures"`
	LockedUntilMs   int64                   `json:"lockedUntilMs"`
	LastFailureAtMs int64                   `json:"lastFailureAtMs"`
}

// SigningKeyRecord 是签名钥。PrivateKeyPKCS8 是 PKCS#8 DER 的标准 base64。
type SigningKeyRecord struct {
	Kid             string               `json:"kid"`
	PrivateKeyPKCS8 string               `json:"privateKeyPkcs8"`
	PublicJWK       gen.Ed25519PublicJWK `json:"publicJwk"`
	ActiveFromMs    int64                `json:"activeFromMs"`
	RetiredAtMs     *int64               `json:"retiredAtMs"`
}

// SourceRecord 是登记的源。
type SourceRecord struct {
	SourceID       string               `json:"sourceId"`
	Name           string               `json:"name"`
	Kind           string               `json:"kind"`
	PublicJWK      gen.Ed25519PublicJWK `json:"publicJwk"`
	CoreVersion    string               `json:"coreVersion"`
	Capabilities   []string             `json:"capabilities"`
	RegisteredAtMs int64                `json:"registeredAtMs"`
	LastSeenAtMs   *int64               `json:"lastSeenAtMs"`
	RevokedAtMs    *int64               `json:"revokedAtMs"`
	TrustedOrigins []string             `json:"trustedOrigins"`
}

// RegistrationToken 是注册令牌（只存哈希）。AccountID 是签发它的账号，源登记后归它。
type RegistrationToken struct {
	TokenHash   string `json:"tokenHash"`
	ExpiresAtMs int64  `json:"expiresAtMs"`
	AccountID   string `json:"accountId"`
}

// DeviceRecord 是设备（会话）。Subject 是 acct:<账号名> 或 guest:<linkId>:<随机>。
type DeviceRecord struct {
	DeviceID string `json:"deviceId"`
	Subject  string `json:"subject"`
	Platform string `json:"platform"`
	Name     string `json:"name"`
	// RefreshHash 是当前刷新令牌的哈希。
	RefreshHash string `json:"refreshHash"`
	// PreviousRefreshHash 是上一个刷新令牌的哈希：它再出现就是重放，撤销整台设备。
	PreviousRefreshHash *string `json:"previousRefreshHash"`
	Rotation            int64   `json:"rotation"`
	CreatedAtMs         int64   `json:"createdAtMs"`
	ExpiresAtMs         int64   `json:"expiresAtMs"`
	RevokedAtMs         *int64  `json:"revokedAtMs"`
	LastSeenAtMs        *int64  `json:"lastSeenAtMs"`
}

// LinkScope 是分享范围（已规范化，见 core.NormalizeLinkScope）；整台时链接上没有它。
type LinkScope struct {
	WorkspaceID *string `json:"workspaceId,omitempty"`
	SessionID   *string `json:"sessionId,omitempty"`
	ReadOnly    *bool   `json:"readOnly,omitempty"`
}

// LinkRecord 是分享链接。
type LinkRecord struct {
	LinkID       string     `json:"linkId"`
	SourceID     string     `json:"sourceId"`
	InvitationID string     `json:"invitationId"`
	Label        string     `json:"label"`
	Role         string     `json:"role"`
	SecretHash   string     `json:"secretHash"`
	MaxUses      *int64     `json:"maxUses"`
	Uses         int64      `json:"uses"`
	CreatedAtMs  int64      `json:"createdAtMs"`
	ExpiresAtMs  int64      `json:"expiresAtMs"`
	RevokedAtMs  *int64     `json:"revokedAtMs"`
	Scope        *LinkScope `json:"scope,omitempty"`
}

// TenantSettings 是租户级设置。Issuer 是断言与令牌的 iss，也是页面来源与中继地址的根。
type TenantSettings struct {
	Issuer         string   `json:"issuer"`
	AllowGuests    bool     `json:"allowGuests"`
	TrustedOrigins []string `json:"trustedOrigins"`
}

// Tenant 是租户行：设置加中继自己的设置（边缘限流额度、受信代理、服务端名称）与修订号。
// RateLimits 与 TrustedProxy 原样保存 JSON，不在这里解释。
type Tenant struct {
	TenantSettings
	RateLimits   json.RawMessage `json:"rateLimits,omitempty"`
	TrustedProxy json.RawMessage `json:"trustedProxy,omitempty"`
	DisplayName  *string         `json:"displayName,omitempty"`
	Revision     int64           `json:"revision"`
}

// TenantSnapshot 是整个租户的一致快照，导入导出与迁移用；各列表按创建顺序。
type TenantSnapshot struct {
	Tenant        Tenant
	Accounts      []AccountRecord
	SigningKeys   []SigningKeyRecord
	Sources       []SourceRecord
	Registrations []RegistrationToken
	Devices       []DeviceRecord
	Links         []LinkRecord
}

// AccountStore 是账号端口。查不到答 nil。
type AccountStore interface {
	FindByName(ctx context.Context, tenantID, name string) (*AccountRecord, error)
	Get(ctx context.Context, tenantID, accountID string) (*AccountRecord, error)
	// Update 原子地读-改-写；账号不存在时 fn 拿到 nil。
	Update(ctx context.Context, tenantID, accountID string, fn func(draft *AccountRecord) error) error
	// ResetPassword 原子地换口令记录、清锁，并撤销 sub = accountID 的全部未撤销设备（访客不受影响）；
	// keepDeviceID 非空时留下这一台（改口令的当前设备）。账号不存在答 false。
	ResetPassword(ctx context.Context, tenantID, accountID string, password identity.PasswordRecord, nowMs int64, keepDeviceID string) (bool, error)
}

// DeviceStore 是设备端口。
type DeviceStore interface {
	Get(ctx context.Context, tenantID, deviceID string) (*DeviceRecord, error)
	ListBySubject(ctx context.Context, tenantID, subject string) ([]DeviceRecord, error)
	// Insert 新增一台，顺手清掉 IsStaleDevice 为真的旧行。
	Insert(ctx context.Context, tenantID string, device DeviceRecord, nowMs int64) error
	Update(ctx context.Context, tenantID, deviceID string, fn func(draft *DeviceRecord) error) error
	// Touch 批量记「最近使用」（只往后推）。
	Touch(ctx context.Context, tenantID string, seen map[string]int64) error
}

// SourceStore 是源端口。
type SourceStore interface {
	Get(ctx context.Context, tenantID, sourceID string) (*SourceRecord, error)
	List(ctx context.Context, tenantID string) ([]SourceRecord, error)
	// Put 原子替换：fn 拿到现有记录（或 nil）答新记录；替换后排到最后。
	Put(ctx context.Context, tenantID, sourceID string, fn func(existing *SourceRecord) (SourceRecord, error)) (SourceRecord, error)
	Update(ctx context.Context, tenantID, sourceID string, fn func(draft *SourceRecord) error) error
}

// RegistrationStore 是注册令牌端口。
type RegistrationStore interface {
	// Add 清掉过期的；同时有效的已满 max 条答 false（什么也不写），否则加入答 true。
	Add(ctx context.Context, tenantID string, token RegistrationToken, nowMs int64, max int) (bool, error)
	// Consume 一次性：命中未过期的同哈希就删掉，答签发它的账号；没命中 ok 为 false。
	Consume(ctx context.Context, tenantID, tokenHash string, nowMs int64) (accountID string, ok bool, err error)
}

// LinkStore 是链接端口。
type LinkStore interface {
	Get(ctx context.Context, tenantID, linkID string) (*LinkRecord, error)
	// List 按创建顺序；sourceID 非空时只列这个源的。
	List(ctx context.Context, tenantID, sourceID string) ([]LinkRecord, error)
	Insert(ctx context.Context, tenantID string, link LinkRecord) error
	Update(ctx context.Context, tenantID, linkID string, fn func(draft *LinkRecord) error) error
	// Revoke 原子地撤销链接与它名下的全部访客设备（sub 以 guest:<linkID>: 开头）。
	// 答链接指向的源；没有这条有效链接 ok 为 false。
	Revoke(ctx context.Context, tenantID, linkID string, nowMs int64) (sourceID string, ok bool, err error)
}

// TenantStore 是租户设置端口；没有这个租户返回 ErrNoTenant。
type TenantStore interface {
	Get(ctx context.Context, tenantID string) (TenantSettings, error)
}

// Ports 是控制核用到的全部存储端口。
type Ports struct {
	Accounts      AccountStore
	Devices       DeviceStore
	Sources       SourceStore
	Registrations RegistrationStore
	Links         LinkStore
	Tenants       TenantStore
	Traffic       TrafficStore
}

// KeyStore 是签名钥的读取。个人中转每次读库，别的进程 rotate-key 之后立即可见；
// PostgreSQL 实现读缓存，察觉修订号变了再刷新。
type KeyStore interface {
	Issuer(ctx context.Context) (string, error)
	SigningKeys(ctx context.Context) ([]SigningKeyRecord, error)
}

// Backend 是一个打开的库：端口、签名钥与租户级管理操作，SQLite 与 PostgreSQL 各一个实现。
type Backend interface {
	Ports() *Ports
	Keys(tenantID string) KeyStore
	// CreateTenant 建租户（已有同 id 的会失败）。
	CreateTenant(ctx context.Context, tenantID string, tenant Tenant) error
	// AddAccount 直接放一个账号（建租户与测试装配用）。
	AddAccount(ctx context.Context, tenantID string, account AccountRecord) error
	ImportTenant(ctx context.Context, tenantID string, snapshot TenantSnapshot) error
	ExportTenant(ctx context.Context, tenantID string) (TenantSnapshot, error)
	// Revision 是租户当前的修订号（跨进程变更检测）；没有这个租户 ok 为 false。
	Revision(ctx context.Context, tenantID string) (revision int64, ok bool, err error)
	Close() error
}
