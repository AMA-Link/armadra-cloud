// Package redis 是控制面的 Redis 实现（多节点；设计 §3.6，对应
// packages/relay-runtime-node/src/control/redis.ts，键与频道逐项一致）。
//
//	| 功能          | 实现                                                                        |
//	| 路由          | <前缀>route:src:<id> = JSON，SET … PX；Touch 用 PEXPIRE；Delete 用脚本按 tunnelId CAS |
//	| 在线 / 命令   | 本节点立即投递 + PUBLISH <前缀>presence / <前缀>ctl                         |
//	| 撤销          | <前缀>revoked:<jti>，SET … PXAT，PUBLISH <前缀>revoked；各节点内存一份，订阅连接每次就绪都 SCAN 全量重读 |
//	| 速率限流      | <前缀>limit:<种类>:<键的 SHA-256>，脚本 INCR + 首次 PEXPIRE，各节点共用一个桶；出错退回本节点内存 |
//	| 并发限流      | 本节点内存                                                                  |
//
// 订阅用单独的连接，断线后自己重连（退避）并重新订阅；每次订阅就绪都全量重读撤销，断线期间的
// 撤销不漏。不写令牌明文（撤销只有 jti）。
package redis

import (
	"context"
	"errors"
	"fmt"
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
}

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

	stop     context.CancelFunc
	psMu     sync.Mutex
	ps       *goredis.PubSub
	stopped  bool
	loopDone chan struct{}
	ready    chan struct{}
	readyErr chan error
	once     sync.Once
	closeErr error
}

type channels struct{ presence, command, revoked string }

var _ control.Cluster = (*Plane)(nil)

// New 连上 Redis、订阅三个频道、全量读一遍撤销表，然后返回；连不上或读不了就报错。
// 订阅连接之后断线会自己重连并再读一遍。
func New(ctx context.Context, o Options) (*Plane, error) {
	client := o.Client
	owns := false
	if client == nil {
		if o.URL == "" {
			return nil, errors.New("Redis 控制面缺 Client 或 URL")
		}
		parsed, err := goredis.ParseURL(o.URL)
		if err != nil {
			// 解析错误可能带连接串片段：不转述。
			return nil, errors.New("Redis 连接串不合格式")
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
	loopCtx, stop := context.WithCancel(context.Background())
	p := &Plane{
		Base: cluster.NewBase(o.Options), client: client, ownsCli: owns, prefix: prefix,
		chans:        channels{presence: prefix + "presence", command: prefix + "ctl", revoked: prefix + "revoked"},
		deleteScript: goredis.NewScript(deleteRoute), hitScript: goredis.NewScript(hit),
		stop: stop, loopDone: make(chan struct{}), ready: make(chan struct{}), readyErr: make(chan error, 1),
	}
	go p.subscribeLoop(loopCtx, o.ReconnectMin, o.ReconnectMax)
	select {
	case <-p.ready:
		return p, nil
	case err := <-p.readyErr:
		_ = p.Close()
		return nil, err
	case <-ctx.Done():
		_ = p.Close()
		return nil, errors.New("Redis 控制面启动超时或被取消")
	}
}

func (p *Plane) routeKey(sourceID string) string { return p.prefix + "route:src:" + sourceID }
func (p *Plane) revokedKey(jti string) string    { return p.prefix + "revoked:" + jti }
func (p *Plane) limitKey(id string) string       { return p.prefix + "limit:" + id }

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
			case p.readyErr <- fmt.Errorf("Redis 订阅失败：%w", sanitize(err)):
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

// sanitize 去掉可能带连接串的细节，只留类别。
func sanitize(err error) error {
	var opErr interface{ Timeout() bool }
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return errors.New("超时")
	case errors.As(err, &opErr):
		return errors.New("网络错误")
	}
	msg := err.Error()
	if i := strings.Index(msg, "://"); i >= 0 {
		return errors.New("连接出错")
	}
	return err
}

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
	if err := p.reloadRevoked(ctx); err != nil {
		if first {
			return err
		}
		p.Report("reload revoked", err)
	}
	if first {
		close(p.ready)
	}
	for {
		msg, err := ps.ReceiveMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		p.receive([]byte(msg.Payload))
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
	}
}

// reloadRevoked SCAN 出全部 <前缀>revoked:*，到期时刻取 PEXPIRETIME。
func (p *Plane) reloadRevoked(ctx context.Context) error {
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
	p.Background("publish "+p.chans.command, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return p.client.Publish(ctx, p.chans.command, payload).Err()
	})
}

func (p *Plane) remoteHit(ctx context.Context, id string, window time.Duration) (int64, time.Duration, error) {
	reply, err := p.hitScript.Run(ctx, p.client, []string{p.limitKey(id)}, window.Milliseconds()).Slice()
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
		p.Settle()
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
