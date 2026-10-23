package postgres

import (
	"context"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// 链接的 SaaS 侧查询：链接表在核心端口里，这里只补 SaaS 要按一批主机列链接的查询（设计 §4.7）。

func (r reader) LinksForSources(ctx context.Context, tenantID string, sourceIDs []string, limit int) ([]store.LinkRecord, error) {
	if len(sourceIDs) == 0 {
		return nil, nil
	}
	return list(ctx, r.q, deref(toLink),
		"SELECT "+linkColumns+" FROM links WHERE tenant_id = $1 AND source_id = ANY($2) ORDER BY seq LIMIT $3",
		tenantID, sourceIDs, limit)
}
