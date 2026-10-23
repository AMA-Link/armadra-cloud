package relayctl_test

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

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

// 内层索引缺了而 PostgreSQL 有的令牌（重启、Redis 丢数据）照样被撤并踢；两边都命中的只踢一次；只在内层的也撤到。
func TestDurableRevokesTokensMissingFromInnerIndexAndKicksOncePerDevice(t *testing.T) {
	e := openDurable(t)
	onlyPG := func(jti, account, source, device string) {
		t.Helper()
		err := e.soft.Tx(ctx, func(tx saas.Tx) error {
			return tx.Issued().Insert(ctx, "cloud", saas.IssuedToken{JTI: jti, AccountID: account, SourceID: source, DeviceID: device, ExpMs: 9_000_000})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	onlyPG("r.d1.s1.pgonly", "a1", "s1", "d1")
	onlyPG("r.d1.s2.pgonly", "a1", "s2", "d1")
	both := relayctl.Issued{Subject: "a1", SourceID: "s1", DeviceID: "d1", JTI: "s.d1.s1.both", ExpMs: 9_000_000}
	if err := e.d.IndexIssued(ctx, both); err != nil {
		t.Fatal(err)
	}
	// 内层独有（PostgreSQL 已被别处删掉）：同设备同源，不该多踢一次。
	issue(t, e.mem, "a1", "s1", "d1", "r.d1.s1.memonly", 9_000_000)
	issue(t, e.mem, "a1", "s3", "d1", "r.d1.s3.memonly", 9_000_000)

	e.d.Revocations().DeviceRevoked(ctx, "a1", "d1")
	for _, jti := range []string{"r.d1.s1.pgonly", "r.d1.s2.pgonly", "s.d1.s1.both", "r.d1.s1.memonly", "r.d1.s3.memonly"} {
		if !e.mem.IsRevoked(jti) {
			t.Errorf("内层没撤到 %s", jti)
		}
	}
	if n := e.count(t, "SELECT count(*)::int FROM relay_revoked_tokens"); n != 3 {
		t.Fatalf("PostgreSQL 撤销表只该有 PostgreSQL 命中的 3 枚：%d", n)
	}
	var kicks []string
	for _, c := range e.mem.Commands() {
		if c.Type == relayctl.CommandKick {
			kicks = append(kicks, c.SourceID+"|"+c.Subject+"|"+c.DeviceID)
		}
	}
	sort.Strings(kicks)
	if want := []string{"s1|a1|d1", "s2|a1|d1", "s3|a1|d1"}; !reflect.DeepEqual(kicks, want) {
		t.Fatalf("按 (源, subject, 设备) 去重的踢人：%v", kicks)
	}
	// RevokeWhere 同样把 PostgreSQL 独有的令牌撤到内层。
	onlyPG("r.d2.s9.x", "a2", "s9", "d2")
	if n, err := e.d.RevokeWhere(ctx, relayctl.Filter{Subject: "a2"}); err != nil || n != 1 || !e.mem.IsRevoked("r.d2.s9.x") {
		t.Fatalf("RevokeWhere：%d %v", n, err)
	}
}

// failingInner 让指定 jti 的内层登记 / 撤销出错，其余照常。
type failingInner struct {
	*relayctl.Memory
	failIndex, failRevoke string
}

func (f failingInner) IndexIssued(c context.Context, issued relayctl.Issued) error {
	if issued.JTI == f.failIndex {
		return errors.New("内层登记失败")
	}
	return f.Memory.IndexIssued(c, issued)
}

func (f failingInner) Revoke(c context.Context, filter relayctl.Filter, keep string) ([]relayctl.Issued, error) {
	if filter.JTI != "" && filter.JTI == f.failRevoke {
		return nil, errors.New("内层撤销失败")
	}
	return f.Memory.Revoke(c, filter, keep)
}

// 内层对某枚令牌出错：记下继续，剩余令牌与最后的按过滤撤销照常跑，最后合并返回错误。
func TestDurableContinuesAfterInnerErrors(t *testing.T) {
	e := openDurable(t)
	inner := failingInner{Memory: e.mem, failIndex: "r.d1.s1.a", failRevoke: "r.d1.s1.b"}
	d := relayctl.NewDurable(inner, e.soft, "cloud", func() time.Time { return time.UnixMilli(1_000_000) }, nil)
	for _, jti := range []string{"r.d1.s1.a", "r.d1.s1.b", "r.d1.s1.c"} {
		err := e.soft.Tx(ctx, func(tx saas.Tx) error {
			return tx.Issued().Insert(ctx, "cloud", saas.IssuedToken{JTI: jti, AccountID: "a1", SourceID: "s1", DeviceID: "d1", ExpMs: 9_000_000})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// 只在内层的令牌：最后的按过滤撤销必须仍然跑。
	issue(t, e.mem, "a1", "s1", "d1", "r.d1.s1.memonly", 9_000_000)

	hits, err := d.Revoke(ctx, relayctl.Filter{Subject: "a1", SourceID: "s1"}, "")
	if err == nil || !strings.Contains(err.Error(), "内层登记失败") || !strings.Contains(err.Error(), "内层撤销失败") {
		t.Fatalf("应合并返回两个错误：%v", err)
	}
	if !e.mem.IsRevoked("r.d1.s1.c") || !e.mem.IsRevoked("r.d1.s1.memonly") {
		t.Fatal("出错之后的令牌与按过滤的内层撤销照常跑")
	}
	if n := e.count(t, "SELECT count(*)::int FROM relay_revoked_tokens"); n != 3 {
		t.Fatalf("PostgreSQL 撤销表是权威：%d", n)
	}
	if len(hits) < 3 {
		t.Fatalf("命中至少含 PostgreSQL 的 3 枚：%v", hits)
	}
}
