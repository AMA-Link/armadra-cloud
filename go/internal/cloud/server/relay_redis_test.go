package server_test

import (
	"context"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/server"
	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/cluster"
	controlredis "github.com/AMA-Link/armadra-cloud/go/internal/control/redis"
)

// fakeNode 是一个假的中继节点：用同一份 Redis 前缀建一个 Plane，订阅命令与撤销。
type fakeNode struct {
	plane *controlredis.Plane
	cmds  chan control.NodeCommand
}

func (r *rig) newFakeNode(id string) *fakeNode {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	plane, err := controlredis.New(ctx, controlredis.Options{
		Options: cluster.Options{Issuer: appOrigin, NodeID: id, Mode: "saas"},
		URL:     redisURL(r.t), Prefix: r.relayPrefix, NodeChannel: true,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = plane.Close() })
	n := &fakeNode{plane: plane, cmds: make(chan control.NodeCommand, 64)}
	plane.NodeCommands().Subscribe(func(c control.NodeCommand) { n.cmds <- c })
	return n
}

func (n *fakeNode) revoked(jti string) bool {
	ok, _ := n.plane.Tokens().IsRevoked(context.Background(), jti)
	return ok
}

// waitKick 等一条符合的 kick，其余命令略过。
func (n *fakeNode) waitKick(t *testing.T, sourceID, subject string) control.NodeCommand {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case c := <-n.cmds:
			if c.Type == control.CommandKick && c.SourceID == sourceID && c.Subject == subject {
				return c
			}
		case <-deadline:
			t.Fatalf("等不到 kick：%s %s", sourceID, subject)
		}
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("等不到：%s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// startPeer 在同一份 PostgreSQL 与 Redis 上再起一个控制面实例。
func (r *rig) startPeer() *server.Running {
	r.t.Helper()
	run, err := server.Serve(context.Background(), r.cfg, r.deps)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(run.Close)
	return run
}

func (r *rig) redisClient() *goredis.Client {
	r.t.Helper()
	opts, err := goredis.ParseURL(redisURL(r.t))
	if err != nil {
		r.t.Fatal(err)
	}
	c := goredis.NewClient(opts)
	r.t.Cleanup(func() { _ = c.Close() })
	return c
}

// Redis 实现：撤销授予 → 已签断言与中继令牌进 PostgreSQL 与 Redis 的撤销表，假节点最终判为已撤销，并收到按人踢的命令。
func TestRedisRelayRevocationReachesFakeNode(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{redisRelay: true})
	node := r.newFakeNode("node-1")
	owner, friend := r.verified(), r.verified()
	h := r.addHost(owner, nil, "0.11.0")
	g := r.mustGrant(owner, h.id, guestOf(friend.accountID), "driver", nil)
	tok := r.assertion(friend, h.id)
	r.expect(tok, 200, "")
	a, b := jtisOf(t, tok)
	if n := r.issuedCount(friend.accountID, h.id); n != 2 {
		t.Fatalf("PostgreSQL 的签发索引：%d", n)
	}
	rc := r.redisClient()
	if n, _ := rc.ZCard(context.Background(), r.issuedPrefix+"acct:"+friend.accountID).Result(); n != 2 {
		t.Fatalf("Redis 的账号集合：%d", n)
	}

	r.expect(r.do("DELETE", "/v1/grants/"+g, nil, owner.bearer()), 200, "")
	eventually(t, "两枚令牌到达假节点", func() bool { return node.revoked(a) && node.revoked(b) })
	kick := node.waitKick(t, h.id, friend.accountID)
	if kick.Reason != "grant_revoked" {
		t.Fatalf("kick：%+v", kick)
	}
	if n := r.queryInt("SELECT count(*)::int FROM relay_revoked_tokens WHERE jti = $1 OR jti = $2", a, b); n != 2 {
		t.Fatalf("PostgreSQL 的撤销表：%d", n)
	}
	if n, _ := rc.ZCard(context.Background(), r.issuedPrefix+"acct:"+friend.accountID).Result(); n != 0 {
		t.Fatalf("撤销后集合应清空：%d", n)
	}
}

// heartbeatHint 读 relay:nodes：表空 503，有节点按 rendezvous 排序，排空的去掉（不再恒 503）。
func TestRedisRelayNodeTableDrivesHeartbeatHint(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{redisRelay: true})
	owner := r.verified()
	h := r.addHost(owner, nil, "0.11.0")
	r.expect(r.do("GET", "/v1/sources/me/relay", nil, r.sourceAuth(h)), 503, "unavailable")
	rc := r.redisClient()
	for _, id := range []string{"a", "b", "c", "d"} {
		info := control.NodeInfo{URL: "wss://" + id + ".edge.example.com/t/v1", Weight: 1, Draining: id == "d"}
		if err := rc.HSet(context.Background(), r.relayPrefix+"nodes", id, cluster.EncodeNode(info)).Err(); err != nil {
			t.Fatal(err)
		}
	}
	res := r.do("GET", "/v1/sources/me/relay", nil, r.sourceAuth(h))
	r.expect(res, 200, "")
	nodes := res.json["nodes"].([]any)
	if len(nodes) != 3 {
		t.Fatalf("应答前三个：%s", res.body)
	}
	want := control.RankNodes(h.id, []control.NodeInfo{
		{ID: "a", URL: "wss://a.edge.example.com/t/v1", Weight: 1}, {ID: "b", URL: "wss://b.edge.example.com/t/v1", Weight: 1},
		{ID: "c", URL: "wss://c.edge.example.com/t/v1", Weight: 1},
	})
	for i, n := range nodes {
		m := n.(map[string]any)
		if m["url"] != want[i].URL || m["weight"] != float64(want[i].Weight) {
			t.Fatalf("第 %d 个：%v，应为 %+v", i, m, want[i])
		}
	}
}

