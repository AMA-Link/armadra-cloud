package saas

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
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

// 团队额度键缺失（或坏了）时节点的缺省：取最严格的套餐（Free 的并发 10，SaaS 实施设计 §4.10），速率按它
// 每条连接每分钟 60 次给；可用 RELAY_TEAM_CONCURRENT / RELAY_TEAM_CONNECT_PER_MINUTE 覆盖。控制面在启动与
// 订阅重新就绪时全量重写 relay:quota:team:*（S8），正常运行时不会走到缺省。
const (
	DefaultTeamConcurrent       = 10
	DefaultTeamConnectPerMinute = 600
)

// teamLimitRules 是团队维度的限流规则（并入控制面的规则表；并发的上限另由额度键覆盖）。
func teamLimitRules(cfg Config) map[control.LimitKind]control.LimitRule {
	return map[control.LimitKind]control.LimitRule{
		LimitEdgeConnectTeam:    {Max: cfg.TeamConnectPerMinute, Window: time.Minute},
		LimitEdgeConcurrentTeam: {Concurrent: true, Max: cfg.TeamConcurrent},
	}
}

// failOpenLogEvery 是「Redis 出错、团队并发放行」这条日志的最短间隔（另有计数指标）。
const failOpenLogEvery = 10 * time.Second

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

// concRefresh 给本节点还在的成员续期：只续已存在的（ZADD XX），不把刚释放的成员加回去。
const concRefresh = `
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
for i = 2, #ARGV do redis.call('ZADD', KEYS[1], 'XX', now + tonumber(ARGV[1]), ARGV[i]) end
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
	// fallback 是额度键缺失或坏了时的并发上限。
	fallback int
	// release 承接名额的 Redis 释放：自己的工作协程，不与边缘的账号并发释放共用队列；出错不重试。
	release *control.Dispatcher
	// stop 在 close 的排空超时之后取消：排队与进行中的释放立刻放弃（成员由过期兜底），不拖住下线。
	stop         context.Context
	cancelStop   context.CancelFunc
	flushTimeout time.Duration

	incident atomic.Bool
	// failOpen 是 Redis 出错时团队并发放行的次数（指标）；lastFailLog 给日志限频。
	failOpen    atomic.Int64
	lastFailLog atomic.Int64

	mu     sync.Mutex
	quotas map[string]int
	// live 是本节点占着的成员：团队 → 成员集合（续期用）。
	live map[string]map[string]struct{}

	admitScript, refreshScript *goredis.Script
}

func newGate(client goredis.UniversalClient, prefix, node string, fallback int, limits func() control.Limiter, onError func(string, error)) *gate {
	stop, cancelStop := context.WithCancel(context.Background())
	return &gate{
		stop: stop, cancelStop: cancelStop, flushTimeout: releaseFlushTimeout,
		client: client, prefix: prefix, node: node, limits: limits, onError: onError, fallback: fallback,
		release: control.NewDispatcher(1, 0),
		quotas:  map[string]int{}, live: map[string]map[string]struct{}{},
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
	return g.fallback
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
		g.failOpen.Add(1)
		now := time.Now().UnixMilli()
		if last := g.lastFailLog.Load(); now-last >= failOpenLogEvery.Milliseconds() && g.lastFailLog.CompareAndSwap(last, now) {
			g.onError("team concurrency", err)
		}
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
			// 投递即返回（调用方可能在 epoll 循环上）；只试一次，失败由成员过期兜底。
			g.release.Go(in.Team, func() { g.zrem(key, member) })
		})
	}, "", nil
}

// zrem 释放一个并发成员；下线放弃之后不再碰 Redis。
func (g *gate) zrem(key, member string) {
	if g.stop.Err() != nil {
		return
	}
	rctx, cancel := context.WithTimeout(g.stop, 5*time.Second)
	defer cancel()
	if err := g.client.ZRem(rctx, key, member).Err(); err != nil && g.stop.Err() == nil {
		g.onError("team concurrency release", err)
	}
}

// releaseFlushTimeout 是下线时等已投递的释放做完的最长时间。
const releaseFlushTimeout = 5 * time.Second

// close 等已投递的释放做完（最多 releaseFlushTimeout）；超时就放弃还排着的释放，
// 否则 Redis 卡住时每条释放各等满超时，Close 要等的就是队列长度乘 5 秒。
func (g *gate) close() {
	if !g.release.Flush(g.flushTimeout) {
		g.cancelStop()
	}
	g.release.Close()
	g.cancelStop()
}

// render 是准入闸的指标（接在原有指标之后）。
func (g *gate) render() string {
	return "# HELP relay_team_concurrency_fail_open_total Team concurrency checks let through because Redis failed.\n" +
		"# TYPE relay_team_concurrency_fail_open_total counter\n" +
		"relay_team_concurrency_fail_open_total " + strconv.FormatInt(g.failOpen.Load(), 10) + "\n"
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
	failed := 0
	var last error
	for team, args := range teams {
		if err := g.refreshScript.Run(ctx, g.client, []string{g.concKey(team)}, args...).Err(); err != nil {
			failed++
			last = err
		}
	}
	if failed > 0 {
		g.onError(fmt.Sprintf("team concurrency refresh (%d/%d)", failed, len(teams)), last)
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
				q, err := cluster.DecodeQuotaValue(team, []byte(text))
				if err != nil {
					// 坏了的额度按最严格的缺省（不进表），报出来让运维看到。
					g.onError("team quota decode", err)
					continue
				}
				quotas[team] = q.Concurrent
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
		in, err := cluster.DecodeIncidentValue(raw)
		if err != nil {
			// 坏了的开关值不改现状，报出来。
			g.onError("incident decode", err)
		} else {
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
