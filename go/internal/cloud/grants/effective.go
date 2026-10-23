// Package grants 是授予、有效权限与断言合成（设计 §4.6）：grants.* 五个过程、sources.assertion、
// sources.accessList / accessRevoke、收窄即撤销的流程（§4.8），以及 core.AccessPolicy 的 SaaS 实现 Policy。
package grants

import (
	"slices"
	"sort"

	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// 有效权限的来路（grants.effective 的 from[].via）。
const (
	ViaOwner  = "owner"
	ViaTeam   = "team"
	ViaMember = "member"
	ViaGroup  = "group"
	ViaGuest  = "guest"
)

// roleRank 是 core 四档角色的高低：viewer < editor < operator < driver。
var roleRank = map[string]int{"viewer": 1, "editor": 2, "operator": 3, "driver": 4}

// ValidRole 报告是不是 core 的四档角色之一。
func ValidRole(role string) bool { return roleRank[role] > 0 }

// OwnerRole 是主机所有者的角色。
const OwnerRole = "driver"

// Source 是有效权限的一个来路。
type Source struct {
	Via          string
	GrantID      string
	Role         string
	WorkspaceIDs []string
	ExpiresAtMs  *int64
}

// Effective 是一个账号对一台主机的有效权限：纯查询 + 纯函数合成，不落库。
type Effective struct {
	// Role 空 = 没有访问权。
	Role string
	// Whole 为真是整台主机；否则 WorkspaceIDs 是允许的工作空间（升序去重）。
	Whole        bool
	WorkspaceIDs []string
	// ExpiresAtMs 是各来源中最早的非空到期；nil = 不过期。
	ExpiresAtMs *int64
	From        []Source
}

// None 报告没有访问权。
func (e Effective) None() bool { return e.Role == "" }

// Owner 报告来路里有主机所有者。
func (e Effective) Owner() bool {
	return slices.ContainsFunc(e.From, func(s Source) bool { return s.Via == ViaOwner })
}

// GuestOnly 报告所有来路都是 guest 授予（个人空间主机上的访客，要过 role-only 版本门）。
func (e Effective) GuestOnly() bool {
	return !e.None() && !slices.ContainsFunc(e.From, func(s Source) bool { return s.Via != ViaGuest })
}

func isStaff(role saas.TeamRole) bool {
	return role == saas.RoleOwner || role == saas.RoleAdmin || role == saas.RoleMember
}

// Compose 合成账号对主机的有效权限（设计 §4.6）：
//
//  1. 账号停用或主机已撤销 → 无。
//  2. 是主机所有者 → owner、driver、整台（团队主机要求仍是团队的 owner / admin / member）。
//  3. 团队主机：必须是该团队成员、团队未删除；team 授予只给 owner / admin / member，member 授予给本人，
//     guest 授予给 guest 角色的本人；billing 什么都没有。个人空间只有所有者与 guest 授予。
//  4. 忽略已撤销与 expires_at_ms ≤ now 的授予。
//  5. 角色取最高；范围取并集（任一来源整台则整台）；到期取各来源中最早的非空到期。
func Compose(b saas.AccessBasis, accountID string, nowMs int64) Effective {
	if !b.AccountActive || !b.SourceFound || b.SourceRevoked {
		return Effective{}
	}
	team := b.TeamID != ""
	if team && (b.TeamDeleted || b.TeamRole == "") {
		return Effective{}
	}
	var from []Source
	if b.OwnerID == accountID && (!team || isStaff(b.TeamRole)) {
		from = append(from, Source{Via: ViaOwner, Role: OwnerRole, WorkspaceIDs: []string{}})
	}
	for _, g := range b.Grants {
		if !g.ActiveAt(nowMs) || !ValidRole(g.Role) {
			continue
		}
		via := ""
		switch g.SubjectKind {
		case saas.GrantSubjectTeam:
			if team && isStaff(b.TeamRole) && g.SubjectID == b.TeamID {
				via = ViaTeam
			}
		case saas.GrantSubjectMember:
			if team && isStaff(b.TeamRole) && g.SubjectID == accountID {
				via = ViaMember
			}
		case saas.GrantSubjectGuest:
			if g.SubjectID == accountID && (!team || b.TeamRole == saas.RoleGuest) {
				via = ViaGuest
			}
		}
		if via != "" {
			from = append(from, Source{
				Via: via, GrantID: g.GrantID, Role: g.Role, WorkspaceIDs: sortedUnique(g.WorkspaceIDs), ExpiresAtMs: g.ExpiresAtMs,
			})
		}
	}
	if len(from) == 0 {
		return Effective{}
	}
	out := Effective{From: from}
	scope := map[string]bool{}
	for _, source := range from {
		if roleRank[source.Role] > roleRank[out.Role] {
			out.Role = source.Role
		}
		if len(source.WorkspaceIDs) == 0 {
			out.Whole = true
		}
		for _, id := range source.WorkspaceIDs {
			scope[id] = true
		}
		if source.ExpiresAtMs != nil && (out.ExpiresAtMs == nil || *source.ExpiresAtMs < *out.ExpiresAtMs) {
			exp := *source.ExpiresAtMs
			out.ExpiresAtMs = &exp
		}
	}
	out.WorkspaceIDs = []string{}
	if !out.Whole {
		for id := range scope {
			out.WorkspaceIDs = append(out.WorkspaceIDs, id)
		}
		sort.Strings(out.WorkspaceIDs)
	}
	return out
}

func sortedUnique(ids []string) []string {
	out := append([]string{}, ids...)
	sort.Strings(out)
	return slices.Compact(out)
}

// Narrowed 报告 after 相对 before 是不是收窄：由有变无、角色降低、范围变小或到期提前（设计 §4.8：任何收窄都撤销
// 已签的断言与令牌；放宽不撤）。
func Narrowed(before, after Effective) bool {
	if before.None() {
		return false
	}
	if after.None() || roleRank[after.Role] < roleRank[before.Role] {
		return true
	}
	if !after.Whole {
		if before.Whole {
			return true
		}
		for _, id := range before.WorkspaceIDs {
			if !slices.Contains(after.WorkspaceIDs, id) {
				return true
			}
		}
	}
	switch {
	case after.ExpiresAtMs == nil:
		return false
	case before.ExpiresAtMs == nil:
		return true
	}
	return *after.ExpiresAtMs < *before.ExpiresAtMs
}

// ScopeClaims 是断言的 scp：整台主机不带，否则每个工作空间一项 ws:<id>。
func (e Effective) ScopeClaims() []string {
	if e.Whole {
		return nil
	}
	out := make([]string, 0, len(e.WorkspaceIDs))
	for _, id := range e.WorkspaceIDs {
		out = append(out, "ws:"+id)
	}
	return out
}