// Redis 清空（只清本测试前缀）后，新起的实例从 PostgreSQL 重放：撤销键、签发索引、团队额度都回来，
// 之后才启动的节点读得到；旧实例继续可用。
func TestRedisRelayReplayAfterRedisWipe(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{redisRelay: true})
	owner, friend := r.verified(), r.verified()
	h := r.addHost(owner, nil, "0.11.0")
	r.mustGrant(owner, h.id, guestOf(friend.accountID), "driver", nil)
	revokedTok := r.assertion(friend, h.id)
	r.expect(revokedTok, 200, "")
	revokedA, revokedB := jtisOf(t, revokedTok)
	r.expect(r.do("DELETE", "/v1/sources/"+h.id+"/access/"+friend.accountID, nil, owner.bearer()), 200, "")
	// 重新授予并签发：留下一条仍有效的索引。
	r.mustGrant(owner, h.id, guestOf(friend.accountID), "driver", nil)
	live := r.assertion(friend, h.id)
	r.expect(live, 200, "")
	liveA, _ := jtisOf(t, live)
	team := r.seedTeam("wipe", map[string]string{owner.accountID: "owner"})

	wipePrefixes(t, r.relayPrefix, r.issuedPrefix)
	rc := r.redisClient()
	ctx := context.Background()
	if n, _ := rc.Exists(ctx, r.relayPrefix+"revoked:"+revokedA).Result(); n != 0 {
		t.Fatal("清空失败")
	}
	r.startPeer() // 启动时重放，完成后才开监听
	if n, _ := rc.Exists(ctx, r.relayPrefix+"revoked:"+revokedA, r.relayPrefix+"revoked:"+revokedB).Result(); n != 2 {
		t.Fatalf("撤销键应从 PostgreSQL 重放回来：%d", n)
	}
	if n, _ := rc.ZCard(ctx, r.issuedPrefix+"acct:"+friend.accountID).Result(); n != 2 {
		t.Fatalf("签发索引应重建（仍有效的断言与中继令牌）：%d", n)
	}
	raw, err := rc.Get(ctx, r.relayPrefix+"quota:team:"+team).Bytes()
	if err != nil {
		t.Fatalf("团队额度键应全量重写：%v", err)
	}
	if q, err := cluster.DecodeQuotaValue(team, raw); err != nil || q.Concurrent != 10 {
		t.Fatalf("Free 团队并发 10：%+v %v", q, err)
	}
	late := r.newFakeNode("late")
	if !late.revoked(revokedA) || !late.revoked(revokedB) || late.revoked(liveA) {
		t.Fatal("清空之后才启动的节点应按重放的撤销键判断")
	}
	// 重放后的撤销照常到达：重放回来的索引能被找到并撤掉。
	r.expect(r.do("DELETE", "/v1/sources/"+h.id+"/access/"+friend.accountID, nil, owner.bearer()), 200, "")
	eventually(t, "重放后的索引被撤到节点", func() bool { return late.revoked(liveA) })
}

// relay:presence 的上下线：主机最近在线与核心版本落盘（只往后推）。
func TestRedisRelayPresenceRecordsLastSeen(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{redisRelay: true, flushEvery: 100 * time.Millisecond})
	node := r.newFakeNode("node-1")
	owner := r.verified()
	h := r.addHost(owner, nil, "0.10.0")
	at := r.clock.Ms() + 5000
	if err := node.plane.Presence().Publish(context.Background(), control.PresenceEvent{
		Type: control.PresenceSourceOnline, SourceID: h.id, Node: "node-1", TunnelID: "t1", CoreVersion: "0.12.0", AtMs: at,
	}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "last_seen 与版本落盘", func() bool {
		return r.queryInt("SELECT count(*)::int FROM sources WHERE source_id = $1 AND last_seen_at_ms = $2 AND core_version = '0.12.0'", h.id, at) == 1
	})
	// 更早的下线事件不会把最近在线往回推。
	if err := node.plane.Presence().Publish(context.Background(), control.PresenceEvent{
		Type: control.PresenceSourceOffline, SourceID: h.id, Node: "node-1", TunnelID: "t1", AtMs: at - 1000,
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if r.queryInt("SELECT count(*)::int FROM sources WHERE source_id = $1 AND last_seen_at_ms = $2", h.id, at) != 1 {
		t.Fatal("最近在线只往后推")
	}
}
