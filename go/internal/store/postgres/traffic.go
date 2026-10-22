package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// 流量统计端口（表 relay_traffic_buckets，迁移 0003）。一批行先按键合并（同一条 INSERT … ON CONFLICT
// 不能两次命中同一行），再按维度排序（固定加锁顺序，多节点同时写不死锁）、按块多行插入；累加在库里做，
// 同一桶不丢计数。写入不锁租户行、不碰修订号。

// trafficChunk：每条 INSERT 最多这么多行（10 列，远低于 65 535 个参数的上限）。
const trafficChunk = 1_000

type traffic ports

func trafficStore(p ports) store.TrafficStore { return traffic(p) }

func (t traffic) Upsert(ctx context.Context, tenantID string, rows []store.TrafficRow) error {
	merged := store.MergeTraffic(rows)
	if len(merged) == 0 {
		return nil
	}
	return t.d.tx(ctx, pgx.TxOptions{}, func(tx pgx.Tx) error {
		for from := 0; from < len(merged); from += trafficChunk {
			chunk := merged[from:min(from+trafficChunk, len(merged))]
			var (
				values strings.Builder
				args   = make([]any, 0, len(chunk)*10)
			)
			for i, row := range chunk {
				if i > 0 {
					values.WriteString(", ")
				}
				base := i * 10
				values.WriteString("(")
				for c := 1; c <= 10; c++ {
					if c > 1 {
						values.WriteString(", ")
					}
					fmt.Fprintf(&values, "$%d", base+c)
				}
				values.WriteString(")")
				args = append(args, tenantID, row.BucketStartMs, row.Node, row.SourceID, row.Subject,
					row.DeviceID, row.Kind, row.Connections, row.BytesToSource, row.BytesToClient)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO relay_traffic_buckets
			  (tenant_id, bucket_start_ms, node, source_id, subject, device_id, kind,
			   connections, bytes_to_source, bytes_to_client) VALUES `+values.String()+`
			  ON CONFLICT (tenant_id, bucket_start_ms, node, source_id, subject, device_id, kind)
			  DO UPDATE SET
			    connections = relay_traffic_buckets.connections + excluded.connections,
			    bytes_to_source = relay_traffic_buckets.bytes_to_source + excluded.bytes_to_source,
			    bytes_to_client = relay_traffic_buckets.bytes_to_client + excluded.bytes_to_client`, args...); err != nil {
				return err
			}
		}
		return nil
	})
}

func (t traffic) Query(ctx context.Context, tenantID string, query store.TrafficQuery) ([]store.TrafficRow, error) {
	size := int64(1)
	if query.Granularity == store.GranularityDay {
		size = store.DayMs
	}
	args := []any{size, tenantID, query.FromMs, query.ToMs}
	filters := ""
	if query.SourceID != "" {
		args = append(args, query.SourceID)
		filters += fmt.Sprintf(" AND source_id = $%d", len(args))
	}
	if query.Subject != "" {
		args = append(args, query.Subject)
		filters += fmt.Sprintf(" AND subject = $%d", len(args))
	}
	args = append(args, query.Limit)
	statement := fmt.Sprintf(`SELECT (bucket_start_ms / $1::bigint) * $1::bigint AS bucket_start_ms,
	    node, source_id, subject, device_id, kind,
	    sum(connections)::bigint AS connections, sum(bytes_to_source)::bigint AS bytes_to_source,
	    sum(bytes_to_client)::bigint AS bytes_to_client
	  FROM relay_traffic_buckets
	  WHERE tenant_id = $2 AND bucket_start_ms >= $3 AND bucket_start_ms < $4%s
	  GROUP BY 1, node, source_id, subject, device_id, kind
	  ORDER BY 1, node COLLATE "C", source_id COLLATE "C", subject COLLATE "C",
	    device_id COLLATE "C", kind COLLATE "C"
	  LIMIT $%d`, filters, len(args))
	return list(ctx, t.d.pool, func(s scanner) (store.TrafficRow, error) {
		var row store.TrafficRow
		err := s.Scan(&row.BucketStartMs, &row.Node, &row.SourceID, &row.Subject, &row.DeviceID, &row.Kind,
			&row.Connections, &row.BytesToSource, &row.BytesToClient)
		return row, err
	}, statement, args...)
}

func (t traffic) Prune(ctx context.Context, tenantID string, beforeMs int64) (int64, error) {
	tag, err := t.d.pool.Exec(ctx,
		"DELETE FROM relay_traffic_buckets WHERE tenant_id = $1 AND bucket_start_ms < $2", tenantID, beforeMs)
	return tag.RowsAffected(), err
}
