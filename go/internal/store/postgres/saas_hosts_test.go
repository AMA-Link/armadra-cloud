package postgres_test

import (
	"fmt"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas/teamscope"
)

func hostRecord(id string, registeredAt int64) store.SourceRecord {
	return store.SourceRecord{
		SourceID: id, Name: "主机 " + id, Kind: "desktop", CoreVersion: "0.10.0", Capabilities: []string{},
		RegisteredAtMs: registeredAt, TrustedOrigins: []string{},
	}
}

// seedHost 在一个 SaaS 事务里写源与空间行（登记事务的形状）。
func (e *saasEnv) seedHost(id, teamID, owner string, registeredAt int64) {
	e.t.Helper()
	err := e.db.SaaS().Tx(e.ctx, func(tx saas.Tx) error {
		if _, err := tx.Ports().Sources.Put(e.ctx, saasTenant, id, func(*store.SourceRecord) (store.SourceRecord, error) {
			return hostRecord(id, registeredAt), nil
		}); err != nil {
			return err
		}
		return tx.Hosts().UpsertSpace(e.ctx, saasTenant, saas.HostSpace{SourceID: id, TeamID: teamID, OwnerAccountID: owner}, registeredAt)
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *saasEnv) members(team string, roles map[string]string) {
	e.t.Helper()
	for account, role := range roles {
		e.must(`INSERT INTO team_members (tenant_id, team_id, account_id, role, joined_at_ms) VALUES ($1, $2, $3, $4, 2)`,
			saasTenant, team, account, role)
	}
}

func hostIDs(hosts []saas.Host) []string {
	out := []string{}
	for _, h := range hosts {
		out = append(out, h.SourceID)
	}
	return out
}

func sameIDs(got []saas.Host, want ...string) bool {
	ids := hostIDs(got)
	if len(ids) != len(want) {
		return false
	}
	for i := range ids {
		if ids[i] != want[i] {
			return false
		}
	}
	return true
}

// 登记事务与延迟外键：Put 是同事务里删了再插，source_spaces 在场时提交照常成功；
// 空间行没有对应的源、或真删源而空间行还在，提交时失败。
func TestHostsDeferredForeignKey(t *testing.T) {
	e := openSaaS(t)
	e.account("a1")
	s := e.db.SaaS()
	e.seedHost("h1", "", "a1", 5)
	// 重新登记：Put 删了再插，期间 source_spaces 暂时悬空，提交时已回来。
	e.seedHost("h1", "", "a1", 6)
	if n := e.count("SELECT count(*)::int FROM source_spaces WHERE source_id = 'h1'"); n != 1 {
		t.Fatalf("重登记后应仍有一行空间：%d", n)
	}
	// 空间行引用不存在的源：提交失败。
	err := s.Tx(e.ctx, func(tx saas.Tx) error {
		return tx.Hosts().UpsertSpace(e.ctx, saasTenant, saas.HostSpace{SourceID: "ghost", OwnerAccountID: "a1"}, 7)
	})
	if err == nil {
		t.Fatal("没有源的空间行应在提交时被拒")
	}
	// 真删源而空间行还在：提交失败。
	if err := e.exec("DELETE FROM sources WHERE source_id = 'h1'"); err == nil {
		t.Fatal("删源而空间行还在应被拒")
	}
}

// 目录的可见性、筛选与游标都在查询里。
func TestListHostsVisibilityAndFilters(t *testing.T) {
	e := openSaaS(t)
	for _, id := range []string{"owner", "admin", "member", "other", "billing", "guest", "stranger"} {
		e.account(id)
	}
	e.team("t1", "owner")
	e.team("t2", "other")
	e.members("t1", map[string]string{"admin": "admin", "member": "member", "other": "member", "billing": "billing", "guest": "guest"})
	e.seedHost("p-owner", "", "owner", 10) // 个人空间
	e.seedHost("t1-owner", "t1", "owner", 20)
	e.seedHost("t1-member", "t1", "member", 30)
	e.seedHost("t1-other", "t1", "other", 40)
	e.seedHost("t2-other", "t2", "other", 50)
	r := e.db.SaaS().Read()
	list := func(f saas.HostFilter) []saas.Host {
		t.Helper()
		hosts, err := r.ListHosts(e.ctx, saasTenant, f)
		if err != nil {
			t.Fatal(err)
		}
		return hosts
	}
	cases := []struct {
		viewer string
		want   []string
	}{
		{"owner", []string{"p-owner", "t1-owner", "t1-member", "t1-other"}}, // owner 看团队全部
		{"admin", []string{"t1-owner", "t1-member", "t1-other"}},            // admin 看团队全部，不含别人的个人空间
		{"member", []string{"t1-member"}},                                   // member 只看自己登记的
		{"other", []string{"t1-other", "t2-other"}},                         // 两个团队各自的
		{"billing", []string{}},                                             // billing / guest 没有授予就看不到
		{"guest", []string{}},
		{"stranger", []string{}},
	}
	for _, c := range cases {
		if got := list(saas.HostFilter{ViewerID: c.viewer}); !sameIDs(got, c.want...) {
			t.Errorf("%s 看到 %v，应为 %v", c.viewer, hostIDs(got), c.want)
		}
	}
	// 查询者的角色与团队缺省角色一并带出。
	for _, h := range list(saas.HostFilter{ViewerID: "admin"}) {
		if h.ViewerRole != teamscope.Admin || h.TeamID != "t1" {
			t.Fatalf("admin 的视角：%+v", h)
		}
	}
	// 按空间、单台、已撤销筛选。
	if got := list(saas.HostFilter{ViewerID: "owner", SpaceKind: saas.SpacePersonal}); !sameIDs(got, "p-owner") {
		t.Fatalf("个人空间：%v", hostIDs(got))
	}
	if got := list(saas.HostFilter{ViewerID: "owner", SpaceKind: saas.SpaceTeam, TeamID: "t1"}); !sameIDs(got, "t1-owner", "t1-member", "t1-other") {
		t.Fatalf("团队空间：%v", hostIDs(got))
	}
	if got := list(saas.HostFilter{ViewerID: "owner", SpaceKind: saas.SpaceTeam, TeamID: "t2"}); len(got) != 0 {
		t.Fatalf("不是 t2 成员：%v", hostIDs(got))
	}
	if got := list(saas.HostFilter{ViewerID: "member", SourceID: "t1-owner"}); len(got) != 0 {
		t.Fatalf("别人的主机取不到：%v", hostIDs(got))
	}
	e.must("UPDATE sources SET revoked_at_ms = 99 WHERE source_id = 't1-member'")
	if got := list(saas.HostFilter{ViewerID: "admin", Revoked: true}); !sameIDs(got, "t1-member") {
		t.Fatalf("已撤销：%v", hostIDs(got))
	}
	if got := list(saas.HostFilter{ViewerID: "admin"}); !sameIDs(got, "t1-owner", "t1-other") {
		t.Fatalf("未撤销：%v", hostIDs(got))
	}
	// 团队进入恢复期：团队主机全部不可见，个人空间的不受影响。
	e.must("UPDATE teams SET deleted_at_ms = 1, purge_at_ms = 2 WHERE team_id = 't1'")
	if got := list(saas.HostFilter{ViewerID: "owner"}); !sameIDs(got, "p-owner") {
		t.Fatalf("恢复期内：%v", hostIDs(got))
	}
}

func TestListHostsTagsQueryAndCursor(t *testing.T) {
	e := openSaaS(t)
	e.account("a1")
	for i := 1; i <= 5; i++ {
		e.seedHost(fmt.Sprintf("h%d", i), "", "a1", int64(i/2*10)) // h2、h3 同一时刻，靠 source_id 定序
	}
	e.must("UPDATE sources SET name = '100%_练习\\机' WHERE source_id = 'h4'")
	e.must("UPDATE source_spaces SET tags = '{gpu,prod}' WHERE source_id IN ('h1', 'h2')")
	e.must("UPDATE source_spaces SET tags = '{gpu}' WHERE source_id = 'h3'")
	r := e.db.SaaS().Read()
	list := func(f saas.HostFilter) []saas.Host {
		t.Helper()
		f.ViewerID = "a1"
		hosts, err := r.ListHosts(e.ctx, saasTenant, f)
		if err != nil {
			t.Fatal(err)
		}
		return hosts
	}
	if got := list(saas.HostFilter{Tags: []string{"gpu", "prod"}}); !sameIDs(got, "h1", "h2") {
		t.Fatalf("标签取且：%v", hostIDs(got))
	}
	if got := list(saas.HostFilter{Tags: []string{"gpu"}}); !sameIDs(got, "h1", "h2", "h3") {
		t.Fatalf("单标签：%v", hostIDs(got))
	}
	// 通配符按字面匹配：% _ \ 都不是通配。
	for query, want := range map[string][]string{"100%": {"h4"}, "%": {"h4"}, "_练": {"h4"}, "\\机": {"h4"}, "主机 h1": {"h1"}, "ZHUJI": {}, "": {"h1", "h2", "h3", "h4", "h5"}} {
		if got := list(saas.HostFilter{Query: query}); !sameIDs(got, want...) {
			t.Errorf("q=%q：%v，应为 %v", query, hostIDs(got), want)
		}
	}
	// 游标按 (registered_at_ms, source_id) 升序；翻页不漏不重。
	var all []string
	var after *saas.HostCursor
	for {
		page := list(saas.HostFilter{After: after, Limit: 2})
		for _, h := range page {
			all = append(all, h.SourceID)
		}
		if len(page) < 2 {
			break
		}
		last := page[len(page)-1]
		after = &saas.HostCursor{RegisteredAtMs: last.RegisteredAtMs, SourceID: last.SourceID}
	}
	if fmt.Sprint(all) != "[h1 h2 h3 h4 h5]" {
		t.Fatalf("翻页：%v", all)
	}
}

func TestHostRepoPatchUpsertAndCounts(t *testing.T) {
	e := openSaaS(t)
	e.account("a1")
	e.team("t1", "a1")
	e.team("t2", "a1")
	e.seedHost("h1", "t1", "a1", 5)
	e.seedHost("h2", "", "a1", 6)
	s := e.db.SaaS()
	tags := []string{"gpu"}
	err := s.Tx(e.ctx, func(tx saas.Tx) error {
		// teamID 与库里的空间不符：什么也不改。
		if ok, err := tx.Hosts().Patch(e.ctx, saasTenant, "h1", "t2", saas.HostPatch{Tags: &tags}, 7); err != nil || ok {
			t.Fatalf("别的团队不能改：%v %v", ok, err)
		}
		if ok, err := tx.Hosts().Patch(e.ctx, saasTenant, "h1", "", saas.HostPatch{Tags: &tags}, 7); err != nil || ok {
			t.Fatalf("团队主机不能当个人主机改：%v %v", ok, err)
		}
		if ok, err := tx.Hosts().Patch(e.ctx, saasTenant, "h1", "t1", saas.HostPatch{Tags: &tags, Settings: []byte(`{"allowGuests":false}`)}, 7); err != nil || !ok {
			t.Fatalf("同团队可以改：%v %v", ok, err)
		}
		if ok, err := tx.Hosts().Patch(e.ctx, saasTenant, "h2", "", saas.HostPatch{Tags: &tags}, 7); err != nil || !ok {
			t.Fatalf("个人空间可以改：%v %v", ok, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := e.count("SELECT count(*)::int FROM source_spaces WHERE source_id = 'h1' AND tags = '{gpu}' AND settings->>'allowGuests' = 'false'"); n != 1 {
		t.Fatal("h1 的标签与设置应已写入")
	}
	// 同一空间重登记保留标签与设置；换了空间重置。
	e.seedHost("h1", "t1", "a1", 8)
	if n := e.count("SELECT count(*)::int FROM source_spaces WHERE source_id = 'h1' AND tags = '{gpu}' AND settings IS NOT NULL"); n != 1 {
		t.Fatal("同空间重登记应保留标签与设置")
	}
	e.seedHost("h1", "t2", "a1", 9)
	if n := e.count("SELECT count(*)::int FROM source_spaces WHERE source_id = 'h1' AND team_id = 't2' AND tags = '{}' AND settings IS NULL"); n != 1 {
		t.Fatal("换空间应重置标签与设置")
	}
	// 计数：不含被排除的、已撤销的。
	err = s.Tx(e.ctx, func(tx saas.Tx) error {
		if n, err := tx.Hosts().CountPersonal(e.ctx, saasTenant, "a1", ""); err != nil || n != 1 {
			t.Fatalf("个人空间：%d %v", n, err)
		}
		if n, err := tx.Hosts().CountPersonal(e.ctx, saasTenant, "a1", "h2"); err != nil || n != 0 {
			t.Fatalf("排除自己：%d %v", n, err)
		}
		if n, err := tx.Hosts().CountTeam(e.ctx, scopeOf("t2", teamscope.Owner), ""); err != nil || n != 1 {
			t.Fatalf("t2：%d %v", n, err)
		}
		if n, err := tx.Hosts().CountTeam(e.ctx, scopeOf("t1", teamscope.Owner), ""); err != nil || n != 0 {
			t.Fatalf("t1 已搬走：%d %v", n, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	e.must("UPDATE sources SET revoked_at_ms = 1 WHERE source_id = 'h2'")
	err = s.Tx(e.ctx, func(tx saas.Tx) error {
		if n, err := tx.Hosts().CountPersonal(e.ctx, saasTenant, "a1", ""); err != nil || n != 0 {
			t.Fatalf("已撤销不计：%d %v", n, err)
		}
		space, err := tx.SpaceOf(e.ctx, saasTenant, "h2")
		if err != nil || space == nil || !space.Revoked || space.OwnerAccountID != "a1" {
			t.Fatalf("空间行：%+v %v", space, err)
		}
		if gone, err := tx.SpaceOf(e.ctx, saasTenant, "nope"); err != nil || gone != nil {
			t.Fatalf("不存在：%+v %v", gone, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestHostRegistrationTokens(t *testing.T) {
	e := openSaaS(t)
	e.account("a1")
	e.account("a2")
	e.team("t1", "a1")
	s := e.db.SaaS()
	add := func(hash, account, team string, expires int64, max int) bool {
		t.Helper()
		var added bool
		err := s.Tx(e.ctx, func(tx saas.Tx) error {
			var err error
			added, err = tx.Hosts().AddRegistration(e.ctx, saasTenant, saas.RegistrationToken{
				TokenHash: hash, AccountID: account, TeamID: team, ExpiresAtMs: expires,
			}, 100, max)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return added
	}
	// 上限按账号计：a1 满了不影响 a2。
	if !add("1111", "a1", "", 1000, 2) || !add("2222", "a1", "t1", 1000, 2) {
		t.Fatal("未满应能加")
	}
	if add("3333", "a1", "", 1000, 2) {
		t.Fatal("a1 已满")
	}
	if !add("4444", "a2", "", 1000, 2) {
		t.Fatal("a2 不受 a1 影响")
	}
	// 过期的先清掉再数。
	e.must("UPDATE registration_tokens SET expires_at_ms = 50 WHERE token_hash = '1111'")
	if !add("5555", "a1", "", 1000, 2) {
		t.Fatal("过期的不占名额")
	}
	// 看一眼不消耗；消耗一次性并带出空间；过期的不认。
	err := s.Tx(e.ctx, func(tx saas.Tx) error {
		peek, err := tx.Hosts().PeekRegistration(e.ctx, saasTenant, "2222", 100)
		if err != nil || peek == nil || peek.AccountID != "a1" || peek.TeamID != "t1" {
			t.Fatalf("peek：%+v %v", peek, err)
		}
		if again, err := tx.Hosts().PeekRegistration(e.ctx, saasTenant, "2222", 100); err != nil || again == nil {
			t.Fatal("peek 不该消耗")
		}
		got, err := tx.Hosts().ConsumeRegistration(e.ctx, saasTenant, "2222", 100)
		if err != nil || got == nil || got.TeamID != "t1" || got.AccountID != "a1" {
			t.Fatalf("consume：%+v %v", got, err)
		}
		if got, err := tx.Hosts().ConsumeRegistration(e.ctx, saasTenant, "2222", 100); err != nil || got != nil {
			t.Fatalf("一次性：%+v %v", got, err)
		}
		if got, err := tx.Hosts().ConsumeRegistration(e.ctx, saasTenant, "4444", 2000); err != nil || got != nil {
			t.Fatalf("过期不认：%+v %v", got, err)
		}
		if got, err := tx.Hosts().PeekRegistration(e.ctx, saasTenant, "4444", 2000); err != nil || got != nil {
			t.Fatalf("过期不认：%+v %v", got, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// 回滚时消耗不生效。
	boom := fmt.Errorf("回滚")
	err = s.Tx(e.ctx, func(tx saas.Tx) error {
		if got, err := tx.Hosts().ConsumeRegistration(e.ctx, saasTenant, "5555", 100); err != nil || got == nil {
			t.Fatalf("consume：%+v %v", got, err)
		}
		return boom
	})
	if err == nil {
		t.Fatal("应回滚")
	}
	if n := e.count("SELECT count(*)::int FROM registration_tokens WHERE token_hash = '5555'"); n != 1 {
		t.Fatal("回滚后令牌应还在")
	}
}
