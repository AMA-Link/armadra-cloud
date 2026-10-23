package postgres_test

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas/teamscope"
)

func scopeOf(team string, role saas.TeamRole) saas.TeamScope {
	return teamscope.Issue(saasTenant, team, role)
}

func newTeam(id string) saas.Team {
	return saas.Team{
		TeamID: id, Slug: id, Name: "团队 " + id, Settings: json.RawMessage(`{"allowGuests":true}`), CreatedBy: "a1", CreatedAtMs: 10, UpdatedAtMs: 10,
	}
}

// 团队范围的方法只看自己的团队：用 A 的范围读不到 B 的成员，也改不了 B 的行。
func TestTeamRepoIsScopedToOneTeam(t *testing.T) {
	e := openSaaS(t)
	for _, id := range []string{"a1", "a2", "a3"} {
		e.account(id)
	}
	s := e.db.SaaS()
	err := s.Tx(e.ctx, func(tx saas.Tx) error {
		if err := tx.Teams().Create(e.ctx, saasTenant, newTeam("ta"), "a1"); err != nil {
			return err
		}
		return tx.Teams().Create(e.ctx, saasTenant, newTeam("tb"), "a2")
	})
	if err != nil {
		t.Fatal(err)
	}
	a := scopeOf("ta", teamscope.Owner)
	err = s.Tx(e.ctx, func(tx saas.Tx) error {
		if m, err := tx.Teams().Member(e.ctx, a, "a2"); err != nil || m != nil {
			t.Fatalf("A 的范围不该读到 B 的成员：%+v %v", m, err)
		}
		if err := tx.Teams().SetRole(e.ctx, a, "a2", teamscope.Guest); err != nil { // 影响 0 行
			return err
		}
		if err := tx.Teams().RemoveMember(e.ctx, a, "a2"); err != nil {
			return err
		}
		if err := tx.Teams().Rename(e.ctx, a, "改名", nil, 11); err != nil {
			return err
		}
		return tx.Teams().AddMember(e.ctx, a, "a3", teamscope.Member, "a1", 12)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := e.count("SELECT count(*)::int FROM team_members WHERE team_id = 'tb' AND role = 'owner'"); got != 1 {
		t.Fatal("B 的 owner 不该被 A 的范围动到")
	}
	if got := e.count("SELECT count(*)::int FROM teams WHERE team_id = 'tb' AND name = '团队 tb'"); got != 1 {
		t.Fatal("B 的名字不该被改")
	}
	members, err := s.Read().TeamsOf(e.ctx, saasTenant, "a3")
	if err != nil || len(members) != 1 || members[0].Team.TeamID != "ta" || members[0].Role != teamscope.Member || members[0].Plan != "free" {
		t.Fatalf("a3 的团队：%+v %v", members, err)
	}
	access, err := s.Read().TeamAccess(e.ctx, saasTenant, "tb", "a3")
	if err != nil || access != nil {
		t.Fatalf("a3 不是 B 的成员：%+v %v", access, err)
	}
	// slug 唯一冲突映射成 ErrTeamSlugTaken。
	err = s.Tx(e.ctx, func(tx saas.Tx) error {
		dup := newTeam("tc")
		dup.Slug = "ta"
		return tx.Teams().Create(e.ctx, saasTenant, dup, "a3")
	})
	if !errors.Is(err, saas.ErrTeamSlugTaken) {
		t.Fatalf("slug 冲突：%v", err)
	}
}

// 不经服务层的行锁，只靠触发器：两个事务各降一个 owner，恰好一个提交失败（映射成 ErrTeamLastOwner）。
func TestTeamRepoLastOwnerGuardUnderConcurrency(t *testing.T) {
	e := openSaaS(t)
	e.account("a1")
	e.account("a2")
	s := e.db.SaaS()
	if err := s.Tx(e.ctx, func(tx saas.Tx) error {
		if err := tx.Teams().Create(e.ctx, saasTenant, newTeam("t1"), "a1"); err != nil {
			return err
		}
		return tx.Teams().AddMember(e.ctx, scopeOf("t1", teamscope.Owner), "a2", teamscope.Owner, "a1", 11)
	}); err != nil {
		t.Fatal(err)
	}
	scope := scopeOf("t1", teamscope.Owner)
	holds := []*held{
		hold(t, e, func(tx saas.Tx) error { return tx.Teams().SetRole(e.ctx, scope, "a1", teamscope.Admin) }),
		hold(t, e, func(tx saas.Tx) error { return tx.Teams().SetRole(e.ctx, scope, "a2", teamscope.Admin) }),
	}
	errs := make([]error, len(holds))
	var wg sync.WaitGroup
	for i, h := range holds {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = h.commit() }()
	}
	wg.Wait()
	failed := 0
	for _, err := range errs {
		if err != nil {
			if !errors.Is(err, saas.ErrTeamLastOwner) {
				t.Fatalf("应映射成 team_last_owner：%v", err)
			}
			failed++
		}
	}
	if failed != 1 || e.count("SELECT count(*)::int FROM team_members WHERE role = 'owner'") != 1 {
		t.Fatalf("应恰好一个失败、剩一个 owner：%v", errs)
	}
}

// 加锁顺序：先锁了账号再锁团队会死锁，端口直接拒绝。
func TestTeamLockBeforeAccountOrder(t *testing.T) {
	e := openSaaS(t)
	e.account("a1")
	s := e.db.SaaS()
	if err := s.Tx(e.ctx, func(tx saas.Tx) error { return tx.Teams().Create(e.ctx, saasTenant, newTeam("t1"), "a1") }); err != nil {
		t.Fatal(err)
	}
	err := s.Tx(e.ctx, func(tx saas.Tx) error {
		if _, err := tx.Accounts().LockAccount(e.ctx, saasTenant, "a1"); err != nil {
			return err
		}
		_, err := tx.Teams().LockTeam(e.ctx, saasTenant, "t1")
		return err
	})
	if !errors.Is(err, saas.ErrLockOrder) {
		t.Fatalf("先账号后团队应报 ErrLockOrder：%v", err)
	}
	if err := s.Tx(e.ctx, func(tx saas.Tx) error {
		if _, err := tx.Teams().LockTeam(e.ctx, saasTenant, "t1"); err != nil {
			return err
		}
		_, err := tx.Accounts().LockAccount(e.ctx, saasTenant, "a1")
		return err
	}); err != nil {
		t.Fatalf("先团队后账号应通过：%v", err)
	}
}

// 清除只在恢复期已满时动手，并清走子行；审计行保留。
func TestTeamPurgeOnlyWhenDue(t *testing.T) {
	e := openSaaS(t)
	e.account("a1")
	e.source("s1")
	s := e.db.SaaS()
	scope := scopeOf("t1", teamscope.Owner)
	if err := s.Tx(e.ctx, func(tx saas.Tx) error {
		if err := tx.Teams().Create(e.ctx, saasTenant, newTeam("t1"), "a1"); err != nil {
			return err
		}
		return tx.Teams().SetDeleted(e.ctx, scope, 20, 100)
	}); err != nil {
		t.Fatal(err)
	}
	e.must(`INSERT INTO source_spaces (tenant_id, source_id, team_id, owner_account_id, created_at_ms, updated_at_ms) VALUES ($1, 's1', 't1', 'a1', 1, 1)`, saasTenant)
	e.must(`INSERT INTO source_grants (tenant_id, grant_id, source_id, team_id, subject_kind, subject_id, role, granted_by, created_at_ms, updated_at_ms)
	  VALUES ($1, 'g1', 's1', 't1', 'team', 't1', 'viewer', 'a1', 1, 1)`, saasTenant)
	e.must(`INSERT INTO audit_log (at_ms, event_id, tenant_id, team_id, actor_kind, action) VALUES ($1, 'e1', $2, 't1', 'system', 'team.create')`,
		time0(), saasTenant)

	var purged bool
	var sources []string
	purge := func(now int64) error {
		return s.Tx(e.ctx, func(tx saas.Tx) (err error) {
			purged, sources, err = tx.Teams().Purge(e.ctx, saasTenant, "t1", now)
			return err
		})
	}
	if err := purge(99); err != nil || purged {
		t.Fatalf("恢复期未满不该清除：%v %v", purged, err)
	}
	due, err := s.Read().DueTeams(e.ctx, saasTenant, 100, 10)
	if err != nil || len(due) != 1 || due[0] != "t1" {
		t.Fatalf("到点的团队：%v %v", due, err)
	}
	if err := purge(100); err != nil || !purged || len(sources) != 1 || sources[0] != "s1" {
		t.Fatalf("清除：%v %v %v", purged, sources, err)
	}
	for _, table := range []string{"teams", "team_members", "source_spaces", "source_grants", "entitlements"} {
		if n := e.count("SELECT count(*)::int FROM " + table); n != 0 {
			t.Fatalf("%s 里还有 %d 行", table, n)
		}
	}
	if e.count("SELECT count(*)::int FROM sources WHERE source_id = 's1' AND revoked_at_ms = 100") != 1 {
		t.Fatal("主机应被撤销")
	}
	if e.count("SELECT count(*)::int FROM audit_log WHERE team_id = 't1'") != 1 {
		t.Fatal("审计行应保留")
	}
	// 重放：团队已经没了，答 false 不报错。
	if err := purge(200); err != nil || purged {
		t.Fatalf("重放：%v %v", purged, err)
	}
}

func time0() int64 { return 1_700_000_000_000 }

// 邀请仓储：过期的 pending 先落成 expired 才不挡新邀请；待处理邀请计入额度；重发清令牌。
func TestInvitationRepo(t *testing.T) {
	e := openSaaS(t)
	e.account("a1")
	s := e.db.SaaS()
	scope := scopeOf("t1", teamscope.Owner)
	if err := s.Tx(e.ctx, func(tx saas.Tx) error { return tx.Teams().Create(e.ctx, saasTenant, newTeam("t1"), "a1") }); err != nil {
		t.Fatal(err)
	}
	invite := func(tx saas.Tx, id, email string, role saas.TeamRole, expires int64) error {
		return tx.Invitations().Insert(e.ctx, scope, saas.Invitation{
			InvitationID: id, Email: email, TeamRole: role, InvitedBy: "a1", CreatedAtMs: 100, ExpiresAtMs: expires,
		})
	}
	if err := s.Tx(e.ctx, func(tx saas.Tx) error {
		if err := invite(tx, "i1", "Bob@Example.com", teamscope.Member, 1000); err != nil {
			return err
		}
		return invite(tx, "i2", "carol@example.com", teamscope.Guest, 5000)
	}); err != nil {
		t.Fatal(err)
	}
	// 同团队同邮箱已有 pending：冲突（邮箱不分大小写）。
	err := s.Tx(e.ctx, func(tx saas.Tx) error { return invite(tx, "i3", "bob@example.com", teamscope.Member, 9000) })
	if !errors.Is(err, saas.ErrConflict) {
		t.Fatalf("重复 pending：%v", err)
	}
	// i1 到期后：先 ExpireStale 再插就成功。
	if err := s.Tx(e.ctx, func(tx saas.Tx) error {
		if err := tx.Invitations().ExpireStale(e.ctx, scope, "BOB@example.com", 2000); err != nil {
			return err
		}
		return invite(tx, "i3", "bob@example.com", teamscope.Member, 9000)
	}); err != nil {
		t.Fatalf("过期后重新邀请：%v", err)
	}
	got, err := s.Read().Invitation(e.ctx, saasTenant, "i1")
	if err != nil || got == nil || got.State != saas.InvitationExpired || got.Email != "bob@example.com" || got.DecidedAtMs == nil {
		t.Fatalf("i1：%+v %v", got, err)
	}
	// 待处理的邀请计入额度：i3（member）与 i2（guest）。
	var pending saas.Seats
	if err := s.Tx(e.ctx, func(tx saas.Tx) (err error) {
		pending, err = tx.Invitations().PendingSeats(e.ctx, scope, 2000)
		return err
	}); err != nil || pending != (saas.Seats{Members: 1, Guests: 1}) {
		t.Fatalf("待处理计数：%+v %v", pending, err)
	}
	// 重发：新有效期、令牌哈希清空、次数加一；发信作业写回哈希。
	if err := s.Tx(e.ctx, func(tx saas.Tx) error {
		if err := tx.Invitations().SetToken(e.ctx, saasTenant, "i3", "hash-1", 2100); err != nil {
			return err
		}
		return tx.Invitations().Reissue(e.ctx, saasTenant, "i3", 20000)
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Read().Invitation(e.ctx, saasTenant, "i3")
	if got.TokenHash != "" || got.SentAtMs != nil || got.ResendCount != 1 || got.ExpiresAtMs != 20000 {
		t.Fatalf("重发后：%+v", got)
	}
	// 收件箱：只列发给这个邮箱、待处理、没过期的。
	received, err := s.Read().ReceivedInvitations(e.ctx, saasTenant, "Bob@example.com", 2000)
	if err != nil || len(received) != 1 || received[0].InvitationID != "i3" {
		t.Fatalf("收件箱：%+v %v", received, err)
	}
	// 到期落库：i2 在 6000 过期。
	var expired []saas.Invitation
	if err := s.Tx(e.ctx, func(tx saas.Tx) (err error) {
		expired, err = tx.Invitations().ExpireDue(e.ctx, saasTenant, 6000)
		return err
	}); err != nil || len(expired) != 1 || expired[0].InvitationID != "i2" {
		t.Fatalf("到期落库：%+v %v", expired, err)
	}
}
