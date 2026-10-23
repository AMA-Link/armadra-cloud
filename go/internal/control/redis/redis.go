// Package redis 是控制面的 Redis 实现（多节点；设计 §3.6，对应
// 已删除的 TypeScript 版 packages/relay-runtime-node/src/control/redis.ts，键与频道逐项一致）。
//
//	| 功能          | 实现                                                                        |
//	| 路由          | <前缀>route:src:<id> = JSON，SET … PX；Touch 用 PEXPIRE；Delete 用脚本按 tunnelId CAS |
//	| 在线 / 命令   | 本节点立即投递 + PUBLISH <前缀>presence / <前缀>ctl                         |
//	| 撤销          | <前缀>revoked:<jti>，SET … PXAT，PUBLISH <前缀>revoked；各节点内存一份，订阅连接每次就绪都 SCAN 全量重读 |
//	| 速率限流      | <前缀>limit:<种类>:<键的 SHA-256>，脚本 INCR + 首次 PEXPIRE，各节点共用一个桶；出错退回本节点内存 |
//	| 并发限流      | 本节点内存                                                                  |
//
// 需要 Redis 7.0 以上：撤销全量重读用 PEXPIRETIME（7.0 新增），更老的版本启动时报错并说明。
//
// 订阅用单独的连接，断线后自己重连（退避）并重新订阅；每次订阅就绪都全量重读撤销，断线期间的
// 撤销不漏。不写令牌明文（撤销只有 jti）。
package redis

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/cluster"
)

// DefaultPrefix 是键与频道的缺省前缀。
const DefaultPrefix = "relay:"

// Options 配置 Redis 控制面。
type Options struct {
	cluster.Options
	// Client 是已连好的客户端（调用方管它的生命周期，Close 不关它）；与 URL 二选一。
	Client goredis.UniversalClient
	// URL 是 redis://… 或 rediss://…（含口令，不进日志与错误）；给了就自建客户端，Close 时关。
	URL string
	// Prefix 是键与频道的前缀；缺省 "relay:"。
	Prefix string
	// ReconnectMin 与 ReconnectMax 是订阅连接断线后的重连退避；缺省 200ms 到 5s。
	ReconnectMin, ReconnectMax time.Duration
	// ReloadEvery 是周期性全量重读撤销的间隔，兜底发布失败或订阅半开时漏掉的撤销；缺省 45 秒。
	ReloadEvery time.Duration
	// LivenessEvery 是订阅连接的活性检查间隔：这么久没收到任何东西就发 PING，
	// 再一个间隔还没动静就当断线重连；缺省 15 秒。
	LivenessEvery time.Duration
	// ReloadFault 是测试用的故障注入：返回错误时这一次重读当作失败。
	ReloadFault func() error

	// 以下只有 SaaS 节点（G5）用，personal 不设、行为不变。

	// NodeChannel 为真时另订阅 <前缀>node:<NodeID>：控制面发给持有节点的命令，与 <前缀>ctl 同形。
	// 控制面同时照样广播，所以同一条命令可能到两次（命令都是幂等的）。
	NodeChannel bool
	// OnSignal 收 Plane 自己不处理的广播（kind: quota / incident），在订阅协程里调用，不能阻塞。
	OnSignal func(cluster.Message)
	// OnSubscribed 在每次订阅就绪（撤销全量重读之后）调用：SaaS 节点据此全量读额度与事故开关。
	// 答错误时这次就绪作废，退避重连后再来（第一次就失败则 New 报错）。
	OnSubscribed func(ctx context.Context) error
}

// ErrTooOld 表示 Redis 版本低于 7.0。
var ErrTooOld = errors.New("版本太低：Redis 需要 7.0 以上（撤销重读用 PEXPIRETIME）")

const deleteRoute = `
local value = redis.call('GET', KEYS[1])
if value and cjson.decode(value).tunnelId == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0`

const hit = `
local count = redis.call('INCR', KEYS[1])
if count == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[1]) end
local ttl = redis.call('PTTL', KEYS[1])
if ttl < 0 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
  ttl = tonumber(ARGV[1])
end
return { count, ttl }`

// Plane 是 Redis 控制面。
type Plane struct {
	*cluster.Base
	client  goredis.UniversalClient
	ownsCli bool
	prefix  string
	chans   channels

	deleteScript *goredis.Script
	hitScript    *goredis.Script

	reloadEvery, liveness time.Duration
	fault                 func() error
	onSignal              func(cluster.Message)
	onSubscribed          func(ctx context.Context) error
	stop                  context.CancelFunc
	psMu                  sync.Mutex
	ps                    *goredis.PubSub
	stopped               bool
	loopDone              chan struct{}
	ready                 chan struct{}
	readyErr              chan error
	once                  sync.Once
	closeErr              error
}

