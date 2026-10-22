package postgres_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/cluster"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/controltest"
	cp "github.com/AMA-Link/armadra-cloud/go/internal/control/postgres"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/pgtest"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/postgres"
)

// env 是一个迁移好的临时 schema，加上给频道用的唯一前缀（NOTIFY 的频道是整库共用的，
// 并行的别的测试不能串进来）。
type env struct {
	t       *testing.T
	url     string
	schema  string
	channel string
	admin   *pgxpool.Pool
}

func newEnv(t *testing.T) *env {
	t.Helper()
	url, schema := pgtest.Schema(t)
	ctx := context.Background()
	db, err := postgres.Open(ctx, postgres.Options{URL: url, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e := &env{t: t, url: url, schema: schema, channel: schema + "_", admin: db.Pool()}
	return e
}

type node struct {
	*cp.Plane
	app    string
	errors *errorLog
}

type errorLog struct {
	mu   sync.Mutex
	list []string
}

func (l *errorLog) add(op string) { l.mu.Lock(); l.list = append(l.list, op); l.mu.Unlock() }
func (l *errorLog) has(prefix string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, op := range l.list {
		if strings.HasPrefix(op, prefix) {
			return true
		}
	}
	return false
}

func (e *env) pool(app string) *pgxpool.Pool {
	e.t.Helper()
	config, err := pgxpool.ParseConfig(e.url)
	if err != nil {
		e.t.Fatal(err)
	}
	config.MaxConns = 4
	config.ConnConfig.RuntimeParams["search_path"] = e.schema
	config.ConnConfig.RuntimeParams["application_name"] = app
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(pool.Close)
	return pool
}

func (e *env) node(name string, over func(*cp.Options)) *node {
	e.t.Helper()
	log := &errorLog{}
	app := "armadra-go-test-" + e.schema + "-" + name
	o := cp.Options{
		Options: cluster.Options{
			Issuer: "https://relay.example.test", NodeID: name, Limits: controltest.Limited,
			OnError: func(op string, _ error) { log.add(op) },
		},
		Pool: e.pool(app), ChannelPrefix: e.channel,
		ReconnectMin: 50 * time.Millisecond, ReconnectMax: 200 * time.Millisecond,
	}
	if over != nil {
		over(&o)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	plane, err := cp.New(ctx, o)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = plane.Close() })
	return &node{Plane: plane, app: app, errors: log}
}

func (e *env) pair() controltest.Pair {
	a, b := e.node("node-a", nil), e.node("node-b", nil)
	return controltest.Pair{
		A: controltest.Node{Cluster: a.Plane, Settle: a.Settle},
		B: controltest.Node{Cluster: b.Plane, Settle: b.Settle},
	}
}

func TestClusterContract(t *testing.T) {
	controltest.Contract(t, func(t *testing.T) controltest.Pair { return newEnv(t).pair() })
}

// 表不在（没迁移）就拒绝启动并说明先迁移。
func TestRefusesToStartWithoutTables(t *testing.T) {
	url, schema := pgtest.Schema(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	// schema 是空的：没有任何中继控制面的表。
	_, err = cp.New(ctx, cp.Options{Options: cluster.Options{Issuer: "x"}, URL: url, Schema: schema})
	if err == nil || !strings.Contains(err.Error(), "先运行迁移") {
		t.Fatalf("没有表应拒绝启动：%v", err)
	}
}

func TestNewErrorsDoNotLeakTheConnectionString(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := cp.New(ctx, cp.Options{Options: cluster.Options{Issuer: "x"}, URL: "postgres://armadra:s3cret-pass@127.0.0.1:1/x?connect_timeout=1"})
	if err == nil || strings.Contains(err.Error(), "s3cret-pass") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("连不上的错误：%v", err)
	}
	if _, err := cp.New(ctx, cp.Options{URL: "://坏的 s3cret-pass"}); err == nil || strings.Contains(err.Error(), "s3cret-pass") {
		t.Fatalf("坏连接串的错误：%v", err)
	}
}

// 表的形状与 TypeScript 一致：Go 写的行 TypeScript 读得到；NOTIFY 的负载是 TypeScript 认得的 JSON。
func TestWireFormatMatchesTypeScript(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	n := e.node("node-a", nil)

	if err := n.Routes().Set(ctx, controltest.Source, control.Route{Node: "node-a", TunnelID: "t1", SinceMs: 7, CoreVersion: "0.3.0"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	var node, tunnel, version string
	var since, expires int64
	err := e.admin.QueryRow(ctx, "SELECT node, tunnel_id, since_ms, core_version, expires_at_ms FROM relay_routes WHERE source_id = $1", controltest.Source).
		Scan(&node, &tunnel, &since, &version, &expires)
	if err != nil || node != "node-a" || tunnel != "t1" || since != 7 || version != "0.3.0" {
		t.Fatalf("路由行：%v %v %v %v %v", node, tunnel, since, version, err)
	}
	if left := expires - time.Now().UnixMilli(); left <= 0 || left > 60_000 {
		t.Fatalf("路由应带绝对到期毫秒：%d", left)
	}

	// 另开一个监听连接，看三个频道上的负载。
	listener, err := pgx.Connect(ctx, e.url+"?search_path="+e.schema)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close(ctx) }()
	for _, c := range []string{"relay_presence", "relay_ctl", "relay_revoked"} {
		if _, err := listener.Exec(ctx, "LISTEN "+pgx.Identifier{e.channel + c}.Sanitize()); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.Presence().Publish(ctx, control.PresenceEvent{Type: control.PresenceSourceOnline, SourceID: "s", Node: "n", TunnelID: "t", CoreVersion: "0.3.0", AtMs: 3}); err != nil {
		t.Fatal(err)
	}
	n.Command(control.NodeCommand{Type: control.CommandCloseTunnel, SourceID: "s"})
	if err := n.Tokens().Revoke(ctx, "jti-y", time.UnixMilli(4102444800000)); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		e.channel + "relay_presence": `"kind":"presence","event":{"type":"sourceOnline","sourceId":"s","node":"n","tunnelId":"t","coreVersion":"0.3.0","atMs":3}`,
		e.channel + "relay_ctl":      `"kind":"command","command":{"type":"closeTunnel","sourceId":"s"}`,
		e.channel + "relay_revoked":  `"kind":"revoked","jti":"jti-y","expMs":4102444800000`,
	}
	for i := 0; i < 3; i++ {
		wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		msg, err := listener.WaitForNotification(wctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(msg.Payload, want[msg.Channel]) || !strings.Contains(msg.Payload, `"origin":"`) {
			t.Fatalf("%s 的负载：%s", msg.Channel, msg.Payload)
		}
		delete(want, msg.Channel)
	}
	var exp int64
	if err := e.admin.QueryRow(ctx, "SELECT exp_ms FROM relay_revoked_tokens WHERE jti = 'jti-y'").Scan(&exp); err != nil || exp != 4102444800000 {
		t.Fatalf("撤销行：%d %v", exp, err)
	}
	// 同一个 jti 再撤销一次取较晚的到期。
	if err := n.Tokens().Revoke(ctx, "jti-y", time.UnixMilli(4102444700000)); err != nil {
		t.Fatal(err)
	}
	_ = e.admin.QueryRow(ctx, "SELECT exp_ms FROM relay_revoked_tokens WHERE jti = 'jti-y'").Scan(&exp)
	if exp != 4102444800000 {
		t.Fatalf("到期取较晚者：%d", exp)
	}
}

// 启动时全量读一遍撤销表；监听连接被断开后重连并再读一遍：断线期间只写了表、没收到通知的撤销也不漏。
func TestRevocationReloadOnStartAndReconnect(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	at := time.Now().Add(time.Minute).UnixMilli()
	if _, err := e.admin.Exec(ctx, "INSERT INTO relay_revoked_tokens (jti, exp_ms) VALUES ('before-start', $1), ('long-gone', $2)", at, time.Now().Add(-time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	n := e.node("node-a", nil)
	if revoked, _ := n.Tokens().IsRevoked(ctx, "before-start"); !revoked {
		t.Fatal("启动时应全量读到已有的撤销")
	}
	if revoked, _ := n.Tokens().IsRevoked(ctx, "long-gone"); revoked {
		t.Fatal("过期的不算")
	}

	terminateListener(t, e, n)
	if _, err := e.admin.Exec(ctx, "INSERT INTO relay_revoked_tokens (jti, exp_ms) VALUES ('during-outage', $1)", at); err != nil {
		t.Fatal(err)
	}
	controltest.Eventually(t, "重连后全量重读到断线期间的撤销", func() bool {
		r, _ := n.Tokens().IsRevoked(ctx, "during-outage")
		return r
	})
	other := e.node("node-b", nil)
	if err := other.Tokens().Revoke(ctx, "after-reconnect", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	controltest.Eventually(t, "重连后通知仍能收到", func() bool {
		r, _ := n.Tokens().IsRevoked(ctx, "after-reconnect")
		return r
	})
	// 断线后还能收到在线事件与命令（三个频道都重新监听了）。
	var mu sync.Mutex
	var events []control.PresenceEvent
	n.Presence().Subscribe(func(e control.PresenceEvent) { mu.Lock(); events = append(events, e); mu.Unlock() })
	if err := other.Presence().Publish(ctx, control.PresenceEvent{Type: control.PresenceSourceOffline, SourceID: controltest.Source, Node: "node-b", TunnelID: "t", AtMs: 1}); err != nil {
		t.Fatal(err)
	}
	controltest.Eventually(t, "重连后在线事件仍能收到", func() bool { mu.Lock(); defer mu.Unlock(); return len(events) == 1 })
}

// terminateListener 断开某个节点的监听连接（按测试给它的 application_name 找，且只找正在 LISTEN
// 的那条，不碰库里别人的连接）。
func terminateListener(t *testing.T, e *env, n *node) {
	t.Helper()
	ctx := context.Background()
	var killed int
	err := e.admin.QueryRow(ctx, `SELECT count(*) FROM (
		SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE application_name = $1 AND pid <> pg_backend_pid() AND query ILIKE 'LISTEN%') s`, n.app).Scan(&killed)
	if err != nil || killed == 0 {
		t.Fatalf("没找到监听连接：%d %v", killed, err)
	}
}

// 库出错时速率限流退回本节点内存，并报告。
func TestLimitFallsBackToLocalWhenDatabaseFails(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	n := e.node("node-a", nil)
	if _, err := e.admin.Exec(ctx, "ALTER TABLE relay_limits RENAME TO relay_limits_gone"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		r, err := n.Limits().Hit(ctx, control.LimitLoginIP, "203.0.113.1")
		if err != nil || !r.Allowed {
			t.Fatalf("退回本节点内存后前 5 次应放行：%+v %v", r, err)
		}
	}
	if r, _ := n.Limits().Hit(ctx, control.LimitLoginIP, "203.0.113.1"); r.Allowed || r.RetryAfter <= 0 {
		t.Fatalf("本节点内存的桶第 6 次应拒：%+v", r)
	}
	if !n.errors.has("limit") {
		t.Fatal("退回本节点应报告")
	}
}

// 清扫：过期的路由、限流桶与撤销记录被删，没过期的留着。
func TestSweepRemovesExpiredRows(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	n := e.node("node-a", func(o *cp.Options) { o.SweepEvery = 50 * time.Millisecond })
	past, future := time.Now().Add(-time.Minute).UnixMilli(), time.Now().Add(time.Hour).UnixMilli()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO relay_routes VALUES ('old','n','t',1,'v',$1), ('new','n','t',1,'v',$2)", []any{past, future}},
		{"INSERT INTO relay_limits VALUES ('old',1,$1), ('new',1,$2)", []any{past, future}},
		{"INSERT INTO relay_revoked_tokens VALUES ('old',$1), ('new',$2)", []any{past, future}},
	} {
		if _, err := e.admin.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	controltest.Eventually(t, "过期行被清掉", func() bool {
		var routes, limits, tokens int
		_ = e.admin.QueryRow(ctx, "SELECT (SELECT count(*) FROM relay_routes), (SELECT count(*) FROM relay_limits), (SELECT count(*) FROM relay_revoked_tokens)").Scan(&routes, &limits, &tokens)
		return routes == 1 && limits == 1 && tokens == 1
	})
	_ = n
}

// 非首次的重读失败不能当没事继续：返回错误、退避、重连、重新监听、再读。
func TestReloadFailureRetriesInsteadOfFailingOpen(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var fail atomic.Int32
	n := e.node("node-a", func(o *cp.Options) {
		o.ReloadFault = func() error {
			if fail.Add(-1) >= 0 {
				return errors.New("注入的查询失败")
			}
			fail.Store(0)
			return nil
		}
	})
	fail.Store(0)
	terminateListener(t, e, n)
	fail.Store(2)
	if _, err := e.admin.Exec(ctx, "INSERT INTO relay_revoked_tokens (jti, exp_ms) VALUES ('during-outage', $1)", time.Now().Add(time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	controltest.Eventually(t, "重读失败后重试，断线期间的撤销最终生效", func() bool {
		r, _ := n.Tokens().IsRevoked(ctx, "during-outage")
		return r
	})
	if !n.errors.has("listener") {
		t.Fatal("重读失败应报告并重连")
	}
}

func TestPeriodicReloadCatchesMissedNotifications(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	n := e.node("node-a", func(o *cp.Options) { o.ReloadEvery = 100 * time.Millisecond })
	if _, err := e.admin.Exec(ctx, "INSERT INTO relay_revoked_tokens (jti, exp_ms) VALUES ('silent', $1)", time.Now().Add(time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	controltest.Eventually(t, "周期重读到静默写入的撤销", func() bool {
		r, _ := n.Tokens().IsRevoked(ctx, "silent")
		return r
	})
}

func TestLivenessCheckKeepsListenerWorking(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.node("node-a", func(o *cp.Options) { o.LivenessEvery = 50 * time.Millisecond })
	b := e.node("node-b", nil)
	time.Sleep(400 * time.Millisecond)
	if err := b.Tokens().Revoke(ctx, "after-idle", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	controltest.Eventually(t, "空闲之后仍收到通知", func() bool {
		r, _ := a.Tokens().IsRevoked(ctx, "after-idle")
		return r
	})
	if a.errors.has("listener") {
		t.Fatal("活性检查不该把正常的连接判成断线")
	}
}
