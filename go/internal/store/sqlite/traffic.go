package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// 流量统计端口（表 relay_traffic_buckets，迁移 0002）。写入不碰租户修订号：统计不是
// 「状态被改」，serve 的外部改动检测不该被它触发。

const upsertTraffic = `INSERT INTO relay_traffic_buckets
  (tenant_id, bucket_start_ms, node, source_id, subject, device_id, kind,
   connections, bytes_to_source, bytes_to_client)
  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
  ON CONFLICT (tenant_id, bucket_start_ms, node, source_id, subject, device_id, kind)
  DO UPDATE SET
    connections = connections + excluded.connections,
    bytes_to_source = bytes_to_source + excluded.bytes_to_source,
    bytes_to_client = bytes_to_client + excluded.bytes_to_client`

type traffic ports

func trafficStore(p ports) store.TrafficStore { return traffic(p) }

func (t traffic) Upsert(ctx context.Context, tenantID string, rows []store.TrafficRow) error {
	if len(rows) == 0 {
		return nil
	}
	return t.d.write(ctx, func(q *sql.Conn) error {
		for _, row := range rows {
			if _, err := q.ExecContext(ctx, upsertTraffic, tenantID, row.BucketStartMs, row.Node, row.SourceID,
				row.Subject, row.DeviceID, row.Kind, row.Connections, row.BytesToSource, row.BytesToClient); err != nil {
				return err
			}
		}
		return nil
	})
}

func (t traffic) Query(ctx context.Context, tenantID string, query store.TrafficQuery) (out []store.TrafficRow, err error) {
	bucket := "bucket_start_ms"
	if query.Granularity == store.GranularityDay {
		bucket = fmt.Sprintf("(bucket_start_ms / %d) * %d", store.DayMs, store.DayMs)
	}
	where := []string{"tenant_id = ?", "bucket_start_ms >= ?", "bucket_start_ms < ?"}
	args := []any{tenantID, query.FromMs, query.ToMs}
	if query.SourceID != "" {
		where = append(where, "source_id = ?")
		args = append(args, query.SourceID)
	}
	if query.Subject != "" {
		where = append(where, "subject = ?")
		args = append(args, query.Subject)
	}
	args = append(args, query.Limit)
	statement := fmt.Sprintf(`SELECT %s AS bucket_start_ms, node, source_id, subject, device_id, kind,
	     sum(connections) AS connections, sum(bytes_to_source) AS bytes_to_source,
	     sum(bytes_to_client) AS bytes_to_client
	   FROM relay_traffic_buckets
	   WHERE %s
	   GROUP BY 1, node, source_id, subject, device_id, kind
	   ORDER BY 1, node, source_id, subject, device_id, kind
	   LIMIT ?`, bucket, strings.Join(where, " AND "))
	err = t.d.read(ctx, func(q *sql.Conn) error {
		rows, err := q.QueryContext(ctx, statement, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var row store.TrafficRow
			if err := rows.Scan(&row.BucketStartMs, &row.Node, &row.SourceID, &row.Subject, &row.DeviceID,
				&row.Kind, &row.Connections, &row.BytesToSource, &row.BytesToClient); err != nil {
				return err
			}
			out = append(out, row)
		}
		return rows.Err()
	})
	return out, err
}

func (t traffic) Prune(ctx context.Context, tenantID string, beforeMs int64) (removed int64, err error) {
	err = t.d.write(ctx, func(q *sql.Conn) error {
		res, err := q.ExecContext(ctx,
			"DELETE FROM relay_traffic_buckets WHERE tenant_id = ? AND bucket_start_ms < ?", tenantID, beforeMs)
		if err != nil {
			return err
		}
		removed, err = res.RowsAffected()
		return err
	})
	return removed, err
}