type channels struct{ presence, command, revoked, node string }

var _ control.Cluster = (*Plane)(nil)

// New 连上 Redis、订阅三个频道、全量读一遍撤销表，然后返回；连不上或读不了就报错。
// 订阅连接之后断线会自己重连并再读一遍。
func New(ctx context.Context, o Options) (*Plane, error) {
	client := o.Client
	owns := false
	if client == nil {
		if o.URL == "" {
			return nil, errors.New("控制面缺 Redis 的 Client 或 URL")
		}
		parsed, err := goredis.ParseURL(o.URL)
		if err != nil {
			// 解析错误可能带连接串片段：不转述。
			return nil, errors.New("连接串不合格式（Redis）")
		}
		client, owns = goredis.NewClient(parsed), true
	}
	prefix := o.Prefix
	if prefix == "" {
		prefix = DefaultPrefix
	}
	if o.ReconnectMin <= 0 {
		o.ReconnectMin = 200 * time.Millisecond
	}
	if o.ReconnectMax <= 0 {
		o.ReconnectMax = 5 * time.Second
	}
	if o.ReloadEvery <= 0 {
		o.ReloadEvery = 45 * time.Second
	}
	if o.LivenessEvery <= 0 {
		o.LivenessEvery = 15 * time.Second
	}
	loopCtx, stop := context.WithCancel(context.Background())
	p := &Plane{
		Base: cluster.NewBase(o.Options), client: client, ownsCli: owns, prefix: prefix,
		chans:        channels{presence: prefix + "presence", command: prefix + "ctl", revoked: prefix + "revoked"},
		deleteScript: goredis.NewScript(deleteRoute), hitScript: goredis.NewScript(hit),
		reloadEvery: o.ReloadEvery, liveness: o.LivenessEvery, fault: o.ReloadFault,
		onSignal: o.OnSignal, onSubscribed: o.OnSubscribed,
		stop: stop, loopDone: make(chan struct{}), ready: make(chan struct{}), readyErr: make(chan error, 1),
	}
	if o.NodeChannel && o.NodeID != "" {
		p.chans.node = prefix + "node:" + o.NodeID
	}
	p.Classify = classify
	go p.subscribeLoop(loopCtx, o.ReconnectMin, o.ReconnectMax)
	select {
	case <-p.ready:
		go p.reloadLoop(loopCtx)
		return p, nil
	case err := <-p.readyErr:
		_ = p.Close()
		return nil, err
	case <-ctx.Done():
		_ = p.Close()
		return nil, errors.New("控制面启动超时或被取消（Redis）")
	}
}

func (p *Plane) routeKey(sourceID string) string { return p.prefix + "route:src:" + sourceID }
func (p *Plane) revokedKey(jti string) string    { return p.prefix + "revoked:" + jti }
func (p *Plane) limitKey(id string) string       { return p.prefix + "limit:" + id }

// Client 是 Plane 用的 Redis 客户端（SaaS 节点在同一个连接池上读写节点表、额度、并发计数与 JWKS）。
func (p *Plane) Client() goredis.UniversalClient { return p.client }

// Prefix 是键与频道的前缀。
func (p *Plane) Prefix() string { return p.prefix }

// subscribeLoop 订阅三个频道并处理消息；断线就退避重连，每次订阅就绪都全量重读撤销。
func (p *Plane) subscribeLoop(ctx context.Context, minWait, maxWait time.Duration) {
	defer close(p.loopDone)
	wait := minWait
	for ctx.Err() == nil {
		first := !p.isReady()
		err := p.subscribeOnce(ctx, first)
		if err == nil || ctx.Err() != nil {
			return
		}
		if !p.isReady() {
			// 第一次就失败：启动失败，交给 New 报告。
			select {
			case p.readyErr <- fmt.Errorf("订阅失败（Redis）：%w", sanitize(err)):
			default:
			}
			return
		}
		p.Report("subscriber", sanitize(err))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, maxWait)
	}
}

func (p *Plane) isReady() bool {
	select {
	case <-p.ready:
		return true
	default:
		return false
	}
}

// classify 只留错误类别：不带主机、端口、口令与服务端的原文。
func classify(err error) string {
	var netErr net.Error
	var redisErr goredis.Error
	switch {
	case errors.Is(err, ErrTooOld):
		return ErrTooOld.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return "超时"
	case errors.As(err, &netErr):
		return "网络错误"
	case errors.Is(err, io.EOF):
		return "连接被断开"
	case errors.As(err, &redisErr):
		first, _, _ := strings.Cut(err.Error(), " ")
		return "Redis 答复错误：" + first
	}
	return "连接出错"
}

