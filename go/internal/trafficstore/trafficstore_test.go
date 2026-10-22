package trafficstore_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/sqlite"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/storetest"
	"github.com/AMA-Link/armadra-cloud/go/internal/traffic"
	"github.com/AMA-Link/armadra-cloud/go/internal/trafficstore"
)

func TestRowsMapEveryField(t *testing.T) {
	rows := trafficstore.Rows([]traffic.Row{{
		BucketStartMs: 3_600_000, Node: "n", Key: traffic.Key{SourceID: "s", Subject: "acct:a", DeviceID: "d", Kind: traffic.KindE2E},
		Connections: 2, BytesToSource: 30, BytesToClient: 40,
	}})
	want := store.TrafficRow{BucketStartMs: 3_600_000, Node: "n", SourceID: "s", Subject: "acct:a", DeviceID: "d", Kind: "e2e",
		Connections: 2, BytesToSource: 30, BytesToClient: 40}
	if len(rows) != 1 || rows[0] != want {
		t.Fatalf("%+v", rows)
	}
}

// 累加器 → 适配器 → 真实的 SQLite 存储：同键相加，查得回来。
func TestMeterFlushesIntoTheStore(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "relay.db"), sqlite.Options{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.CreateTenant(ctx, "default", store.Tenant{TenantSettings: storetest.Settings()}); err != nil {
		t.Fatal(err)
	}
	at := time.UnixMilli(7_200_000 + 5)
	meter := traffic.NewMeter(traffic.MeterOptions{
		Node: "relay-1", Now: func() time.Time { return at },
		Write: trafficstore.Writer(db.Ports().Traffic, "default"),
	})
	counter := meter.Counter(traffic.Key{SourceID: "s", Subject: "acct:a", Kind: traffic.KindE2E})
	counter.Connection()
	counter.Bytes(traffic.ToSource, 100)
	counter.Bytes(traffic.ToClient, 250)
	if err := meter.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	counter.Bytes(traffic.ToClient, 50) // 第二批：同键相加
	if err := meter.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Ports().Traffic.Query(ctx, "default", store.TrafficQuery{FromMs: 0, ToMs: 10_800_000, Limit: 10})
	if err != nil || len(rows) != 1 {
		t.Fatalf("%+v %v", rows, err)
	}
	r := rows[0]
	if r.BucketStartMs != 7_200_000 || r.Node != "relay-1" || r.Kind != "e2e" || r.Connections != 1 || r.BytesToSource != 100 || r.BytesToClient != 300 {
		t.Fatalf("%+v", r)
	}
}

// 存储写失败：行并回内存，下次再写（累加器的规则不因适配器而变）。
func TestWriteFailureKeepsRows(t *testing.T) {
	failing := &flaky{fail: true}
	meter := traffic.NewMeter(traffic.MeterOptions{Node: "n", Write: trafficstore.Writer(failing, "default")})
	meter.Counter(traffic.Key{SourceID: "s", Kind: traffic.KindE2E}).Connection()
	if err := meter.Flush(context.Background()); err == nil {
		t.Fatal("应报错")
	}
	failing.fail = false
	if err := meter.Flush(context.Background()); err != nil || len(failing.got) != 1 || failing.got[0].Connections != 1 {
		t.Fatalf("%+v %v", failing.got, err)
	}
}

type flaky struct {
	store.TrafficStore
	fail bool
	got  []store.TrafficRow
}

func (f *flaky) Upsert(_ context.Context, _ string, rows []store.TrafficRow) error {
	if f.fail {
		return errors.New("写不进去")
	}
	f.got = append(f.got, rows...)
	return nil
}
