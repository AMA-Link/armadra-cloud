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
// 并核过主体合法性。已有同主体的有效授予就按「角色取高、范围并集、到期取晚（不过期最晚）」合并进那一条，不建第二条；
// 已经到期却还没被 grant.expire 撤销的授予当作不存在（先以 system 撤销，再建新的）。合并只会放宽，所以不撤令牌。
// 答最终的授予与是否新建。
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
	patch := saas.GrantPatch{}
	if roleRank[in.Role] > roleRank[current.Role] {
		role := in.Role
		patch.Role = &role
		current.Role = role
	}
	if merged := mergeWorkspaces(current.WorkspaceIDs, in.WorkspaceIDs); !slices.Equal(merged, sortedUnique(current.WorkspaceIDs)) {
		if len(merged) > MaxScopeEntries {
			return saas.Grant{}, false, core.Fail(gen.ErrConflict) // 并集超过一个断言能带的范围
		}
		patch.WorkspaceIDs = &merged
		current.WorkspaceIDs = merged
	}
	switch {
	case current.ExpiresAtMs == nil: // 已经不过期，最宽
	case in.ExpiresAtMs == nil:
		patch.ClearExpiry = true
		current.ExpiresAtMs = nil
	case *in.ExpiresAtMs > *current.ExpiresAtMs:
		expires := *in.ExpiresAtMs
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

// mergeWorkspaces 是两份范围的并集；任一份是整台（空）则结果是整台（空）。
func mergeWorkspaces(a, b []string) []string {
	if len(a) == 0 || len(b) == 0 {
		return []string{}
	}
	return sortedUnique(append(slices.Clone(a), b...))
}
