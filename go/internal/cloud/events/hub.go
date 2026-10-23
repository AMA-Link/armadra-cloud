package events

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
)

// Channel 是领域事件的 Redis 频道（设计 §2.4）。
const Channel = "cloud:events"

// HubOptions 配置跨实例分发。
type HubOptions struct {
	Client goredis.UniversalClient
	// Channel 缺省 Channel；测试用随机频道隔离。
	Channel string
	// Origin 是本实例的标识（cloud/<实例 id>），自己发的消息不再投一次。
	Origin string
	// Bus 是本机的事件总线：Hub 自己是它的 publisher，本机的分发调 Bus.Deliver。
	Bus *core.EventBus
	// Prepare 在本机分发每个事件之前调用（可以做 I/O，同一个协程里按序）：装配方据此把授予 / 成员变动
	// 转成连接的可见集合更新。可为 nil。
	Prepare func(ctx context.Context, event core.Event, audience *core.Audience)
	// OnResubscribed 在订阅断线后重新就绪时调用：断线期间的事件丢了，装配方据此让 me-stream 的客户端重连重拉。
	OnResubscribed func()
	Log            *obs.Logger
	// Buffer 是本机分发队列与出站队列的深度；缺省 1024。队列满了丢事件并记警告（事件只是提示，客户端重拉）。
	Buffer int
	// ReconnectMin 与 ReconnectMax 是订阅断线后的重连退避；缺省 200ms 到 5s。
	ReconnectMin, ReconnectMax time.Duration
	// PrepareBudget 是每个事件 Prepare 的总预算；缺省 5 秒。超时未处理完的账号转由 me-stream 的 30 秒心跳兜底
	// （可见集合最多过期一个心跳），并计入 PrepareTimeouts，不让慢事件卡住后面的分发。
	PrepareBudget time.Duration
}

// Hub 把领域事件分发给所有实例：Publish 先在本机排队分发、再异步发到 Redis；订阅别的实例的消息，按序本机分发。
// 同一实例发出的事件在本机与对端都保持发出的顺序。
type Hub struct {
	o       HubOptions
	channel string

	local chan item
	out   chan []byte

	stop            context.CancelFunc
	wg              sync.WaitGroup
	ready           chan struct{}
	psMu            sync.Mutex
	ps              *goredis.PubSub
	dropped         atomic.Int64
	prepareTimeouts atomic.Int64
	closed          atomic.Bool
	once            sync.Once
}

type item struct {
	event    core.Event
	audience *core.Audience
}

var _ core.EventPublisher = (*Hub)(nil)

// NewHub 建分发器；Start 之前不收不发。
func NewHub(o HubOptions) *Hub {
	if o.Buffer <= 0 {
		o.Buffer = 1024
	}
	if o.ReconnectMin <= 0 {
		o.ReconnectMin = 200 * time.Millisecond
	}
	if o.ReconnectMax <= 0 {
		o.ReconnectMax = 5 * time.Second
	}
	if o.PrepareBudget <= 0 {
		o.PrepareBudget = 5 * time.Second
	}
	if o.Log == nil {
		o.Log = obs.Nop()
	}
	channel := o.Channel
	if channel == "" {
		channel = Channel
	}
	return &Hub{o: o, channel: channel, local: make(chan item, o.Buffer), out: make(chan []byte, o.Buffer), ready: make(chan struct{})}
}

// Dropped 是因队列满而丢掉的事件数。
func (h *Hub) Dropped() int64 { return h.dropped.Load() }

// PrepareTimeouts 是 Prepare 超出总预算的次数（指标）：这些事件的可见集合更新转由心跳兜底。
func (h *Hub) PrepareTimeouts() int64 { return h.prepareTimeouts.Load() }

// Start 起三个协程（订阅、本机分发、出站发布），等订阅就绪后返回；订阅不上报错。
func (h *Hub) Start(ctx context.Context) error {
	loopCtx, stop := context.WithCancel(context.Background())
	h.stop = stop
	h.wg.Add(3)
	failed := make(chan error, 1)
	go func() { defer h.wg.Done(); h.subscribeLoop(loopCtx, failed) }()
	go func() { defer h.wg.Done(); h.deliverLoop(loopCtx) }()
	go func() { defer h.wg.Done(); h.publishLoop(loopCtx) }()
	select {
	case <-h.ready:
		return nil
	case err := <-failed:
		h.Close()
		return err
	case <-ctx.Done():
		h.Close()
		return errors.New("领域事件订阅启动超时或被取消（Redis）")
	}
}

