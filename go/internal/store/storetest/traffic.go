package storetest

import (
	"math"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

func trafficContract(t *testing.T, create func(t *testing.T) store.Backend) {
	hour := store.HourMs
	t0 := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC).UnixMilli()
	row := func(mutate ...func(*store.TrafficRow)) store.TrafficRow {
		r := store.TrafficRow{
			BucketStartMs: t0, Node: "n1", SourceID: s1, Subject: "acct:alice", Kind: "ws",
			Connections: 1, BytesToSource: 10, BytesToClient: 100,
		}
		for _, m := range mutate {
			m(&r)
		}
		return r
	}
	all := store.TrafficQuery{FromMs: 0, ToMs: math.MaxInt64 / 2, Granularity: "hour", Limit: 1000}
	query := func(e *env, tenant string, q store.TrafficQuery) []store.TrafficRow {
		rows, err := e.ports.Traffic.Query(e.ctx, tenant, q)
		e.must(err)
		return rows
	}
	upsert := func(e *env, tenant string, rows ...store.TrafficRow) {
		e.must(e.ports.Traffic.Upsert(e.ctx, tenant, rows))
	}

	t.Run("同一桶与维度的行累加，不同维度各一行；租户隔离", func(t *testing.T) { // 例 11
		e := fresh(t, create)
		upsert(e, "t1", row(), row(func(r *store.TrafficRow) { r.Kind = "http" }))
		upsert(e, "t1", row(func(r *store.TrafficRow) { r.Connections, r.BytesToSource, r.BytesToClient = 2, 5, 7 }))
		upsert(e, "t2", row(func(r *store.TrafficRow) { r.Connections = 9 }))
		upsert(e, "t1")
		eq(t, query(e, "t1", all), []store.TrafficRow{
			row(func(r *store.TrafficRow) { r.Kind = "http" }),
			row(func(r *store.TrafficRow) { r.Connections, r.BytesToSource, r.BytesToClient = 3, 15, 107 }),
		}, "t1 的行")
		eq(t, query(e, "t2", all), []store.TrafficRow{row(func(r *store.TrafficRow) { r.Connections = 9 })}, "t2 的行")
	})

	t.Run("超过 32 位的计数原样往返", func(t *testing.T) { // 例 12
		e := fresh(t, create)
		big := int64(1)<<40 + 3
		upsert(e, "t1", row(func(r *store.TrafficRow) { r.BytesToClient = big }))
		upsert(e, "t1", row(func(r *store.TrafficRow) { r.BytesToClient = big }))
		rows := query(e, "t1", all)
		if len(rows) != 1 || rows[0].BytesToClient != 2*big {
			t.Fatalf("bytesToClient：%+v", rows)
		}
	})

	t.Run("查询：桶起点 ∈ [fromMs, toMs)，按源与 sub 过滤，排序固定，limit 截断", func(t *testing.T) { // 例 13
		e := fresh(t, create)
		upsert(e, "t1",
			row(func(r *store.TrafficRow) { r.BucketStartMs, r.SourceID = t0+2*hour, s2 }),
			row(func(r *store.TrafficRow) { r.BucketStartMs, r.Subject = t0+hour, "guest:x:1" }),
			row(func(r *store.TrafficRow) { r.BucketStartMs, r.Node = t0+hour, "n0" }),
			row(),
			row(func(r *store.TrafficRow) { r.BucketStartMs, r.DeviceID = t0+hour, "dev-b" }),
			row(func(r *store.TrafficRow) { r.BucketStartMs, r.DeviceID = t0+hour, "dev-a" }),
		)
		window := all
		window.FromMs, window.ToMs = t0+hour, t0+2*hour
		var got []string
		for _, r := range query(e, "t1", window) {
			got = append(got, r.Node+"/"+r.Subject+"/"+r.DeviceID)
		}
		eq(t, got, []string{"n0/acct:alice/", "n1/acct:alice/dev-a", "n1/acct:alice/dev-b", "n1/guest:x:1/"}, "窗口内的排序")

		bySource := all
		bySource.SourceID = s2
		rows := query(e, "t1", bySource)
		if len(rows) != 1 || rows[0].BucketStartMs != t0+2*hour {
			t.Fatalf("按源过滤：%+v", rows)
		}
		bySubject := all
		bySubject.Subject = "guest:x:1"
		eq(t, query(e, "t1", bySubject),
			[]store.TrafficRow{row(func(r *store.TrafficRow) { r.BucketStartMs, r.Subject = t0+hour, "guest:x:1" })}, "按 sub 过滤")
		limited := all
		limited.Limit = 2
		eq(t, query(e, "t1", limited), []store.TrafficRow{
			row(),
			row(func(r *store.TrafficRow) { r.BucketStartMs, r.Node = t0+hour, "n0" }),
		}, "limit 截断")
	})

	t.Run("按天合并：同一 UTC 日、同维度的小时桶相加", func(t *testing.T) { // 例 14
		e := fresh(t, create)
		at := func(offset int64) func(*store.TrafficRow) {
			return func(r *store.TrafficRow) { r.BucketStartMs = t0 + offset }
		}
		upsert(e, "t1",
			row(at(hour)), row(at(23*hour)), row(at(24*hour)),
			row(at(5*hour), func(r *store.TrafficRow) { r.Kind = "media" }))
		day := all
		day.Granularity = "day"
		eq(t, query(e, "t1", day), []store.TrafficRow{
			row(func(r *store.TrafficRow) { r.Kind = "media" }),
			row(func(r *store.TrafficRow) { r.Connections, r.BytesToSource, r.BytesToClient = 2, 20, 200 }),
			row(at(24 * hour)),
		}, "按天合并")
	})

	t.Run("保留期：删掉桶起点早于界限的行，答删了几行；只动本租户", func(t *testing.T) { // 例 15
		e := fresh(t, create)
		at := func(offset int64) func(*store.TrafficRow) {
			return func(r *store.TrafficRow) { r.BucketStartMs = t0 + offset }
		}
		upsert(e, "t1", row(at(0)), row(at(hour)), row(at(2*hour)))
		upsert(e, "t2", row(at(0)))
		removed, err := e.ports.Traffic.Prune(e.ctx, "t1", t0+2*hour)
		e.must(err)
		if removed != 2 {
			t.Fatalf("应删 2 行，删了 %d", removed)
		}
		if removed, _ = e.ports.Traffic.Prune(e.ctx, "t1", t0+2*hour); removed != 0 {
			t.Fatalf("再删应为 0，得到 %d", removed)
		}
		rows := query(e, "t1", all)
		if len(rows) != 1 || rows[0].BucketStartMs != t0+2*hour {
			t.Fatalf("剩下的行：%+v", rows)
		}
		if len(query(e, "t2", all)) != 1 {
			t.Fatal("t2 不该被动")
		}
	})
}
