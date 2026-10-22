package core

import (
	"context"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// LimitKind 是控制面用到的限流种类（与中继内核的限流种类同名）。
type LimitKind string

// 限流种类。
const (
	LimitLoginIP      LimitKind = "login.ip"
	LimitLinkAcceptIP LimitKind = "link.accept.ip"
)

// LimitResult 是一次限流判定。
type LimitResult struct {
	Allowed bool
	// RetryAfter 在不允许时给出重试等待。
	RetryAfter time.Duration
}

// RateLimiter 是控制面对限流的依赖。
type RateLimiter interface {
	Hit(ctx context.Context, kind LimitKind, key string) (LimitResult, error)
}

// PresenceEvent 是中继的在线事件。
type PresenceEvent struct {
	Online      bool
	SourceID    string
	CoreVersion string
	AtMs        int64
}

// PresenceBus 订阅源的上下线。
type PresenceBus interface {
	Subscribe(fn func(PresenceEvent)) (unsubscribe func())
}

// ConnectionFilter 是要断开的客户端连接：按源、或按中继令牌 sub 的前缀（访客 guest:<linkId>:）。
type ConnectionFilter struct {
	SourceID      string
	SubjectPrefix string
}

// RelayPort 是控制面对中继的依赖：查在线、关隧道、断开客户端。
type RelayPort interface {
	// Route 答源在线时的 coreVersion；不在线 ok 为 false。
	Route(ctx context.Context, sourceID string) (coreVersion string, ok bool, err error)
	CloseTunnel(sourceID string)
	Disconnect(filter ConnectionFilter)
}

// PasswordPolicy 是口令策略：答第一条不过的规则（错误码），全过答空串。
type PasswordPolicy func(password string, names []string) gen.ErrorCode

// DefaultPasswordPolicy 是随包的四条口令策略（identity.Violation）。
func DefaultPasswordPolicy(password string, names []string) gen.ErrorCode {
	return violation(password, names)
}

// Context 是服务的装配：一个租户的端口 + 签发 + 时钟 + 事件。
type Context struct {
	TenantID       string
	Ports          *store.Ports
	Signer         *Signer
	Limits         RateLimiter
	Presence       PresenceBus
	Relay          RelayPort
	PasswordPolicy PasswordPolicy
	// Now 缺省 time.Now。
	Now func() time.Time
	// Policy 是访问策略；nil = personal 的 personalPolicy。
	Policy AccessPolicy
	// RelayOrigin 是源的中继来源（Grant / 摘要 / 登记答复里的 relayOrigin）；nil = issuer。
	RelayOrigin func(sourceID string) string
	// RelayBaseURL 是源在中继上的根地址；nil = <issuer>/s/<sourceId>。
	RelayBaseURL func(sourceID string) string
	// TunnelNodes 是源连隧道的节点（heartbeatHint 的 nodes，cloud-api §17）；nil = issuer 的
	// wss://…/t/v1 单节点。Workers 中继答 …/t/v1?src=<sourceId>，把隧道路由到源的对象。
	TunnelNodes func(ctx context.Context, sourceID string) ([]TunnelNode, error)
	// Challenge 是人机挑战（cloud-api §16）：配置了 siteKey 与 secret 才有；nil = 不要求挑战。
	Challenge *ChallengeGate
	Events    *EventBus
}

// NowMs 是当前时刻（毫秒）。
func (c *Context) NowMs() int64 {
	if c.Now == nil {
		return time.Now().UnixMilli()
	}
	return c.Now().UnixMilli()
}

// Emit 发事件；audience 非 nil 时只发给那条链接的访客。
func (c *Context) Emit(event Event, audience *Audience) {
	if c.Events != nil {
		c.Events.Emit(event, audience)
	}
}

// Subscribe 订阅事件。
func (c *Context) Subscribe(fn func(Event, *Audience)) func() {
	if c.Events == nil {
		return func() {}
	}
	return c.Events.Subscribe(fn)
}

// policy 缺省随包的策略。
func (c *Context) policy() PasswordPolicy {
	if c.PasswordPolicy == nil {
		return DefaultPasswordPolicy
	}
	return c.PasswordPolicy
}

func (c *Context) hit(ctx context.Context, kind LimitKind, key string) error {
	if c.Limits == nil {
		return nil
	}
	result, err := c.Limits.Hit(ctx, kind, key)
	if err != nil {
		return err
	}
	if !result.Allowed {
		return FailAfter(gen.ErrRateLimited, result.RetryAfter)
	}
	return nil
}

func (c *Context) route(ctx context.Context, sourceID string) (string, bool, error) {
	if c.Relay == nil {
		return "", false, nil
	}
	return c.Relay.Route(ctx, sourceID)
}

func (c *Context) relayOrigin(issuer, sourceID string) string {
	if c.RelayOrigin != nil {
		return c.RelayOrigin(sourceID)
	}
	return issuer
}

func (c *Context) relayBaseURL(issuer, sourceID string) string {
	if c.RelayBaseURL != nil {
		return c.RelayBaseURL(sourceID)
	}
	return RelayBaseURL(issuer, sourceID)
}

func (c *Context) tunnelNodes(ctx context.Context, issuer, sourceID string) ([]TunnelNode, error) {
	if c.TunnelNodes != nil {
		return c.TunnelNodes(ctx, sourceID)
	}
	return []TunnelNode{{URL: TunnelURL(issuer), Weight: 1}}, nil
}
