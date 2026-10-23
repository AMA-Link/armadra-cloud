package core

import (
	"context"
	"sync"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
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

// TicketStore 是票据的外部存放处（SaaS：Redis，任一实例发的票可以在任一实例兑换）。键是票的哈希，
// 值只有 sub 与设备 id；Take 一次性（GETDEL）。缺省没有：票在进程内存里（personal）。
type TicketStore interface {
	Put(ctx context.Context, key, subject, deviceID string, ttl time.Duration) error
	// Take 取走并删除；不在或已过期 ok 为 false。
	Take(ctx context.Context, key string) (subject, deviceID string, ok bool, err error)
}

// MeStream 是 me-stream 的控制面一侧。
type MeStream struct {
	c        *Context
	external TicketStore

	mu          sync.Mutex
	tickets     map[string]ticketEntry
	attached    map[*attachment]struct{}
	offPresence func()
	resyncing   map[string]*resyncState
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

// UseTicketStore 把票据存到外部存放处（SaaS）；nil 恢复进程内存。
func (m *MeStream) UseTicketStore(store TicketStore) { m.external = store }

// IssueTicket 是 Ticket 的可出错版（外部存放处可能失败）。
func (m *MeStream) IssueTicket(ctx context.Context, caller Caller) (Ticket, error) {
	if m.external == nil {
		return m.Ticket(caller), nil
	}
	ticket := RandomToken(32)
	expiresAtMs := m.c.NowMs() + WSTicketTTL.Milliseconds()
	if err := m.external.Put(ctx, SecretHash(SecretTicket, ticket), caller.Subject, caller.DeviceID, WSTicketTTL); err != nil {
		return Ticket{}, err
	}
	return Ticket{Ticket: ticket, ExpiresAtMs: expiresAtMs}, nil
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
	if m.external != nil {
		return m.redeemStored(ctx, key)
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

// redeemStored 兑换外部存放处的票：由 sub 与设备 id 重建调用方，设备失效答 ok=false。
func (m *MeStream) redeemStored(ctx context.Context, key string) (Caller, bool, error) {
	if key == "" {
		return Caller{}, false, nil
	}
	subject, deviceID, ok, err := m.external.Take(ctx, key)
	if err != nil || !ok {
		return Caller{}, false, err
	}
	device, err := m.c.Ports.Devices.Get(ctx, m.c.TenantID, deviceID)
	if err != nil || device == nil || device.Subject != subject || !IsLiveDevice(*device, m.c.NowMs()) {
		return Caller{}, false, err
	}
	caller, err := CallerFor(ctx, m.c, *device)
	if err != nil {
		if code, isCode := CodeOf(err); isCode && code == gen.ErrUnauthenticated {
			return Caller{}, false, nil
		}
		return Caller{}, false, err
	}
	return caller, true, nil
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
	caller  Caller
	watcher Watcher
	// ready 在 Watch 完成之前为假：这段时间到的事件先存进 pending，Watch 完成后按序补投
	// （先登记、先订阅，再算可见集合，不让这个窗口里的授予变动漏掉）。
	mu         sync.Mutex
	ready      bool
	pending    []queuedEvent
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

// offer 按可见性过滤后放进队列；不做任何 I/O。可见性在 Refresh 之前判：主机撤销这类事件会让 Refresh 把主机从可见集合里
// 删掉，但撤销之前看得到它的连接仍要收到这条事件。
func (a *attachment) offer(event Event, audience *Audience) {
	a.mu.Lock()
	if !a.ready {
		if len(a.pending) < attachQueue {
			a.pending = append(a.pending, queuedEvent{event, audience})
		} else if a.onOverflow != nil {
			a.onOverflow()
		}
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()
	a.deliver(event, audience)
}

func (a *attachment) deliver(event Event, audience *Audience) {
	sees := a.watcher.Sees(event.EventSource())
	a.watcher.Refresh(event)
	if _, internal := event.(InternalEvent); internal {
		return
	}
	if audience != nil {
		if !audience.Includes(a.caller) {
			return
		}
	} else if !sees {
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
	detach, _, err = m.AttachResyncable(caller, send, onOverflow)
	return detach, err
}

// AttachResyncable 同 Attach，另答一个重算函数：整份重算这个连接的事件可见性（Watcher 实现了 Resyncer 才有效果；
// 装配方的 30 秒心跳用它兜住漏掉的授予 / 成员事件，过期窗口不超过一个心跳）。
//
// 次序：先登记、先订阅事件总线，再解析可见集合，窗口里到的事件先存着、解析完按序补投。
func (m *MeStream) AttachResyncable(caller Caller, send func(Event), onOverflow func()) (detach func(), resync func(context.Context) error, err error) {
	a := &attachment{
		caller: caller, queue: make(chan queuedEvent, attachQueue), done: make(chan struct{}),
		send: send, onOverflow: onOverflow,
	}
	offBus := m.c.Subscribe(a.offer)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	watcher, watchErr := m.c.access().Watch(ctx, caller)
	cancel()
	if watchErr != nil {
		offBus()
		return nil, nil, watchErr
	}
	a.watcher = watcher
	go a.run()
	m.mu.Lock()
	m.attached[a] = struct{}{}
	if len(m.attached) == 1 && m.c.Presence != nil {
		m.offPresence = m.c.Presence.Subscribe(m.onPresence)
	}
	m.mu.Unlock()
	// 补投 Watch 期间到的事件；补投期间新到的继续排在后面。
	for {
		a.mu.Lock()
		if len(a.pending) == 0 {
			a.ready = true
			a.mu.Unlock()
			break
		}
		batch := a.pending
		a.pending = nil
		a.mu.Unlock()
		for _, item := range batch {
			a.deliver(item.event, item.audience)
		}
	}
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
		}, func(ctx context.Context) error {
			if r, ok := a.watcher.(Resyncer); ok {
				return r.Resync(ctx)
			}
			return nil
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

// Resyncer 是可选接口：Watcher 实现它就能在成员 / 角色变了之后整份重算可见集合（I/O，由 MeStream.Resync 在专门的
// 协程里调，不在 Refresh 里）。
type Resyncer interface {
	Resync(ctx context.Context) error
}

// Resync 让账号在本实例上的全部连接整份重算事件可见性（团队成员变动：它能看到的主机一次变了很多）。
// 实现了 Resyncer 的 Watcher 才会重算；答重算失败的次数。同一账号的重算合并：已有一轮在跑时，新的请求只标记「还要再来一轮」
// 并等那条协程做完（连续多次成员变动只多算一轮）。
func (m *MeStream) Resync(ctx context.Context, accountID string) (failed int) {
	m.mu.Lock()
	if m.resyncing == nil {
		m.resyncing = map[string]*resyncState{}
	}
	if st, running := m.resyncing[accountID]; running {
		st.dirty = true
		done := st.done
		m.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
		}
		return 0
	}
	st := &resyncState{done: make(chan struct{})}
	m.resyncing[accountID] = st
	m.mu.Unlock()
	for {
		for _, a := range m.attachmentsOf(accountID) {
			if r, ok := a.watcher.(Resyncer); ok {
				if r.Resync(ctx) != nil {
					failed++
				}
			}
		}
		m.mu.Lock()
		if st.dirty && ctx.Err() == nil {
			st.dirty = false
			m.mu.Unlock()
			continue
		}
		delete(m.resyncing, accountID)
		m.mu.Unlock()
		close(st.done)
		return failed
	}
}

type resyncState struct {
	dirty bool
	done  chan struct{}
}

// Accounts 是本实例上有连接的账号（去重）。
func (m *MeStream) Accounts() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]struct{}{}
	var out []string
	for a := range m.attached {
		if a.caller.AccountID == "" {
			continue
		}
		if _, dup := seen[a.caller.AccountID]; !dup {
			seen[a.caller.AccountID] = struct{}{}
			out = append(out, a.caller.AccountID)
		}
	}
	return out
}

func (m *MeStream) attachmentsOf(accountID string) []*attachment {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*attachment
	for a := range m.attached {
		if a.caller.AccountID == accountID {
			out = append(out, a)
		}
	}
	return out
}

// Clear 清掉所有票据（关闭时）。
func (m *MeStream) Clear() {
	m.mu.Lock()
	m.tickets = map[string]ticketEntry{}
	m.mu.Unlock()
}
