package store

import (
	"context"
	"sort"
	"strconv"
)

// 流量统计的粒度与类别（cloud-api §19）。
const (
	GranularityHour = "hour"
	GranularityDay  = "day"
	HourMs          = int64(3_600_000)
	DayMs           = int64(86_400_000)
)

// TrafficRow 是流量统计的一行：一个 UTC 整小时桶 × 节点 × 源 × 中继令牌 sub × dev × 类别的累计。
// 只有元数据，没有路径、头、IP 与正文。
type TrafficRow struct {
	BucketStartMs int64  `json:"bucketStartMs"`
	Node          string `json:"node"`
	SourceID      string `json:"sourceId"`
	Subject       string `json:"subject"`
	// DeviceID 是中继令牌的 dev；令牌没带时为空串。
	DeviceID      string `json:"deviceId"`
	Kind          string `json:"kind"`
	Connections   int64  `json:"connections"`
	BytesToSource int64  `json:"bytesToSource"`
	BytesToClient int64  `json:"bytesToClient"`
}

// TrafficQuery 是流量查询。
type TrafficQuery struct {
	// FromMs 含：桶起点 ≥ FromMs。
	FromMs int64
	// ToMs 不含：桶起点 < ToMs。
	ToMs int64
	// SourceID 与 Subject 非空时按它过滤。
	SourceID string
	Subject  string
	// Granularity 为 day 时按 UTC 自然日合并。
	Granularity string
	// Limit 最多答这么多行（合并之后）。
	Limit int
}

// TrafficStore 是流量统计的存储。写入不碰租户修订号（统计不算「状态被改」）。
type TrafficStore interface {
	// Upsert 在一个事务里逐行累加：同一桶与维度的行三个计数相加，没有就插入。
	Upsert(ctx context.Context, tenantID string, rows []TrafficRow) error
	// Query 按桶、节点、源、sub、dev、类别排序（按字节序）。
	Query(ctx context.Context, tenantID string, query TrafficQuery) ([]TrafficRow, error)
	// Prune 删掉桶起点早于 beforeMs 的行（保留期），答删了几行。
	Prune(ctx context.Context, tenantID string, beforeMs int64) (int64, error)
}

// TrafficKey 是一行的维度键（不含计数）；NUL 分隔，维度里不会出现 NUL。
func TrafficKey(row TrafficRow) string {
	return strconv.FormatInt(row.BucketStartMs, 10) + "\x00" + row.Node + "\x00" + row.SourceID + "\x00" +
		row.Subject + "\x00" + row.DeviceID + "\x00" + row.Kind
}

// CompareTraffic 按桶、节点、源、sub、dev、类别排序（字节序，与 SQL 的二进制排序一致）。
func CompareTraffic(left, right TrafficRow) int {
	switch {
	case left.BucketStartMs < right.BucketStartMs:
		return -1
	case left.BucketStartMs > right.BucketStartMs:
		return 1
	}
	pairs := [][2]string{
		{left.Node, right.Node}, {left.SourceID, right.SourceID}, {left.Subject, right.Subject},
		{left.DeviceID, right.DeviceID}, {left.Kind, right.Kind},
	}
	for _, p := range pairs {
		switch {
		case p[0] < p[1]:
			return -1
		case p[0] > p[1]:
			return 1
		}
	}
	return 0
}

// MergeTraffic 把同一维度的行合并（计数相加），按固定顺序返回：多节点同时写重叠的桶时
// 加锁顺序固定，不会互相死锁。
func MergeTraffic(rows []TrafficRow) []TrafficRow {
	merged := map[string]*TrafficRow{}
	var order []*TrafficRow
	for _, row := range rows {
		key := TrafficKey(row)
		if found, ok := merged[key]; ok {
			found.Connections += row.Connections
			found.BytesToSource += row.BytesToSource
			found.BytesToClient += row.BytesToClient
			continue
		}
		copied := row
		merged[key] = &copied
		order = append(order, &copied)
	}
	out := make([]TrafficRow, len(order))
	for i, row := range order {
		out[i] = *row
	}
	sort.SliceStable(out, func(i, j int) bool { return CompareTraffic(out[i], out[j]) < 0 })
	return out
}
