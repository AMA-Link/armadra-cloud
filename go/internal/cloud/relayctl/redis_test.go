package relayctl_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/cluster"
	controlredis "github.com/AMA-Link/armadra-cloud/go/internal/control/redis"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/identity"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/pgtest"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/postgres"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

func redisURL(t testing.TB) string {
	t.Helper()
	url := os.Getenv("REDIS_URL")
	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("CI 里没有 REDIS_URL")
		}
		t.Skip("没有 Redis：设置 REDIS_URL，跳过")
	}
	return url
}

// redisEnv 给每个测试一组随机前缀（节点用的 relay-test:xxxx: 与签发索引用的 cloud-test:xxxx:），结束时只删这两个前缀下的键，
// 不碰别的键，更不清库。
type redisEnv struct {
	t      *testing.T
	url    string
	prefix string
	issued string
	admin  *goredis.Client
}

func newRedisEnv(t *testing.T) *redisEnv {
	t.Helper()
	url := redisURL(t)
	opts, err := goredis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	var raw [6]byte
	_, _ = rand.Read(raw[:])
	id := hex.EncodeToString(raw[:])
	e := &redisEnv{t: t, url: url, prefix: "relay-test:" + id + ":", issued: "cloud-test:" + id + ":", admin: goredis.NewClient(opts)}
	t.Cleanup(func() {
		e.wipe()
		_ = e.admin.Close()
	})
	return e
}

// wipe 只删本测试两个前缀下的键（模拟「Redis 被清空」）。
func (e *redisEnv) wipe() {
	for _, prefix := range []string{e.prefix, e.issued} {
		iter := e.admin.Scan(context.Background(), 0, prefix+"*", 200).Iterator()
		for iter.Next(context.Background()) {
			e.admin.Del(context.Background(), iter.Val())
		}
	}
}

type nodeSink struct {
	mu       sync.Mutex
	commands []control.NodeCommand
	quotas   []control.TeamQuota
	incident []control.Incident
}

func (s *nodeSink) snapshot() ([]control.NodeCommand, []control.TeamQuota, []control.Incident) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]control.NodeCommand(nil), s.commands...), append([]control.TeamQuota(nil), s.quotas...), append([]control.Incident(nil), s.incident...)
}

// newNode 是一个假节点：订阅 relay:ctl、relay:node:<id>、relay:revoked，记下收到的命令与信号。
func (e *redisEnv) newNode(id string) (*controlredis.Plane, *nodeSink) {
	e.t.Helper()
	sink := &nodeSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	plane, err := controlredis.New(ctx, controlredis.Options{
		Options: cluster.Options{Issuer: "https://x.test", NodeID: id, Mode: "saas"}, URL: e.url, Prefix: e.prefix,
		NodeChannel: true,
		OnSignal: func(m cluster.Message) {
			sink.mu.Lock()
			defer sink.mu.Unlock()
			switch m.Kind {
			case cluster.KindQuota:
				sink.quotas = append(sink.quotas, m.Quota)
			case cluster.KindIncident:
				sink.incident = append(sink.incident, m.Incident)
			}
		},
	})
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = plane.Close() })
	plane.NodeCommands().Subscribe(func(c control.NodeCommand) {
		sink.mu.Lock()
		sink.commands = append(sink.commands, c)
		sink.mu.Unlock()
	})
	return plane, sink
}

func (e *redisEnv) newControl(instance string, revokeFailed func(context.Context, string, int64)) (*relayctl.Redis, *controlredis.Plane) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	plane, err := controlredis.New(ctx, controlredis.Options{
		Options: cluster.Options{Issuer: "https://x.test", Mode: "saas", Origin: "cloud/" + instance}, URL: e.url, Prefix: e.prefix,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = plane.Close() })
	return relayctl.NewRedis(relayctl.RedisOptions{Plane: plane, IssuedPrefix: e.issued, RevokeFailed: revokeFailed}), plane
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

func inHour() int64 { return time.Now().Add(time.Hour).UnixMilli() }

