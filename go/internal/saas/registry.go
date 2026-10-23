package saas

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/cluster"
)

// 节点表（SaaS 实施设计 §2.4 的 N3）：哈希 <前缀>nodes，字段是节点 id，值是 cluster.EncodeNode；
// 每个字段 HPEXPIRE 60 秒（Redis 7.4 起），节点每 20 秒续写一次，节点崩溃后最多留 60 秒。
const (
	nodeTTL   = 60 * time.Second
	nodeEvery = 20 * time.Second
)

// registry 维护本节点在节点表里的那一项。
type registry struct {
	client goredis.UniversalClient
	key    string
	info   control.NodeInfo
	counts func() (connections, tunnels int)
	every  time.Duration

	draining atomic.Bool
	mu       sync.Mutex // 串行写，不让并发的两次写乱序
	onError  func(operation string, err error)
}

func newRegistry(client goredis.UniversalClient, prefix string, info control.NodeInfo, counts func() (int, int), onError func(string, error)) *registry {
	return &registry{client: client, key: prefix + "nodes", info: info, counts: counts, every: nodeEvery, onError: onError}
}

// write 写一次本节点的那一项并续期。
func (r *registry) write(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	info := r.info
	info.Connections, info.Tunnels = r.counts()
	info.Draining = r.draining.Load()
	_, err := r.client.TxPipelined(ctx, func(pipe goredis.Pipeliner) error {
		pipe.HSet(ctx, r.key, info.ID, cluster.EncodeNode(info))
		pipe.HPExpire(ctx, r.key, nodeTTL, info.ID)
		return nil
	})
	return err
}

// drain 标成排空并立即写一次（控制面的 heartbeatHint 立刻不再给它）。
func (r *registry) drain(ctx context.Context) {
	r.draining.Store(true)
	if err := r.write(ctx); err != nil {
		r.onError("node table", err)
	}
}

// remove 从节点表里删掉本节点（正常下线的最后一步）。
func (r *registry) remove(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.client.HDel(ctx, r.key, r.info.ID).Err(); err != nil {
		r.onError("node table", err)
	}
}

// run 每 every 写一次，ctx 结束时返回。
func (r *registry) run(ctx context.Context) {
	ticker := time.NewTicker(r.every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := r.write(wctx); err != nil && !errors.Is(err, context.Canceled) {
				r.onError("node table", err)
			}
			cancel()
		}
	}
}

// ReadNodes 读节点表的全部项（坏记录跳过）。控制面的 heartbeatHint 与节点间转发找对端都用它。
func ReadNodes(ctx context.Context, client goredis.UniversalClient, prefix string) ([]control.NodeInfo, error) {
	all, err := client.HGetAll(ctx, prefix+"nodes").Result()
	if err != nil {
		return nil, err
	}
	out := make([]control.NodeInfo, 0, len(all))
	for id, value := range all {
		if n, err := cluster.DecodeNode(id, []byte(value)); err == nil {
			out = append(out, n)
		}
	}
	return out, nil
}

// ReadNode 读节点表的一项；没有答 (nil, nil)。
func ReadNode(ctx context.Context, client goredis.UniversalClient, prefix, id string) (*control.NodeInfo, error) {
	value, err := client.HGet(ctx, prefix+"nodes", id).Bytes()
	if errors.Is(err, goredis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	n, err := cluster.DecodeNode(id, value)
	if err != nil {
		return nil, nil
	}
	return &n, nil
}
