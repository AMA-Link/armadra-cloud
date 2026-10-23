package server

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/config"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/cluster"
	controlredis "github.com/AMA-Link/armadra-cloud/go/internal/control/redis"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// RelayPrefix 是控制面与中继节点共用的 Redis 键前缀（设计 §2.4）。
const RelayPrefix = controlredis.DefaultPrefix

// newRedisControl 建 Redis 版对节点的约定：Plane（Origin = cloud/<实例 id>）、relayctl.Redis 与重放。
// Plane 每次订阅就绪（包括断线重连）都回调重放，启动时由 Serve 在监听之前再显式跑一次。
func newRedisControl(ctx context.Context, cfg *config.Config, saasStore saas.Store, client goredis.UniversalClient, logger *obs.Logger, now func() time.Time, deps Deps, out *assembly) (*relayctl.Redis, *relayctl.Sync, error) {
	instance := newInstanceID()
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
