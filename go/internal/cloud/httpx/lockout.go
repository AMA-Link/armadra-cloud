package httpx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"sync/atomic"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// LockoutPrefix 是未知邮箱失败计数在 Redis 里的键前缀。
const LockoutPrefix = "cloud:lockout:"

// failScript 记一次失败，套与账号行相同的 NextLockout（常量取自协议包的 THROTTLE）：
// 此刻锁着答 {1, 锁到的时刻} 且不计数；否则按「最后一次失败超过窗口则清零」计数，达阈值起锁 base，
// 之后每多一次翻倍，封顶 max。答 {0, 0}。
const failScript = `
local h = redis.call('HMGET', KEYS[1], 'failures', 'lockedUntil', 'lastFailure')
local failures = tonumber(h[1]) or 0
local lockedUntil = tonumber(h[2]) or 0
local last = tonumber(h[3]) or 0
local now = tonumber(ARGV[1])
if lockedUntil > now then return { 1, lockedUntil } end
if now - last > tonumber(ARGV[5]) then failures = 0 end
failures = failures + 1
lockedUntil = 0
local threshold = tonumber(ARGV[2])
if failures >= threshold then
  local doublings = math.min(failures - threshold, 16)
  lockedUntil = now + math.min(tonumber(ARGV[3]) * (2 ^ doublings), tonumber(ARGV[4]))
end
redis.call('HSET', KEYS[1], 'failures', failures, 'lastFailure', now, 'lockedUntil', string.format('%.0f', lockedUntil))
redis.call('PEXPIRE', KEYS[1], ARGV[6])
return { 0, 0 }`

// 故障时的失败关闭（设计 §4.3）：计数共享在 Redis，Redis 不可用时已注册账号（计数在账号行）与未注册邮箱
// （计数在 Redis）的锁定行为必然分叉——任何回落到各实例进程内的计数都会在多实例下暴露邮箱是否注册。
// 所以不回落：auth.login 在分出「已注册 / 未注册」之前先过 Guard（对 Redis 做一次探活），Redis 不可用时
// 对两类邮箱一律答 503 unavailable（带重试等待），不比对口令、不计数；分叉之后的最终答复同样复核一次 Guard
// （已注册的路径在口令比对失败后、未注册的路径在 Fail 出错时），所以探活与记账之间 Redis 掉线也不会让两类邮箱
// 的答案不同。登录因此在 Redis 故障期间整体不可用（Redis 本来也是 /ready 的硬依赖，实例会被摘出轮换）；已登录
// 的会话、刷新与其他过程不受影响。

// guardTimeout 是探活的超时；guardBackoff 是出错之后快速失败的时长（别去打一个正在出问题的 Redis）。
const (
	guardTimeout = 500 * time.Millisecond
	guardBackoff = time.Second
	// RetryAfterUnavailable 是故障时 503 的重试等待。
	RetryAfterUnavailable = 5 * time.Second
)

// UnknownLockouts 是 core.UnknownStore 的 Redis 实现：未注册邮箱（按规范化邮箱的 SHA-256）的失败计数与
// 锁定放在 Redis，多个实例共享，所以锁定不暴露邮箱有没有注册，也不因实例不同而不一致。
// Redis 出错时失败关闭（见上）：Fail 答 503 unavailable，每次记警告日志并计入指标
// armadra_cloud_lockout_unavailable_total，运维据此告警。
type UnknownLockouts struct {
	Client  goredis.UniversalClient
	OnError func(error)
	// Now 缺省 time.Now（探活的快速失败窗口用）。
	Now func() time.Time

	script      *goredis.Script
	unavailable atomic.Int64

	mu        sync.Mutex
	downUntil time.Time
}

var _ core.UnknownStore = (*UnknownLockouts)(nil)

// NewUnknownLockouts 建 Redis 版未知名字计数。
func NewUnknownLockouts(client goredis.UniversalClient, onError func(error)) *UnknownLockouts {
	return &UnknownLockouts{Client: client, OnError: onError, Now: time.Now, script: goredis.NewScript(failScript)}
}

// Unavailable 是因 Redis 不可用而拒绝的次数（指标用）。
func (u *UnknownLockouts) Unavailable() int64 { return u.unavailable.Load() }

func (u *UnknownLockouts) now() time.Time {
	if u.Now == nil {
		return time.Now()
	}
	return u.Now()
}

// markDown 记一次 Redis 出错：之后一小段时间内 Guard 直接失败。
func (u *UnknownLockouts) markDown(err error) error {
	u.unavailable.Add(1)
	if u.OnError != nil && err != nil {
		u.OnError(err)
	}
	u.mu.Lock()
	u.downUntil = u.now().Add(guardBackoff)
	u.mu.Unlock()
	return core.FailAfter(gen.ErrUnavailable, RetryAfterUnavailable)
}

// Guard 探活 Redis：不可用答 503 unavailable（带重试等待）。登录在分出已注册 / 未注册之前调用，
// 分叉之后的最终答复前再调一次（设计 §4.3）。
func (u *UnknownLockouts) Guard(ctx context.Context) error {
	u.mu.Lock()
	down := u.now().Before(u.downUntil)
	u.mu.Unlock()
	if down {
		u.unavailable.Add(1)
		return core.FailAfter(gen.ErrUnavailable, RetryAfterUnavailable)
	}
	probe, cancel := context.WithTimeout(ctx, guardTimeout)
	defer cancel()
	// 写探活，不是 PING：Redis 拒写（OOM、READONLY、MISCONF）而 PING 照常答时，未注册路径的记账脚本会失败、
	// 已注册路径却不碰 Redis，两类邮箱就能被区分。探活与记账用同一个键空间、同样是写命令。
	if err := u.Client.Set(probe, LockoutPrefix+"probe", "1", 5*time.Second).Err(); err != nil {
		return u.markDown(err)
	}
	return nil
}

// Fail 实现 core.UnknownStore。Redis 出错答 503 unavailable，不计数也不回落。
func (u *UnknownLockouts) Fail(ctx context.Context, name string, nowMs int64) (int64, bool, error) {
	sum := sha256.Sum256([]byte(name))
	reply, err := u.script.Run(ctx, u.Client, []string{LockoutPrefix + hex.EncodeToString(sum[:])},
		nowMs, gen.IdentityThrottleLockThreshold, gen.IdentityThrottleLockBaseMs, gen.IdentityThrottleLockMaxMs,
		gen.IdentityThrottleFailureWindowMs, gen.IdentityThrottleFailureWindowMs+gen.IdentityThrottleLockMaxMs).Slice()
	if err != nil || len(reply) != 2 {
		return 0, false, u.markDown(err)
	}
	locked, _ := reply[0].(int64)
	until, _ := reply[1].(int64)
	return until, locked == 1, nil
}
