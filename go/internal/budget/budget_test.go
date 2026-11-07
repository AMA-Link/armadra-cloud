package budget_test

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/budget"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/sqlite"
	"github.com/AMA-Link/armadra-cloud/go/internal/traffic"
	"github.com/AMA-Link/armadra-cloud/go/internal/trafficstore"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)} }

// 到线拒绝：第 N+1 条通道被拒，Retry-After 到下一个 UTC 零点；拒绝计数；每天只报一次「首次」。
func TestRequestsRejectedAtLimit(t *testing.T) {
	c := newClock()
	b := budget.New(budget.Limits{Requests: 3}, c.now)
	for i := 0; i < 3; i++ {
		if v := b.Admit(); !v.OK {
			t.Fatalf("第 %d 条应放行", i+1)
		}
	}
	v := b.Admit()
	if v.OK || !v.First || v.RetryAfter != 14*time.Hour {
		t.Fatalf("第 4 条应被拒，等到 UTC 零点：%+v", v)
	}
	if v := b.Admit(); v.OK || v.First {
		t.Fatalf("同一天第二次拒绝不再是首次：%+v", v)
	}
	if u := b.Usage(); u.Requests != 3 || u.Rejected != 2 {
		t.Fatalf("%+v", u)
	}
}

// 字节到线也拒绝新通道；已建立的流不切断：AddBytes 超额照样累加。
func TestBytesRejectNewButKeepCounting(t *testing.T) {
	c := newClock()
	b := budget.New(budget.Limits{Bytes: 1000}, c.now)
	if !b.Admit().OK {
		t.Fatal("没超额应放行")
	}
	b.AddBytes(900)
	if !b.Admit().OK {
		t.Fatal("900 < 1000 应放行")
	}
	b.AddBytes(500) // 已建立的流继续传，超额也记
	if b.Admit().OK {
		t.Fatal("1400 ≥ 1000 应拒绝新通道")
	}
	if u := b.Usage(); u.Bytes != 1400 {
		t.Fatalf("超额的字节也要记：%+v", u)
	}
	b.AddBytes(-5)
	b.AddBytes(0)
	if b.Usage().Bytes != 1400 {
		t.Fatal("非正数不记")
	}
}

// 跨 UTC 日重置：零点之后用量清零，重新放行。
func TestResetsAcrossUTCDay(t *testing.T) {
	c := newClock()
	b := budget.New(budget.Limits{Requests: 1, Bytes: 100}, c.now)
	b.Admit()
	b.AddBytes(100)
	if b.Admit().OK {
		t.Fatal("当天应拒绝")
	}
	c.add(13*time.Hour + 59*time.Minute + 59*time.Second) // 23:59:59
	if b.Admit().OK {
		t.Fatal("零点之前仍拒绝")
	}
	c.add(time.Second) // 00:00:00 次日
	if u := b.Usage(); u.Requests != 0 || u.Bytes != 0 {
		t.Fatalf("换日应清零：%+v", u)
	}
	if !b.Admit().OK {
		t.Fatal("换日后应放行")
	}
	// 次日再用尽，又是当天首次。
	if v := b.Admit(); v.OK || !v.First {
		t.Fatalf("次日再拒绝应是当天首次：%+v", v)
	}
}

// 没配的项不限；全缺省时 Enabled 为假。
func TestUnlimitedAndEnabled(t *testing.T) {
	if (budget.Limits{}).Enabled() || !(budget.Limits{Bytes: 1}).Enabled() || !(budget.Limits{Requests: 1}).Enabled() {
		t.Fatal("Enabled")
	}
	b := budget.New(budget.Limits{Bytes: 10}, newClock().now)
	for i := 0; i < 100; i++ {
		if !b.Admit().OK {
			t.Fatal("没限通道数就不该拒")
		}
	}
}

func ledger(t *testing.T) store.TrafficStore {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "relay.db"), sqlite.Options{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.CreateTenant(context.Background(), "default", store.Tenant{TenantSettings: store.TenantSettings{Issuer: "https://relay.test", TrustedOrigins: []string{}}}); err != nil {
		t.Fatal(err)
	}
	return db.Ports().Traffic
}

