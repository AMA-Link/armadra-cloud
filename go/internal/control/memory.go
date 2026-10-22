package control

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// 超过这个条目数时，写入顺手清一遍过期项。
const sweepAt = 1024

// MemoryOptions 配置 Memory。
type MemoryOptions struct {
	Issuer string
	// Mode 缺省 "personal"。
	Mode string
	// JWKS 返回公钥集的 JSON；缺省是空集。
	JWKS func(ctx context.Context) ([]byte, error)
	// Now 供测试换时钟，缺省 time.Now。
	Now func() time.Time
	// Limits 覆盖缺省限流规则（按种类合并）。
	Limits map[LimitKind]LimitRule
	// Revoked 是撤销表之外的撤销判断，两者任一为真即视为撤销。
	Revoked func(ctx context.Context, jti string) (bool, error)
}

type routeEntry struct {
	route     Route
	expiresAt time.Time
}

type window struct {
	count   int
	resetAt time.Time
}

// bus 是一组订阅者：一个出错不影响其余与发布方。
type bus[T any] struct {
	mu   sync.Mutex
	next int
	subs []busSub[T]
}

type busSub[T any] struct {
	id int
	fn func(T)
}

func (b *bus[T]) add(fn func(T)) func() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	id := b.next
	b.subs = append(b.subs, busSub[T]{id: id, fn: fn})
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		for i, s := range b.subs {
			if s.id == id {
				b.subs = append(b.subs[:i:i], b.subs[i+1:]...)
				return
			}
		}
	}
}

func (b *bus[T]) emit(v T) {
	b.mu.Lock()
	subs := append([]busSub[T](nil), b.subs...)
	b.mu.Unlock()
	for _, s := range subs {
		func() {
			defer func() { _ = recover() }() // 一个订阅者出错不影响其它订阅者与发布方。
			s.fn(v)
		}()
	}
}

// Memory 是 Plane 的进程内实现（personal 模式；也给测试用）。路由、限流、撤销都在 map 里，
// 过期是惰性的（读时判）加写时顺手清扫；不写文件、不连任何外部服务。可并发使用。
type Memory struct {
	issuer  string
	mode    string
	jwks    func(ctx context.Context) ([]byte, error)
	now     func() time.Time
	rules   map[LimitKind]LimitRule
	revoked func(ctx context.Context, jti string) (bool, error)

	mu         sync.Mutex
	routes     map[string]*routeEntry
	windows    map[string]*window
	concurrent map[string]int
	revokedTab map[string]time.Time

	presence bus[PresenceEvent]
	commands bus[NodeCommand]
}

// NewMemory 建一个进程内控制面。
func NewMemory(o MemoryOptions) *Memory {
	m := &Memory{
		issuer:     o.Issuer,
		mode:       o.Mode,
		jwks:       o.JWKS,
		now:        o.Now,
		revoked:    o.Revoked,
		rules:      make(map[LimitKind]LimitRule, len(DefaultLimitRules)),
		routes:     map[string]*routeEntry{},
		windows:    map[string]*window{},
		concurrent: map[string]int{},
		revokedTab: map[string]time.Time{},
	}
	if m.mode == "" {
		m.mode = "personal"
	}
	if m.now == nil {
		m.now = time.Now
	}
	for k, r := range DefaultLimitRules {
		m.rules[k] = r
	}
	for k, r := range o.Limits {
		m.rules[k] = r
	}
	return m
}

// Mode 实现 Plane。
func (m *Memory) Mode() string { return m.mode }

// Issuer 实现 Plane。
func (m *Memory) Issuer() string { return m.issuer }

// JWKS 实现 Plane。
func (m *Memory) JWKS(ctx context.Context) ([]byte, error) {
	if m.jwks == nil {
		return []byte(`{"keys":[]}`), nil
	}
	return m.jwks(ctx)
}

// Routes 实现 Plane。
func (m *Memory) Routes() RouteTable { return (*memRoutes)(m) }

// Presence 实现 Plane。
func (m *Memory) Presence() Presence { return (*memPresence)(m) }

// Limits 实现 Plane。
func (m *Memory) Limits() Limiter { return (*memLimits)(m) }

// Tokens 实现 Plane。
func (m *Memory) Tokens() TokenStore { return (*memTokens)(m) }

// NodeCommands 实现 Plane；Memory 总有命令通道。
func (m *Memory) NodeCommands() NodeCommands { return (*memCommands)(m) }

// Command 把命令发给本进程的订阅者（personal 撤销源时关它的隧道）。
func (m *Memory) Command(c NodeCommand) { m.commands.emit(c) }

// RouteCount 返回当前未过期的路由数（测试与 /health 用）。
func (m *Memory) RouteCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	at := m.now()
	n := 0
	for _, e := range m.routes {
		if e.expiresAt.After(at) {
			n++
		}
	}
	return n
}

