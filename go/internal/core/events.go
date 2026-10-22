package core

import "sync"

// /v1/me/stream 的事件总线（进程内）。SaaS 换成跨节点的实现时保持同一个形状。

// Event 是给 /v1/me/stream 的事件（不含 ping / pong）。JSON 形状见协议包 events.ts。
type Event interface {
	// EventSource 是事件关心的源（没有时为空）。
	EventSource() string
}

// SourceOnline：源上线。
type SourceOnline struct {
	Type        string `json:"type"`
	SourceID    string `json:"sourceId"`
	SinceMs     int64  `json:"sinceMs"`
	RelayOrigin string `json:"relayOrigin"`
}

// SourceOffline：源下线。
type SourceOffline struct {
	Type     string `json:"type"`
	SourceID string `json:"sourceId"`
	AtMs     int64  `json:"atMs"`
}

// SourceRevoked：源被撤销。
type SourceRevoked struct {
	Type     string `json:"type"`
	SourceID string `json:"sourceId"`
}

// AccessRevoked：访客的访问被撤销（只发给那条链接的访客）。
type AccessRevoked struct {
	Type     string `json:"type"`
	SourceID string `json:"sourceId"`
}

// LinkUsedBy 是谁用了链接。
type LinkUsedBy struct {
	AccountID *string `json:"accountId,omitempty"`
	Guest     *bool   `json:"guest,omitempty"`
}

// LinkUsed：链接被用了一次。
type LinkUsed struct {
	Type     string     `json:"type"`
	LinkID   string     `json:"linkId"`
	SourceID string     `json:"sourceId,omitempty"`
	OrgID    string     `json:"orgId,omitempty"`
	By       LinkUsedBy `json:"by"`
	AtMs     int64      `json:"atMs"`
}

// EventSource 实现 Event。
func (e SourceOnline) EventSource() string  { return e.SourceID }
func (e SourceOffline) EventSource() string { return e.SourceID }
func (e SourceRevoked) EventSource() string { return e.SourceID }
func (e AccessRevoked) EventSource() string { return e.SourceID }
func (e LinkUsed) EventSource() string      { return e.SourceID }

// NewSourceOnline 等是带正确 type 的构造函数。
func NewSourceOnline(sourceID string, sinceMs int64, relayOrigin string) SourceOnline {
	return SourceOnline{Type: "sourceOnline", SourceID: sourceID, SinceMs: sinceMs, RelayOrigin: relayOrigin}
}

// NewSourceOffline 构造 sourceOffline。
func NewSourceOffline(sourceID string, atMs int64) SourceOffline {
	return SourceOffline{Type: "sourceOffline", SourceID: sourceID, AtMs: atMs}
}

// NewSourceRevoked 构造 sourceRevoked。
func NewSourceRevoked(sourceID string) SourceRevoked {
	return SourceRevoked{Type: "sourceRevoked", SourceID: sourceID}
}

// NewAccessRevoked 构造 accessRevoked。
func NewAccessRevoked(sourceID string) AccessRevoked {
	return AccessRevoked{Type: "accessRevoked", SourceID: sourceID}
}

// Audience 指定事件只发给某条链接的访客。
type Audience struct{ LinkID string }

// EventBus 是事件总线：Emit 同步地调每个订阅者，订阅者出错（panic）不影响其它订阅者。
type EventBus struct {
	mu        sync.Mutex
	next      int
	listeners map[int]func(Event, *Audience)
}

// NewEventBus 建一个总线。
func NewEventBus() *EventBus { return &EventBus{listeners: map[int]func(Event, *Audience){}} }

// Emit 发一个事件；给了 audience 就只发给那条链接的访客。
func (b *EventBus) Emit(event Event, audience *Audience) {
	b.mu.Lock()
	listeners := make([]func(Event, *Audience), 0, len(b.listeners))
	for id := 0; id < b.next; id++ {
		if fn, ok := b.listeners[id]; ok {
			listeners = append(listeners, fn)
		}
	}
	b.mu.Unlock()
	for _, fn := range listeners {
		func() {
			defer func() { _ = recover() }()
			fn(event, audience)
		}()
	}
}

// Subscribe 订阅事件，答退订函数。
func (b *EventBus) Subscribe(fn func(Event, *Audience)) (unsubscribe func()) {
	b.mu.Lock()
	id := b.next
	b.next++
	b.listeners[id] = fn
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		delete(b.listeners, id)
		b.mu.Unlock()
	}
}
