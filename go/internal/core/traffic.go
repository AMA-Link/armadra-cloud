package core

import (
	"context"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// 中继流量统计（cloud-api §19）的查询服务 me.traffic。存储端口是 store.TrafficStore；
// 一行 = 一个 UTC 整小时桶 × 节点 × 源 × 中继令牌 sub × dev × 类别的累计，不记路径、头、IP 与正文。

// TrafficMaxRows：me.traffic 一次最多答这么多行，超出答 bad_request（缩小时间范围或按源过滤）。
const TrafficMaxRows = 10_000

// TrafficSummaryInput 是 me.traffic 的入参。
type TrafficSummaryInput struct {
	FromMs   int64
	ToMs     int64
	SourceID string
	// Granularity 是 hour 或 day。
	Granularity string
}

// Traffic 是流量服务。
type Traffic struct{ c *Context }

// NewTraffic 建流量服务。
func NewTraffic(c *Context) *Traffic { return &Traffic{c: c} }

// Summary：账号本人看本租户全部源（含已撤销的源留下的历史），访客只看 subject 是自己的行。
// FromMs ≥ ToMs 或超过 TrafficMaxRows 行答 bad_request。
func (t *Traffic) Summary(ctx context.Context, caller Caller, input TrafficSummaryInput) ([]store.TrafficRow, error) {
	if input.FromMs >= input.ToMs {
		return nil, Fail(gen.ErrBadRequest)
	}
	granularity := input.Granularity
	if granularity == "" {
		granularity = store.GranularityHour
	}
	query := store.TrafficQuery{
		FromMs: input.FromMs, ToMs: input.ToMs, SourceID: input.SourceID,
		Granularity: granularity, Limit: TrafficMaxRows + 1,
	}
	if caller.Role != RoleOwner {
		query.Subject = caller.Subject
	}
	rows, err := t.c.Ports.Traffic.Query(ctx, t.c.TenantID, query)
	if err != nil {
		return nil, err
	}
	if len(rows) > TrafficMaxRows {
		return nil, Fail(gen.ErrBadRequest)
	}
	if rows == nil {
		rows = []store.TrafficRow{}
	}
	return rows, nil
}
