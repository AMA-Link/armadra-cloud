// Package coretest 是控制核服务的测试装配与服务合同：packages/control-core/src/testing/
// services-contract.ts 的 16 个用例逐例翻译成 Go，只经端口跑一遍登录、会话、源、链接、撤销与
// me-stream 扇出，SQLite 与 PostgreSQL 各跑一遍。只在 _test.go 里用。
package coretest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// Clock 是可控的时钟。
type Clock struct {
	mu sync.Mutex
	ms int64
}

// Now 实现 core.Context.Now。
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.UnixMilli(c.ms)
}

// Ms 是当前毫秒。
func (c *Clock) Ms() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ms
}

// Set 拨时钟。
func (c *Clock) Set(ms int64) {
	c.mu.Lock()
	c.ms = ms
	c.mu.Unlock()
}

// Options 是装配选项。
type Options struct {
	TenantID       string
	Issuer         string
	NowMs          int64
	DisallowGuests bool
	Policy         core.PasswordPolicy
	// Limit 非空时决定每次限流的结果（缺省不限流）。
	Limit func(kind core.LimitKind, key string) core.LimitResult
}

// Harness 是一个租户、一把签名钥、不限流（除非给了 Limit）、可控时钟的装配。
type Harness struct {
	Context *core.Context
	Backend store.Backend
	Clock   *Clock

	mu            sync.Mutex
	events        []core.Event
	disconnects   []core.ConnectionFilter
	closedTunnels []string
	online        map[string]string
	presence      map[int]func(core.PresenceEvent)
	nextListener  int
}

// DefaultNow 是装配的默认时刻：2026-10-10 12:00 UTC。
var DefaultNow = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC).UnixMilli()

// NewHarness 在 backend 里建租户与签名钥，装出一套服务用的 Context。
func NewHarness(t testing.TB, backend store.Backend, options Options) *Harness {
	t.Helper()
	ctx := context.Background()
	if options.TenantID == "" {
		options.TenantID = "t1"
	}
	if options.Issuer == "" {
		options.Issuer = "https://cloud.armadra.test"
	}
	if options.NowMs == 0 {
		options.NowMs = DefaultNow
	}
	h := &Harness{
		Backend: backend, Clock: &Clock{ms: options.NowMs},
		online: map[string]string{}, presence: map[int]func(core.PresenceEvent){},
	}
	settings := store.TenantSettings{Issuer: options.Issuer, AllowGuests: !options.DisallowGuests, TrustedOrigins: []string{}}
	if err := backend.CreateTenant(ctx, options.TenantID, store.Tenant{TenantSettings: settings}); err != nil {
		t.Fatal(err)
	}
	key, err := store.GenerateSigningKey("k1", options.NowMs)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.AddSigningKey(ctx, options.TenantID, key); err != nil {
		t.Fatal(err)
	}
	bus := core.NewEventBus()
	bus.Subscribe(func(event core.Event, _ *core.Audience) {
		h.mu.Lock()
		h.events = append(h.events, event)
		h.mu.Unlock()
	})
	h.Context = &core.Context{
		TenantID:       options.TenantID,
		Ports:          backend.Ports(),
		Signer:         core.NewSigner(backend.Keys(options.TenantID), h.Clock.Now),
		Limits:         limiter{options.Limit},
		Presence:       presenceBus{h},
		Relay:          relayPort{h},
		PasswordPolicy: options.Policy,
		Now:            h.Clock.Now,
		Events:         bus,
	}
	return h
}

type limiter struct {
	fn func(kind core.LimitKind, key string) core.LimitResult
}

func (l limiter) Hit(_ context.Context, kind core.LimitKind, key string) (core.LimitResult, error) {
	if l.fn == nil {
		return core.LimitResult{Allowed: true}, nil
	}
	return l.fn(kind, key), nil
}

type presenceBus struct{ h *Harness }

func (p presenceBus) Subscribe(fn func(core.PresenceEvent)) func() {
	p.h.mu.Lock()
	id := p.h.nextListener
	p.h.nextListener++
	p.h.presence[id] = fn
	p.h.mu.Unlock()
	return func() {
		p.h.mu.Lock()
		delete(p.h.presence, id)
		p.h.mu.Unlock()
	}
}

type relayPort struct{ h *Harness }

func (r relayPort) Route(_ context.Context, sourceID string) (string, bool, error) {
	r.h.mu.Lock()
	defer r.h.mu.Unlock()
	version, ok := r.h.online[sourceID]
	return version, ok, nil
}

func (r relayPort) CloseTunnel(sourceID string) {
	r.h.mu.Lock()
	r.h.closedTunnels = append(r.h.closedTunnels, sourceID)
	r.h.mu.Unlock()
}

func (r relayPort) Disconnect(filter core.ConnectionFilter) {
	r.h.mu.Lock()
	r.h.disconnects = append(r.h.disconnects, filter)
	r.h.mu.Unlock()
}

// SetOnline 把一个源当作在线（relay.Route 答它）。
func (h *Harness) SetOnline(sourceID, coreVersion string) {
	h.mu.Lock()
	h.online[sourceID] = coreVersion
	h.mu.Unlock()
}

// Publish 发一个中继在线事件。
func (h *Harness) Publish(event core.PresenceEvent) {
	h.mu.Lock()
	listeners := make([]func(core.PresenceEvent), 0, len(h.presence))
	for _, fn := range h.presence {
		listeners = append(listeners, fn)
	}
	h.mu.Unlock()
	for _, fn := range listeners {
		fn(event)
	}
}

// Events 是总线上发过的事件。
func (h *Harness) Events() []core.Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]core.Event(nil), h.events...)
}

// Disconnects 是中继被要求断开的过滤器。
func (h *Harness) Disconnects() []core.ConnectionFilter {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]core.ConnectionFilter(nil), h.disconnects...)
}

// ClosedTunnels 是被要求关掉的隧道。
func (h *Harness) ClosedTunnels() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.closedTunnels...)
}