// liveRoute 取未过期的路由，过期的顺手删掉。调用方持锁。
func (m *Memory) liveRoute(sourceID string) *routeEntry {
	e, ok := m.routes[sourceID]
	if !ok {
		return nil
	}
	if !e.expiresAt.After(m.now()) {
		delete(m.routes, sourceID)
		return nil
	}
	return e
}

type memRoutes Memory

func (r *memRoutes) Get(_ context.Context, sourceID string) (*Route, error) {
	m := (*Memory)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.liveRoute(sourceID)
	if e == nil {
		return nil, nil
	}
	c := e.route
	return &c, nil
}

func (r *memRoutes) Set(_ context.Context, sourceID string, route Route, ttl time.Duration) error {
	m := (*Memory)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.routes) >= sweepAt {
		at := m.now()
		for k, e := range m.routes {
			if !e.expiresAt.After(at) {
				delete(m.routes, k)
			}
		}
	}
	m.routes[sourceID] = &routeEntry{route: route, expiresAt: m.now().Add(ttl)}
	return nil
}

func (r *memRoutes) Touch(_ context.Context, sourceID string, ttl time.Duration) error {
	m := (*Memory)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.liveRoute(sourceID); e != nil {
		e.expiresAt = m.now().Add(ttl)
	}
	return nil
}

func (r *memRoutes) Delete(_ context.Context, sourceID, tunnelID string) error {
	m := (*Memory)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.routes[sourceID]; ok && e.route.TunnelID == tunnelID {
		delete(m.routes, sourceID)
	}
	return nil
}

type memPresence Memory

func (p *memPresence) Publish(_ context.Context, ev PresenceEvent) error {
	(*Memory)(p).presence.emit(ev)
	return nil
}

func (p *memPresence) Subscribe(fn func(PresenceEvent)) func() {
	return (*Memory)(p).presence.add(fn)
}

type memCommands Memory

func (c *memCommands) Subscribe(fn func(NodeCommand)) func() {
	return (*Memory)(c).commands.add(fn)
}

type memLimits Memory

func limitID(kind LimitKind, key string) string { return string(kind) + "\x00" + key }

func (l *memLimits) Hit(_ context.Context, kind LimitKind, key string) (LimitResult, error) {
	m := (*Memory)(l)
	rule, ok := m.rules[kind]
	if !ok {
		return LimitResult{}, fmt.Errorf("未知的限流种类 %q", kind)
	}
	id := limitID(kind, key)
	m.mu.Lock()
	defer m.mu.Unlock()
	if rule.Concurrent {
		used := m.concurrent[id]
		if used >= rule.Max {
			return LimitResult{}, nil
		}
		m.concurrent[id] = used + 1
		return LimitResult{Allowed: true}, nil
	}
	at := m.now()
	if len(m.windows) >= sweepAt {
		for k, w := range m.windows {
			if !w.resetAt.After(at) {
				delete(m.windows, k)
			}
		}
	}
	w := m.windows[id]
	if w == nil || !w.resetAt.After(at) {
		w = &window{resetAt: at.Add(rule.Window)}
		m.windows[id] = w
	}
	if w.count >= rule.Max {
		return LimitResult{RetryAfter: w.resetAt.Sub(at)}, nil
	}
	w.count++
	return LimitResult{Allowed: true}, nil
}

func (l *memLimits) Release(_ context.Context, kind LimitKind, key string) error {
	m := (*Memory)(l)
	if !m.rules[kind].Concurrent {
		return nil
	}
	id := limitID(kind, key)
	m.mu.Lock()
	defer m.mu.Unlock()
	if used := m.concurrent[id]; used <= 1 {
		delete(m.concurrent, id)
	} else {
		m.concurrent[id] = used - 1
	}
	return nil
}

type memTokens Memory

func (t *memTokens) IsRevoked(ctx context.Context, jti string) (bool, error) {
	m := (*Memory)(t)
	m.mu.Lock()
	until, ok := m.revokedTab[jti]
	if ok && !until.After(m.now()) {
		delete(m.revokedTab, jti)
		ok = false
	}
	m.mu.Unlock()
	if ok {
		return true, nil
	}
	if m.revoked != nil {
		return m.revoked(ctx, jti)
	}
	return false, nil
}

func (t *memTokens) Revoke(_ context.Context, jti string, exp time.Time) error {
	m := (*Memory)(t)
	m.mu.Lock()
	defer m.mu.Unlock()
	at := m.now()
	if len(m.revokedTab) >= sweepAt {
		for k, until := range m.revokedTab {
			if !until.After(at) {
				delete(m.revokedTab, k)
			}
		}
	}
	if exp.After(at) {
		m.revokedTab[jti] = exp
	}
	return nil
}