// 撤销经 Plane 写 relay:revoked:<jti> 并广播：假节点最终判为已撤销；索引集合与 jti 键清掉；按账号、主机、jti 三种选法都找得到。
func TestRedisRevokeReachesNodeAndClearsIndex(t *testing.T) {
	e := newRedisEnv(t)
	rc, _ := e.newControl("a", nil)
	node, _ := e.newNode("node-1")
	exp := inHour()
	for _, i := range []relayctl.Issued{
		{Subject: "a1", SourceID: "s1", DeviceID: "d1", JTI: "r.d1.s1.x", ExpMs: exp},
		{Subject: "a1", SourceID: "s2", DeviceID: "d1", JTI: "r.d1.s2.x", ExpMs: exp},
		{Subject: "a2", SourceID: "s1", DeviceID: "d2", JTI: "r.d2.s1.y", ExpMs: exp},
		{Subject: relayctl.NoAccount, SourceID: "s1", JTI: "t.s1.z", ExpMs: exp},
		{Subject: "a3", SourceID: "s1", DeviceID: "d3", JTI: "gone", ExpMs: time.Now().Add(-time.Minute).UnixMilli()},
	} {
		if err := rc.IndexIssued(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := e.admin.ZCard(ctx, e.issued+"src:s1").Result(); n != 3 {
		t.Fatalf("已到期的不进索引，主机集合有 3 条：%d", n)
	}
	hit, err := rc.Revoke(ctx, relayctl.Filter{Subject: "a1", SourceID: "s1"}, "")
	if err != nil || len(hit) != 1 || hit[0].JTI != "r.d1.s1.x" || hit[0].ExpMs != exp || hit[0].DeviceID != "d1" {
		t.Fatalf("按账号 ∩ 主机撤：%+v %v", hit, err)
	}
	eventually(t, "节点判为已撤销", func() bool { ok, _ := node.Tokens().IsRevoked(ctx, "r.d1.s1.x"); return ok })
	if ok, _ := node.Tokens().IsRevoked(ctx, "r.d1.s2.x"); ok {
		t.Fatal("别的主机上的令牌不该被撤")
	}
	if n, _ := e.admin.Exists(ctx, e.prefix+"revoked:r.d1.s1.x").Result(); n != 1 {
		t.Fatal("撤销键应写在节点前缀下")
	}
	// 保留一台设备；撤销过的不会再命中。
	hit, err = rc.Revoke(ctx, relayctl.Filter{Subject: "a1"}, "d1")
	if err != nil || len(hit) != 0 {
		t.Fatalf("保留 d1：%+v %v", hit, err)
	}
	// 只知道 jti（签发方复核）。
	hit, err = rc.Revoke(ctx, relayctl.Filter{JTI: "r.d2.s1.y"}, "")
	if err != nil || len(hit) != 1 || hit[0].Subject != "a2" {
		t.Fatalf("按 jti 撤：%+v %v", hit, err)
	}
	// 主机的全部（含隧道令牌）。
	n, err := rc.RevokeWhere(ctx, relayctl.Filter{SourceID: "s1"})
	if err != nil || n != 1 {
		t.Fatalf("撤主机 s1 剩下的（隧道令牌）：%d %v", n, err)
	}
	eventually(t, "隧道令牌被撤", func() bool { ok, _ := node.Tokens().IsRevoked(ctx, "t.s1.z"); return ok })
	if left, _ := e.admin.ZCard(ctx, e.issued+"acct:a1").Result(); left != 1 {
		t.Fatalf("a1 在 s2 上的令牌还在：%d", left)
	}
	if n, _ := e.admin.ZCard(ctx, e.issued+"src:s1").Result(); n != 0 {
		t.Fatalf("s1 集合应空：%d", n)
	}
	// 全空过滤什么也不撤。
	if hit, err := rc.Revoke(ctx, relayctl.Filter{}, ""); err != nil || len(hit) != 0 {
		t.Fatalf("空过滤：%+v %v", hit, err)
	}
}

// 命令：总是经 relay:ctl 广播；已知持有节点再发 relay:node:<id>。形状带 G5 的过滤字段；kick 缺 sourceId 拒绝。
func TestRedisCommandsReachNodes(t *testing.T) {
	e := newRedisEnv(t)
	rc, _ := e.newControl("a", nil)
	holder, held := e.newNode("node-1")
	other, others := e.newNode("node-2")
	_ = holder
	_ = other
	// node-1 持有 s1。
	if err := e.admin.Set(ctx, e.prefix+"route:src:s1", cluster.EncodeRoute(control.Route{Node: "node-1", TunnelID: "t", SinceMs: 1, CoreVersion: "0.10.0"}), time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := rc.Route(ctx, "s1"); err != nil || !ok || v != "0.10.0" {
		t.Fatalf("路由：%q %v %v", v, ok, err)
	}
	if _, ok, _ := rc.Route(ctx, "nope"); ok {
		t.Fatal("没有路由就是不在线")
	}
	if err := rc.Kick(ctx, relayctl.KickFilter{SourceID: "s1", Subject: "a1", DeviceID: "d1"}, "device_revoked"); err != nil {
		t.Fatal(err)
	}
	if err := rc.Kick(ctx, relayctl.KickFilter{SourceID: "s2", SubjectPrefix: "guest:l1:"}, "x"); err != nil {
		t.Fatal(err)
	}
	if err := rc.CloseTunnel(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if err := rc.Kick(ctx, relayctl.KickFilter{Subject: "a1"}, "x"); err == nil {
		t.Fatal("kick 缺 sourceId 要拒绝")
	}
	// 广播：node-2 恰好收到三条（s1 的 kick、s2 的 kick、s1 的 closeTunnel）。
	eventually(t, "广播到达", func() bool { c, _, _ := others.snapshot(); return len(c) >= 3 })
	cmds, _, _ := others.snapshot()
	sort.Slice(cmds, func(i, j int) bool {
		return string(cmds[i].Type)+cmds[i].SourceID < string(cmds[j].Type)+cmds[j].SourceID
	})
	want := []control.NodeCommand{
		{Type: control.CommandCloseTunnel, SourceID: "s1"},
		{Type: control.CommandKick, SourceID: "s1", Subject: "a1", DeviceID: "d1", Reason: "device_revoked"},
		{Type: control.CommandKick, SourceID: "s2", SubjectPrefix: "guest:l1:", Reason: "x"},
	}
	if len(cmds) != 3 || cmds[0] != want[0] || cmds[1] != want[1] || cmds[2] != want[2] {
		t.Fatalf("广播的命令：%+v", cmds)
	}
	// 持有节点：对 s1 的两条命令广播 + 节点频道各一次，共多收两条。
	eventually(t, "持有节点额外收到节点频道", func() bool { c, _, _ := held.snapshot(); return len(c) >= 5 })
	// drain：带 node，发到该节点的频道。
	if err := rc.Drain(ctx, "node-2"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "drain", func() bool {
		c, _, _ := others.snapshot()
		for _, cmd := range c {
			if cmd.Type == control.CommandDrain && cmd.Node == "node-2" {
				return true
			}
		}
		return false
	})
	if err := rc.Drain(ctx, ""); err == nil {
		t.Fatal("drain 缺 node 要拒绝")
	}
}

// 节点表：读 relay:nodes，去掉排空的与坏记录，按 id 排序；额度与事故开关：先写键再广播。
func TestRedisNodesQuotaAndIncident(t *testing.T) {
	e := newRedisEnv(t)
	rc, _ := e.newControl("a", nil)
	_, sink := e.newNode("node-1")
	put := func(id string, info control.NodeInfo) {
		t.Helper()
		info.ID = id
		if err := e.admin.HSet(ctx, e.prefix+"nodes", id, cluster.EncodeNode(info)).Err(); err != nil {
			t.Fatal(err)
		}
	}
	put("b", control.NodeInfo{URL: "wss://b.test/t/v1", Weight: 2, Version: "0.10.0", Connections: 3, Tunnels: 1})
	put("a", control.NodeInfo{URL: "wss://a.test/t/v1", Weight: 1})
	put("c", control.NodeInfo{URL: "wss://c.test/t/v1", Weight: 1, Draining: true})
	if err := e.admin.HSet(ctx, e.prefix+"nodes", "bad", "{").Err(); err != nil {
		t.Fatal(err)
	}
	nodes, err := rc.Nodes(ctx)
	if err != nil || len(nodes) != 2 || nodes[0].ID != "a" || nodes[1].ID != "b" || nodes[1].URL != "wss://b.test/t/v1" || nodes[1].Connections != 3 {
		t.Fatalf("节点表：%+v %v", nodes, err)
	}
	// AllNodes（运营后台列节点）含排空中的。
	all, err := rc.AllNodes(ctx)
	if err != nil || len(all) != 3 || all[2].ID != "c" || !all[2].Draining || all[0].Draining {
		t.Fatalf("全部节点（含排空）：%+v %v", all, err)
	}

	if err := rc.SetTeamQuota(ctx, "t1", relayctl.Quota{Concurrent: 7}); err != nil {
		t.Fatal(err)
	}
	if err := rc.SetTeamQuota(ctx, "t2", relayctl.Quota{Concurrent: -1}); err != nil {
		t.Fatal(err)
	}
	raw, err := e.admin.Get(ctx, e.prefix+"quota:team:t1").Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if q, err := cluster.DecodeQuotaValue("t1", raw); err != nil || q.Concurrent != 7 {
		t.Fatalf("额度键：%+v %v", q, err)
	}
	raw, _ = e.admin.Get(ctx, e.prefix+"quota:team:t2").Bytes()
	if q, err := cluster.DecodeQuotaValue("t2", raw); err != nil || q.Concurrent != math.MaxInt32 {
		t.Fatalf("不限写成大数（节点不收负数）：%+v %v", q, err)
	}
	eventually(t, "额度广播", func() bool { _, q, _ := sink.snapshot(); return len(q) >= 2 })
	if _, q, _ := sink.snapshot(); q[0].TeamID != "t1" || q[0].Concurrent != 7 {
		t.Fatalf("额度广播：%+v", q)
	}
	if err := rc.ClearTeamQuota(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.admin.Exists(ctx, e.prefix+"quota:team:t1").Result(); n != 0 {
		t.Fatal("清除后键应不在")
	}

	if err := rc.SetIncident(ctx, relayctl.Incident{RejectNewConnections: true, AtMs: 42}); err != nil {
		t.Fatal(err)
	}
	raw, err = e.admin.Get(ctx, e.prefix+"incident").Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if in, err := cluster.DecodeIncidentValue(raw); err != nil || !in.RejectNewConnections || in.AtMs != 42 {
		t.Fatalf("事故键：%+v %v", in, err)
	}
	eventually(t, "事故广播", func() bool { _, _, i := sink.snapshot(); return len(i) == 1 && i[0].RejectNewConnections })
}

// failRevokedSet 让写撤销键的 SET 失败（别的命令照常），模拟 Redis 写撤销时出错。
type failRevokedSet struct {
	prefix string
	fail   *bool
	mu     *sync.Mutex
	tries  *int
}

func (failRevokedSet) DialHook(next goredis.DialHook) goredis.DialHook { return next }
func (failRevokedSet) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return next
}
func (h failRevokedSet) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		args := cmd.Args()
		if len(args) > 1 {
			if name, _ := args[0].(string); name == "set" || name == "SET" {
				if key, _ := args[1].(string); len(key) > len(h.prefix)+8 && key[:len(h.prefix)+8] == h.prefix+"revoked:" {
					h.mu.Lock()
					defer h.mu.Unlock()
					if *h.fail {
						*h.tries++
						return errors.New("dial tcp 10.0.0.1:6379: secret-host refused")
					}
				}
			}
		}
		return next(ctx, cmd)
	}
}

// 撤销写 Redis 重试 3 次仍失败：记下并交给 revokeFailed（relay.revoke 作业），错误不带连接信息；
// 条目仍算命中（调用方据此踢人）；Redis 恢复后同一条撤销可以重放（幂等）。
func TestRedisRevokeFailureHandsOffToJob(t *testing.T) {
	e := newRedisEnv(t)
	opts, _ := goredis.ParseURL(e.url)
	client := goredis.NewClient(opts)
	t.Cleanup(func() { _ = client.Close() })
	var (
		mu    sync.Mutex
		fail  = true
		tries int
	)
	client.AddHook(failRevokedSet{prefix: e.prefix, fail: &fail, mu: &mu, tries: &tries})
	plane, err := controlredis.New(ctx, controlredis.Options{
		Options: cluster.Options{Issuer: "https://x.test", Mode: "saas", Origin: "cloud/a"}, Client: client, Prefix: e.prefix,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plane.Close() })
	var failed []string
	rc := relayctl.NewRedis(relayctl.RedisOptions{
		Plane: plane, IssuedPrefix: e.issued,
		RevokeFailed: func(_ context.Context, jti string, _ int64) { failed = append(failed, jti) },
	})
	exp := inHour()
	if err := rc.IndexIssued(ctx, relayctl.Issued{Subject: "a1", SourceID: "s1", DeviceID: "d1", JTI: "j1", ExpMs: exp}); err != nil {
		t.Fatal(err)
	}
	hit, err := rc.Revoke(ctx, relayctl.Filter{Subject: "a1"}, "")
	if err == nil || len(hit) != 1 {
		t.Fatalf("命中仍答，错误要报：%+v %v", hit, err)
	}
	if msg := err.Error(); msg != "Redis 操作失败" {
		t.Fatalf("错误不带连接信息：%q", msg)
	}
	mu.Lock()
	n := tries
	fail = false
	mu.Unlock()
	if n != 3 || len(failed) != 1 || failed[0] != "j1" {
		t.Fatalf("重试 3 次后交给作业：tries=%d failed=%v", n, failed)
	}
	// 作业重放同一条撤销（PostgreSQL 已有）：Restore 幂等地写回。
	if err := rc.Restore(ctx, []relayctl.Revoked{{JTI: "j1", ExpMs: exp}}, nil); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.admin.Exists(ctx, e.prefix+"revoked:j1").Result(); n != 1 {
		t.Fatal("重放后撤销键应在")
	}
	if err := rc.Restore(ctx, []relayctl.Revoked{{JTI: "j1", ExpMs: exp}}, nil); err != nil {
		t.Fatalf("重放要幂等：%v", err)
	}
}

// 重放：Redis 被清空（只清本测试的前缀）后，从 PostgreSQL 重放撤销表、签发索引与团队额度，节点重读也看得到。
func TestSyncRestoresRedisFromPostgres(t *testing.T) {
	e := newRedisEnv(t)
	url, schema := pgtest.Schema(t)
	db, err := postgres.Open(ctx, postgres.Options{URL: url, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.CreateTenant(ctx, "cloud", store.Tenant{TenantSettings: store.TenantSettings{Issuer: "https://x.test", TrustedOrigins: []string{}}}); err != nil {
		t.Fatal(err)
	}
	st := db.SaaS()
	pw := identity.PasswordRecord{KDF: "scrypt", Cost: 16384, Block: 8, Parallel: 1, Length: 32, Salt: "c2FsdA==", Hash: "aGFzaA=="}
	err = st.Tx(ctx, func(tx saas.Tx) error {
		if err := tx.Accounts().Create(ctx, "cloud", saas.Account{
			AccountRecord: store.AccountRecord{AccountID: "a1", Name: "a1", CreatedAtMs: 1, UpdatedAtMs: 1, Password: pw}, Email: "a1@x.test", DisplayName: "A", Locale: "en",
		}); err != nil {
			return err
		}
		for _, id := range []string{"t-free", "t-big"} {
			if err := tx.Teams().Create(ctx, "cloud", saas.Team{TeamID: id, Slug: id, Name: id, Settings: []byte(`{}`), CreatedBy: "a1", CreatedAtMs: 1, UpdatedAtMs: 1}, "a1"); err != nil {
				return err
			}
		}
		_, err := tx.Accounts().LockAccount(ctx, "cloud", "a1")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `UPDATE entitlements SET overrides = '{"concurrent": 50}' WHERE tenant_id = 'cloud' AND space_id = 't-big'`); err != nil {
		t.Fatal(err)
	}

	rc, _ := e.newControl("a", nil)
	d := relayctl.NewDurable(rc, st, "cloud", nil, nil)
	sync := &relayctl.Sync{Store: st, Tenant: "cloud", Redis: rc}
	for _, i := range []relayctl.Issued{
		{Subject: "a1", SourceID: "s1", DeviceID: "d1", JTI: "r.live", ExpMs: inHour()},
		{Subject: "a1", SourceID: "s1", DeviceID: "d2", JTI: "r.dead", ExpMs: inHour()},
		{Subject: relayctl.NoAccount, SourceID: "s1", JTI: "t.live", ExpMs: inHour()},
	} {
		if err := d.IndexIssued(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := d.RevokeWhere(ctx, relayctl.Filter{Subject: "a1", DeviceID: "d2"}); err != nil || n != 1 {
		t.Fatalf("撤 d2：%d %v", n, err)
	}
	if err := sync.Run(ctx); err != nil {
		t.Fatal(err)
	}

	// 清空 Redis。
	e.wipe()
	if n, _ := e.admin.Exists(ctx, e.prefix+"revoked:r.dead").Result(); n != 0 {
		t.Fatal("清空失败")
	}
	node, _ := e.newNode("node-1") // 清空之后才启动的节点：靠键恢复，不靠广播
	if ok, _ := node.Tokens().IsRevoked(ctx, "r.dead"); ok {
		t.Fatal("清空后节点不该知道撤销")
	}
	if err := sync.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sync.Ready(ctx); err != nil {
		t.Fatalf("重放完成后就绪：%v", err)
	}
	if ttl, _ := e.admin.PTTL(ctx, e.prefix+"revoked:r.dead").Result(); ttl <= 0 {
		t.Fatalf("撤销键应带到期：%v", ttl)
	}
	acct, _ := e.admin.ZRange(ctx, e.issued+"acct:a1", 0, -1).Result()
	if len(acct) != 1 || acct[0] != "a1|s1|d1|r.live" {
		t.Fatalf("账号集合：%v", acct)
	}
	if src, _ := e.admin.ZRange(ctx, e.issued+"src:s1", 0, -1).Result(); len(src) != 2 {
		t.Fatalf("主机集合（含隧道令牌）：%v", src)
	}
	for team, want := range map[string]int{"t-free": 10, "t-big": 50} {
		raw, err := e.admin.Get(ctx, e.prefix+"quota:team:"+team).Bytes()
		if err != nil {
			t.Fatalf("%s 的额度键：%v", team, err)
		}
		if q, err := cluster.DecodeQuotaValue(team, raw); err != nil || q.Concurrent != want {
			t.Fatalf("%s 的额度：%+v %v", team, q, err)
		}
	}
	// 重放之后的撤销照常：账号集合里的 r.live 被撤，并进撤销表。
	if n, err := d.RevokeWhere(ctx, relayctl.Filter{Subject: "a1"}); err != nil || n != 1 {
		t.Fatalf("重放后撤销：%d %v", n, err)
	}
	if n, _ := e.admin.Exists(ctx, e.prefix+"revoked:r.live").Result(); n != 1 {
		t.Fatal("重放后的撤销应写进 Redis")
	}
	// 节点（订阅就绪时全量重读）。第二个节点启动即读到 r.dead。
	late, _ := e.newNode("node-2")
	if ok, _ := late.Tokens().IsRevoked(ctx, "r.dead"); !ok {
		t.Fatal("重放后新启动的节点应读到撤销")
	}
}

// 重放进行中不就绪。
func TestSyncReadyFailsWhileSyncing(t *testing.T) {
	e := newRedisEnv(t)
	rc, _ := e.newControl("a", nil)
	gate := make(chan struct{})
	entered := make(chan struct{})
	s := &relayctl.Sync{Store: blockingStore{inner: nil, enter: entered, wait: gate}, Tenant: "cloud", Redis: rc}
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	<-entered
	if err := s.Ready(ctx); !errors.Is(err, relayctl.ErrSyncing) {
		t.Fatalf("重放中应不就绪：%v", err)
	}
	close(gate)
	if err := <-done; err == nil {
		t.Fatal("假存储的 Tx 返回错误，Run 应报错")
	}
	if err := s.Ready(ctx); err != nil {
		t.Fatalf("结束后就绪：%v", err)
	}
}

type blockingStore struct {
	saas.Store
	inner saas.Store
	enter chan struct{}
	wait  chan struct{}
}

func (b blockingStore) Tx(context.Context, func(saas.Tx) error) error {
	close(b.enter)
	<-b.wait
	return errors.New("假存储")
}

// core 的撤销钩子经 Redis 实现：撤设备撤令牌并按 (源, subject, 设备) 踢；撤源只撤令牌。
func TestRedisSinkRevokesAndKicks(t *testing.T) {
	e := newRedisEnv(t)
	rc, _ := e.newControl("a", nil)
	_, sink := e.newNode("node-1")
	exp := inHour()
	for _, i := range []relayctl.Issued{
		{Subject: "a1", SourceID: "s1", DeviceID: "d1", JTI: "j1", ExpMs: exp},
		{Subject: "a1", SourceID: "s1", DeviceID: "d1", JTI: "j2", ExpMs: exp},
		{Subject: "a1", SourceID: "s2", DeviceID: "d1", JTI: "j3", ExpMs: exp},
		{Subject: "a1", SourceID: "s1", DeviceID: "d9", JTI: "j4", ExpMs: exp},
		{Subject: "guest:l1:x", SourceID: "s1", DeviceID: "", JTI: "g1", ExpMs: exp},
	} {
		if err := rc.IndexIssued(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
	rc.Revocations().DeviceRevoked(ctx, "a1", "d1")
	eventually(t, "每个 (源, 设备) 一条 kick", func() bool { c, _, _ := sink.snapshot(); return len(c) >= 2 })
	cmds, _, _ := sink.snapshot()
	got := map[string]bool{}
	for _, c := range cmds {
		if c.Type != control.CommandKick || c.Subject != "a1" || c.DeviceID != "d1" || c.Reason != "device_revoked" {
			t.Fatalf("命令：%+v", c)
		}
		got[c.SourceID] = true
	}
	if !got["s1"] || !got["s2"] || len(cmds) != 2 {
		t.Fatalf("应只给 s1、s2 各一条：%+v", cmds)
	}
	rc.Revocations().LinkRevoked(ctx, "s1", "l1")
	if ok, _ := e.admin.Exists(ctx, e.prefix+"revoked:g1").Result(); ok != 1 {
		t.Fatal("链接撤销要撤访客令牌")
	}
	rc.Revocations().SourceRevoked(ctx, "s1")
	if ok, _ := e.admin.Exists(ctx, e.prefix+"revoked:j4").Result(); ok != 1 {
		t.Fatal("主机撤销要撤其余令牌")
	}
	if c, _, _ := sink.snapshot(); len(c) != 2 {
		t.Fatalf("链接与主机撤销只撤令牌、不踢（core 自己踢）：%d", len(c))
	}
	// core.RelayPort：缺 sourceId 的 Disconnect 不发。
	rc.Relay().Disconnect(core.ConnectionFilter{})
	rc.Relay().CloseTunnel("s2")
	eventually(t, "closeTunnel", func() bool {
		c, _, _ := sink.snapshot()
		for _, cmd := range c {
			if cmd.Type == control.CommandCloseTunnel && cmd.SourceID == "s2" {
				return true
			}
		}
		return false
	})
}

// 外壳往返：控制面实例发出的每种广播，用原始订阅取到字节，外壳恰是 {origin, kind, event|command|jti+expMs}，
// origin 是 cloud/<实例 id>，cluster.ParseBroadcast 解回原样（形状与节点一致，不手写线上格式）。
func TestBroadcastShellsRoundTripThroughWire(t *testing.T) {
	e := newRedisEnv(t)
	rc, _ := e.newControl("inst-7", nil)
	sub := e.admin.Subscribe(ctx, e.prefix+"ctl", e.prefix+"revoked", e.prefix+"node:node-9")
	t.Cleanup(func() { _ = sub.Close() })
	if _, err := sub.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // 其余订阅确认
		if _, err := sub.Receive(ctx); err != nil {
			t.Fatal(err)
		}
	}
	exp := inHour()
	if err := rc.IndexIssued(ctx, relayctl.Issued{Subject: "a1", SourceID: "s1", DeviceID: "d1", JTI: "j1", ExpMs: exp}); err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(e.admin.Set(ctx, e.prefix+"route:src:s1", cluster.EncodeRoute(control.Route{Node: "node-9", TunnelID: "t", SinceMs: 1}), time.Minute).Err())
	_, err := rc.Revoke(ctx, relayctl.Filter{JTI: "j1"}, "")
	must(err)
	must(rc.Kick(ctx, relayctl.KickFilter{SourceID: "s1", Subject: "a1", SubjectPrefix: "p:", DeviceID: "d1"}, "why"))
	must(rc.SetTeamQuota(ctx, "t1", relayctl.Quota{Concurrent: 4}))
	must(rc.SetIncident(ctx, relayctl.Incident{RejectNewConnections: true, AtMs: 9}))

	var got []cluster.Message
	deadline := time.Now().Add(5 * time.Second)
	for len(got) < 5 { // revoked、ctl 上的 kick、持有节点频道上的 kick、quota、incident
		if time.Now().After(deadline) {
			t.Fatalf("只收到 %d 条", len(got))
		}
		msg, err := sub.ReceiveTimeout(ctx, 500*time.Millisecond)
		if err != nil {
			continue
		}
		m, ok := msg.(*goredis.Message)
		if !ok {
			continue
		}
		parsed, err := cluster.ParseBroadcast([]byte(m.Payload))
		if err != nil {
			t.Fatalf("解不开 %s：%v", m.Payload, err)
		}
		if parsed.Origin != "cloud/inst-7" {
			t.Fatalf("origin：%q", parsed.Origin)
		}
		got = append(got, parsed)
	}
	kinds := map[string]int{}
	for _, m := range got {
		kinds[m.Kind]++
		switch m.Kind {
		case cluster.KindRevoked:
			if m.JTI != "j1" || m.ExpMs != exp {
				t.Fatalf("revoked：%+v", m)
			}
		case cluster.KindCommand:
			if m.Command != (control.NodeCommand{Type: control.CommandKick, SourceID: "s1", Subject: "a1", SubjectPrefix: "p:", DeviceID: "d1", Reason: "why"}) {
				t.Fatalf("command：%+v", m.Command)
			}
		case cluster.KindQuota:
			if m.Quota != (control.TeamQuota{TeamID: "t1", Concurrent: 4}) {
				t.Fatalf("quota：%+v", m.Quota)
			}
		case cluster.KindIncident:
			if m.Incident != (control.Incident{RejectNewConnections: true, AtMs: 9}) {
				t.Fatalf("incident：%+v", m.Incident)
			}
		}
	}
	if kinds[cluster.KindRevoked] != 1 || kinds[cluster.KindCommand] != 2 || kinds[cluster.KindQuota] != 1 || kinds[cluster.KindIncident] != 1 {
		t.Fatalf("种类与次数（kick 在 ctl 与持有节点频道各一条）：%v", kinds)
	}
}
