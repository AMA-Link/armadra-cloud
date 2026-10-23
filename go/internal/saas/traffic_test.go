package saas

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/AMA-Link/armadra-cloud/go/internal/store/pgtest"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/postgres"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil/relayenv"
)

func withSearchPath(url, schema string) string {
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	return url + sep + "search_path=" + schema
}

// 流量只在入口节点计、带 team，写进 relay_traffic_buckets（租户 cloud）；节点停下时最后落一次库。
func TestTrafficWithTeam(t *testing.T) {
	url, schema := pgtest.Schema(t)
	db, err := postgres.Open(context.Background(), postgres.Options{URL: url, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	r := newRig(t)
	n := r.start("a", func(c *Config) { c.DatabaseURL = withSearchPath(url, schema) })
	r.env.StartCore(t, n.URL, relayenv.CoreOptions{Capabilities: []string{"e2e"}})
	eventually(t, "路由", func() bool { t2, _ := n.Plane.Hub.Lookup(context.Background(), r.env.SourceID); return t2 != nil })
	c := r.channel(n, r.relayToken(relayenv.Claims{"team": "team_1", "dev": "dev_1"}))
	c.echo(t, []byte("hello traffic"))
	_ = c.c.Close()
	n.stop()

	conn, err := pgx.Connect(context.Background(), withSearchPath(url, schema))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var (
		tenant, node, source, subject, device, kind, team string
		connections, up, down                             int64
	)
	err = conn.QueryRow(context.Background(), `SELECT tenant_id, node, source_id, subject, device_id, kind, team_id,
	  connections, bytes_to_source, bytes_to_client FROM relay_traffic_buckets`).
		Scan(&tenant, &node, &source, &subject, &device, &kind, &team, &connections, &up, &down)
	if err != nil {
		t.Fatal(err)
	}
	if tenant != Tenant || node != "a" || source != r.env.SourceID || subject != "acct_test" || device != "dev_1" ||
		kind != "e2e" || team != "team_1" || connections != 1 || up != 13 || down != 13 {
		t.Fatalf("%s %s %s %s %s %s %s %d %d %d", tenant, node, source, subject, device, kind, team, connections, up, down)
	}
}

// 账本里没有 0009（控制面还没 migrate 到）或连不上：拒绝启动，错误不带连接串。
func TestTrafficRefusesStaleLedger(t *testing.T) {
	url, schema := pgtest.Schema(t)
	_, err := openTraffic(context.Background(), withSearchPath(url, schema))
	if err == nil || strings.Contains(err.Error(), url) {
		t.Fatalf("空 schema（没有账本）应拒绝：%v", err)
	}
	conn, err := pgx.Connect(context.Background(), withSearchPath(url, schema))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(context.Background(), "CREATE TABLE cloud_migrations (id integer PRIMARY KEY, name text NOT NULL, sha384 text NOT NULL, applied_at_ms bigint NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	_, err = openTraffic(context.Background(), withSearchPath(url, schema))
	if err == nil || !strings.Contains(err.Error(), "数据库版本落后") {
		t.Fatalf("账本落后应拒绝：%v", err)
	}
}
