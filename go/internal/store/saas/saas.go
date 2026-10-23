// Package saas 是 SaaS 的存储端口（设计 §4.11），只有 PostgreSQL 实现（postgres.DB.SaaS()）。
//
// 每个仓储方法的第一个参数是 tenantID（或已含它的 TeamScope）；团队范围的方法收 TeamScope，
// 查询必写 team_id = $2。核心端口（store.Ports）与它共用同一个连接池；需要同事务写核心表时，
// 由 postgres 包把核心端口绑到同一个事务。仓储方法随各批（S1b 起）加入，本批只立骨架。
package saas

import (
	"context"
	"errors"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas/teamscope"
)

// ErrLockOrder 表示同一事务里违反了统一的加锁顺序（团队行 → 账号行 → 其余）：先锁了账号再锁团队。
// 两个事务反序互等会死锁，所以顺序写进 Tx 实现里强制执行（设计 §4.11）。
var ErrLockOrder = errors.New("加锁顺序反了：必须先锁团队再锁账号")

// TeamRole 与 TeamScope 是 teamscope 包类型的别名，方便端口签名使用。
type (
	TeamRole  = teamscope.Role
	TeamScope = teamscope.Scope
)

// 团队角色的取值（teamscope.Role 的常量）：不能 import teamscope 的包用它们比较角色。
const (
	RoleOwner   = teamscope.Owner
	RoleAdmin   = teamscope.Admin
	RoleMember  = teamscope.Member
	RoleBilling = teamscope.Billing
	RoleGuest   = teamscope.Guest
)

// Store 是 SaaS 的存储端口。
type Store interface {
	// Tx 在一个读已提交事务里执行 fn；fn 返回错误即回滚。提交后按序执行 tx.AfterCommit 登记的回调
	// （发 Redis 撤销、节点命令、事件）。死锁与序列化失败自动重试 3 次（fn 会被重新执行，
	// 重试前丢弃上一次登记的回调）。fn 与提交返回的约束冲突已映射成 *Error。
	Tx(ctx context.Context, fn func(tx Tx) error) error
	// Read 是不开事务的只读查询。
	Read() Reader
	// Queue 是作业队列的非事务操作（领取、续租、完成、失败、监听）。
	Queue() JobQueue
}

// Reader 是只读查询；Tx 也实现它。
type Reader interface {
	TeamReader
	InvitationReader
	HostReader
	// Ping 探活：连接池里能取到连接并执行一条语句。
	Ping(ctx context.Context) error
	// Account 按账号 id 读完整的账号行；没有答 nil。
	Account(ctx context.Context, tenantID, accountID string) (*Account, error)
	// AccountByEmail 按邮箱（转小写后比对）读账号；没有答 nil。
	AccountByEmail(ctx context.Context, tenantID, email string) (*Account, error)
	// Memberships 是账号所属的团队（排除已删除的），按加入顺序。
	Memberships(ctx context.Context, tenantID, accountID string) ([]Membership, error)
	// HasCredential 报告账号是否登记了某种凭据（totp / passkey / oauth）。
	HasCredential(ctx context.Context, tenantID, accountID, kind string) (bool, error)
	// Incident 读事故开关（platform_settings 键 incident）；没写过答全关。
	Incident(ctx context.Context, tenantID string) (Incident, error)
	// EmailToken 按令牌哈希读邮件令牌（含已用、已过期的）；没有答 nil。
	EmailToken(ctx context.Context, tenantID, tokenHash string) (*EmailToken, error)
}

