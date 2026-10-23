package grants_test

import (
	"reflect"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/grants"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

const now = int64(1_000)

func ptr(v int64) *int64 { return &v }

func grant(id, kind, subject, role string, ws []string, exp *int64) saas.Grant {
	return saas.Grant{GrantID: id, SourceID: "h", TeamID: "t1", SubjectKind: kind, SubjectID: subject, Role: role, WorkspaceIDs: ws, ExpiresAtMs: exp}
}

func base(teamRole saas.TeamRole, gs ...saas.Grant) saas.AccessBasis {
	return saas.AccessBasis{
		AccountActive: true, SourceFound: true, OwnerID: "someone", TeamID: "t1", TeamRole: teamRole, Grants: gs,
	}
}

// 主体 × 团队角色 × 授予种类：谁能拿到什么来路。
func TestComposePrincipalsByTeamRole(t *testing.T) {
	roles := []saas.TeamRole{saas.RoleOwner, saas.RoleAdmin, saas.RoleMember, saas.RoleBilling, saas.RoleGuest, ""}
	cases := []struct {
		name  string
		grant saas.Grant
		// via[角色] 空 = 没有访问权
		want map[saas.TeamRole]string
	}{
		{"没有授予", saas.Grant{}, map[saas.TeamRole]string{}},
		{"team 授予（团队全员）", grant("g", "team", "t1", "viewer", nil, nil), map[saas.TeamRole]string{
			saas.RoleOwner: "team", saas.RoleAdmin: "team", saas.RoleMember: "team"}},
		{"member 授予（给本人）", grant("g", "member", "me", "viewer", nil, nil), map[saas.TeamRole]string{
			saas.RoleOwner: "member", saas.RoleAdmin: "member", saas.RoleMember: "member"}},
		{"别人的 member 授予", grant("g", "member", "other", "viewer", nil, nil), map[saas.TeamRole]string{}},
		{"guest 授予（只给 guest 角色）", grant("g", "guest", "me", "viewer", nil, nil), map[saas.TeamRole]string{saas.RoleGuest: "guest"}},
		{"别的团队的 team 授予", grant("g", "team", "t2", "viewer", nil, nil), map[saas.TeamRole]string{}},
		{"分组授予（M2）不算", grant("g", "group", "grp", "viewer", nil, nil), map[saas.TeamRole]string{}},
	}
	for _, c := range cases {
		for _, role := range roles {
			var gs []saas.Grant
			if c.grant.GrantID != "" {
				gs = append(gs, c.grant)
			}
			got := grants.Compose(base(role, gs...), "me", now)
			via := ""
			if !got.None() {
				via = got.From[0].Via
			}
			if want := c.want[role]; via != want {
				t.Errorf("%s / 团队角色 %q：来路 %q，应为 %q", c.name, role, via, want)
			}
		}
	}
}

// 主机所有者：个人空间恒为 driver 整台；团队主机还要仍是 owner / admin / member。
func TestComposeOwner(t *testing.T) {
	personal := saas.AccessBasis{AccountActive: true, SourceFound: true, OwnerID: "me"}
	got := grants.Compose(personal, "me", now)
	if got.Role != "driver" || !got.Whole || !got.Owner() || got.GuestOnly() || got.ExpiresAtMs != nil {
		t.Fatalf("个人空间所有者：%+v", got)
	}
	for role, want := range map[saas.TeamRole]bool{
		saas.RoleOwner: true, saas.RoleAdmin: true, saas.RoleMember: true, saas.RoleBilling: false, saas.RoleGuest: false, "": false,
	} {
		b := base(role)
		b.OwnerID = "me"
		if got := !grants.Compose(b, "me", now).None(); got != want {
			t.Errorf("团队主机的所有者，团队角色 %q：%v，应为 %v", role, got, want)
		}
	}
}

// 前置条件：账号停用、主机撤销、没有空间行、团队删除一律无。
func TestComposeGates(t *testing.T) {
	owner := saas.AccessBasis{AccountActive: true, SourceFound: true, OwnerID: "me"}
	gates := map[string]func(saas.AccessBasis) saas.AccessBasis{
		"账号停用":   func(b saas.AccessBasis) saas.AccessBasis { b.AccountActive = false; return b },
		"主机撤销":   func(b saas.AccessBasis) saas.AccessBasis { b.SourceRevoked = true; return b },
		"没有这台主机": func(b saas.AccessBasis) saas.AccessBasis { b.SourceFound = false; return b },
	}
	for name, mutate := range gates {
		if got := grants.Compose(mutate(owner), "me", now); !got.None() {
			t.Errorf("%s：%+v", name, got)
		}
	}
	b := base(saas.RoleMember, grant("g", "team", "t1", "driver", nil, nil))
	if grants.Compose(b, "me", now).None() {
		t.Fatal("基线应有权限")
	}
	b.TeamDeleted = true
	if !grants.Compose(b, "me", now).None() {
		t.Fatal("团队删除后无")
	}
	b.TeamDeleted, b.TeamSuspended = false, true
	if grants.Compose(b, "me", now).None() {
		t.Fatal("团队停用只挡断言，不改有效权限")
	}
}

// 范围取并集、角色不放大（使范围内每处都被 ≥ 它的来路覆盖的最大者）、到期取有贡献来路中最早的非空；已撤销与到期的忽略。
func TestComposeMerge(t *testing.T) {
	revoked := grant("rev", "member", "me", "driver", nil, nil)
	revoked.RevokedAtMs = ptr(1)
	cases := []struct {
		name   string
		grants []saas.Grant
		role   string
		whole  bool
		ws     []string
		exp    *int64
		from   int
	}{
		{"单条整台", []saas.Grant{grant("a", "member", "me", "editor", nil, nil)}, "editor", true, []string{}, nil, 1},
		{"单条限范围", []saas.Grant{grant("a", "member", "me", "editor", []string{"w2", "w1", "w1"}, nil)}, "editor", false, []string{"w1", "w2"}, nil, 1},
		{"driver@w1 + viewer 整台 = viewer 整台（不放大）", []saas.Grant{
			grant("a", "member", "me", "driver", []string{"w1"}, nil), grant("b", "team", "t1", "viewer", nil, nil)}, "viewer", true, []string{}, nil, 2},
		{"driver@w1 + viewer@w2 = viewer {w1,w2}", []saas.Grant{
			grant("a", "member", "me", "driver", []string{"w1"}, nil), grant("b", "team", "t1", "viewer", []string{"w2"}, nil)}, "viewer", false, []string{"w1", "w2"}, nil, 2},
		{"editor@w1 + driver@w1 = driver {w1}", []saas.Grant{
			grant("a", "member", "me", "editor", []string{"w1"}, nil), grant("b", "team", "t1", "driver", []string{"w1"}, nil)}, "driver", false, []string{"w1"}, nil, 2},
		{"driver 整台 + viewer@w1 = driver 整台", []saas.Grant{
			grant("a", "member", "me", "driver", nil, nil), grant("b", "team", "t1", "viewer", []string{"w1"}, nil)}, "driver", true, []string{}, nil, 2},
		{"整台取整台来路中最高的", []saas.Grant{
			grant("a", "member", "me", "operator", nil, nil), grant("b", "team", "t1", "viewer", nil, nil), grant("c", "team", "t1", "driver", []string{"w1"}, nil)}, "operator", true, []string{}, nil, 3},
		{"重叠范围：editor{w1,w2} + driver{w2,w3} = editor {w1,w2,w3}", []saas.Grant{
			grant("a", "member", "me", "editor", []string{"w1", "w2"}, nil), grant("b", "team", "t1", "driver", []string{"w2", "w3"}, nil)}, "editor", false, []string{"w1", "w2", "w3"}, nil, 2},
		{"被舍弃的来路不拖短到期：driver@w1 早到期不影响 viewer 整台", []saas.Grant{
			grant("a", "member", "me", "driver", []string{"w1"}, ptr(3000)), grant("b", "team", "t1", "viewer", nil, ptr(9000))}, "viewer", true, []string{}, ptr(9000), 2},
		{"整台里较低的整台来路到期不参与", []saas.Grant{
			grant("a", "member", "me", "editor", nil, ptr(9000)), grant("b", "team", "t1", "viewer", nil, ptr(3000))}, "editor", true, []string{}, ptr(9000), 2},
		{"范围取并集，角色取各处最低的最高档", []saas.Grant{
			grant("a", "member", "me", "viewer", []string{"w1"}, nil), grant("b", "team", "t1", "operator", []string{"w3", "w2"}, nil)}, "viewer", false, []string{"w1", "w2", "w3"}, nil, 2},
		{"到期取最早的非空", []saas.Grant{
			grant("a", "member", "me", "viewer", nil, ptr(5000)), grant("b", "team", "t1", "viewer", nil, ptr(3000)), grant("c", "team", "t1", "viewer", nil, nil)}, "viewer", true, []string{}, ptr(3000), 3},
		{"到期（≤ now）与撤销的不算", []saas.Grant{
			grant("a", "member", "me", "driver", nil, ptr(now)), grant("b", "team", "t1", "viewer", nil, ptr(now+1)), revoked}, "viewer", true, []string{}, ptr(now + 1), 1},
		{"全部到期 = 无", []saas.Grant{grant("a", "member", "me", "driver", nil, ptr(now-1))}, "", false, nil, nil, 0},
	}
	for _, c := range cases {
		got := grants.Compose(base(saas.RoleMember, c.grants...), "me", now)
		if got.Role != c.role || got.Whole != c.whole || !reflect.DeepEqual(got.WorkspaceIDs, c.ws) || !reflect.DeepEqual(got.ExpiresAtMs, c.exp) || len(got.From) != c.from {
			t.Errorf("%s：%+v", c.name, got)
		}
	}
}

// 个人空间：只有所有者与 guest 授予；guest 授予要过 role-only 版本门（GuestOnly）。
func TestComposePersonalGuest(t *testing.T) {
	b := saas.AccessBasis{AccountActive: true, SourceFound: true, OwnerID: "boss", Grants: []saas.Grant{
		{GrantID: "g", SourceID: "h", SubjectKind: "guest", SubjectID: "me", Role: "operator", WorkspaceIDs: []string{"w1"}},
		{GrantID: "x", SourceID: "h", SubjectKind: "guest", SubjectID: "other", Role: "driver"},
	}}
	got := grants.Compose(b, "me", now)
	if got.Role != "operator" || got.Whole || !got.GuestOnly() || got.Owner() || !reflect.DeepEqual(got.ScopeClaims(), []string{"ws:w1"}) {
		t.Fatalf("访客：%+v", got)
	}
	b.Grants = append(b.Grants, saas.Grant{GrantID: "g2", SourceID: "h", SubjectKind: "guest", SubjectID: "me", Role: "driver", WorkspaceIDs: []string{"w2"}})
	if two := grants.Compose(b, "me", now); two.Role != "operator" || !reflect.DeepEqual(two.WorkspaceIDs, []string{"w1", "w2"}) {
		t.Fatalf("访客两条限范围：%+v", two)
	}
	b.Grants = b.Grants[:2]
	if !grants.Compose(b, "other", now).GuestOnly() {
		t.Fatal("另一个访客")
	}
	if !grants.Compose(b, "stranger", now).None() {
		t.Fatal("陌生人")
	}
	if grants.Compose(b, "boss", now).GuestOnly() {
		t.Fatal("所有者不是 guest-only")
	}
	if (grants.Effective{Whole: true}).ScopeClaims() != nil {
		t.Fatal("整台主机不带 scp")
	}
}

// 收窄判定：由有变无、降角色、缩范围、到期提前算收窄；放宽与不变不算。
func TestNarrowed(t *testing.T) {
	eff := func(role string, whole bool, ws []string, exp *int64) grants.Effective {
		if ws == nil {
			ws = []string{}
		}
		return grants.Effective{Role: role, Whole: whole, WorkspaceIDs: ws, ExpiresAtMs: exp}
	}
	none := grants.Effective{}
	cases := []struct {
		name          string
		before, after grants.Effective
		want          bool
	}{
		{"本来就没有", none, eff("viewer", true, nil, nil), false},
		{"没有 → 没有", none, none, false},
		{"有 → 无", eff("viewer", true, nil, nil), none, true},
		{"不变", eff("editor", true, nil, nil), eff("editor", true, nil, nil), false},
		{"降角色", eff("driver", true, nil, nil), eff("operator", true, nil, nil), true},
		{"升角色", eff("viewer", true, nil, nil), eff("driver", true, nil, nil), false},
		{"整台 → 限范围", eff("editor", true, nil, nil), eff("editor", false, []string{"w1"}, nil), true},
		{"限范围 → 整台", eff("editor", false, []string{"w1"}, nil), eff("editor", true, nil, nil), false},
		{"范围缩小", eff("editor", false, []string{"w1", "w2"}, nil), eff("editor", false, []string{"w1"}, nil), true},
		{"范围放大", eff("editor", false, []string{"w1"}, nil), eff("editor", false, []string{"w1", "w2"}, nil), false},
		{"范围换了一个", eff("editor", false, []string{"w1"}, nil), eff("editor", false, []string{"w2"}, nil), true},
		{"不过期 → 有到期", eff("editor", true, nil, nil), eff("editor", true, nil, ptr(9)), true},
		{"到期提前", eff("editor", true, nil, ptr(9)), eff("editor", true, nil, ptr(5)), true},
		{"到期推后", eff("editor", true, nil, ptr(5)), eff("editor", true, nil, ptr(9)), false},
		{"driver@w1 + viewer@w2 → 加上 viewer 整台后变 viewer 整台（角色降低）", eff("driver", false, []string{"w1"}, nil), eff("viewer", true, nil, nil), true},
		{"viewer 整台 → driver 整台", eff("viewer", true, nil, nil), eff("driver", true, nil, nil), false},
		{"有到期 → 不过期", eff("editor", true, nil, ptr(5)), eff("editor", true, nil, nil), false},
	}
	for _, c := range cases {
		if got := grants.Narrowed(c.before, c.after); got != c.want {
			t.Errorf("%s：%v，应为 %v", c.name, got, c.want)
		}
	}
}
