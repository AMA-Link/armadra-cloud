package core

import (
	"context"
	"sync"
	"time"
)

// /v1/me/stream（契约 §6，协议包 §5.7）的控制面一侧：票据与事件扇出。WebSocket 本身由装配方
// 托管（personal：internal/personal 的 stream）。
//
//   - 票据：POST /v1/me/stream/ticket（会话）→ 30 秒、一次性，只在内存，存哈希；
//   - 事件：owner 收全部源的；访客只收链接指向的那个源的，以及发给它那条链接的 accessRevoked；
//   - 源上下线来自中继的在线事件，只转发仍在登记里的源。
//
// 扇出在发事件的 goroutine 里同步进行（访客的可见性要查一次库）；send 由装配方保证不阻塞。

const maxTickets = 1_000

// Ticket 是 me-stream 的一次性票据。
type Ticket struct {
	Ticket      string `json:"ticket"`
	ExpiresAtMs int64  `json:"expiresAtMs"`
}

type ticketEntry struct {
	caller      Caller
	expiresAtMs int64
}

// MeStream 是 me-stream 的控制面一侧。
type MeStream struct {
	c *Context

	mu          sync.Mutex
	tickets     map[string]ticketEntry
	attached    map[*attachment]struct{}
	offPresence func()
}

// NewMeStream 建 me-stream 核。
func NewMeStream(c *Context) *MeStream {
	return &MeStream{c: c, tickets: map[string]ticketEntry{}, attached: map[*attachment]struct{}{}}
}

func (m *MeStream) sweep(nowMs int64) {
	for key, entry := range m.tickets {
		if entry.expiresAtMs <= nowMs {
			delete(m.tickets, key)
		}
	}
}

// Ticket 给调用方发一张 30 秒、一次性的票。
func (m *MeStream) Ticket(caller Caller) Ticket {
	nowMs := m.c.NowMs()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweep(nowMs)
	if len(m.tickets) >= maxTickets {
		m.tickets = map[string]ticketEntry{}
	}
	ticket := RandomToken(32)
	expiresAtMs := nowMs + WSTicketTTL.Milliseconds()
	m.tickets[SecretHash(SecretTicket, ticket)] = ticketEntry{caller: caller, expiresAtMs: expiresAtMs}
	return Ticket{Ticket: ticket, ExpiresAtMs: expiresAtMs}
}

// Redeem 一次性兑换：票不在、过期或设备已失效答 ok=false。
func (m *MeStream) Redeem(ctx context.Context, ticket string) (Caller, bool, error) {
	key := ""
	if ticket != "" {
		key = SecretHash(SecretTicket, ticket)
	}
	m.mu.Lock()
	entry, found := m.tickets[key]
	delete(m.tickets, key)
	m.mu.Unlock()
	if !found || entry.expiresAtMs <= m.c.NowMs() {
		return Caller{}, false, nil
	}
	valid, err := m.StillValid(ctx, entry.caller)
	if err != nil || !valid {
		return Caller{}, false, err
	}
	return entry.caller, true, nil
}

// StillValid：设备仍有效（未登出、未撤销、未过期）。
func (m *MeStream) StillValid(ctx context.Context, caller Caller) (bool, error) {
	device, err := m.c.Ports.Devices.Get(ctx, m.c.TenantID, caller.DeviceID)
	if err != nil || device == nil {
		return false, err
	}
	return IsLiveDevice(*device, m.c.NowMs()), nil
}

// 每个连接的事件队列深度；满了就丢这条事件并通知装配方（它决定断开还是重新同步）。
const attachQueue = 256

type queuedEvent struct {
	event    Event
	audience *Audience
}

// attachment 是一个连接：自己的有界队列和一条协程，发事件的一方只往队列里放，永不阻塞，
// 先后顺序不变。访客能看到的源在 Attach 时解析一次（撤销由 accessRevoked 事件与装配方的复核负责）。
type attachment struct {
	caller     Caller
	watcher    Watcher
	queue      chan queuedEvent
	done       chan struct{}
	send       func(Event)
	onOverflow func()
}

