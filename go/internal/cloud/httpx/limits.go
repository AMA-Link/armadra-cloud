package httpx

import (
	"context"
	"strings"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/clientip"
	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// 速率限流的种类（设计 §5.4）。桶在 Redis 的 cloud:limit: 前缀下，窗口固定。
const (
	LimitLoginIP      control.LimitKind = "login.ip"
	LimitLoginAcct    control.LimitKind = "login.acct"
	LimitRegisterIP   control.LimitKind = "register.ip"
	LimitResetIP      control.LimitKind = "reset.ip"
	LimitResetEmail   control.LimitKind = "reset.email"
	LimitVerifyAcct   control.LimitKind = "verify.acct"
	LimitTokenIP      control.LimitKind = "token.ip"
	LimitDeviceStart  control.LimitKind = "device.start.ip"
	LimitDeviceGet    control.LimitKind = "device.get.ip"
	LimitDevicePoll   control.LimitKind = "device.poll.ip"
	LimitDeviceGuess  control.LimitKind = "device.guess.acct"
	LimitStepUpDevice control.LimitKind = "stepup.dev"
	LimitMFAAcct      control.LimitKind = "mfa.acct"
	LimitAssertAcct   control.LimitKind = "assert.acct"
	LimitTrustSource  control.LimitKind = "trust.src"
	LimitInviteAcct   control.LimitKind = "invite.acct"
	LimitInviteResend control.LimitKind = "invite.resend"
	LimitLinkAccept   control.LimitKind = "link.accept.ip"
)

// LimitPrefix 是 Redis 里限流桶的键前缀。
const LimitPrefix = "cloud:limit:"

// Rules 是 §5.4 的额度表。
var Rules = map[control.LimitKind]control.LimitRule{
	LimitLoginIP:      {Max: 20, Window: time.Minute},
	LimitLoginAcct:    {Max: 20, Window: time.Minute},
	LimitRegisterIP:   {Max: 10, Window: time.Hour},
	LimitResetIP:      {Max: 10, Window: time.Hour},
	LimitResetEmail:   {Max: 3, Window: time.Hour},
	LimitVerifyAcct:   {Max: 3, Window: time.Hour},
	LimitTokenIP:      {Max: 30, Window: time.Minute},
	LimitDeviceStart:  {Max: 30, Window: time.Hour},
	LimitDeviceGet:    {Max: 30, Window: time.Minute},
	LimitDevicePoll:   {Max: 120, Window: time.Minute},
	LimitDeviceGuess:  {Max: 10, Window: time.Hour},
	LimitStepUpDevice: {Max: 10, Window: 10 * time.Minute},
	LimitMFAAcct:      {Max: 10, Window: 10 * time.Minute},
	LimitAssertAcct:   {Max: 60, Window: time.Minute},
	LimitTrustSource:  {Max: 30, Window: time.Minute},
	LimitInviteAcct:   {Max: 50, Window: time.Hour},
	LimitInviteResend: {Max: 5, Window: time.Hour},
	LimitLinkAccept:   {Max: 20, Window: time.Minute},
}

// Limits 是控制面的限流器：包一个 control.Limiter（Redis 版或测试用的内存版），既满足 core.RateLimiter
// （核心里的 login.ip、link.accept.ip），也给 SaaS 自己的种类用。
type Limits struct{ limiter control.Limiter }

var _ core.RateLimiter = (*Limits)(nil)

// NewLimits 包一个限流器。
func NewLimits(limiter control.Limiter) *Limits { return &Limits{limiter: limiter} }

// limitKey 规整限流键：种类以 .ip 结尾的键是客户端 IP，IPv6 截到 /64（一个终端用户常有整段 /64，
// 不截就能靠换地址绕开限流）。只影响限流键，不影响别处用到的 IP。
func limitKey(kind control.LimitKind, key string) string {
	if strings.HasSuffix(string(kind), ".ip") {
		return clientip.RateKey(key)
	}
	return key
}

// Hit 实现 core.RateLimiter。
func (l *Limits) Hit(ctx context.Context, kind core.LimitKind, key string) (core.LimitResult, error) {
	result, err := l.limiter.Hit(ctx, control.LimitKind(kind), limitKey(control.LimitKind(kind), key))
	return core.LimitResult{Allowed: result.Allowed, RetryAfter: result.RetryAfter}, err
}

// Check 记一次，超限答 429 rate_limited（带 details.retryAfterMs 与 Retry-After）。
func (l *Limits) Check(ctx context.Context, kind control.LimitKind, key string) error {
	result, err := l.limiter.Hit(ctx, kind, limitKey(kind, key))
	if err != nil {
		return err
	}
	if !result.Allowed {
		return core.FailAfter(gen.ErrRateLimited, result.RetryAfter)
	}
	return nil
}

// peeker 是能只读桶的限流器（Redis 版）。
type peeker interface {
	Peek(ctx context.Context, kind control.LimitKind, key string) (control.LimitResult, error)
}

// Blocked 只读检查：桶已满答 429 rate_limited，不计数。配 Record 做「只统计失败」的桶（猜码：
// 同一账号换 IP 也逃不掉）。限流器不支持只读时放行，由 Record 的计数兜底。
func (l *Limits) Blocked(ctx context.Context, kind control.LimitKind, key string) error {
	p, ok := l.limiter.(peeker)
	if !ok {
		return nil
	}
	result, err := p.Peek(ctx, kind, limitKey(kind, key))
	if err != nil {
		return err
	}
	if !result.Allowed {
		return core.FailAfter(gen.ErrRateLimited, result.RetryAfter)
	}
	return nil
}

// Record 记一次失败，不判断结果。
func (l *Limits) Record(ctx context.Context, kind control.LimitKind, key string) error {
	_, err := l.limiter.Hit(ctx, kind, limitKey(kind, key))
	return err
}
