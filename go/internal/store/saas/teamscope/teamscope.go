// Package teamscope 定义 TeamScope：「已核过成员与角色」的团队范围。
//
// 字段不导出，只有 Issue 能构造；团队范围的仓储方法只收 Scope，不收裸 teamID，
// 跨团队读取在编译期就写不出来。Issue 只应由 cloud/teams 的 Require 调用（核过成员与角色之后），
// 所以这个包单独成包：别的包 import 它就是在越界，评审与 repo-check 都盯这一条。
package teamscope

// Role 是团队内的角色（能力设计 §1）。
type Role string

// 团队角色。
const (
	Owner   Role = "owner"
	Admin   Role = "admin"
	Member  Role = "member"
	Billing Role = "billing"
	Guest   Role = "guest"
)

// Valid 报告 r 是不是五个角色之一。
func (r Role) Valid() bool {
	switch r {
	case Owner, Admin, Member, Billing, Guest:
		return true
	}
	return false
}

// Scope 是已核过成员与角色的团队范围。零值不可用（TeamID 为空）。
type Scope struct {
	tenantID string
	teamID   string
	role     Role
}

// Issue 构造一个 Scope。调用方必须已经核过账号是该团队的成员且角色为 role；
// 只有 cloud/teams 的 Require 可以调用。
func Issue(tenantID, teamID string, role Role) Scope {
	return Scope{tenantID: tenantID, teamID: teamID, role: role}
}

// TenantID 是存储租户。
func (s Scope) TenantID() string { return s.tenantID }

// TeamID 是团队 id。
func (s Scope) TeamID() string { return s.teamID }

// Role 是调用者在该团队里的角色。
func (s Scope) Role() Role { return s.role }

// IsZero 报告是不是没经 Issue 的零值。
func (s Scope) IsZero() bool { return s.teamID == "" }
