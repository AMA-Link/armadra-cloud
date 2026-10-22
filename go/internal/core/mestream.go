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

	mu      sync.Mutex
	tickets map[string]ticketEntry
}

// NewMeStream 建 me-stream 核。
func NewMeStream(c *Context) *MeStream {
	return &MeStream{c: c, tickets: map[string]ticketEntry{}}
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

// sees：这个调用方能看到这个源吗（访客只看链接指向的那个）。
func (m *MeStream) sees(ctx context.Context, caller Caller, sourceID string) bool {
	if caller.Role == RoleOwner {
		return true
	}
	if caller.LinkID == "" {
		return false
	}
	link, err := m.c.Ports.Links.Get(ctx, m.c.TenantID, caller.LinkID)
	return err == nil && link != nil && link.SourceID == sourceID
}

// Attach 订阅这个调用方能看到的事件；答退订。
func (m *MeStream) Attach(caller Caller, send func(Event)) (detach func()) {
	deliver := func(event Event, audience *Audience) {
		if audience != nil {
			if caller.LinkID == audience.LinkID {
				send(event)
			}
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if m.sees(ctx, caller, event.EventSource()) {
			send(event)
		}
	}
	offBus := m.c.Subscribe(deliver)
	offPresence := func() {}
	if m.c.Presence != nil {
		offPresence = m.c.Presence.Subscribe(func(presence PresenceEvent) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			source, err := m.c.Ports.Sources.Get(ctx, m.c.TenantID, presence.SourceID)
			if err != nil || source == nil || source.RevokedAtMs != nil {
				return
			}
			if presence.Online {
				issuer, err := m.c.Signer.Issuer(ctx)
				if err != nil {
					return
				}
				deliver(NewSourceOnline(presence.SourceID, presence.AtMs, issuer), nil)
				return
			}
			deliver(NewSourceOffline(presence.SourceID, presence.AtMs), nil)
		})
	}
	return func() {
		offBus()
		offPresence()
	}
}

// Clear 清掉所有票据（关闭时）。
func (m *MeStream) Clear() {
	m.mu.Lock()
	m.tickets = map[string]ticketEntry{}
	m.mu.Unlock()
}
