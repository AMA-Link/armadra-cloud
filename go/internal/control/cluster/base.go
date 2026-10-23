package cluster

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
)

// Options 是两种多节点实现共用的选项。
type Options struct {
	Issuer string
	// Mode 缺省 "personal"。
	Mode string
	// NodeID 是本节点的名字（路由的 Node 由隧道层写；这里只记着，供日志与诊断）。
	NodeID string
	// Origin 是广播外壳里的发送方标识（自己发的不再投递一次）；缺省随机。控制面实例用 cloud/<实例 id>，
	// 同一份 Redis 上节点与控制面的 origin 因此互不相同。
	Origin string
	// JWKS 返回公钥集的 JSON；缺省是空集。
	JWKS func(ctx context.Context) ([]byte, error)
	// Now 供测试换时钟，缺省 time.Now。
	Now func() time.Time
	// Limits 覆盖缺省限流规则（按种类合并）。
	Limits map[control.LimitKind]control.LimitRule
	// Revoked 是撤销表之外的撤销判断（personal：按存储判源 / 设备是否已撤），任一为真即撤销。
	Revoked func(ctx context.Context, jti string) (bool, error)
	// OnError 在后台操作（续路由、广播、清扫、限流退回本节点）失败时回调；
	// 只给操作名与错误，不带令牌与正文。
	OnError func(operation string, err error)
}

// Base 是多节点控制面的共用部分：本节点内存控制面（并发限流、远端出错时的速率限流退回、
// 进程内的在线事件与节点命令总线）、撤销表的内存副本、后台任务跟踪。
type Base struct {
	opts   Options
	Local  *control.Memory
	Origin string
	rules  map[control.LimitKind]control.LimitRule

	mu      sync.Mutex
	revoked map[string]int64 // jti → 到期毫秒

	tasks sync.WaitGroup
	out   *control.Dispatcher

	// Classify 把驱动错误换成不带主机、用户、库名的类别；后端建好后设置。报告出去的错误都经它。
	Classify func(error) string
}

// NewBase 建共用部分。
func NewBase(o Options) *Base {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Mode == "" {
		o.Mode = "personal"
	}
	rules := make(map[control.LimitKind]control.LimitRule, len(control.DefaultLimitRules))
	for k, r := range control.DefaultLimitRules {
		rules[k] = r
	}
	for k, r := range o.Limits {
		rules[k] = r
	}
	origin := o.Origin
	if origin == "" {
		var raw [8]byte
		_, _ = rand.Read(raw[:])
		origin = hex.EncodeToString(raw[:])
	}
	return &Base{
		opts: o,
		Local: control.NewMemory(control.MemoryOptions{
			Issuer: o.Issuer, Mode: o.Mode, JWKS: o.JWKS, Now: o.Now, Limits: o.Limits,
		}),
		out:     control.NewDispatcher(1, 0),
		Origin:  origin,
		rules:   rules,
		revoked: map[string]int64{},
	}
}

// Now 是配置的时钟。
func (b *Base) Now() time.Time { return b.opts.Now() }

// NodeID 是配置的节点名。
func (b *Base) NodeID() string { return b.opts.NodeID }

// Mode 实现 control.Plane。
func (b *Base) Mode() string { return b.opts.Mode }

// Issuer 实现 control.Plane。
func (b *Base) Issuer() string { return b.opts.Issuer }

// JWKS 实现 control.Plane。
func (b *Base) JWKS(ctx context.Context) ([]byte, error) { return b.Local.JWKS(ctx) }

// NodeCommands 实现 control.Plane：本节点的命令通道（本地发的与广播来的都从这里出）。
func (b *Base) NodeCommands() control.NodeCommands { return b.Local.NodeCommands() }

// EmitPresence 把在线事件投给本节点的订阅者（不广播）。
func (b *Base) EmitPresence(ev control.PresenceEvent) {
	_ = b.Local.Presence().Publish(context.Background(), ev)
}

// EmitCommand 把命令投给本节点的订阅者（不广播）。
func (b *Base) EmitCommand(c control.NodeCommand) { b.Local.Command(c) }

// SubscribePresence 订阅在线事件。
func (b *Base) SubscribePresence(fn func(control.PresenceEvent)) func() {
	return b.Local.Presence().Subscribe(fn)
}

// Report 报告后台失败：错误先经 Classify 换成类别，不把连接串、主机、用户名带进报告。
func (b *Base) Report(operation string, err error) {
	if b.opts.OnError == nil || err == nil {
		return
	}
	if b.Classify != nil {
		err = errors.New(b.Classify(err))
	}
	b.opts.OnError(operation, err)
}

// Background 在后台执行任务：失败只报告；Settle 会等它。
func (b *Base) Background(operation string, fn func() error) {
	b.tasks.Add(1)
	go func() {
		defer b.tasks.Done()
		b.Report(operation, fn())
	}()
}

// Queue 把一次对外广播排进本节点唯一的出站队列：按调用顺序一条条发，不并发。
// （节点命令这类「先 closeTunnel 后 kick」的先后顺序要在对端保持。）Settle 会等它。
func (b *Base) Queue(operation string, fn func() error) {
	b.tasks.Add(1)
	b.out.Go("out", func() {
		defer b.tasks.Done()
		b.Report(operation, fn())
	})
}

