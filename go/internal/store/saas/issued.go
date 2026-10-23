package saas

import "context"

// IssuedToken 是签发索引（cloud_issued_tokens）里的一条：只有标识，没有令牌本身。
type IssuedToken struct {
	JTI string
	// AccountID 是令牌的 sub；隧道令牌属于主机，记 "-"。
	AccountID string
	SourceID  string
	DeviceID  string
	ExpMs     int64
}

// IssuedFilter 选出要撤销的已签令牌：给了的字段都要对上（取交集）；全空不选任何令牌。
type IssuedFilter struct {
	JTI           string
	AccountID     string
	AccountPrefix string
	SourceID      string
	DeviceID      string
	// KeepDeviceID 非空时这台设备的令牌除外。
	KeepDeviceID string
}

// Empty 报告过滤是否什么都没给（不选任何令牌）。
func (f IssuedFilter) Empty() bool {
	return f.JTI == "" && f.AccountID == "" && f.AccountPrefix == "" && f.SourceID == "" && f.DeviceID == ""
}

// IssuedRepo 是签发索引与撤销表（设计 §7.2）。索引的查询、重放、清理一律带 tenant_id；
// 撤销表 relay_revoked_tokens 是 0002 的表，只有 jti 与到期时间。
type IssuedRepo interface {
	// Insert 记一枚已签令牌（同 jti 已有则不动）。
	Insert(ctx context.Context, tenantID string, token IssuedToken) error
	// Revoke 选出符合过滤的未到期令牌，把 jti 写进撤销表（已有则不动），并从签发索引里删掉；答被撤销的令牌。
	Revoke(ctx context.Context, tenantID string, filter IssuedFilter, nowMs int64) ([]IssuedToken, error)
	// RevokeToken 直接把一枚 jti 写进撤销表（已有则不动），不经签发索引：节点令牌（nodes revoke）没有账号与主机。
	RevokeToken(ctx context.Context, jti string, expMs int64) error
	// Active 是签发索引里未到期的令牌（serve 启动与 Redis 重连时重建 cloud:issued:*）。
	Active(ctx context.Context, tenantID string, nowMs int64) ([]IssuedToken, error)
	// Revoked 是撤销表里未到期的令牌（重放进 Redis）。
	Revoked(ctx context.Context, nowMs int64) ([]IssuedToken, error)
	// GC 删到期的签发索引与撤销行（issued.gc），答删了几行。
	GC(ctx context.Context, tenantID string, nowMs int64) (int, error)
}
