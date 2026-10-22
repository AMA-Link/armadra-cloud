// Package trafficstore 把流量累加器（traffic.Meter）的落库接到存储端口（store.TrafficStore）：
// traffic.Row 与 store.TrafficRow 一一对应，这里只做字段搬运。装配方（personal、saas）这样接：
//
//	meter := traffic.NewMeter(traffic.MeterOptions{Node: nodeID, Write: trafficstore.Writer(ports.Traffic, tenantID)})
//	go meter.Run(ctx) // 每 60 秒 Flush，ctx 结束时最后 Flush 一次
package trafficstore

import (
	"context"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/traffic"
)

// Writer 返回 traffic.Meter 用的 Writer：一批行在一次 Upsert 里累加进租户的统计表。
func Writer(ts store.TrafficStore, tenantID string) traffic.Writer {
	return func(ctx context.Context, rows []traffic.Row) error {
		return ts.Upsert(ctx, tenantID, Rows(rows))
	}
}

// Rows 把累加器的行换成存储的行。
func Rows(rows []traffic.Row) []store.TrafficRow {
	out := make([]store.TrafficRow, len(rows))
	for i, r := range rows {
		out[i] = store.TrafficRow{
			BucketStartMs: r.BucketStartMs, Node: r.Node, SourceID: r.SourceID, Subject: r.Subject,
			DeviceID: r.DeviceID, Kind: string(r.Kind),
			Connections: r.Connections, BytesToSource: r.BytesToSource, BytesToClient: r.BytesToClient,
		}
	}
	return out
}
