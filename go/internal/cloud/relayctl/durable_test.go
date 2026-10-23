package relayctl_test

import (
	"reflect"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/pgtest"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/postgres"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

type durableEnv struct {
	d    *relayctl.Durable
	mem  *relayctl.Memory
	db   *postgres.DB
	soft saas.Store
}

func openDurable(t *testing.T) *durableEnv {
	t.Helper()
	url, schema := pgtest.Schema(t)
	db, err := postgres.Open(ctx, postgres.Options{URL: url, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.CreateTenant(ctx, "cloud", store.Tenant{TenantSettings: store.TenantSettings{Issuer: "https://x.test", TrustedOrigins: []string{}}}); err != nil {
		t.Fatal(err)
	}
	mem, clk := newMemory()
	_ = clk
	return &durableEnv{d: relayctl.NewDurable(mem, db.SaaS(), "cloud", clk.now, nil), mem: mem, db: db, soft: db.SaaS()}
}

func (e *durableEnv) count(t *testing.T, sql string) int {
	t.Helper()
	var n int
	if err := e.db.Pool().QueryRow(ctx, sql).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 签发先落库再进内层；撤销在 PostgreSQL 与内层两边都撤，撤销表有行、索引行被删。
func TestDurableIndexesAndRevokesInBothPlaces(t *testing.T) {
	e := openDurable(t)
	index := func(subject, source, device, jti string) {
		t.Helper()
		if err := e.d.IndexIssued(ctx, relayctl.Issued{Subject: subject, SourceID: source, DeviceID: device, JTI: jti, ExpMs: 9_000_000}); err != nil {
			t.Fatal(err)
		}
	}
	index("a1", "s1", "d1", "s.d1.s1.x")
	index("a1", "s1", "d1", "r.d1.s1.x")
	index("a1", "s2", "d1", "r.d1.s2.x")
	index("a2", "s1", "d2", "r.d2.s1.y")
	if n := e.count(t, "SELECT count(*)::int FROM cloud_issued_tokens WHERE tenant_id = 'cloud'"); n != 4 {
		t.Fatalf("落库的索引：%d", n)
	}
	n, err := e.d.RevokeWhere(ctx, relayctl.Filter{Subject: "a1", SourceID: "s1"})
	if err != nil || n != 2 {
		t.Fatalf("撤了几枚：%d %v", n, err)
	}
	if got, want := jtis(e.mem), []string{"r.d1.s1.x", "s.d1.s1.x"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("内层撤销表：%v", got)
	}
	if n := e.count(t, "SELECT count(*)::int FROM relay_revoked_tokens"); n != 2 {
		t.Fatalf("PostgreSQL 撤销表：%d", n)
	}
	if n := e.count(t, "SELECT count(*)::int FROM cloud_issued_tokens"); n != 2 {
		t.Fatalf("撤销后索引：%d", n)
	}
	// 只落在 PostgreSQL（内层已丢失，例如重启）的令牌照样撤到；只在内层的也撤到。
	e.mem.RevokeWhere(ctx, relayctl.Filter{Subject: "a2"})
	if n, err := e.d.RevokeWhere(ctx, relayctl.Filter{JTI: "r.d2.s1.y"}); err != nil || n != 1 {
		t.Fatalf("按 jti 撤：%d %v", n, err)
	}
	if n := e.count(t, "SELECT count(*)::int FROM relay_revoked_tokens"); n != 3 {
		t.Fatalf("PostgreSQL 撤销表：%d", n)
	}
	// 全空的过滤什么也不撤。
	if n, err := e.d.RevokeWhere(ctx, relayctl.Filter{}); err != nil || n != 0 {
		t.Fatalf("空过滤：%d %v", n, err)
	}
}

// core 的撤销钩子也落盘：撤设备、撤其余设备、撤源。
func TestDurableSinkPersists(t *testing.T) {
	e := openDurable(t)
	for _, c := range [][4]string{
		{"a1", "s1", "d1", "r.d1.s1.a"}, {"a1", "s1", "d2", "r.d2.s1.b"}, {"a1", "s2", "d3", "r.d3.s2.c"}, {"-", "s2", "", "t.s2.x"},
	} {
		if err := e.d.IndexIssued(ctx, relayctl.Issued{Subject: c[0], SourceID: c[1], DeviceID: c[2], JTI: c[3], ExpMs: 9_000_000}); err != nil {
			t.Fatal(err)
		}
	}
	sink := e.d.Revocations()
	sink.DeviceRevoked(ctx, "a1", "d1")
	if n := e.count(t, "SELECT count(*)::int FROM relay_revoked_tokens"); n != 1 {
		t.Fatalf("撤设备：%d", n)
	}
	sink.DevicesRevoked(ctx, "a1", "d3")
	if n := e.count(t, "SELECT count(*)::int FROM relay_revoked_tokens"); n != 2 {
		t.Fatalf("撤其余设备（d3 保留）：%d", n)
	}
	sink.SourceRevoked(ctx, "s2")
	if n := e.count(t, "SELECT count(*)::int FROM relay_revoked_tokens"); n != 4 {
		t.Fatalf("撤源：%d", n)
	}
	if n := e.count(t, "SELECT count(*)::int FROM cloud_issued_tokens"); n != 0 {
		t.Fatalf("索引应清空：%d", n)
	}
	if got := jtis(e.mem); len(got) != 4 {
		t.Fatalf("内层也撤了：%v", got)
	}
}
