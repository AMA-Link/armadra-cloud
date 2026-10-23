package redis_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/cluster"
	cr "github.com/AMA-Link/armadra-cloud/go/internal/control/redis"
)

// SaaS 节点（G5）：订阅自己的 <前缀>node:<id>、收 quota / incident 广播、每次订阅就绪都回调。
func TestNodeChannelSignalsAndSubscribedHook(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var (
		mu      sync.Mutex
		signals []cluster.Message
		cmds    []control.NodeCommand
	)
	var subscribed atomic.Int32
	n := e.node("node-a", func(o *cr.Options) {
		o.NodeChannel = true
		o.OnSignal = func(m cluster.Message) { mu.Lock(); signals = append(signals, m); mu.Unlock() }
		o.OnSubscribed = func(context.Context) error { subscribed.Add(1); return nil }
	})
	n.NodeCommands().Subscribe(func(c control.NodeCommand) { mu.Lock(); cmds = append(cmds, c); mu.Unlock() })
	if subscribed.Load() != 1 {
		t.Fatalf("启动时就绪回调一次：%d", subscribed.Load())
	}
	if n.Client() == nil || n.Prefix() != e.prefix {
		t.Fatal("要暴露客户端与前缀")
	}

	// 控制面发到本节点频道的命令（带 G5 的过滤字段）。
	kick := control.NodeCommand{Type: control.CommandKick, SourceID: "s", Subject: "acct_1", Reason: "grant_revoked"}
	must(t, e.admin.Publish(ctx, e.prefix+"node:node-a", cluster.EncodeCommand("cloud/1", kick)).Err())
	// 发给别的节点的不收。
	must(t, e.admin.Publish(ctx, e.prefix+"node:node-b", cluster.EncodeCommand("cloud/1", control.NodeCommand{Type: control.CommandDrain, Node: "node-b"})).Err())
	must(t, e.admin.Publish(ctx, e.prefix+"ctl", cluster.EncodeQuota("cloud/1", control.TeamQuota{TeamID: "t1", Concurrent: 2})).Err())
	must(t, e.admin.Publish(ctx, e.prefix+"ctl", cluster.EncodeIncident("cloud/1", control.Incident{RejectNewConnections: true, AtMs: 5})).Err())
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		done := len(cmds) >= 1 && len(signals) >= 2
		mu.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(cmds) != 1 || cmds[0] != kick {
		t.Fatalf("本节点频道的命令：%+v", cmds)
	}
	if len(signals) != 2 || signals[0].Quota.TeamID != "t1" || !signals[1].Incident.RejectNewConnections {
		t.Fatalf("quota / incident：%+v", signals)
	}
}

// 就绪回调失败时启动失败（不对外答就绪）。
func TestSubscribedHookFailureFailsStart(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := cr.New(ctx, cr.Options{
		Options: cluster.Options{Issuer: "https://x", NodeID: "n"}, URL: e.url, Prefix: e.prefix,
		OnSubscribed: func(context.Context) error { return context.DeadlineExceeded },
	})
	if err == nil {
		t.Fatal("就绪回调失败应报错")
	}
}
