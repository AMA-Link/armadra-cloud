package grants

import (
	"context"
	"slices"
	"sort"

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
// 来路里最早的非空到期（与 Compose 同规则）。合成结果可能比已有授予窄（角色降、范围缩、到期提前），此时 narrowed 为真，
// 调用方要在提交后撤销该账号在这台主机上已签的令牌。已经到期却还没被 grant.expire 撤销的授予当作不存在（先以 system 撤销，再建新的）。
// 答最终的授予、是否新建、是否收窄了已有授予。
func ApplyInvitation(ctx context.Context, tx saas.Tx, tenantID string, nowMs int64, in InviteGrant) (grant saas.Grant, created, narrowed bool, err error) {
	existing, err := tx.ListGrants(ctx, tenantID, saas.GrantQuery{SourceID: in.SourceID, AccountID: in.AccountID})
	if err != nil {
		return saas.Grant{}, false, false, err
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
			return saas.Grant{}, false, false, err
		}
		if err := tx.Jobs().Cancel(ctx, tenantID, JobExpire, g.GrantID); err != nil {
			return saas.Grant{}, false, false, err
		}
	}
	if current == nil {
		grant := saas.Grant{
			GrantID: core.NewUUID(), SourceID: in.SourceID, TeamID: in.TeamID, SubjectKind: in.SubjectKind, SubjectID: in.AccountID,
			Role: in.Role, WorkspaceIDs: sortedUnique(in.WorkspaceIDs), ExpiresAtMs: in.ExpiresAtMs, GrantedBy: in.GrantedBy,
			Origin: saas.GrantOriginInvitation, InvitationID: in.InvitationID, CreatedAtMs: nowMs, UpdatedAtMs: nowMs,
		}
		if err := tx.Grants().Insert(ctx, tenantID, grant); err != nil {
			return saas.Grant{}, false, false, err
		}
		return grant, true, false, scheduleExpiry(ctx, tx, tenantID, grant.GrantID, grant.ExpiresAtMs, nowMs)
	}
	old := Source{Role: current.Role, WorkspaceIDs: sortedUnique(current.WorkspaceIDs), ExpiresAtMs: current.ExpiresAtMs}
	add := Source{Role: in.Role, WorkspaceIDs: sortedUnique(in.WorkspaceIDs), ExpiresAtMs: in.ExpiresAtMs}
	before, merged := mergeSources([]Source{old}), mergeSources([]Source{old, add})
	narrowed = Narrowed(before, merged)
	ws := merged.WorkspaceIDs
	if merged.Whole {
		ws = []string{}
	}
	if len(ws) > MaxScopeEntries {
		return saas.Grant{}, false, false, core.Fail(gen.ErrConflict) // 并集超过一个断言能带的范围
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
		return *current, false, false, nil
	}
	if ok, err := tx.Grants().Update(ctx, tenantID, current.GrantID, patch, nowMs); err != nil || !ok {
		if err == nil {
			err = core.Fail(gen.ErrConflict)
		}
		return saas.Grant{}, false, false, err
	}
	if patch.ClearExpiry || patch.ExpiresAtMs != nil {
		if err := scheduleExpiry(ctx, tx, tenantID, current.GrantID, current.ExpiresAtMs, nowMs); err != nil {
			return saas.Grant{}, false, false, err
		}
	}
	return *current, false, narrowed, nil
}

// mergeSources 把几个来路合成一个 (角色, 范围, 到期)，规则与 Compose 一致：
//   - 范围 S = 并集，任一来路整台则整台；
//   - 角色 R = 最大的角色，使 S 中每一处都被某个角色 ≥ R 的来路覆盖：整台时是整台来路中的最高角色，
//     只有限工作空间来路时是 min over w∈S 的「覆盖 w 的来路中的最高角色」；
//   - 到期取对 S 与 R 有贡献的来路中最早的非空到期：整台时是角色恰为 R 的整台来路，限范围时是角色 ≥ R 的来路。
func mergeSources(from []Source) Effective {
	out := Effective{From: from}
	scope := map[string]bool{}
	wholeRank := 0
	for _, source := range from {
		if len(source.WorkspaceIDs) == 0 {
			out.Whole = true
			wholeRank = max(wholeRank, roleRank[source.Role])
		}
		for _, id := range source.WorkspaceIDs {
			scope[id] = true
		}
	}
	out.WorkspaceIDs = []string{}
	rank := wholeRank
	if !out.Whole {
		for id := range scope {
			out.WorkspaceIDs = append(out.WorkspaceIDs, id)
		}
		sort.Strings(out.WorkspaceIDs)
		rank = 0
		for _, id := range out.WorkspaceIDs {
			best := 0
			for _, source := range from {
				if slices.Contains(source.WorkspaceIDs, id) {
					best = max(best, roleRank[source.Role])
				}
			}
			if rank == 0 || best < rank {
				rank = best
			}
		}
	}
	for role, r := range roleRank {
		if r == rank {
			out.Role = role
		}
	}
	for _, source := range from {
		contributes := roleRank[source.Role] >= rank
		if out.Whole {
			contributes = len(source.WorkspaceIDs) == 0 && roleRank[source.Role] == rank
		}
		if contributes && source.ExpiresAtMs != nil && (out.ExpiresAtMs == nil || *source.ExpiresAtMs < *out.ExpiresAtMs) {
			exp := *source.ExpiresAtMs
			out.ExpiresAtMs = &exp
		}
	}
	return out
}
