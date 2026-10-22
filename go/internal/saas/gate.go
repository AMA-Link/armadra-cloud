package saas

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/cluster"
	"github.com/AMA-Link/armadra-cloud/go/internal/edge"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// 团队维度的两种限流（SaaS 实施设计 §2.4 的 N4）。
const (
	// LimitEdgeConnectTeam 是团队的建连速率（各节点共用一个 Redis 桶）。
	LimitEdgeConnectTeam control.LimitKind = "edge.connect.team"
	// LimitEdgeConcurrentTeam 是团队的跨节点并发；上限来自 relay:quota:team:<teamId>，缺键用节点缺省。
	LimitEdgeConcurrentTeam control.LimitKind = "edge.concurrent.team"
)

// TeamLimitRules 是节点缺省的团队额度（控制面的 relay:quota:team:* 覆盖并发上限）。
var TeamLimitRules = map[control.LimitKind]control.LimitRule{
	LimitEdgeConnectTeam:    {Max: 6000, Window: time.Minute},
	LimitEdgeConcurrentTeam: {Concurrent: true, Max: 256},
}

// memberTTL 是并发计数里每个成员的有效期：节点每 memberRefresh 续一次，节点崩溃后成员最多留这么久。
const (
	memberTTL     = 60 * time.Second
	memberRefresh = 20 * time.Second
)

// 并发计数：有序集合 <前缀>conc:team:<teamId>，成员 <节点>:<随机>，分数是到期毫秒（Redis 的 TIME），
// 先清掉到期的再数，没满才加。整个判断在一个脚本里，各节点之间不会超发。
const concAdmit = `
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now)
if redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[2]) then return 0 end
redis.call('ZADD', KEYS[1], now + tonumber(ARGV[3]), ARGV[1])
redis.call('PEXPIRE', KEYS[1], tonumber(ARGV[3]) * 2)
return 1`

// concRefresh 给本节点还活着的成员续期（Redis 清空过也会补回来）。
const concRefresh = `
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
for i = 2, #ARGV do redis.call('ZADD', KEYS[1], now + tonumber(ARGV[1]), ARGV[i]) end
redis.call('PEXPIRE', KEYS[1], tonumber(ARGV[1]) * 2)
return 1`

// gate 是 SaaS 节点的准入（edge.Gate）：事故开关、团队速率与跨节点并发。
// 额度与事故开关在订阅就绪时全量读、之后按 kind: quota / incident 广播更新。
type gate struct {
	client  goredis.UniversalClient
	prefix  string
	node    string
	limits  func() control.Limiter
	onError func(operation string, err error)

	incident atomic.Bool

	mu     sync.Mutex
	quotas map[string]int
	// live 是本节点占着的成员：团队 → 成员集合（续期用）。
	live map[string]map[string]struct{}

	admitScript, refreshScript *goredis.Script
}

func newGate(client goredis.UniversalClient, prefix, node string, limits func() control.Limiter, onError func(string, error)) *gate {
	return &gate{
		client: client, prefix: prefix, node: node, limits: limits, onError: onError,
		quotas: map[string]int{}, live: map[string]map[string]struct{}{},
		admitScript: goredis.NewScript(concAdmit), refreshScript: goredis.NewScript(concRefresh),
	}
}

func (g *gate) concKey(team string) string  { return g.prefix + "conc:team:" + team }
func (g *gate) quotaKey(team string) string { return g.prefix + "quota:team:" + team }
func (g *gate) incidentKey() string         { return g.prefix + "incident" }

// Refuse 实现 edge.Gate：事故开关开着时拒绝新的端到端通道。
func (g *gate) Refuse() gen.ErrorCode {
	if g.incident.Load() {
		return gen.ErrLimitReached
	}
	return ""
}

// Rejecting 是事故开关是否开着（隧道升级也看它）。
func (g *gate) Rejecting() bool { return g.incident.Load() }

// limit 是团队的并发上限：控制面写的额度，没有就用节点缺省。
func (g *gate) limit(team string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if n, ok := g.quotas[team]; ok {
		return n
	}
	return TeamLimitRules[LimitEdgeConcurrentTeam].Max
}

