package saas

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
)

// AuditCursor 是审计列表分页的游标：(at_ms, event_id) 之后（按时间倒序）。
type AuditCursor struct {
	AtMs    int64
	EventID string
}

type auditCursorJSON struct {
	A int64  `json:"a"`
	E string `json:"e"`
}

// Encode 是游标的线上编码（对客户端不透明）。
func (c AuditCursor) Encode() string {
	data, _ := json.Marshal(auditCursorJSON{A: c.AtMs, E: c.EventID})
	return base64.RawURLEncoding.EncodeToString(data)
}

// DecodeAuditCursor 解游标；不合格式答 ErrBadCursor。
func DecodeAuditCursor(text string) (*AuditCursor, error) {
	data, err := base64.RawURLEncoding.DecodeString(text)
	var c auditCursorJSON
	if err != nil || json.Unmarshal(data, &c) != nil || c.E == "" {
		return nil, errors.New("游标不合格式")
	}
	return &AuditCursor{AtMs: c.A, EventID: c.E}, nil
}

// AuditQuery 是审计与安全日志的筛选。零值的字段不筛。
type AuditQuery struct {
	// FromMs 含，ToMs 不含；0 = 不限（审计时间不会是 0）。
	FromMs, ToMs int64
	// Actor 是主体 id（actor_id 精确相等）。
	Actor string
	// SourceID 筛涉及的主机。
	SourceID string
	// ActionPrefix 是动作名前缀（仓储自己转义 % _ \）。
	ActionPrefix string
	// After 非空时只取排在它之后的（时间倒序）。
	After *AuditCursor
	// Limit 大于 0 时最多取这么多行。
	Limit int
}

// RetentionRepo 是保留期与分区之外的清理（设计 §6.1 的 audit.retention、traffic.retention）。
// 都是系统作业，没有调用者，所以收裸 id；每次最多删 limit 行，调用方循环到 0 为止（不一次锁住大批行）。
type RetentionRepo interface {
	// TeamsWithAuditBefore 是在 beforeMs 之前还有审计行的团队 id（升序，id 大于 afterTeamID，最多 limit 个）；
	// 调用方用上一批最后一个 id 翻页，一批里清不干净的团队（保留期比 beforeMs 更长）不会让扫描停在原地。
	TeamsWithAuditBefore(ctx context.Context, tenantID string, beforeMs int64, afterTeamID string, limit int) ([]string, error)
	// PruneTeamAudit 删这个团队早于 beforeMs 的审计行：只动 team_id 相符的行。
	PruneTeamAudit(ctx context.Context, tenantID, teamID string, beforeMs int64, limit int) (int, error)
	// PruneSecurityLog 删早于 beforeMs 的账号安全日志（action 以 security. 开头、没有团队）。
	PruneSecurityLog(ctx context.Context, tenantID string, beforeMs int64, limit int) (int, error)
	// PruneTraffic 删早于 beforeMs 的流量桶。
	PruneTraffic(ctx context.Context, tenantID string, beforeMs int64, limit int) (int, error)
}

// DayBytes 是一天的中继流量（startMs 是当天 UTC 零点）。
type DayBytes struct {
	StartMs int64
	Bytes   int64
}

// UsageReader 是个人空间用量的只读查询（Reader 嵌入它）；团队空间的在 TeamRepo 上。
// 流量口径同团队：入口节点计的端到端字节（kind = e2e）。
type UsageReader interface {
	// PersonalTraffic 是账号个人空间主机在 [startMs, endMs) 的字节数。
	PersonalTraffic(ctx context.Context, tenantID, accountID string, startMs, endMs int64) (int64, error)
	// PersonalTrafficDays 同上，按 UTC 天汇总（只含有流量的天，升序）。
	PersonalTrafficDays(ctx context.Context, tenantID, accountID string, startMs, endMs int64) ([]DayBytes, error)
}
