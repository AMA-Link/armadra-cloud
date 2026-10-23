package server

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/config"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/events"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/grants"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/cluster"
	controlredis "github.com/AMA-Link/armadra-cloud/go/internal/control/redis"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// RelayPrefix 是控制面与中继节点共用的 Redis 键前缀（设计 §2.4）。
const RelayPrefix = controlredis.DefaultPrefix

// newRedisControl 建 Redis 版对节点的约定：Plane（Origin = cloud/<实例 id>）、relayctl.Redis 与重放。
// Plane 每次订阅就绪（包括断线重连）都回调重放，启动时由 Serve 在监听之前再显式跑一次。
func newRedisControl(ctx context.Context, cfg *config.Config, instance string, saasStore saas.Store, client goredis.UniversalClient, logger *obs.Logger, now func() time.Time, deps Deps, out *assembly) (*relayctl.Redis, *relayctl.Sync, error) {
	var late atomic.Pointer[relayctl.Sync]
	plane, err := controlredis.New(ctx, controlredis.Options{
		Options: cluster.Options{
			Issuer: cfg.Issuer, Mode: "saas", Origin: "cloud/" + instance, NodeID: "cloud/" + instance, Now: now,
			OnError: func(operation string, err error) {
				logger.Warn("控制面后台操作失败", obs.Operation(operation), obs.Error(err))
			},
		},
		Client: client, Prefix: orDefault(deps.RelayPrefix, RelayPrefix),
		// 订阅每次重新就绪都重放：Redis 丢数据、断线期间的写都靠它补上。第一次就绪时 syncer 还没建好，
		// 由 Serve 在监听之前显式跑一次，同样挡住 /ready。
		OnSubscribed: func(ctx context.Context) error {
			if s := late.Load(); s != nil {
				return s.Run(ctx)
			}
			return nil
		},
	})
	if err != nil {
		return nil, nil, err
	}
	out.closers = append(out.closers, func() { _ = plane.Close() })
	redisControl := relayctl.NewRedis(relayctl.RedisOptions{
		Plane: plane, Now: now, Log: logger, IssuedPrefix: deps.IssuedPrefix,
		RevokeFailed: relayctl.RevokeFailureEnqueuer(saasStore, TenantID, now, func(err error) {
			logger.Error("撤销兜底作业入库失败", obs.Operation("relayctl.revoke"), obs.Error(err))
		}),
	})
	syncer := &relayctl.Sync{Store: saasStore, Tenant: TenantID, Redis: redisControl, Now: now, Log: logger}
	late.Store(syncer)
	return redisControl, syncer, nil
}

func orDefault(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

// presenceRecorder 累计主机的上下线：最近在线与核心版本只在内存累计，周期性（与设备的最近使用一起）
// 批量落盘 sources.last_seen_at_ms / core_version，只往后推。多个实例各记各的，落盘取较大值，所以互不冲突。
type presenceRecorder struct {
	ports *store.Ports
	log   *obs.Logger

	mu   sync.Mutex
	seen map[string]seenSource
}

type seenSource struct {
	atMs        int64
	coreVersion string
}

func newPresenceRecorder(ports *store.Ports, log *obs.Logger) *presenceRecorder {
	return &presenceRecorder{ports: ports, log: log, seen: map[string]seenSource{}}
}

// Observe 记一个上下线事件；上线带核心版本。
func (r *presenceRecorder) Observe(ev control.PresenceEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.seen[ev.SourceID]
	if ev.AtMs >= entry.atMs {
		entry.atMs = ev.AtMs
	}
	if ev.Type == control.PresenceSourceOnline && ev.CoreVersion != "" {
		entry.coreVersion = ev.CoreVersion
	}
	r.seen[ev.SourceID] = entry
}

// Flush 把累计的批量落盘；失败的条目留着下次再试（不丢）。
func (r *presenceRecorder) Flush(ctx context.Context) {
	r.mu.Lock()
	batch := r.seen
	r.seen = map[string]seenSource{}
	r.mu.Unlock()
	for id, seen := range batch {
		err := r.ports.Sources.Update(ctx, TenantID, id, func(draft *store.SourceRecord) error {
			if draft == nil {
				return nil
			}
			if draft.LastSeenAtMs == nil || *draft.LastSeenAtMs < seen.atMs {
				at := seen.atMs
				draft.LastSeenAtMs = &at
			}
			if seen.coreVersion != "" {
				draft.CoreVersion = seen.coreVersion
			}
			return nil
		})
		if err != nil {
			r.log.Warn("最近在线时间写入失败", obs.Operation("presence.flush"), obs.SourceID(id), obs.Error(err))
			r.mu.Lock()
			if cur, ok := r.seen[id]; !ok || cur.atMs < seen.atMs {
				r.seen[id] = seen
			}
			r.mu.Unlock()
		}
	}
}

// presenceAdapter 把控制面的在线事件接成控制核的 PresenceBus（me-stream 的 sourceOnline / sourceOffline）。
type presenceAdapter struct{ p control.Presence }

func (a presenceAdapter) Subscribe(fn func(core.PresenceEvent)) func() {
	return a.p.Subscribe(func(ev control.PresenceEvent) {
		fn(core.PresenceEvent{
			Online: ev.Type == control.PresenceSourceOnline, SourceID: ev.SourceID, CoreVersion: ev.CoreVersion, AtMs: ev.AtMs,
		})
	})
}

// accessPreparer 是事件在本机分发之前的准备：把授予 / 成员变动转成连接的可见集合更新（设计 §7.1）。
//
//   - grantChanged：受众里在本实例有连接的账号，逐个重算它对这台主机的可见性，作为内部事件 grants.AccessChanged
//     交给连接的 Watcher（core.InternalEvent，不会进客户端队列）；
//   - memberChanged：变动的账号（团队删除时是受众里在本实例有连接的账号）的连接整份重算。
func accessPreparer(c *core.Context, policy *grants.Policy, stream *core.MeStream, log *obs.Logger) func(context.Context, core.Event, *core.Audience) {
	local := func(audience *core.Audience) []string {
		if audience == nil {
			return nil
		}
		var out []string
		for _, id := range stream.Accounts() {
			if audience.Includes(core.Caller{AccountID: id}) {
				out = append(out, id)
			}
		}
		return out
	}
	return func(ctx context.Context, event core.Event, audience *core.Audience) {
		switch e := event.(type) {
		case events.GrantChanged:
			for _, id := range local(audience) {
				_, visible, err := policy.Access(ctx, core.Caller{AccountID: id}, e.SourceID)
				if err != nil {
					log.Warn("重算授予变动后的可见性失败", obs.Operation("events.access"), obs.SourceID(e.SourceID), obs.Error(err))
					continue
				}
				c.Events.Deliver(grants.AccessChanged{SourceID: e.SourceID, AccountID: id, Visible: visible}, nil)
			}
		case events.MemberChanged:
			targets := local(audience)
			if e.AccountID != "" {
				targets = []string{e.AccountID}
			}
			for _, id := range targets {
				if failed := stream.Resync(ctx, id); failed > 0 {
					log.Warn("成员变动后整份重算可见性失败", obs.Operation("events.access"), obs.Count(failed))
				}
			}
		}
	}
}
