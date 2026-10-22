package core

import (
	"sync"
	"sync/atomic"
)

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
//
// 订阅者放在一份写时复制的快照里：Emit 只读快照、不持锁，订阅与退订不会拖住正在进行的扇出，
// 扇出的代价与当前订阅者数成正比（不看历史订阅编号）。同步订阅者不得阻塞（会拖住发事件的
// 一方）；要做网络写的（me-stream 把事件推给客户端）用 SubscribeBuffered。
type EventBus struct {
	mu   sync.Mutex
	next uint64
	subs atomic.Pointer[[]busSub]
}

type busSub struct {
	id uint64
	fn func(Event, *Audience)
}

// NewEventBus 建一个总线。
func NewEventBus() *EventBus { return &EventBus{} }

// Emit 发一个事件；给了 audience 就只发给那条链接的访客。
func (b *EventBus) Emit(event Event, audience *Audience) {
	snapshot := b.subs.Load()
	if snapshot == nil {
		return
	}
	for _, sub := range *snapshot {
		func() {
			defer func() { _ = recover() }()
			sub.fn(event, audience)
		}()
	}
}

// Subscribe 订阅事件，答退订函数（重复调用无害）。
func (b *EventBus) Subscribe(fn func(Event, *Audience)) (unsubscribe func()) {
	b.mu.Lock()
	b.next++
	id := b.next
	b.replace(func(old []busSub) []busSub { return append(old[:len(old):len(old)], busSub{id: id, fn: fn}) })
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.replace(func(old []busSub) []busSub {
			next := make([]busSub, 0, len(old))
			for _, sub := range old {
				if sub.id != id {
					next = append(next, sub)
				}
			}
			return next
		})
	}
}

// replace 用新快照换掉旧的；调用方持锁。
func (b *EventBus) replace(edit func([]busSub) []busSub) {
	var old []busSub
	if p := b.subs.Load(); p != nil {
		old = *p
	}
	next := edit(old)
	b.subs.Store(&next)
}

// SubscribeBuffered 订阅事件，但每个订阅者有自己的有界队列和一条协程：Emit 只往队列里放，
// 永不阻塞；队列满了就丢掉这条事件并调 onOverflow（nil 则忽略），由订阅者决定是断开还是重新同步。
// 同一订阅者收到的事件保持发出的顺序。退订后协程退出，队列里没处理的事件丢弃。
func (b *EventBus) SubscribeBuffered(depth int, fn func(Event, *Audience), onOverflow func()) (unsubscribe func()) {
	if depth <= 0 {
		depth = 256
	}
	type item struct {
		event    Event
		audience *Audience
	}
	queue := make(chan item, depth)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case it := <-queue:
				func() {
					defer func() { _ = recover() }()
					fn(it.event, it.audience)
				}()
			case <-stop:
				return
			}
		}
	}()
	off := b.Subscribe(func(event Event, audience *Audience) {
		select {
		case queue <- item{event, audience}:
		default:
			if onOverflow != nil {
				onOverflow()
			}
		}
	})
	var once sync.Once
	return func() {
		off()
		once.Do(func() { close(stop) })
	}
}
