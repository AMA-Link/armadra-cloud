package saas

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// Host 是 SaaS 的一台主机：核心的源记录，加 source_spaces 的空间、所有者、标签与设置（迁移 0006）。
type Host struct {
	store.SourceRecord
	// TeamID 空 = 个人空间（归 OwnerAccountID）。
	TeamID         string
	OwnerAccountID string
	// OwnerName 是所有者的显示名。
	OwnerName string
	Tags      []string
	// Settings 是 hostSettings 的 JSON；nil = 缺省。
	Settings    json.RawMessage
	CreatedAtMs int64
	UpdatedAtMs int64
	// ViewerRole 是查询者在主机所属团队里的角色；个人空间为空。
	ViewerRole TeamRole
	// TeamDefaultRole 是主机所属团队设置里的 defaultHostRole（主机设置没写 defaultRole 时的预选值）；个人空间为空。
	TeamDefaultRole string
}

// CanManage 报告 viewerID 能否管理这台主机（设计 §4.6）：主机所有者，或团队主机的团队 owner / admin。
// 团队主机的所有者要仍是 owner / admin / member（降为 billing / guest 就失去管理权）；ViewerRole 是查询者
// 的团队角色，所以只对 ListHosts 以 viewerID 查出来的行有意义。
func (h Host) CanManage(viewerID string) bool {
	if h.TeamID == "" {
		return h.OwnerAccountID == viewerID
	}
	if h.ViewerRole == RoleOwner || h.ViewerRole == RoleAdmin {
		return true
	}
	return h.OwnerAccountID == viewerID && h.ViewerRole == RoleMember
}

// HostCursor 是目录分页的游标：(registered_at_ms, source_id) 之后。
type HostCursor struct {
	RegisteredAtMs int64
	SourceID       string
}

// ErrBadCursor 表示游标不是 Encode 出来的。
var ErrBadCursor = errors.New("游标不合格式")

type hostCursorJSON struct {
	R int64  `json:"r"`
	S string `json:"s"`
}

// Encode 是游标的线上编码（对客户端不透明）。
func (c HostCursor) Encode() string {
	data, _ := json.Marshal(hostCursorJSON{R: c.RegisteredAtMs, S: c.SourceID})
	return base64.RawURLEncoding.EncodeToString(data)
}

// DecodeHostCursor 解游标；不合格式答 ErrBadCursor。
func DecodeHostCursor(text string) (*HostCursor, error) {
	data, err := base64.RawURLEncoding.DecodeString(text)
	var c hostCursorJSON
	if err != nil || json.Unmarshal(data, &c) != nil || c.S == "" {
		return nil, ErrBadCursor
	}
	return &HostCursor{RegisteredAtMs: c.R, SourceID: c.S}, nil
}

// HostFilter 是目录查询：只返回 ViewerID 此刻能看到的主机（可见性在查询里，不在调用方）。
type HostFilter struct {
	// ViewerID 是查询者的账号 id。
	ViewerID string
	// NowMs 是判授予到期的时钟（毫秒）。
	NowMs int64
	// SourceID 非空时只取这一台。
	SourceID string
	// SpaceKind 取 SpacePersonal / SpaceTeam；空 = 全部可达的空间。SpaceTeam 要给 TeamID。
	SpaceKind string
	TeamID    string
	// Revoked 为真只列已撤销的，否则只列未撤销的。
	Revoked bool
	// Tags 取「且」：主机要带齐。
	Tags []string
	// Query 是名称的模糊词（调用方不必转义，仓储自己转义 % _ \）。
	Query string
	// After 非空时只取排在它之后的。
	After *HostCursor
	// Limit 大于 0 时最多取这么多行。
	Limit int
}

// HostReader 是主机的只读查询（Reader 嵌入它）。
type HostReader interface {
	// ListHosts 按 (registered_at_ms, source_id) 升序列出 ViewerID 能看到的主机。
	ListHosts(ctx context.Context, tenantID string, filter HostFilter) ([]Host, error)
	// SpaceOf 读主机的空间行；没有答 nil。
	SpaceOf(ctx context.Context, tenantID, sourceID string) (*HostSpace, error)
}

// HostSpace 是主机的空间行连同源的撤销状态，以及判定分享链接与访客所需的设置与团队状态。
type HostSpace struct {
	SourceID       string
	TeamID         string
	OwnerAccountID string
	Revoked        bool
	// Settings 是主机设置 JSON（hostSettings，只存写过的项）；nil = 缺省。
	Settings json.RawMessage
	// TeamSettings、TeamStatus、TeamDeleted 是团队主机所属团队的设置 JSON、状态与是否在恢复期内；个人空间为空。
	TeamSettings json.RawMessage
	TeamStatus   string
	TeamDeleted  bool
	// TeamName 是团队主机所属团队的名字；个人空间为空。
	TeamName string
}

// HostPatch 是对 source_spaces 的部分更新：非 nil 的字段才写。
type HostPatch struct {
	Tags     *[]string
	Settings json.RawMessage
}

// RegistrationToken 是带空间的注册令牌（registration_tokens.team_id 空 = 个人空间）。
type RegistrationToken struct {
	TokenHash   string
	AccountID   string
	TeamID      string
	ExpiresAtMs int64
}

// HostRepo 是主机相关的仓储。
type HostRepo interface {
	// LockSource 对 (租户, 源) 取事务级咨询锁：登记同一个源 id 的并发事务排队。
	// 加锁顺序里它排在团队行与账号行之后。
	LockSource(ctx context.Context, tenantID, sourceID string) error
	// CountPersonal 数账号个人空间里未撤销的主机（不含 excludeSourceID）。
	CountPersonal(ctx context.Context, tenantID, accountID, excludeSourceID string) (int, error)
	// CountTeam 数团队里未撤销的主机（不含 excludeSourceID）。
	CountTeam(ctx context.Context, scope TeamScope, excludeSourceID string) (int, error)
	// UpsertSpace 写主机的空间行：同一空间的重登记保留标签与设置，换了空间则重置。
	UpsertSpace(ctx context.Context, tenantID string, space HostSpace, nowMs int64) error
	// Patch 改标签与设置；teamID 必须与库里的空间相符（空 = 个人空间），不符或没有这台主机答 false。
	Patch(ctx context.Context, tenantID, sourceID, teamID string, patch HostPatch, nowMs int64) (bool, error)
	// AddRegistration 写一枚注册令牌：账号同时有效的令牌已满 max 条答 false（什么也不写）。
	AddRegistration(ctx context.Context, tenantID string, token RegistrationToken, nowMs int64, max int) (bool, error)
	// PeekRegistration 读未过期的令牌但不消耗、不加锁：登记先靠它知道令牌属于哪个空间，再按「团队 → 账号」的顺序加锁。
	PeekRegistration(ctx context.Context, tenantID, tokenHash string, nowMs int64) (*RegistrationToken, error)
	// ConsumeRegistration 一次性消耗未过期的令牌，答它的记录；没命中答 nil。
	ConsumeRegistration(ctx context.Context, tenantID, tokenHash string, nowMs int64) (*RegistrationToken, error)
}