// Tx 是一个事务里的全部仓储。
type Tx interface {
	Reader
	Accounts() AccountRepo       // 邮箱、状态、范围、凭据、邮件令牌、设备码
	Teams() TeamRepo             // 团队、成员、设置；LockTeam / LockAccount
	DeviceCodes() DeviceCodeRepo // 设备码（S3）
	Invitations() InvitationRepo
	Hosts() HostRepo // source_spaces、注册令牌（带空间）
	Grants() GrantRepo
	Trust() TrustRepo
	Audit() AuditRepo       // 只追加
	Jobs() JobRepo          // Enqueue（含 dedupe / slot、NOTIFY）、发件箱
	Settings() SettingsRepo // platform_settings、entitlements
	Issued() IssuedRepo     // cloud_issued_tokens、relay_revoked_tokens
	// AfterCommit 登记一个提交成功之后才执行的回调；回滚、重试时丢弃。回调里的失败只能记录，不能回滚事务。
	AfterCommit(fn func(ctx context.Context))
	// Ports 是绑定到本事务的核心端口（建设备、登记主机等和 SaaS 表写在同一个事务里）：不锁 tenants 行、
	// 不加租户修订号，devices.Insert 的失效设备清理只看这个账号。加锁顺序：团队行 → 账号行 → 其余，
	// 同类按 id 升序；读-改-写在绑定事务里一律行锁（FOR UPDATE）。
	//
	// 禁止在 Tx 闭包里调用用独立 Ports（DB.Ports / DB.SaaSPorts）的 core 函数：独立端口走别的连接，
	// 会等本事务持有的行锁而挂死；闭包里一律用 tx.Ports()，并用 core.Context.Bound 换掉 Ports 与撤销钩子
	// （钩子要经 AfterCommit 投递，见 relayctl.Deferred）。
	Ports() *store.Ports
}

// AccountRepo 是账号相关的仓储。
type AccountRepo interface {
	// LockAccount 对账号行 SELECT … FOR UPDATE，答它存不存在。统一加锁顺序里排在团队行之后；
	// 同一事务里多次调用，id 必须升序，否则 ErrLockOrder。锁失败不改变顺序状态。
	LockAccount(ctx context.Context, tenantID, accountID string) (found bool, err error)
	// LockAccounts 一次锁多个账号：先升序去重再逐个锁（两个事务要同一批账号时不会反序互等）。
	LockAccounts(ctx context.Context, tenantID string, accountIDs ...string) error
	// Create 插一个账号（邮箱已转小写）；邮箱已被占用答 ErrEmailTaken。
	Create(ctx context.Context, tenantID string, account Account) error
	// SetDisplayName 改显示名。
	SetDisplayName(ctx context.Context, tenantID, accountID, displayName string, nowMs int64) error
	// SetEmailVerified 记邮箱已验证（已验证的保持原时刻）。
	SetEmailVerified(ctx context.Context, tenantID, accountID string, nowMs int64) error
	// ChangeEmail 把邮箱换成 newEmail（小写）并记已验证；被别人占用答 ErrEmailTaken。
	ChangeEmail(ctx context.Context, tenantID, accountID, newEmail string, nowMs int64) error
	// InsertEmailToken 写一枚邮件令牌（只有哈希）。
	InsertEmailToken(ctx context.Context, tenantID string, token EmailToken) error
	// ConsumeEmailToken 一步消耗一枚未用、未过期且目的相符的令牌，答它的记录（账号与地址以此为准）；
	// 没消耗到（已用、过期、不存在、目的不符）答 nil。
	ConsumeEmailToken(ctx context.Context, tenantID, tokenHash, purpose string, nowMs int64) (*EmailToken, error)
	// InvalidateEmailTokens 作废账号某目的下所有未用的令牌（记已用）；答作废了几枚。
	InvalidateEmailTokens(ctx context.Context, tenantID, accountID, purpose string, nowMs int64) (int, error)
	// GCEmailTokens 删过期 24 小时以上的令牌（beforeMs = now − 24 小时）。
	GCEmailTokens(ctx context.Context, tenantID string, beforeMs int64) (int, error)
}

// 其余仓储先立名字，方法随各批加入。
type (
	GrantRepo    interface{}
	TrustRepo    interface{}
	SettingsRepo interface{}
	IssuedRepo   interface{}
)

// AuditRepo 是审计与安全日志（只追加）。
type AuditRepo interface {
	// Append 追加一行；event_id 空则由调用方先生成。
	Append(ctx context.Context, tenantID string, entry AuditEntry) error
	// SeenLogin 报告账号从 sinceMs 起有没有在同名设备、同一 IP 前缀登录（或注册）过：新设备提醒用。
	SeenLogin(ctx context.Context, tenantID, accountID, deviceName, ipPrefix string, sinceMs int64) (bool, error)
}