// Run 同步执行并让 Settle 看得见（用于调用方等着结果、但关闭时也要等它收尾的写）。
func (b *Base) Run(fn func()) {
	b.tasks.Add(1)
	defer b.tasks.Done()
	fn()
}

// Settle 等已发出的后台任务做完（测试与关闭前用）。
func (b *Base) Settle() { b.tasks.Wait() }

// Shutdown 等后台任务做完并停掉出站队列的协程。
func (b *Base) Shutdown() {
	b.tasks.Wait()
	b.out.Close()
}

// ------------------------------------------------------------ 撤销

// ReceiveRevoked 记下一条撤销（广播来的、本节点写的都走这里）；已过期的不记。
func (b *Base) ReceiveRevoked(jti string, expMs int64) {
	if expMs <= b.Now().UnixMilli() {
		return
	}
	b.mu.Lock()
	if cur, ok := b.revoked[jti]; !ok || cur < expMs {
		b.revoked[jti] = expMs
	}
	b.mu.Unlock()
}

// MergeRevoked 把全量重读到的撤销并进内存副本，顺手清掉已过期的。撤销记录只会随到期而消失，
// 所以是并集：重读期间收到的广播不会被冲掉。
func (b *Base) MergeRevoked(found map[string]int64) {
	at := b.Now().UnixMilli()
	b.mu.Lock()
	defer b.mu.Unlock()
	for jti, until := range b.revoked {
		if until <= at {
			delete(b.revoked, jti)
		}
	}
	for jti, until := range found {
		if until > at && b.revoked[jti] < until {
			b.revoked[jti] = until
		}
	}
}

// PruneRevoked 清掉内存副本里过期的撤销。
func (b *Base) PruneRevoked() { b.MergeRevoked(nil) }

// RevokedCount 返回内存副本里未过期的撤销数（测试与 /health 用）。
func (b *Base) RevokedCount() int {
	at := b.Now().UnixMilli()
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, until := range b.revoked {
		if until > at {
			n++
		}
	}
	return n
}

// Tokens 是 control.TokenStore：判断只查内存副本（加上 Options.Revoked）；Revoke 先记内存、
// 再调 persist 写存储、再调 broadcast 通知别的节点。
type Tokens struct {
	B         *Base
	Persist   func(ctx context.Context, jti string, expMs int64) error
	Broadcast func(ctx context.Context, jti string, expMs int64)
}

// IsRevoked 实现 control.TokenStore。
func (t Tokens) IsRevoked(ctx context.Context, jti string) (bool, error) {
	b := t.B
	at := b.Now().UnixMilli()
	b.mu.Lock()
	until, ok := b.revoked[jti]
	if ok && until <= at {
		delete(b.revoked, jti)
		ok = false
	}
	b.mu.Unlock()
	if ok {
		return true, nil
	}
	if b.opts.Revoked != nil {
		return b.opts.Revoked(ctx, jti)
	}
	return false, nil
}

// Revoke 实现 control.TokenStore：记到 exp 为止；已过期的不记。
func (t Tokens) Revoke(ctx context.Context, jti string, exp time.Time) error {
	b := t.B
	expMs := exp.UnixMilli()
	if expMs <= b.Now().UnixMilli() {
		return nil
	}
	b.ReceiveRevoked(jti, expMs)
	var err error
	b.Run(func() {
		if err = t.Persist(ctx, jti, expMs); err == nil {
			t.Broadcast(ctx, jti, expMs)
		}
	})
	return err
}

// ------------------------------------------------------------ 限流

// LimitID 是限流桶的键：种类 + 键的 SHA-256（IP、账号不以明文落库，长度也有界）。
func LimitID(kind control.LimitKind, key string) string {
	sum := sha256.Sum256([]byte(key))
	return string(kind) + ":" + hex.EncodeToString(sum[:])
}

// RemoteHit 是一次远端计数：窗口内的第几次、窗口还剩多久。
type RemoteHit func(ctx context.Context, id string, window time.Duration) (count int64, ttl time.Duration, err error)

// Limits 是 control.Limiter：速率类走远端计数（各节点共用一个桶），出错退回本节点内存；
// 并发类只在本节点内存（节点崩溃不留占用）。
type Limits struct {
	B      *Base
	Remote RemoteHit
}

// Hit 实现 control.Limiter。
func (l Limits) Hit(ctx context.Context, kind control.LimitKind, key string) (control.LimitResult, error) {
	b := l.B
	rule, ok := b.rules[kind]
	if !ok {
		return control.LimitResult{}, errUnknownKind(kind)
	}
	if rule.Concurrent {
		return b.Local.Limits().Hit(ctx, kind, key)
	}
	count, ttl, err := l.Remote(ctx, LimitID(kind, key), rule.Window)
	if err != nil {
		b.Report("limit", err)
		return b.Local.Limits().Hit(ctx, kind, key)
	}
	if count <= int64(rule.Max) {
		return control.LimitResult{Allowed: true}, nil
	}
	return control.LimitResult{RetryAfter: max(0, ttl)}, nil
}

// Release 实现 control.Limiter：并发类名额只在本节点内存。
func (l Limits) Release(ctx context.Context, kind control.LimitKind, key string) error {
	return l.B.Local.Limits().Release(ctx, kind, key)
}