func sanitize(err error) error { return errors.New(classify(err)) }

// subscribeOnce 建一条订阅连接、等三个订阅确认、重读撤销、收消息，直到连接断掉。
// 答 nil 表示被要求停止；答 err 表示断线（调用方重连）。
func (p *Plane) subscribeOnce(ctx context.Context, first bool) error {
	ps := p.client.Subscribe(ctx)
	defer func() { _ = ps.Close() }()
	// 阻塞中的读不看 ctx 取消，要靠关订阅连接来打断：Close 时关当前这条。
	p.psMu.Lock()
	if p.stopped {
		p.psMu.Unlock()
		return nil
	}
	p.ps = ps
	p.psMu.Unlock()
	chans := []string{p.chans.presence, p.chans.command, p.chans.revoked}
	if p.chans.node != "" {
		chans = append(chans, p.chans.node)
	}
	if err := ps.Subscribe(ctx, chans...); err != nil {
		return err
	}
	for confirmed := 0; confirmed < len(chans); {
		msg, err := ps.Receive(ctx)
		if err != nil {
			return err
		}
		if _, ok := msg.(*goredis.Subscription); ok {
			confirmed++
		}
	}
	// 订阅已生效之后再读全量：读的过程中发生的撤销要么在扫描里、要么走广播，不会漏。
	// 读不全就不能算订阅就绪（否则断线期间的撤销会被漏判）：返回错误，由外层退避、重连、重订阅、再读。
	if err := p.reloadRevoked(ctx); err != nil {
		return err
	}
	if p.onSubscribed != nil {
		if err := p.onSubscribed(ctx); err != nil {
			return err
		}
	}
	if first {
		close(p.ready)
	}
	waiting := false // 已发 PING、还没收到任何东西
	for {
		msg, err := ps.ReceiveTimeout(ctx, p.liveness)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
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
		if m, ok := msg.(*goredis.Message); ok {
			p.receive([]byte(m.Payload))
		}
	}
}

// reloadLoop 周期性全量重读撤销：兜底发布 / 通知失败或订阅半开时漏掉的撤销。
func (p *Plane) reloadLoop(ctx context.Context) {
	ticker := time.NewTicker(p.reloadEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			p.Report("reload revoked", p.reloadRevoked(rctx))
			cancel()
		}
	}
}

func (p *Plane) receive(payload []byte) {
	m, err := cluster.ParseBroadcast(payload)
	if err != nil || m.Origin == p.Origin {
		return
	}
	switch m.Kind {
	case cluster.KindPresence:
		p.EmitPresence(m.Presence)
	case cluster.KindCommand:
		p.EmitCommand(m.Command)
	case cluster.KindRevoked:
		p.ReceiveRevoked(m.JTI, m.ExpMs)
	case cluster.KindQuota, cluster.KindIncident:
		if p.onSignal != nil {
			p.onSignal(m)
		}
	}
}

// reloadRevoked SCAN 出全部 <前缀>revoked:*，到期时刻取 PEXPIRETIME。
func (p *Plane) reloadRevoked(ctx context.Context) error {
	err := p.reloadRevokedOnce(ctx)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "unknown command") {
		return ErrTooOld
	}
	return err
}

