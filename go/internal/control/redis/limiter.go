package redis

import (
	"context"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/cluster"
)

// Limiter 是可单独构造的 Redis 速率限流器（不带路由、撤销与订阅）：SaaS 控制面用自己的前缀
// （cloud:limit:）建一个，不借第二个 Plane。桶是 <前缀><种类>:<键的 SHA-256>，脚本与 Plane 同一份
// （INCR + 首次 PEXPIRE），各实例共用一个桶；Redis 出错时退回本进程内存的固定窗口（不放行无限次）。
type Limiter struct {
	client  goredis.UniversalClient
	prefix  string
	rules   map[control.LimitKind]control.LimitRule
	script  *goredis.Script
	onError func(error)
	now     func() time.Time

	mu    sync.Mutex
	local map[string]*localWindow
}

type localWindow struct {
	count   int64
	resetAt time.Time
}

var _ control.Limiter = (*Limiter)(nil)

// NewLimiter 建限流器。rules 是种类到规则的表（只认速率类，Concurrent 的规则不支持）；
// onError 报告 Redis 失败（可为 nil）。
func NewLimiter(client goredis.UniversalClient, prefix string, rules map[control.LimitKind]control.LimitRule, onError func(error)) *Limiter {
	copied := make(map[control.LimitKind]control.LimitRule, len(rules))
	for kind, rule := range rules {
		copied[kind] = rule
	}
	return &Limiter{
		client: client, prefix: prefix, rules: copied, script: goredis.NewScript(hit),
		onError: onError, now: time.Now, local: map[string]*localWindow{},
	}
}

// Hit 实现 control.Limiter：窗口内第 Max+1 次起被拒，答还要等多久。
func (l *Limiter) Hit(ctx context.Context, kind control.LimitKind, key string) (control.LimitResult, error) {
	rule, ok := l.rules[kind]
	if !ok || rule.Concurrent || rule.Window <= 0 {
		return control.LimitResult{}, errUnknownKind(kind)
	}
	id := cluster.LimitID(kind, key)
	count, ttl, err := runHit(ctx, l.script, l.client, l.prefix+id, rule.Window)
	if err != nil {
		if l.onError != nil {
			l.onError(err)
		}
		count, ttl = l.localHit(id, rule.Window)
	}
	if count <= int64(rule.Max) {
		return control.LimitResult{Allowed: true}, nil
	}
	return control.LimitResult{RetryAfter: max(0, ttl)}, nil
}

// Release 实现 control.Limiter：速率类什么也不做。
func (l *Limiter) Release(context.Context, control.LimitKind, string) error { return nil }

func (l *Limiter) localHit(id string, window time.Duration) (int64, time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.local) > 4096 {
		for key, w := range l.local {
			if !w.resetAt.After(now) {
				delete(l.local, key)
			}
		}
	}
	w := l.local[id]
	if w == nil || !w.resetAt.After(now) {
		w = &localWindow{resetAt: now.Add(window)}
		l.local[id] = w
	}
	w.count++
	return w.count, w.resetAt.Sub(now)
}

type unknownKind control.LimitKind

func (k unknownKind) Error() string { return "没有这种限流：" + string(k) }

func errUnknownKind(kind control.LimitKind) error { return unknownKind(kind) }
