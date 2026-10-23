package grants

import (
	"context"
	"slices"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// InviteGrant 是被接受的主机邀请要落成的授予（设计 §4.7）：团队主机上接受者是 owner / admin / member 时主体为
// member，是 guest 角色的成员时为 guest；个人空间主机一律 guest。
type InviteGrant struct {
	SourceID string
	// TeamID 空 = 个人空间主机。
	TeamID string
	// SubjectKind 是 saas.GrantSubjectMember 或 saas.GrantSubjectGuest。
	SubjectKind string
	AccountID   string
	Role        string
	// WorkspaceIDs 已经过 CleanWorkspaces；空 = 整台主机。
	WorkspaceIDs []string
	// ExpiresAtMs 是授予的到期（取自邀请的 grant_expires_at_ms，不是邀请自身的有效期）；nil = 不过期。
	ExpiresAtMs  *int64
	GrantedBy    string
	InvitationID string
}

// ApplyInvitation 在事务里把主机邀请落成云端授予（origin = invitation）。调用方已按「团队 → 主机」的顺序加过锁，
// 并核过主体合法性。已有同主体的有效授予就和邀请合成一条（不建第二条）：范围取并集（任一整台即整台），角色取「使范围内每一处
// 都被角色 ≥ 它的来路覆盖」的最高角色（不放大——driver@w1 加 viewer 整台是 viewer 整台，不是 driver 整台），到期取对结果有贡献的
// 来路里最早的非空到期（与 Compose 同规则，composeSources）；合成会降低他已有的角色时答 409 conflict（不静默降级）。
// 已经到期却还没被 grant.expire 撤销的授予当作不存在（先以 system 撤销，再建新的）。
// 答最终的授予与是否新建。合成（含新增一条受限的授予）可能让账号的有效权限变窄，调用方要在前后各取一份有效权限，
// `Narrowed` 时在提交后撤销已签的令牌（grants.RevokeAccess）。
func ApplyInvitation(ctx context.Context, tx saas.Tx, tenantID string, nowMs int64, in InviteGrant) (saas.Grant, bool, error) {
	existing, err := tx.ListGrants(ctx, tenantID, saas.GrantQuery{SourceID: in.SourceID, AccountID: in.AccountID})
	if err != nil {
		return saas.Grant{}, false, err
	}
	var current *saas.Grant
	for i := range existing {
		g := existing[i]
		if g.SubjectKind != in.SubjectKind || g.SubjectID != in.AccountID {
			continue
		}
		if g.ActiveAt(nowMs) {
			current = &existing[i]
			continue
		}
		if ok, err := tx.Grants().Revoke(ctx, tenantID, g.GrantID, saas.GrantRevokedBySystem, nowMs); err != nil || !ok {
			if err == nil {
				err = core.Fail(gen.ErrConflict) // 并发里已被别人撤销：整个接受回滚，让调用方重试
			}
			return saas.Grant{}, false, err
		}
		if err := tx.Jobs().Cancel(ctx, tenantID, JobExpire, g.GrantID); err != nil {
			return saas.Grant{}, false, err
		}
	}
	if current == nil {
		grant := saas.Grant{
			GrantID: core.NewUUID(), SourceID: in.SourceID, TeamID: in.TeamID, SubjectKind: in.SubjectKind, SubjectID: in.AccountID,
			Role: in.Role, WorkspaceIDs: sortedUnique(in.WorkspaceIDs), ExpiresAtMs: in.ExpiresAtMs, GrantedBy: in.GrantedBy,
			Origin: saas.GrantOriginInvitation, InvitationID: in.InvitationID, CreatedAtMs: nowMs, UpdatedAtMs: nowMs,
		}
		if err := tx.Grants().Insert(ctx, tenantID, grant); err != nil {
			return saas.Grant{}, false, err
		}
		return grant, true, scheduleExpiry(ctx, tx, tenantID, grant.GrantID, grant.ExpiresAtMs, nowMs)
	}
	old := Source{Role: current.Role, WorkspaceIDs: sortedUnique(current.WorkspaceIDs), ExpiresAtMs: current.ExpiresAtMs}
	add := Source{Role: in.Role, WorkspaceIDs: sortedUnique(in.WorkspaceIDs), ExpiresAtMs: in.ExpiresAtMs}
	merged := composeSources([]Source{old, add})
	if roleRank[merged.Role] < roleRank[current.Role] {
		// 更宽但角色更低的邀请会降低他在某些工作空间上已有的角色（driver@w1 + viewer 整台）：不静默降级，
		// 也不放大，答 409 让管理者用 grants.update 明确处理。
		return saas.Grant{}, false, core.Fail(gen.ErrConflict)
	}
	ws := merged.WorkspaceIDs
	if merged.Whole {
		ws = []string{}
	}
	if len(ws) > MaxScopeEntries {
		return saas.Grant{}, false, core.Fail(gen.ErrConflict) // 并集超过一个断言能带的范围
	}
	patch := saas.GrantPatch{}
	if merged.Role != current.Role {
		role := merged.Role
		patch.Role = &role
		current.Role = role
	}
	if !slices.Equal(ws, sortedUnique(current.WorkspaceIDs)) {
		patch.WorkspaceIDs = &ws
		current.WorkspaceIDs = ws
	}
	switch {
	case merged.ExpiresAtMs == nil && current.ExpiresAtMs != nil:
		patch.ClearExpiry = true
		current.ExpiresAtMs = nil
	case merged.ExpiresAtMs != nil && (current.ExpiresAtMs == nil || *merged.ExpiresAtMs != *current.ExpiresAtMs):
		expires := *merged.ExpiresAtMs
		patch.ExpiresAtMs = &expires
		current.ExpiresAtMs = &expires
	}
	if patch.Role == nil && patch.WorkspaceIDs == nil && patch.ExpiresAtMs == nil && !patch.ClearExpiry {
		return *current, false, nil
	}
	if ok, err := tx.Grants().Update(ctx, tenantID, current.GrantID, patch, nowMs); err != nil || !ok {
		if err == nil {
			err = core.Fail(gen.ErrConflict)
		}
		return saas.Grant{}, false, err
	}
	if patch.ClearExpiry || patch.ExpiresAtMs != nil {
		if err := scheduleExpiry(ctx, tx, tenantID, current.GrantID, current.ExpiresAtMs, nowMs); err != nil {
			return saas.Grant{}, false, err
		}
	}
	return *current, false, nil
}