func (p *Plane) reloadRevokedOnce(ctx context.Context) error {
	if p.fault != nil {
		if err := p.fault(); err != nil {
			return err
		}
	}
	found := map[string]int64{}
	at := p.Now().UnixMilli()
	prefix := p.revokedKey("")
	var cursor uint64
	for {
		keys, next, err := p.client.Scan(ctx, cursor, prefix+"*", 500).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			cmds, err := p.client.Pipelined(ctx, func(pipe goredis.Pipeliner) error {
				for _, key := range keys {
					pipe.PExpireTime(ctx, key)
				}
				return nil
			})
			if err != nil && !errors.Is(err, goredis.Nil) {
				return err
			}
			for i, cmd := range cmds {
				expiry, err := cmd.(*goredis.DurationCmd).Result()
				if err != nil {
					continue
				}
				// PEXPIRETIME 答毫秒时间戳；go-redis 按 DurationCmd 解成「纳秒」数，要换回来。
				ms := int64(expiry / time.Millisecond)
				if ms > at {
					found[strings.TrimPrefix(keys[i], prefix)] = ms
				}
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	p.MergeRevoked(found)
	return nil
}

func (p *Plane) publish(ctx context.Context, channel string, payload []byte) {
	p.Report("publish "+channel, p.client.Publish(ctx, channel, payload).Err())
}

// ------------------------------------------------------------ control.Plane

// Routes 实现 control.Plane。
func (p *Plane) Routes() control.RouteTable { return routes{p} }

// Presence 实现 control.Plane。
func (p *Plane) Presence() control.Presence { return presence{p} }

// Limits 实现 control.Plane。
func (p *Plane) Limits() control.Limiter {
	return cluster.Limits{B: p.Base, Remote: p.remoteHit}
}

// Tokens 实现 control.Plane。
func (p *Plane) Tokens() control.TokenStore {
	return cluster.Tokens{
		B: p.Base,
		Persist: func(ctx context.Context, jti string, expMs int64) error {
			return p.client.Do(ctx, "SET", p.revokedKey(jti), "1", "PXAT", expMs).Err()
		},
		Broadcast: func(ctx context.Context, jti string, expMs int64) {
			p.publish(ctx, p.chans.revoked, cluster.EncodeRevoked(p.Origin, jti, expMs))
		},
	}
}

// Command 实现 control.Cluster：本节点立即收到，其余节点经 <前缀>ctl 广播收到。
func (p *Plane) Command(c control.NodeCommand) {
	p.EmitCommand(c)
	payload := cluster.EncodeCommand(p.Origin, c)
	p.Queue("publish "+p.chans.command, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return p.client.Publish(ctx, p.chans.command, payload).Err()
	})
}

func (p *Plane) remoteHit(ctx context.Context, id string, window time.Duration) (int64, time.Duration, error) {
	return runHit(ctx, p.hitScript, p.client, p.limitKey(id), window)
}

// runHit 跑限流脚本：键里 INCR，首次设窗口，答窗口内的第几次与窗口还剩多久。
func runHit(ctx context.Context, script *goredis.Script, client goredis.Scripter, key string, window time.Duration) (int64, time.Duration, error) {
	reply, err := script.Run(ctx, client, []string{key}, window.Milliseconds()).Slice()
	if err != nil {
		return 0, 0, err
	}
	if len(reply) != 2 {
		return 0, 0, errors.New("限流脚本答复形状不对")
	}
	count, ok1 := reply[0].(int64)
	ttl, ok2 := reply[1].(int64)
	if !ok1 || !ok2 {
		return 0, 0, errors.New("限流脚本答复形状不对")
	}
	return count, time.Duration(ttl) * time.Millisecond, nil
}

// RevokedCount 见 cluster.Base。Settle 等已发出的后台写做完。

// Close 停掉订阅、等后台任务；自建的客户端一并关。重复调用无害。
func (p *Plane) Close() error {
	p.once.Do(func() {
		p.stop()
		p.psMu.Lock()
		p.stopped = true
		if p.ps != nil {
			_ = p.ps.Close()
		}
		p.psMu.Unlock()
		<-p.loopDone
		p.Shutdown()
		if p.ownsCli {
			p.closeErr = p.client.Close()
		}
	})
	return p.closeErr
}

type routes struct{ p *Plane }

func (r routes) Get(ctx context.Context, sourceID string) (*control.Route, error) {
	value, err := r.p.client.Get(ctx, r.p.routeKey(sourceID)).Bytes()
	if errors.Is(err, goredis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	route, err := cluster.DecodeRoute(value)
	if err != nil {
		return nil, fmt.Errorf("路由记录坏了：%s", sourceID)
	}
	return &route, nil
}

func (r routes) Set(ctx context.Context, sourceID string, route control.Route, ttl time.Duration) error {
	var err error
	r.p.Run(func() {
		err = r.p.client.Set(ctx, r.p.routeKey(sourceID), cluster.EncodeRoute(route), max(ttl, time.Millisecond)).Err()
	})
	return err
}

// Touch 与 Delete 失败只报告（OnError），不返回：它们在心跳与收尾路径上，调用方无从补救。
func (r routes) Touch(ctx context.Context, sourceID string, ttl time.Duration) error {
	r.p.Run(func() {
		r.p.Report("route touch", r.p.client.PExpire(ctx, r.p.routeKey(sourceID), ttl).Err())
	})
	return nil
}

func (r routes) Delete(ctx context.Context, sourceID, tunnelID string) error {
	r.p.Run(func() {
		r.p.Report("route delete", r.p.deleteScript.Run(ctx, r.p.client, []string{r.p.routeKey(sourceID)}, tunnelID).Err())
	})
	return nil
}

type presence struct{ p *Plane }

func (pr presence) Publish(ctx context.Context, ev control.PresenceEvent) error {
	pr.p.EmitPresence(ev)
	pr.p.Run(func() { pr.p.publish(ctx, pr.p.chans.presence, cluster.EncodePresence(pr.p.Origin, ev)) })
	return nil
}

func (pr presence) Subscribe(fn func(control.PresenceEvent)) func() {
	return pr.p.SubscribePresence(fn)
}
