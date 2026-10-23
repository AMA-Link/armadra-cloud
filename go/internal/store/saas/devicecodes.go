package saas

import "context"

// 设备码的状态（迁移 0004 的 CHECK）。
const (
	DeviceCodePending  = "pending"
	DeviceCodeApproved = "approved"
	DeviceCodeDenied   = "denied"
	DeviceCodeConsumed = "consumed"
	DeviceCodeExpired  = "expired"
)

// DeviceCode 是 device_codes 的一行（设计 §3.4、能力设计 §11.5）。DeviceCodeHash 是设备码明文的哈希，
// 明文只在 deviceStart 的答复里出现一次，库里没有。
type DeviceCode struct {
	DeviceCodeHash string
	UserCode       string
	Status         string
	Platform       string
	Name           string
	PushPublicKey  *string
	// AccountID 是批准（或拒绝）的账号；pending 时为空。
	AccountID    *string
	IPPrefix     string
	CreatedAtMs  int64
	ExpiresAtMs  int64
	LastPollAtMs *int64
	DecidedAtMs  *int64
}

// DeviceCodeRepo 是设备码的仓储。读都加行锁（FOR UPDATE）：批准、拒绝、轮询兑换都是读-改-写，
// 并发时按行排队，设备只会被兑换一次。
type DeviceCodeRepo interface {
	// Create 插一行 pending。同租户下已有同 userCode 的 pending / approved 行时先把其中已过期的落成 expired；
	// 仍冲突（还有有效的）返回 false，调用方换一个 userCode 重抽。
	Create(ctx context.Context, tenantID string, code DeviceCode, nowMs int64) (inserted bool, err error)
	// Find 按设备码哈希不加锁读；兑换时先用它拿到 account_id，按「账号行 → 其余」的顺序加锁后再 Get 复核。
	Find(ctx context.Context, tenantID, deviceCodeHash string) (*DeviceCode, error)
	// Get 按设备码哈希读并锁行；没有答 nil。
	Get(ctx context.Context, tenantID, deviceCodeHash string) (*DeviceCode, error)
	// GetByUserCode 按用户码读并锁行，同一用户码有多行（旧的已结案）时取最新创建的；没有答 nil。
	GetByUserCode(ctx context.Context, tenantID, userCode string) (*DeviceCode, error)
	// Update 写回状态、账号、轮询与决定时间（行锁已在手）。
	Update(ctx context.Context, tenantID string, code DeviceCode) error
	// DeleteExpiredBefore 删 expires_at_ms 早于 beforeMs 的行，答删了几行（作业 deviceCodes.gc，S2 接）。
	DeleteExpiredBefore(ctx context.Context, tenantID string, beforeMs int64) (int64, error)
}