// InternalEvent 标记只给 Watcher.Refresh 的内部事件（例如授予变动让可见集合更新）：offer 把它交给 Watcher 后
// 就丢掉，绝不进客户端的事件队列。
type InternalEvent interface {
	Event
	InternalOnly()
}

// offer 按可见性过滤后放进队列；不做任何 I/O。
func (a *attachment) offer(event Event, audience *Audience) {
	a.watcher.Refresh(event)
	if _, internal := event.(InternalEvent); internal {
		return
	}
	if audience != nil {
		if a.caller.LinkID != audience.LinkID {
			return
		}
	} else if !a.watcher.Sees(event.EventSource()) {
		return
	}
	select {
	case a.queue <- queuedEvent{event, audience}:
	default:
		if a.onOverflow != nil {
			a.onOverflow()
		}
	}
}

func (a *attachment) run() {
	for {
		select {
		case <-a.done:
			return
		case item := <-a.queue:
			func() {
				defer func() { _ = recover() }()
				a.send(item.event)
			}()
		}
	}
}

// Attach 订阅这个调用方能看到的事件；答退订。send 在连接自己的协程里被调用（不在发事件的协程里）；
// 队列满了丢掉事件并调 onOverflow（可为 nil）。
//
// 源上下线：整个 MeStream 只订阅一次在线事件，每个事件只查一次源与 issuer，再分发给各连接。
//
// 访客的链接查不到（库出错）时返回错误，由装配方关掉连接让客户端重连；链接不存在或已撤销不算错（看不到任何源）。
func (m *MeStream) Attach(caller Caller, send func(Event), onOverflow func()) (detach func(), err error) {
	a := &attachment{
		caller: caller, queue: make(chan queuedEvent, attachQueue), done: make(chan struct{}),
		send: send, onOverflow: onOverflow,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	watcher, watchErr := m.c.access().Watch(ctx, caller)
	cancel()
	if watchErr != nil {
		return nil, watchErr
	}
	a.watcher = watcher
	go a.run()
	offBus := m.c.Subscribe(a.offer)
	m.mu.Lock()
	m.attached[a] = struct{}{}
	if len(m.attached) == 1 && m.c.Presence != nil {
		m.offPresence = m.c.Presence.Subscribe(m.onPresence)
	}
	m.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			offBus()
			m.mu.Lock()
			delete(m.attached, a)
			var stop func()
			if len(m.attached) == 0 {
				stop, m.offPresence = m.offPresence, nil
			}
			m.mu.Unlock()
			if stop != nil {
				stop()
			}
			close(a.done)
			a.watcher.Close()
		})
	}, nil
}

// onPresence：一个在线事件查一次源（与 issuer），再分发给各连接。
func (m *MeStream) onPresence(presence PresenceEvent) {
	m.mu.Lock()
	targets := make([]*attachment, 0, len(m.attached))
	for a := range m.attached {
		targets = append(targets, a)
	}
	m.mu.Unlock()
	if len(targets) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	source, err := m.c.Ports.Sources.Get(ctx, m.c.TenantID, presence.SourceID)
	if err != nil || source == nil || source.RevokedAtMs != nil {
		return
	}
	var event Event
	if presence.Online {
		issuer, err := m.c.Signer.Issuer(ctx)
		if err != nil {
			return
		}
		event = NewSourceOnline(presence.SourceID, presence.AtMs, m.c.relayOrigin(issuer, presence.SourceID))
	} else {
		event = NewSourceOffline(presence.SourceID, presence.AtMs)
	}
	for _, a := range targets {
		a.offer(event, nil)
	}
}

// Clear 清掉所有票据（关闭时）。
func (m *MeStream) Clear() {
	m.mu.Lock()
	m.tickets = map[string]ticketEntry{}
	m.mu.Unlock()
}