// Close 停掉协程。队列里没处理的事件丢弃。
func (h *Hub) Close() {
	h.once.Do(func() {
		h.closed.Store(true)
		if h.stop != nil {
			h.stop()
		}
		h.psMu.Lock()
		if h.ps != nil {
			_ = h.ps.Close()
		}
		h.psMu.Unlock()
		h.wg.Wait()
	})
}

// Publish 实现 core.EventPublisher：本机排队分发，同时异步发到 Redis。
func (h *Hub) Publish(event core.Event, audience *core.Audience) {
	if h.closed.Load() {
		return
	}
	select {
	case h.local <- item{event, audience}:
	default:
		h.drop("本机分发队列满了，丢掉一个事件")
	}
	payload, err := Encode(h.o.Origin, event, audience)
	if err != nil {
		h.o.Log.Warn("事件编码失败", obs.Operation("events.publish"))
		return
	}
	select {
	case h.out <- payload:
	default:
		h.drop("出站队列满了，丢掉一个事件")
	}
}

func (h *Hub) drop(message string) {
	if h.dropped.Add(1)%100 == 1 {
		h.o.Log.Warn(message, obs.Operation("events.queue"))
	}
}

func (h *Hub) publishLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case payload := <-h.out:
			pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := h.o.Client.Publish(pctx, h.channel, payload).Err(); err != nil && ctx.Err() == nil {
				h.o.Log.Warn("领域事件发布失败", obs.Operation("events.publish"), obs.Error(errors.New("操作失败（Redis）")))
			}
			cancel()
		}
	}
}

func (h *Hub) deliverLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case it := <-h.local:
			func() {
				defer func() { _ = recover() }()
				if h.o.Prepare != nil {
					pctx, cancel := context.WithTimeout(ctx, h.o.PrepareBudget) // 每个账号另有自己的超时
					h.o.Prepare(pctx, it.event, it.audience)
					if pctx.Err() != nil && ctx.Err() == nil {
						if h.prepareTimeouts.Add(1)%100 == 1 {
							h.o.Log.Warn("事件准备超出总预算，未处理的账号转由心跳兜底", obs.Operation("events.prepare"))
						}
					}
					cancel()
				}
				h.o.Bus.Deliver(it.event, it.audience)
			}()
		}
	}
}

func (h *Hub) subscribeLoop(ctx context.Context, failed chan<- error) {
	wait := h.o.ReconnectMin
	first := true
	for ctx.Err() == nil {
		err := h.subscribeOnce(ctx, first)
		if ctx.Err() != nil {
			return
		}
		if first && !h.isReady() {
			select {
			case failed <- errors.New("订阅领域事件失败（Redis）"):
			default:
			}
			return
		}
		first = false
		h.o.Log.Warn("领域事件订阅断开，重连", obs.Operation("events.subscribe"), obs.Error(errors.New("连接出错（Redis）")))
		_ = err
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, h.o.ReconnectMax)
	}
}

func (h *Hub) isReady() bool {
	select {
	case <-h.ready:
		return true
	default:
		return false
	}
}

func (h *Hub) subscribeOnce(ctx context.Context, first bool) error {
	ps := h.o.Client.Subscribe(ctx)
	defer func() { _ = ps.Close() }()
	h.psMu.Lock()
	if h.closed.Load() {
		h.psMu.Unlock()
		return nil
	}
	h.ps = ps
	h.psMu.Unlock()
	if err := ps.Subscribe(ctx, h.channel); err != nil {
		return err
	}
	for {
		msg, err := ps.Receive(ctx)
		if err != nil {
			return err
		}
		if _, ok := msg.(*goredis.Subscription); ok {
			break
		}
	}
	if first {
		close(h.ready)
	} else if h.o.OnResubscribed != nil {
		h.o.OnResubscribed()
	}
	waiting := false // 已发 PING、还没收到任何东西
	for {
		msg, err := ps.ReceiveTimeout(ctx, 30*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var timeout interface{ Timeout() bool }
			if errors.As(err, &timeout) && timeout.Timeout() {
				if waiting {
					return errors.New("订阅连接没有响应")
				}
				if err := ps.Ping(ctx); err != nil {
					return err
				}
				waiting = true
				continue
			}
			return err
		}
		waiting = false
		m, ok := msg.(*goredis.Message)
		if !ok {
			continue
		}
		origin, event, audience, err := Decode([]byte(m.Payload))
		if err != nil || origin == h.o.Origin {
			continue
		}
		select {
		case h.local <- item{event, audience}:
		default:
			h.drop("本机分发队列满了，丢掉一个来自别的实例的事件")
		}
	}
}