// Admit 实现 edge.Gate：没有 team（个人空间）不限；有 team 先按团队速率，再占一个并发名额。
// Redis 出错时并发判断放行（只报告，不因 Redis 抖动挡住全部连接）；速率限流自己会退回本节点内存。
func (g *gate) Admit(ctx context.Context, in edge.GateInput) (func(), gen.ErrorCode, error) {
	if in.Team == "" {
		return func() {}, "", nil
	}
	hit, err := g.limits().Hit(ctx, LimitEdgeConnectTeam, in.Team)
	if err != nil {
		return nil, "", err
	}
	if !hit.Allowed {
		return nil, gen.ErrRateLimited, nil
	}
	member := g.node + ":" + randomHex(8)
	key := g.concKey(in.Team)
	ok, err := g.admitScript.Run(ctx, g.client, []string{key}, member, g.limit(in.Team), memberTTL.Milliseconds()).Int()
	if err != nil {
		g.onError("team concurrency", err)
		return func() {}, "", nil
	}
	if ok != 1 {
		return nil, gen.ErrLimitReached, nil
	}
	g.mu.Lock()
	set := g.live[in.Team]
	if set == nil {
		set = map[string]struct{}{}
		g.live[in.Team] = set
	}
	set[member] = struct{}{}
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			delete(g.live[in.Team], member)
			if len(g.live[in.Team]) == 0 {
				delete(g.live, in.Team)
			}
			g.mu.Unlock()
			rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := g.client.ZRem(rctx, key, member).Err(); err != nil {
				g.onError("team concurrency release", err)
			}
		})
	}, "", nil
}

// refresh 给本节点占着的全部成员续期。
func (g *gate) refresh(ctx context.Context) {
	g.mu.Lock()
	teams := make(map[string][]any, len(g.live))
	for team, set := range g.live {
		args := make([]any, 0, len(set)+1)
		args = append(args, memberTTL.Milliseconds())
		for m := range set {
			args = append(args, m)
		}
		teams[team] = args
	}
	g.mu.Unlock()
	for team, args := range teams {
		if err := g.refreshScript.Run(ctx, g.client, []string{g.concKey(team)}, args...).Err(); err != nil {
			g.onError("team concurrency refresh", err)
			return
		}
	}
}

// run 每 memberRefresh 续一次期，ctx 结束时返回。
func (g *gate) run(ctx context.Context) {
	ticker := time.NewTicker(memberRefresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			g.refresh(rctx)
			cancel()
		}
	}
}

// signal 收 kind: quota / incident 广播。
func (g *gate) signal(m cluster.Message) {
	switch m.Kind {
	case cluster.KindQuota:
		g.mu.Lock()
		g.quotas[m.Quota.TeamID] = m.Quota.Concurrent
		g.mu.Unlock()
	case cluster.KindIncident:
		g.incident.Store(m.Incident.RejectNewConnections)
	}
}

// reload 在订阅就绪时全量读额度与事故开关（广播在订阅之后才收得到，断线期间的变化靠这一次补上）。
func (g *gate) reload(ctx context.Context) error {
	quotas := map[string]int{}
	prefix := g.quotaKey("")
	var cursor uint64
	for {
		keys, next, err := g.client.Scan(ctx, cursor, prefix+"*", 500).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			values, err := g.client.MGet(ctx, keys...).Result()
			if err != nil {
				return err
			}
			for i, v := range values {
				text, ok := v.(string)
				if !ok {
					continue
				}
				team := strings.TrimPrefix(keys[i], prefix)
				if q, err := cluster.DecodeQuotaValue(team, []byte(text)); err == nil {
					quotas[team] = q.Concurrent
				}
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	raw, err := g.client.Get(ctx, g.incidentKey()).Bytes()
	switch {
	case errors.Is(err, goredis.Nil):
		g.incident.Store(false)
	case err != nil:
		return err
	default:
		if in, err := cluster.DecodeIncidentValue(raw); err == nil {
			g.incident.Store(in.RejectNewConnections)
		}
	}
	g.mu.Lock()
	g.quotas = quotas
	g.mu.Unlock()
	return nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("读取随机数失败：" + err.Error())
	}
	return hex.EncodeToString(b)
}
