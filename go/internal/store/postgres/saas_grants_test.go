package postgres_test

import (
	"errors"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas/teamscope"
)

func (e *saasEnv) grant(id, source, team, kind, subject, role string, workspaces []string, expires *int64) {
	e.t.Helper()
	err := e.db.SaaS().Tx(e.ctx, func(tx saas.Tx) error {
		return tx.Grants().Insert(e.ctx, saasTenant, saas.Grant{
			GrantID: id, SourceID: source, TeamID: team, SubjectKind: kind, SubjectID: subject, Role: role,
			WorkspaceIDs: workspaces, ExpiresAtMs: expires, GrantedBy: "granter", Origin: saas.GrantOriginGrant, CreatedAtMs: 10,
		})
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func ptr(v int64) *int64 { return &v }

// 授予带来的可见性：member / team / guest 授予，到期与撤销，角色限制（billing 看不到、guest 角色只看 guest 授予）。
func TestListHostsGrantVisibility(t *testing.T) {
	e := openSaaS(t)
	for _, id := range []string{"owner", "admin", "member", "other", "billing", "guest", "stranger", "friend"} {
		e.account(id)
	}
	e.team("t1", "owner")
	e.members("t1", map[string]string{"admin": "admin", "member": "member", "other": "member", "billing": "billing", "guest": "guest"})
	e.seedHost("p1", "", "owner", 10)
	e.seedHost("t1-own", "t1", "owner", 20)
	e.seedHost("t1-m", "t1", "member", 30)
	e.seedHost("t1-o", "t1", "other", 40)
	r := e.db.SaaS().Read()
	list := func(viewer string, nowMs int64) []saas.Host {
		t.Helper()
		hosts, err := r.ListHosts(e.ctx, saasTenant, saas.HostFilter{ViewerID: viewer, NowMs: nowMs})
		if err != nil {
			t.Fatal(err)
		}
		return hosts
	}
	want := func(viewer string, nowMs int64, ids ...string) {
		t.Helper()
		if got := list(viewer, nowMs); !sameIDs(got, ids...) {
			t.Errorf("%s @%d 看到 %v，应为 %v", viewer, nowMs, hostIDs(got), ids)
		}
	}

	// 个人空间：guest 授予让陌生人看到；到期后消失；撤销后消失。
	e.grant("g-p", "p1", "", saas.GrantSubjectGuest, "friend", "viewer", nil, ptr(1000))
	want("friend", 999, "p1")
	want("friend", 1000) // expires_at_ms ≤ now 即失效
	want("stranger", 1)
	e.must("UPDATE source_grants SET revoked_at_ms = 5 WHERE grant_id = 'g-p'")
	want("friend", 1)

	// 团队主机：member 授予（同一个团队的成员）、team 授予（团队全员里的 owner / admin / member）。
	e.grant("g-m", "t1-own", "t1", saas.GrantSubjectMember, "other", "operator", nil, nil)
	e.grant("g-t", "t1-o", "t1", saas.GrantSubjectTeam, "t1", "viewer", []string{"w1"}, nil)
	want("other", 1, "t1-own", "t1-o")
	want("member", 1, "t1-m", "t1-o")          // team 授予到全员（含 member）
	want("admin", 1, "t1-own", "t1-m", "t1-o") // admin 本来就看全部
	want("billing", 1)                         // billing 不受 team 授予
	want("guest", 1)                           // guest 角色不受 team / member 授予
	e.grant("g-g", "t1-own", "t1", saas.GrantSubjectGuest, "guest", "viewer", nil, ptr(50))
	want("guest", 49, "t1-own")
	want("guest", 50)
	// guest 授予不给 member 角色的成员（角色变了授予不再算）。
	e.grant("g-g2", "t1-m", "t1", saas.GrantSubjectGuest, "member", "viewer", nil, nil)
	want("member", 1, "t1-m", "t1-o")
	// 团队进入恢复期：授予也不让团队主机可见，个人空间的 guest 授予不受影响。
	e.grant("g-p2", "p1", "", saas.GrantSubjectGuest, "friend", "viewer", nil, nil)
	e.must("UPDATE teams SET deleted_at_ms = 1, purge_at_ms = 2 WHERE team_id = 't1'")
	want("other", 1)
	want("friend", 1, "p1")
	// 不是成员的人拿到 member 授予也看不到（成员行删了，授予行还在）。
	e.must("UPDATE teams SET deleted_at_ms = NULL, purge_at_ms = NULL WHERE team_id = 't1'")
	e.must("DELETE FROM team_members WHERE account_id = 'other'")
	want("other", 1)
}

func TestAccessBasisAndGrantRepo(t *testing.T) {
	e := openSaaS(t)
	for _, id := range []string{"owner", "u1", "u2"} {
		e.account(id)
	}
	e.team("t1", "owner")
	e.members("t1", map[string]string{"u1": "member"})
	e.seedHost("h1", "t1", "owner", 10)
	e.seedHost("p1", "", "owner", 10)
	s := e.db.SaaS()
	r := s.Read()

	basis, err := r.AccessBasis(e.ctx, saasTenant, "u1", "h1")
	if err != nil || !basis.SourceFound || !basis.AccountActive || basis.TeamID != "t1" || basis.TeamRole != teamscope.Member ||
		basis.OwnerID != "owner" || basis.CoreVersion != "0.10.0" || len(basis.Grants) != 0 {
		t.Fatalf("事实：%+v %v", basis, err)
	}
	if b, _ := r.AccessBasis(e.ctx, saasTenant, "u1", "ghost"); b.SourceFound {
		t.Fatal("没有的主机")
	}
	if b, _ := r.AccessBasis(e.ctx, saasTenant, "nobody", "h1"); b.AccountActive || b.TeamRole != "" {
		t.Fatalf("没有的账号：%+v", b)
	}

	e.grant("gm", "h1", "t1", saas.GrantSubjectMember, "u1", "editor", []string{"w2", "w1"}, nil)
	e.grant("gt", "h1", "t1", saas.GrantSubjectTeam, "t1", "viewer", nil, ptr(100))
	e.grant("gx", "h1", "t1", saas.GrantSubjectMember, "owner", "viewer", nil, nil) // 别人的：不进 u1 的事实
	basis, _ = r.AccessBasis(e.ctx, saasTenant, "u1", "h1")
	if len(basis.Grants) != 2 || basis.Grants[0].GrantID != "gm" || basis.Grants[1].GrantID != "gt" {
		t.Fatalf("适用的授予：%+v", basis.Grants)
	}
	e.must("UPDATE accounts SET status = 'disabled' WHERE account_id = 'u1'")
	if b, _ := r.AccessBasis(e.ctx, saasTenant, "u1", "h1"); b.AccountActive {
		t.Fatal("停用的账号")
	}
	e.must("UPDATE sources SET revoked_at_ms = 9 WHERE source_id = 'p1'")
	if b, _ := r.AccessBasis(e.ctx, saasTenant, "owner", "p1"); !b.SourceRevoked || b.TeamID != "" {
		t.Fatalf("撤销的个人主机：%+v", b)
	}

	// 访问路径上的账号：所有者、授予的主体、团队全体成员。
	accounts, err := r.AccessAccounts(e.ctx, saasTenant, "h1")
	if err != nil || len(accounts) != 2 || accounts[0] != "owner" || accounts[1] != "u1" {
		t.Fatalf("账号：%v %v", accounts, err)
	}
	if ids, _ := r.TeamAccountIDs(e.ctx, saasTenant, "t1"); len(ids) != 2 {
		t.Fatalf("团队成员：%v", ids)
	}

	// 仓储：同主体重复建答冲突；Update 只改给了的项；Revoke / RevokeAccount / DueGrants。
	err = s.Tx(e.ctx, func(tx saas.Tx) error {
		return tx.Grants().Insert(e.ctx, saasTenant, saas.Grant{
			GrantID: "dup", SourceID: "h1", TeamID: "t1", SubjectKind: saas.GrantSubjectMember, SubjectID: "u1", Role: "viewer",
			GrantedBy: "x", Origin: saas.GrantOriginGrant, CreatedAtMs: 1,
		})
	})
	if !errors.Is(err, saas.ErrConflict) {
		t.Fatalf("重复的授予：%v", err)
	}
	role := "driver"
	err = s.Tx(e.ctx, func(tx saas.Tx) error {
		ok, err := tx.Grants().Update(e.ctx, saasTenant, "gm", saas.GrantPatch{Role: &role, ExpiresAtMs: ptr(77)}, 20)
		if err != nil || !ok {
			t.Fatalf("改：%v %v", ok, err)
		}
		if ok, _ := tx.Grants().Update(e.ctx, saasTenant, "gt", saas.GrantPatch{ClearExpiry: true}, 21); !ok {
			t.Fatal("清到期")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	g, _ := r.Grant(e.ctx, saasTenant, "gm")
	if g.Role != "driver" || *g.ExpiresAtMs != 77 || len(g.WorkspaceIDs) != 2 || g.UpdatedAtMs != 20 {
		t.Fatalf("改后：%+v", g)
	}
	if g, _ := r.Grant(e.ctx, saasTenant, "gt"); g.ExpiresAtMs != nil {
		t.Fatalf("到期没清：%+v", g)
	}
	due, _ := r.DueGrants(e.ctx, saasTenant, 77, 10)
	if len(due) != 1 || due[0] != "gm" {
		t.Fatalf("到期的：%v", due)
	}
	err = s.Tx(e.ctx, func(tx saas.Tx) error {
		ids, err := tx.Grants().RevokeAccount(e.ctx, saasTenant, "h1", "u1", "boss", 30)
		if err != nil || len(ids) != 1 || ids[0] != "gm" {
			t.Fatalf("按账号撤：%v %v", ids, err)
		}
		if ok, _ := tx.Grants().Revoke(e.ctx, saasTenant, "gt", saas.GrantRevokedBySystem, 31); !ok {
			t.Fatal("撤销")
		}
		if ok, _ := tx.Grants().Revoke(e.ctx, saasTenant, "gt", "x", 32); ok {
			t.Fatal("已撤销的不再撤")
		}
		if ok, _ := tx.Grants().Update(e.ctx, saasTenant, "gt", saas.GrantPatch{Role: &role}, 33); ok {
			t.Fatal("已撤销的不能改")
		}
		locked, err := tx.Grants().Lock(e.ctx, saasTenant, "gt")
		if err != nil || locked == nil || locked.RevokedBy != saas.GrantRevokedBySystem || *locked.RevokedAtMs != 31 {
			t.Fatalf("加锁读：%+v %v", locked, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	active, _ := r.ListGrants(e.ctx, saasTenant, saas.GrantQuery{SourceID: "h1"})
	if len(active) != 1 || active[0].GrantID != "gx" {
		t.Fatalf("没撤销的：%+v", active)
	}
	if byAccount, _ := r.ListGrants(e.ctx, saasTenant, saas.GrantQuery{AccountID: "owner"}); len(byAccount) != 1 {
		t.Fatalf("按账号列：%+v", byAccount)
	}
}

// 签发索引：撤销按过滤选出、写撤销表、从索引删；查询与清理带租户；前缀里的通配符按字面。
func TestIssuedRepo(t *testing.T) {
	e := openSaaS(t)
	s := e.db.SaaS()
	put := func(tenant, jti, account, source, device string, exp int64) {
		t.Helper()
		e.must(`INSERT INTO cloud_issued_tokens (tenant_id, jti, account_id, source_id, device_id, exp_ms) VALUES ($1, $2, $3, $4, $5, $6)`,
			tenant, jti, account, source, device, exp)
	}
	put(saasTenant, "j1", "a1", "s1", "d1", 1000)
	put(saasTenant, "j2", "a1", "s1", "d2", 1000)
	put(saasTenant, "j3", "a1", "s2", "d1", 1000)
	put(saasTenant, "j4", "a2", "s1", "d1", 1000)
	put(saasTenant, "j5", "guest:L1:x", "s1", "d9", 1000)
	put(saasTenant, "j6", "a1", "s1", "d1", 50) // 已到期
	put("other", "j7", "a1", "s1", "d1", 1000)  // 别的租户
	revoke := func(f saas.IssuedFilter) []string {
		t.Helper()
		var jtis []string
		err := s.Tx(e.ctx, func(tx saas.Tx) error {
			hit, err := tx.Issued().Revoke(e.ctx, saasTenant, f, 100)
			for _, h := range hit {
				jtis = append(jtis, h.JTI)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return jtis
	}
	if got := revoke(saas.IssuedFilter{}); len(got) != 0 {
		t.Fatalf("全空的过滤不选任何令牌：%v", got)
	}
	if got := revoke(saas.IssuedFilter{AccountID: "a1", SourceID: "s1", KeepDeviceID: "d2"}); len(got) != 1 || got[0] != "j1" {
		t.Fatalf("账号 ∩ 源，除一台设备：%v", got)
	}
	if got := revoke(saas.IssuedFilter{AccountPrefix: "guest:L1:"}); len(got) != 1 || got[0] != "j5" {
		t.Fatalf("前缀：%v", got)
	}
	if got := revoke(saas.IssuedFilter{AccountPrefix: "%"}); len(got) != 0 {
		t.Fatalf("通配符按字面：%v", got)
	}
	if got := revoke(saas.IssuedFilter{JTI: "j4"}); len(got) != 1 {
		t.Fatalf("按 jti：%v", got)
	}
	// 其余：撤整个源，不碰别的租户，不撤已到期的。
	if got := revoke(saas.IssuedFilter{SourceID: "s1"}); len(got) != 1 || got[0] != "j2" {
		t.Fatalf("撤整个源：%v", got)
	}
	if n := e.count("SELECT count(*)::int FROM relay_revoked_tokens"); n != 4 {
		t.Fatalf("撤销表：%d", n)
	}
	if n := e.count("SELECT count(*)::int FROM cloud_issued_tokens WHERE tenant_id = 'other'"); n != 1 {
		t.Fatalf("别的租户被动了：%d", n)
	}
	var active, replay []saas.IssuedToken
	var gc int
	err := s.Tx(e.ctx, func(tx saas.Tx) error {
		var err error
		if active, err = tx.Issued().Active(e.ctx, saasTenant, 100); err != nil {
			return err
		}
		if replay, err = tx.Issued().Revoked(e.ctx, 100); err != nil {
			return err
		}
		if err := tx.Issued().Insert(e.ctx, saasTenant, saas.IssuedToken{JTI: "j3", AccountID: "zz", ExpMs: 5}); err != nil {
			return err // 同 jti 已有则不动
		}
		gc, err = tx.Issued().GC(e.ctx, saasTenant, 2000)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].JTI != "j3" || active[0].AccountID != "a1" {
		t.Fatalf("索引里未到期的：%+v", active)
	}
	if len(replay) != 4 {
		t.Fatalf("撤销表未到期的：%+v", replay)
	}
	// GC：本租户的 j3 与 j6，撤销表的 4 行；别的租户的 j7 不删。
	if gc != 6 || e.count("SELECT count(*)::int FROM cloud_issued_tokens") != 1 {
		t.Fatalf("清理：%d", gc)
	}
}
