package saas

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AMA-Link/armadra-cloud/go/internal/traffic"
)

// 流量统计（SaaS 实施设计 §2.4 的 N5）：只在入口节点计，经只有 relay_traffic_buckets 写权限的角色
// （armadra_relay_traffic）写 PostgreSQL；节点不清理保留期（控制面的作业清，SaaS 实施设计 §2.2）。
// team_id 是中继令牌的 team，不进主键，按首次写入的团队计（迁移 0009）。

// Tenant 是 SaaS 的存储租户。
const Tenant = "cloud"

// requiredMigration 是流量表有 team_id 列的迁移（0009_saas_ops）。
const requiredMigration = 9

// trafficChunk：每条 INSERT 最多这么多行（11 列，远低于参数上限）。
const trafficChunk = 1_000

// openTraffic 连库并核对账本：账本里没有 requiredMigration（控制面还没 migrate 到）就拒绝启动。
// 错误不带连接串。
func openTraffic(ctx context.Context, url string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, errors.New("RELAY_DATABASE_URL 不合格式")
	}
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, errors.New("流量库连不上")
	}
	var applied int
	err = pool.QueryRow(ctx, "SELECT count(*) FROM cloud_migrations WHERE id = $1", requiredMigration).Scan(&applied)
	if err != nil {
		pool.Close()
		return nil, errors.New("流量库连不上或读不了账本（cloud_migrations）")
	}
	if applied == 0 {
		pool.Close()
		return nil, errors.New("流量库的数据库版本落后，先运行 armadra-cloud migrate")
	}
	return pool, nil
}

// trafficKey 是表的主键（不含租户；team 不在主键里）。
type trafficKey struct {
	bucket                              int64
	node, source, subject, device, kind string
}

// trafficWriter 是 traffic.Meter 的 Writer：一批行先按主键合并（同一条 INSERT 不能两次命中同一行），
// 按主键排序（固定加锁顺序，多节点同时写不死锁），按块多行插入，计数在库里相加。
func trafficWriter(pool *pgxpool.Pool) traffic.Writer {
	return func(ctx context.Context, rows []traffic.Row) error {
		merged := map[trafficKey]*traffic.Row{}
		for _, r := range rows {
			k := trafficKey{r.BucketStartMs, r.Node, r.SourceID, r.Subject, r.DeviceID, string(r.Kind)}
			// 主键不含 team：同一主键的两行合并时留第一行的 team。
			if cur, ok := merged[k]; ok {
				cur.Connections += r.Connections
				cur.BytesToSource += r.BytesToSource
				cur.BytesToClient += r.BytesToClient
				continue
			}
			c := r
			merged[k] = &c
		}
		if len(merged) == 0 {
			return nil
		}
		list := make([]*traffic.Row, 0, len(merged))
		for _, r := range merged {
			list = append(list, r)
		}
		sort.Slice(list, func(i, j int) bool {
			a, b := list[i], list[j]
			if a.BucketStartMs != b.BucketStartMs {
				return a.BucketStartMs < b.BucketStartMs
			}
			return strings.Join([]string{a.Node, a.SourceID, a.Subject, a.DeviceID, string(a.Kind)}, "\x00") <
				strings.Join([]string{b.Node, b.SourceID, b.Subject, b.DeviceID, string(b.Kind)}, "\x00")
		})
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		for from := 0; from < len(list); from += trafficChunk {
			chunk := list[from:min(from+trafficChunk, len(list))]
			var values strings.Builder
			args := make([]any, 0, len(chunk)*11)
			for i, r := range chunk {
				if i > 0 {
					values.WriteString(", ")
				}
				values.WriteString("(")
				for c := 1; c <= 11; c++ {
					if c > 1 {
						values.WriteString(", ")
					}
					fmt.Fprintf(&values, "$%d", i*11+c)
				}
				values.WriteString(")")
				args = append(args, Tenant, r.BucketStartMs, r.Node, r.SourceID, r.Subject, r.DeviceID, string(r.Kind),
					r.Connections, r.BytesToSource, r.BytesToClient, r.TeamID)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO relay_traffic_buckets
			  (tenant_id, bucket_start_ms, node, source_id, subject, device_id, kind,
			   connections, bytes_to_source, bytes_to_client, team_id) VALUES `+values.String()+`
			  ON CONFLICT (tenant_id, bucket_start_ms, node, source_id, subject, device_id, kind)
			  DO UPDATE SET
			    connections = relay_traffic_buckets.connections + excluded.connections,
			    bytes_to_source = relay_traffic_buckets.bytes_to_source + excluded.bytes_to_source,
			    bytes_to_client = relay_traffic_buckets.bytes_to_client + excluded.bytes_to_client`, args...); err != nil {
				return err
			}
		}
		return tx.Commit(ctx)
	}
}
