// Package saas 是 SaaS 的存储端口（设计 §4.11），只有 PostgreSQL 实现（postgres.DB.SaaS()）。
//
// 每个仓储方法的第一个参数是 tenantID（或已含它的 TeamScope）；团队范围的方法收 TeamScope，
// 查询必写 team_id = $2。核心端口（store.Ports）与它共用同一个连接池；需要同事务写核心表时，
// 由 postgres 包把核心端口绑到同一个事务。仓储方法随各批（S1b 起）加入，本批只立骨架。
package saas

import (
	"context"

	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas/teamscope"
)

// TeamRole 与 TeamScope 是 teamscope 包类型的别名，方便端口签名使用。
type (
	TeamRole  = teamscope.Role
	TeamScope = teamscope.Scope
)

// Store 是 SaaS 的存储端口。
type Store interface {
	// Tx 在一个读已提交事务里执行 fn；fn 返回错误即回滚。提交后按序执行 tx.AfterCommit 登记的回调
	// （发 Redis 撤销、节点命令、事件）。死锁与序列化失败自动重试 3 次（fn 会被重新执行，
	// 重试前丢弃上一次登记的回调）。fn 与提交返回的约束冲突已映射成 *Error。
	Tx(ctx context.Context, fn func(tx Tx) error) error
	// Read 是不开事务的只读查询。
	Read() Reader
}

// Reader 是只读查询；Tx 也实现它。
type Reader interface {
	// Ping 探活：连接池里能取到连接并执行一条语句。
	Ping(ctx context.Context) error
}

// Tx 是一个事务里的全部仓储。
type Tx interface {
	Reader
	Accounts() AccountRepo // 邮箱、状态、范围、凭据、邮件令牌、设备码
	Teams() TeamRepo       // 团队、成员、设置；LockTeam / LockAccount
	Invitations() InvitationRepo
	Hosts() HostRepo // source_spaces、注册令牌（带空间）、登记事务
	Grants() GrantRepo
	Trust() TrustRepo
	Audit() AuditRepo       // 只追加
	Jobs() JobRepo          // Enqueue（含 dedupe / slot、NOTIFY）、发件箱
	Settings() SettingsRepo // platform_settings、entitlements
	Issued() IssuedRepo     // cloud_issued_tokens、relay_revoked_tokens
	// AfterCommit 登记一个提交成功之后才执行的回调；回滚、重试时丢弃。回调里的失败只能记录，不能回滚事务。
	AfterCommit(fn func(ctx context.Context))
}

// AccountRepo 是账号相关的仓储（方法随 S1b 加入）。
type AccountRepo interface {
	// LockAccount 对账号行 SELECT … FOR UPDATE，答它存不存在。
	LockAccount(ctx context.Context, tenantID, accountID string) (found bool, err error)
}

// TeamRepo 是团队相关的仓储（方法随 S4 加入）。团队范围的方法收 TeamScope，不收裸 teamID。
type TeamRepo interface {
	// LockTeam 对团队行 SELECT … FOR UPDATE，答它存不存在。改成员前先锁它，
	// 与 team_owner_guard 触发器用同一把锁（设计 §3.4 0005）。
	LockTeam(ctx context.Context, tenantID, teamID string) (found bool, err error)
}

// 其余仓储先立名字，方法随各批加入。
type (
	InvitationRepo interface{}
	HostRepo       interface{}
	GrantRepo      interface{}
	TrustRepo      interface{}
	AuditRepo      interface{}
	JobRepo        interface{}
	SettingsRepo   interface{}
	IssuedRepo     interface{}
)