// 重启后从账本恢复当天用量（只算今天，昨天的不算）；还没落库的部分从累加器里补；取较大者。
func TestReconcileRestoresFromLedger(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	ts := ledger(t)
	today := c.now().Truncate(24 * time.Hour).UnixMilli()
	err := ts.Upsert(ctx, "default", []store.TrafficRow{
		{BucketStartMs: today - store.HourMs, Node: "n", SourceID: "s", Subject: "a", Kind: "e2e", Connections: 50, BytesToSource: 1000, BytesToClient: 1000}, // 昨天 23:00
		{BucketStartMs: today + store.HourMs, Node: "n", SourceID: "s", Subject: "a", Kind: "e2e", Connections: 2, BytesToSource: 300, BytesToClient: 200},
		{BucketStartMs: today + 2*store.HourMs, Node: "other", SourceID: "s", Subject: "b", Kind: "e2e", Connections: 1, BytesToSource: 100, BytesToClient: 400},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 累加器里还有没落库的：今天一条通道、50 字节。
	meter := traffic.NewMeter(traffic.MeterOptions{Node: "n", Write: trafficstore.Writer(ts, "default"), Now: c.now})
	meter.Add(traffic.Sample{Key: traffic.Key{SourceID: "s", Subject: "a", Kind: traffic.KindE2E}, Connections: 1, BytesToSource: 50})

	b := budget.New(budget.Limits{Requests: 4, Bytes: 100_000}, c.now) // 全新进程
	if err := b.Reconcile(ctx, ts, "default", meter); err != nil {
		t.Fatal(err)
	}
	if u := b.Usage(); u.Requests != 4 || u.Bytes != 1050 {
		t.Fatalf("应恢复为账本今天 3 条 + 累加器 1 条 = 4 条、1050 字节：%+v", u)
	}
	if b.Admit().OK {
		t.Fatal("恢复后已到 4 条上限，应拒绝")
	}
	// 内存里的数更大时不被账本压低。
	b2 := budget.New(budget.Limits{Bytes: 100_000}, c.now)
	b2.AddBytes(5000)
	if err := b2.Reconcile(ctx, ts, "default", nil); err != nil {
		t.Fatal(err)
	}
	if b2.Usage().Bytes != 5000 {
		t.Fatalf("取较大者：%+v", b2.Usage())
	}
	// 别的节点又落了库：并进来。
	if err := ts.Upsert(ctx, "default", []store.TrafficRow{{BucketStartMs: today + 3*store.HourMs, Node: "other", SourceID: "s", Subject: "b", Kind: "e2e", Connections: 1, BytesToSource: 9000}}); err != nil {
		t.Fatal(err)
	}
	if err := b2.Reconcile(ctx, ts, "default", nil); err != nil {
		t.Fatal(err)
	}
	if got := b2.Usage().Bytes; got != 1000+9000 {
		t.Fatalf("账本合计 10000 比内存大：%d", got)
	}
}

// 对齐时跨了零点：旧日的读数被丢弃，不污染新的一天。
func TestObserveIgnoresOtherDay(t *testing.T) {
	c := newClock()
	b := budget.New(budget.Limits{Requests: 10}, c.now)
	yesterday := c.now().Truncate(24*time.Hour).UnixMilli() - store.DayMs
	b.Observe(yesterday, 9, 9)
	if u := b.Usage(); u.Requests != 0 {
		t.Fatalf("%+v", u)
	}
}

func TestRender(t *testing.T) {
	b := budget.New(budget.Limits{Requests: 2, Bytes: 100}, newClock().now)
	b.Admit()
	b.AddBytes(7)
	b.Admit()
	b.Admit()
	text := b.Render()
	for _, line := range []string{
		"relay_daily_requests_used 2", "relay_daily_requests_limit 2",
		"relay_daily_bytes_used 7", "relay_daily_bytes_limit 100", "relay_daily_budget_rejected_total 1",
	} {
		if !strings.Contains(text, line+"\n") {
			t.Fatalf("缺 %q：\n%s", line, text)
		}
	}
	only := budget.New(budget.Limits{Bytes: 5}, newClock().now).Render()
	if strings.Contains(only, "relay_daily_requests") || !strings.Contains(only, "relay_daily_bytes_limit 5") {
		t.Fatalf("只列配了上限的项：\n%s", only)
	}
}
