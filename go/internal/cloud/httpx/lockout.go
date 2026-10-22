package httpx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

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

// UnknownLockouts 是 core.UnknownStore 的 Redis 实现：未注册邮箱（按规范化邮箱的 SHA-256）的失败计数与
// 锁定放在 Redis，多个实例共享，所以锁定不暴露邮箱有没有注册，也不因实例不同而不一致。
// Redis 出错时退回 Fallback（进程内计数），不让登录因为 Redis 抖动而答 500。
type UnknownLockouts struct {
	Client   goredis.UniversalClient
	Fallback core.UnknownStore
	OnError  func(error)

	script *goredis.Script
}

var _ core.UnknownStore = (*UnknownLockouts)(nil)

// NewUnknownLockouts 建 Redis 版未知名字计数。
func NewUnknownLockouts(client goredis.UniversalClient, onError func(error)) *UnknownLockouts {
	return &UnknownLockouts{Client: client, Fallback: core.NewMemoryUnknownStore(), OnError: onError, script: goredis.NewScript(failScript)}
}

// Fail 实现 core.UnknownStore。
func (u *UnknownLockouts) Fail(ctx context.Context, name string, nowMs int64) (int64, bool, error) {
	sum := sha256.Sum256([]byte(name))
	reply, err := u.script.Run(ctx, u.Client, []string{LockoutPrefix + hex.EncodeToString(sum[:])},
		nowMs, gen.IdentityThrottleLockThreshold, gen.IdentityThrottleLockBaseMs, gen.IdentityThrottleLockMaxMs,
		gen.IdentityThrottleFailureWindowMs, gen.IdentityThrottleFailureWindowMs+gen.IdentityThrottleLockMaxMs).Slice()
	if err != nil || len(reply) != 2 {
		if u.OnError != nil && err != nil {
			u.OnError(err)
		}
		return u.Fallback.Fail(ctx, name, nowMs)
	}
	locked, _ := reply[0].(int64)
	until, _ := reply[1].(int64)
	return until, locked == 1, nil
}
