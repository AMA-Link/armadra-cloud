package httpx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync/atomic"

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
//
// 已知退化：回落期间各实例各记各的，且回落前 Redis 里已有的计数不可见——多实例下，故障窗口里「已注册」的
// 账号（计数在账号行，PostgreSQL）和「未注册」的邮箱（计数回落到各实例内存）达到锁定的次数可能不同，
// 攻击者在这个窗口里可能据此区分邮箱是否注册。每次回落都报告（OnError）并计数（Fallbacks，指标
// armadra_cloud_lockout_fallback_total），运维据此告警；上多实例前要定回落策略（设计 §9.1 S8 / S9）。
type UnknownLockouts struct {
	Client   goredis.UniversalClient
	Fallback core.UnknownStore
	OnError  func(error)

	script    *goredis.Script
	fallbacks atomic.Int64
}

var _ core.UnknownStore = (*UnknownLockouts)(nil)

// NewUnknownLockouts 建 Redis 版未知名字计数。
func NewUnknownLockouts(client goredis.UniversalClient, onError func(error)) *UnknownLockouts {
	return &UnknownLockouts{Client: client, Fallback: core.NewMemoryUnknownStore(), OnError: onError, script: goredis.NewScript(failScript)}
}

// Fallbacks 是回落到进程内计数的次数（指标用）。
func (u *UnknownLockouts) Fallbacks() int64 { return u.fallbacks.Load() }

// Fail 实现 core.UnknownStore。
func (u *UnknownLockouts) Fail(ctx context.Context, name string, nowMs int64) (int64, bool, error) {
	sum := sha256.Sum256([]byte(name))
	reply, err := u.script.Run(ctx, u.Client, []string{LockoutPrefix + hex.EncodeToString(sum[:])},
		nowMs, gen.IdentityThrottleLockThreshold, gen.IdentityThrottleLockBaseMs, gen.IdentityThrottleLockMaxMs,
		gen.IdentityThrottleFailureWindowMs, gen.IdentityThrottleFailureWindowMs+gen.IdentityThrottleLockMaxMs).Slice()
	if err != nil || len(reply) != 2 {
		u.fallbacks.Add(1)
		if u.OnError != nil && err != nil {
			u.OnError(err)
		}
		return u.Fallback.Fail(ctx, name, nowMs)
	}
	locked, _ := reply[0].(int64)
	until, _ := reply[1].(int64)
	return until, locked == 1, nil
}
