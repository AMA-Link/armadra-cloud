package core

import (
	"context"
	"sync"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/identity"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// 账号与登录（中继设计 §6.2，平台设计 §17.3）。
//
// 登录：按 IP 限流（login.ip，20 次 / 分钟）→ 人机挑战（配置了才有，cloud-api §16；失败不计入
// 失败次数）→ 按账号名找账号 → 锁定检查 → scrypt 比对（定时安全）→ 失败计数 / 锁定 → 成功清零
// → 建设备 → 签会话。
//
// 锁定与 core 的 identity/throttle.ts 同一算法（常量来自协议包的 THROTTLE）：连续 5 次失败锁
// 1 分钟，此后每多一次翻倍，封顶 15 分钟；最后一次失败一小时后清零；锁着的时候不比对口令，直接答
// account_locked 带重试等待。账号名不对也计数（只在内存），不泄露账号名是否存在。
//
// 改口令（cloud-api §18）：账号本人的会话 + 旧口令。按 IP 限流（与登录同一个桶）→ 挑战（scope
// 含 auth.changePassword 才要）→ 锁定检查 → 新口令策略 → 比对旧口令（失败计入同一个锁定计数）→
// 换口令记录、清锁、撤销本人除当前设备以外的全部设备 → 当前设备的刷新令牌换新。personal passwd
// 走同一个 SetPassword，只是不留设备。

// Lockout 是登录失败的计数与锁。
type Lockout struct {
	Failures        int64
	LockedUntilMs   int64
	LastFailureAtMs int64
}

// lockDurationMs：第 lockThreshold 次失败起锁 lockBaseMs，之后每次翻倍，封顶 lockMaxMs。
func lockDurationMs(failures int64) int64 {
	if failures < gen.IdentityThrottleLockThreshold {
		return 0
	}
	doublings := min(failures-gen.IdentityThrottleLockThreshold, 16)
	return min(int64(gen.IdentityThrottleLockBaseMs)<<doublings, gen.IdentityThrottleLockMaxMs)
}

// NextLockout 是一次失败之后的计数与锁（纯函数）。
func NextLockout(previous Lockout, nowMs int64) Lockout {
	stale := previous.LockedUntilMs <= nowMs && nowMs-previous.LastFailureAtMs > gen.IdentityThrottleFailureWindowMs
	failures := previous.Failures + 1
	if stale {
		failures = 1
	}
	next := Lockout{Failures: failures, LastFailureAtMs: nowMs}
	if duration := lockDurationMs(failures); duration > 0 {
		next.LockedUntilMs = nowMs + duration
	}
	return next
}

const maxUnknownNames = 1_000

// burnPasswordCheck 是未知账号时的等时派生；测试里换成计数器，断言它确实被调用。
var burnPasswordCheck = identity.BurnPasswordCheck

// UnknownStore 是未知名字的失败计数的存放处：单进程用内存（缺省），多实例的 SaaS 用 Redis。
type UnknownStore interface {
	// Fail 记一次对 name 的失败登录：此刻已被锁着答 (锁到的时刻, true) 且不计数；否则按 NextLockout 记账，
	// 答 (0, false)。
	Fail(ctx context.Context, name string, nowMs int64) (lockedUntilMs int64, locked bool, err error)
}

type memoryUnknown struct {
	mu    sync.Mutex
	names map[string]Lockout
}

func (m *memoryUnknown) Fail(_ context.Context, name string, nowMs int64) (int64, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	previous := m.names[name]
	if previous.LockedUntilMs > nowMs {
		return previous.LockedUntilMs, true, nil
	}
	if m.names == nil || len(m.names) >= maxUnknownNames {
		m.names = map[string]Lockout{}
	}
	m.names[name] = NextLockout(previous, nowMs)
	return 0, false, nil
}

// NewMemoryUnknownStore 是进程内的未知名字失败计数（缺省，也是共享存储失败时的退路）。
func NewMemoryUnknownStore() UnknownStore { return &memoryUnknown{} }

// UnknownAccounts 是不存在的账号名的失败计数与锁定：与存在的账号走同一套 NextLockout，连续失败达阈值后
// 同样答 account_locked，所以锁定不暴露某个名字（SaaS：规范化邮箱）有没有注册。被锁着的名字也做一次口令
// 派生，答复时延与存在的账号一致。零值可用（内存计数）；设 Store 换成共享存储。
type UnknownAccounts struct {
	Store UnknownStore

	memory memoryUnknown
}

// Login 记一次对未知名字的登录失败：锁着答 account_locked（带重试等待），否则答 credentials_invalid。
func (u *UnknownAccounts) Login(ctx context.Context, nowMs func() int64, name, password string) error {
	name = truncateRunes(name, 128)
	store := u.Store
	if store == nil {
		store = &u.memory
	}
	burnPasswordCheck(password)
	now := nowMs()
	until, locked, err := store.Fail(ctx, name, now)
	if err != nil {
		return err
	}
	if locked {
		return FailAfter(gen.ErrAccountLocked, time.Duration(until-now)*time.Millisecond)
	}
	return Fail(gen.ErrCredentialsInvalid)
}

// LoginInput 是登录的入参。
type LoginInput struct {
	Account   string
	Password  string
	Device    DeviceInput
	Challenge *ChallengeToken
}

// ChangePasswordInput 是改口令的入参。
type ChangePasswordInput struct {
	// Password 是旧口令。
	Password    string
	NewPassword string
	Challenge   *ChallengeToken
}

// Accounts 是账号服务。
type Accounts struct {
	c *Context

	unknownNames UnknownAccounts
}

// NewAccounts 建账号服务。
func NewAccounts(c *Context) *Accounts {
	return &Accounts{c: c}
}

func lockedFor(lockout Lockout, nowMs int64) time.Duration {
	if lockout.LockedUntilMs > nowMs {
		return time.Duration(lockout.LockedUntilMs-nowMs) * time.Millisecond
	}
	return 0
}

func accountLockout(a *store.AccountRecord) Lockout {
	return Lockout{Failures: a.Failures, LockedUntilMs: a.LockedUntilMs, LastFailureAtMs: a.LastFailureAtMs}
}

func (a *Accounts) loginUnknown(ctx context.Context, name, password string) error {
	return a.unknownNames.Login(ctx, a.c.NowMs, name, password)
}

// recordFailure 失败一次：记进账号的锁定计数。
func (a *Accounts) recordFailure(ctx context.Context, accountID string, nowMs int64) error {
	return a.c.Ports.Accounts.Update(ctx, a.c.TenantID, accountID, func(draft *store.AccountRecord) error {
		if draft != nil {
			next := NextLockout(accountLockout(draft), nowMs)
			draft.Failures, draft.LockedUntilMs, draft.LastFailureAtMs = next.Failures, next.LockedUntilMs, next.LastFailureAtMs
		}
		return nil
	})
}

// VerifyPassword 比对账号的口令（登录的核心，按邮箱登录的 SaaS 复用）：锁着答 account_locked，不比对；
// 口令不对记一次失败（连续失败锁定）答 credentials_invalid；对了清零失败计数与锁。account 为 nil
// （没有这个账号）时仍做一次等时的口令派生，答 credentials_invalid，不泄露账号是否存在。
func VerifyPassword(ctx context.Context, c *Context, account *store.AccountRecord, password string, nowMs int64) error {
	if account == nil {
		burnPasswordCheck(password)
		return Fail(gen.ErrCredentialsInvalid)
	}
	if wait := lockedFor(accountLockout(account), nowMs); wait > 0 {
		// 锁着不比对口令，但也做一次等价的派生：答复时延与未知名字一致（不暴露谁注册过）。
		burnPasswordCheck(password)
		return FailAfter(gen.ErrAccountLocked, wait)
	}
	ok := identity.VerifyPassword(password, account.Password)
	failedAt := c.NowMs()
	if !ok {
		err := c.Ports.Accounts.Update(ctx, c.TenantID, account.AccountID, func(draft *store.AccountRecord) error {
			if draft != nil {
				next := NextLockout(accountLockout(draft), failedAt)
				draft.Failures, draft.LockedUntilMs, draft.LastFailureAtMs = next.Failures, next.LockedUntilMs, next.LastFailureAtMs
			}
			return nil
		})
		if err != nil {
			return err
		}
		return Fail(gen.ErrCredentialsInvalid)
	}
	return c.Ports.Accounts.Update(ctx, c.TenantID, account.AccountID, func(draft *store.AccountRecord) error {
		if draft != nil && (draft.Failures != 0 || draft.LockedUntilMs != 0) {
			draft.Failures, draft.LockedUntilMs = 0, 0
		}
		return nil
	})
}

// Login 登录，答新会话。
func (a *Accounts) Login(ctx context.Context, input LoginInput, ip string) (Session, error) {
	if err := a.c.hit(ctx, LimitLoginIP, ip); err != nil {
		return Session{}, err
	}
	if err := a.c.Challenge.Check(ctx, ScopeLogin, input.Challenge, ip); err != nil {
		return Session{}, err
	}
	account, err := a.c.Ports.Accounts.FindByName(ctx, a.c.TenantID, input.Account)
	if err != nil {
		return Session{}, err
	}
	if account == nil {
		return Session{}, a.loginUnknown(ctx, input.Account, input.Password)
	}
	if err := VerifyPassword(ctx, a.c, account, input.Password, a.c.NowMs()); err != nil {
		return Session{}, err
	}
	nowMs := a.c.NowMs()
	device, refresh, err := CreateDevice(ctx, a.c, account.AccountID, input.Device, nowMs, 0)
	if err != nil {
		return Session{}, err
	}
	return SessionFor(ctx, a.c, device, refresh)
}

// ChangePassword 改口令（cloud-api §18）：成功答当前设备换了刷新令牌的新会话；其余本人设备全部撤销。
func (a *Accounts) ChangePassword(ctx context.Context, caller Caller, input ChangePasswordInput, ip string) (Session, error) {
	if caller.Role != RoleOwner {
		return Session{}, Fail(gen.ErrForbidden)
	}
	if err := a.c.hit(ctx, LimitLoginIP, ip); err != nil {
		return Session{}, err
	}
	if err := a.c.Challenge.Check(ctx, ScopeChangePassword, input.Challenge, ip); err != nil {
		return Session{}, err
	}
	account, err := a.c.Ports.Accounts.Get(ctx, a.c.TenantID, caller.Subject)
	if err != nil {
		return Session{}, err
	}
	if account == nil {
		return Session{}, Fail(gen.ErrUnauthenticated)
	}
	if wait := lockedFor(accountLockout(account), a.c.NowMs()); wait > 0 {
		return Session{}, FailAfter(gen.ErrAccountLocked, wait)
	}
	if err := CheckPassword(a.c, input.NewPassword, account.Name); err != nil {
		return Session{}, err
	}
	if !identity.VerifyPassword(input.Password, account.Password) {
		if err := a.recordFailure(ctx, account.AccountID, a.c.NowMs()); err != nil {
			return Session{}, err
		}
		return Session{}, Fail(gen.ErrCredentialsInvalid)
	}
	nowMs := a.c.NowMs()
	if err := SetPassword(ctx, a.c, account.AccountID, account.Name, input.NewPassword, nowMs, caller.DeviceID); err != nil {
		return Session{}, err
	}
	// 当前设备的刷新令牌也换新：旧的哪怕泄露过也就此作废（不当作重放，不撤销这台）。
	rotated, next, found, err := RotateRefresh(ctx, a.c, caller.DeviceID, nowMs)
	if err != nil {
		return Session{}, err
	}
	if !found {
		return Session{}, Fail(gen.ErrUnauthenticated)
	}
	return SessionFor(ctx, a.c, rotated, next)
}

// RotateRefresh 给一台设备换新刷新令牌：旧的作废（不留作重放检测的「上一个」，不撤销这台）。
// 答换完的设备记录与新刷新令牌明文；设备不在或已撤销 found 为 false。改口令、找回口令之后用。
func RotateRefresh(ctx context.Context, c *Context, deviceID string, nowMs int64) (device store.DeviceRecord, refreshToken string, found bool, err error) {
	err = c.Ports.Devices.Update(ctx, c.TenantID, deviceID, func(draft *store.DeviceRecord) error {
		if draft == nil || draft.RevokedAtMs != nil {
			return nil
		}
		refreshToken = draft.DeviceID + "." + RandomToken(32)
		draft.RefreshHash = RefreshHash(refreshToken)
		draft.PreviousRefreshHash = nil
		draft.Rotation++
		draft.LastSeenAtMs = &nowMs
		device, found = *draft, true
		return nil
	})
	return device, refreshToken, found, err
}

func truncateRunes(s string, n int) string {
	// TypeScript 的 slice 按 UTF-16 码元，这里按码点：只影响超长账号名的内存计数键。
	runes := []rune(s)
	if len(runes) > n {
		return string(runes[:n])
	}
	return s
}

// CheckPassword：口令策略不过就报（码与 core 同名）。
func CheckPassword(c *Context, password, name string) error {
	return CheckPasswordNames(c, password, []string{name})
}

// CheckPasswordNames 同 CheckPassword，口令不得包含 names 里的任何一个（SaaS：邮箱本地部分与显示名）。
func CheckPasswordNames(c *Context, password string, names []string) error {
	if rule := c.policy()(password, names); rule != "" {
		return Fail(rule)
	}
	return nil
}

// SetPassword 是换口令的唯一实现（API 与 personal passwd 共用）：过策略、重新派生、清锁，撤销
// 账号本人的设备（keepDeviceID 那台除外；访客不受影响，由链接管）。账号不存在答 not_found。
func SetPassword(ctx context.Context, c *Context, accountID, name, password string, nowMs int64, keepDeviceID string) error {
	return SetPasswordNames(ctx, c, accountID, []string{name}, password, nowMs, keepDeviceID)
}

// SetPasswordNames 同 SetPassword，策略里的「不得包含」取 names；成功后经撤销钩子通知设备已批量撤销。
func SetPasswordNames(ctx context.Context, c *Context, accountID string, names []string, password string, nowMs int64, keepDeviceID string) error {
	if err := CheckPasswordNames(c, password, names); err != nil {
		return err
	}
	record, err := identity.HashPassword(password)
	if err != nil {
		return err
	}
	found, err := c.Ports.Accounts.ResetPassword(ctx, c.TenantID, accountID, record, nowMs, keepDeviceID)
	if err != nil {
		return err
	}
	if !found {
		return Fail(gen.ErrNotFound)
	}
	c.devicesRevoked(ctx, accountID, keepDeviceID)
	return nil
}

// ChangePasswordByName 是运维改口令（personal passwd）：按账号名找到账号，撤销本人的全部设备。
func ChangePasswordByName(ctx context.Context, c *Context, name, password string, nowMs int64) error {
	if err := CheckPassword(c, password, name); err != nil {
		return err
	}
	account, err := c.Ports.Accounts.FindByName(ctx, c.TenantID, name)
	if err != nil {
		return err
	}
	if account == nil {
		return Fail(gen.ErrNotFound)
	}
	return SetPassword(ctx, c, account.AccountID, account.Name, password, nowMs, "")
}