// JobRepo 是业务事务里的作业入队与发件箱。
type JobRepo interface {
	// Cancel 删掉某种作业里 dedupe_key 相符、还没完成也没被领走的（恢复团队时撤掉清除作业）。
	Cancel(ctx context.Context, tenantID, kind, dedupeKey string) error
	// Enqueue 入队一个作业（ON CONFLICT DO NOTHING）；同一事务里 pg_notify('cloud_jobs')，随提交送达。
	// 被 dedupe / slot 挡掉答 inserted = false。
	Enqueue(ctx context.Context, tenantID string, job NewJob, nowMs int64) (jobID int64, inserted bool, err error)
	// InsertOutbox 写一行发件箱。
	InsertOutbox(ctx context.Context, tenantID string, email OutboxEmail) error
	// Outbox 读发件箱的一行并加行锁（FOR UPDATE）；没有答 nil。
	Outbox(ctx context.Context, tenantID, emailID string) (*OutboxEmail, error)
	// MarkOutboxSent 记发出时刻；MarkOutboxFailed 记最终失败；DeleteOutbox 删（无需发送时）。
	MarkOutboxSent(ctx context.Context, tenantID, emailID string, nowMs int64) error
	MarkOutboxFailed(ctx context.Context, tenantID, emailID string, nowMs int64) error
	DeleteOutbox(ctx context.Context, tenantID, emailID string) error
	// CancelUnsentMail 删账号名下还没发出的某个模板的发件（作业随后发现发件箱行没了就结束）：
	// 账号上的敏感状态变了（改口令、改邮箱），排在队里的旧链接邮件不能再发出去。
	CancelUnsentMail(ctx context.Context, tenantID, accountID, template string) (int, error)
	// TrimOutbox 只留最新的 keep 封还没发也没判失败的发件，更早的连同它们没做完的作业一起删；答删了几封。
	TrimOutbox(ctx context.Context, tenantID string, keep int) (int, error)
	// GCOutbox 删发出或失败早于 beforeMs 的行（outbox.gc，30 天）；GCJobs 删完成早于 doneBeforeMs、
	// 失败早于 failedBeforeMs 的作业（jobs.gc，7 天与 30 天）。
	GCOutbox(ctx context.Context, tenantID string, beforeMs int64) (int, error)
	GCJobs(ctx context.Context, tenantID string, doneBeforeMs, failedBeforeMs int64) (int, error)
}

// JobQueue 是作业队列的非事务操作（设计 §6.1）。
type JobQueue interface {
	// Claim 领取最多 limit 个到期、未被租用、尝试次数未满且种类在 kinds 里（本实例登记过处理器）的作业：
	// 租约到 nowMs+leaseMs，attempts 加一。
	Claim(ctx context.Context, tenantID, worker string, kinds []string, nowMs, leaseMs int64, limit int) ([]Job, error)
	// Release 把本实例持有的作业放回队列、不计这次尝试（关闭中被打断、主动延后）：释放租约，attempts 减一；
	// retryAtMs 大于 0 时把下次执行推到不早于它。
	Release(ctx context.Context, tenantID string, jobID int64, worker string, retryAtMs int64) error
	// SweepExhausted 把尝试次数已满、租约又过期的作业（最后一次执行时实例崩了）置 failed_at_ms、
	// last_error = 'lease_expired'；答处理了几个。
	SweepExhausted(ctx context.Context, tenantID string, nowMs int64) (int, error)
	// Extend 续租；租约已丢（不是 worker 持有）答 false。
	Extend(ctx context.Context, tenantID string, jobID int64, worker string, nowMs, leaseMs int64) (bool, error)
	// Complete 置完成；租约已丢答 false。
	Complete(ctx context.Context, tenantID string, jobID int64, worker string, nowMs int64) (bool, error)
	// Fail 记一次失败：未到 max_attempts 按退避重排，到了置 failed_at_ms（dead = true）。lastError 只是类名或错误码。
	Fail(ctx context.Context, tenantID string, job Job, worker string, nowMs int64, lastError string) (dead bool, err error)
	// Listen 订阅 cloud_jobs 的 NOTIFY，每次通知（与重连后）调 wake；阻塞到 ctx 结束。
	Listen(ctx context.Context, wake func())
}
