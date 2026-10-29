package server_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/grants"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
)

func accountCaller(s session) core.Caller {
	return core.Caller{Subject: s.accountID, AccountID: s.accountID, DeviceID: s.deviceID, Role: core.RoleOwner}
}

func errCode(err error) string {
	if err == nil {
		return "ok"
	}
	if coreErr, ok := err.(*core.Error); ok {
		return string(coreErr.Code)
	}
	return err.Error()
}

// Policy.Visible 的游标分页：每页不超过 limit、游标彼此不同、不漏不重；坏游标 bad_request；core.Sources.List 取全。
func TestPolicyVisiblePagination(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	ctx := context.Background()
	owner, stranger := r.verified(), r.verified()
	team := r.seedTeam("pg", map[string]string{owner.accountID: "owner"})
	var want []string
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("%032x", i+1)
		r.seedHost(id, team, owner.accountID)
		want = append(want, id)
	}
	policy := r.run.Core.Policy
	caller := accountCaller(owner)

	var got []string
	cursors := map[string]bool{}
	for cursor, pages := "", 0; ; pages++ {
		page, next, err := policy.Visible(ctx, caller, core.SourceFilter{Cursor: cursor, Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		if len(page) > 2 {
			t.Fatalf("一页 %d 行，超过 limit", len(page))
		}
		for _, entry := range page {
			got = append(got, entry.Source.SourceID)
			if !entry.Owner && entry.Via != "team" {
				t.Fatalf("来路：%+v", entry)
			}
		}
		if next == "" {
			break
		}
		if cursors[next] || pages > 5 {
			t.Fatalf("游标重复或翻不完：%q", next)
		}
		cursors[next] = true
		cursor = next
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("翻页结果 %v，应为 %v", got, want)
	}
	if _, _, err := policy.Visible(ctx, caller, core.SourceFilter{Cursor: "***"}); errCode(err) != "bad_request" {
		t.Fatalf("坏游标：%v", err)
	}
	// 缺省页大小下取全；core.Sources.List 循环取页到底。
	list, err := core.NewSources(r.run.Core).List(ctx, caller)
	if err != nil || len(list) != 5 {
		t.Fatalf("Sources.List：%d %v", len(list), err)
	}
	if page, next, err := policy.Visible(ctx, accountCaller(stranger), core.SourceFilter{}); err != nil || len(page) != 0 || next != "" {
		t.Fatalf("外人：%v %q %v", page, next, err)
	}
}

// Policy.CanManage：所有者 / 团队 owner、admin 放行；看得到管不了 forbidden；看不到 not_found；空目标、访客一律拒绝。
func TestPolicyCanManage(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	ctx := context.Background()
	owner, admin, member, other, outsider := r.verified(), r.verified(), r.verified(), r.verified(), r.verified()
	team := r.seedTeam("cm", map[string]string{owner.accountID: "owner", admin.accountID: "admin", member.accountID: "member", other.accountID: "member"})
	mine := r.addHost(member, teamSpace(team), "0.10.0")
	theirs := r.addHost(other, teamSpace(team), "0.10.0")
	r.mustGrant(admin, theirs.id, memberOf(member.accountID), "viewer", nil)
	policy := r.run.Core.Policy
	check := func(who session, hostID, want string) {
		t.Helper()
		if got := errCode(policy.CanManage(ctx, accountCaller(who), core.ManageTarget{SourceID: hostID}, core.ManageSourceRevoke)); got != want {
			t.Errorf("%s 管 %s：%s，应为 %s", who.accountID[:6], hostID[:6], got, want)
		}
	}
	check(member, mine.id, "ok")
	check(owner, mine.id, "ok")
	check(admin, mine.id, "ok")
	check(member, theirs.id, "forbidden") // 经授予看得到，管不了
	check(other, theirs.id, "ok")
	check(outsider, mine.id, "not_found")
	check(member, "00000000000000000000000000000000", "not_found")
	// 空目标（不带过滤的 links.list）与访客一律拒绝，不放行成全租户。
	if got := errCode(policy.CanManage(ctx, accountCaller(owner), core.ManageTarget{}, core.ManageLinkList)); got != "forbidden" {
		t.Errorf("空目标：%s", got)
	}
	guest := core.Caller{Subject: "guest:L:x", Role: core.RoleGuest, LinkID: "L"}
	if got := errCode(policy.CanManage(ctx, guest, core.ManageTarget{SourceID: mine.id}, core.ManageLinkList)); got != "forbidden" {
		t.Errorf("访客：%s", got)
	}
	// 目标只填 LinkID：链接不存在 not_found。
	if got := errCode(policy.CanManage(ctx, accountCaller(owner), core.ManageTarget{LinkID: "nope"}, core.ManageLinkRevoke)); got != "not_found" {
		t.Errorf("没有的链接：%s", got)
	}
}

// Policy.Intent / Access：看得到不等于可达；声明按有效权限合成；停用、版本门、到期、scp 上限各自的错误。
func TestPolicyIntentAndAccess(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	ctx := context.Background()
	owner, admin, member, outsider := r.verified(), r.verified(), r.verified(), r.verified()
	team := r.seedTeam("in", map[string]string{owner.accountID: "owner", admin.accountID: "admin", member.accountID: "member"})
	h := r.addHost(owner, teamSpace(team), "0.10.0")
	policy := r.run.Core.Policy
	source, err := r.run.Core.Ports.Sources.Get(ctx, "cloud", h.id)
	if err != nil || source == nil {
		t.Fatal(err)
	}

	// Access：团队 admin 看得到，外人看不到；Intent：admin 没有授予 → source_access_denied。
	if _, ok, err := policy.Access(ctx, accountCaller(admin), h.id); err != nil || !ok {
		t.Fatalf("admin 看得到：%v %v", ok, err)
	}
	if _, ok, err := policy.Access(ctx, accountCaller(outsider), h.id); err != nil || ok {
		t.Fatalf("外人看不到：%v %v", ok, err)
	}
	if _, err := policy.Intent(ctx, accountCaller(admin), *source); errCode(err) != "source_access_denied" {
		t.Fatalf("没有授予：%v", err)
	}
	// 授予后：声明里有 team、role、scp、email、过期时间；个人 owner 之外都带 role。
	exp := r.clock.Ms() + 600_000
	r.mustGrant(owner, h.id, memberOf(admin.accountID), "operator", map[string]any{"workspaceIds": []string{"w2", "w1"}, "expiresAtMs": exp})
	extra, err := policy.Intent(ctx, accountCaller(admin), *source)
	if err != nil {
		t.Fatal(err)
	}
	if extra.Role != "operator" || !reflect.DeepEqual(extra.Scp, []string{"ws:w1", "ws:w2"}) || extra.Team == nil || extra.Team.TeamID != team || extra.Team.Role != "admin" ||
		extra.Email == "" || !extra.EmailVerified || !extra.TrackJTI || extra.ExpiresAtMs != exp {
		t.Fatalf("意图：%+v team=%+v", extra, extra.Team)
	}
	// 团队停用：看得到的人 team_suspended；外人 source_access_denied（不泄露）。
	r.exec("UPDATE teams SET status = 'suspended' WHERE team_id = $1", team)
	if _, err := policy.Intent(ctx, accountCaller(admin), *source); errCode(err) != "team_suspended" {
		t.Fatalf("停用：%v", err)
	}
	if _, err := policy.Intent(ctx, accountCaller(outsider), *source); errCode(err) != "source_access_denied" {
		t.Fatalf("外人：%v", err)
	}
	r.exec("UPDATE teams SET status = 'active' WHERE team_id = $1", team)
	// 版本门：在线版本低于 0.10.0 不签。
	r.run.Relay.SetRoute(h.id, "0.9.0")
	if _, err := policy.Intent(ctx, accountCaller(admin), *source); errCode(err) != "source_core_outdated" {
		t.Fatalf("版本门：%v", err)
	}
	r.run.Relay.SetRoute(h.id, "")
	// 授予不足 1 秒到期：不签。
	r.clock.Advance(599_500 * time.Millisecond)
	if _, err := policy.Intent(ctx, accountCaller(admin), *source); errCode(err) != "source_access_denied" {
		t.Fatalf("快到期：%v", err)
	}
	// 合成后的 scp 超过 64 项：不签（两个授予的范围并集）。
	r.clock.Advance(-599_500 * time.Millisecond)
	var a, b []string
	for i := 0; i < 40; i++ {
		a = append(a, fmt.Sprintf("a%d", i))
		b = append(b, fmt.Sprintf("b%d", i))
	}
	r.mustGrant(owner, h.id, teamOf(team), "viewer", map[string]any{"workspaceIds": b})
	r.expect(r.do("PATCH", "/v1/grants/"+r.queryString("SELECT grant_id FROM source_grants WHERE subject_id = $1", admin.accountID), map[string]any{"workspaceIds": a}, owner.bearer()), 200, "")
	if _, err := policy.Intent(ctx, accountCaller(admin), *source); errCode(err) != "source_access_denied" {
		t.Fatalf("scp 超上限：%v", err)
	}
	// 链接访客转给 core 的 personalPolicy：没有链接什么也看不到。
	guest := core.Caller{Subject: "guest:L:x", Role: core.RoleGuest, LinkID: "L"}
	if _, ok, err := policy.Access(ctx, guest, h.id); err != nil || ok {
		t.Fatalf("访客：%v %v", ok, err)
	}
	_ = grants.MaxScopeEntries
}

// Policy.Watch：连接建立时解析可见集合；授予到手后靠 AccessChanged 更新；Close 之后什么都看不到。
func TestPolicyWatch(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	ctx := context.Background()
	owner, friend := r.verified(), r.verified()
	mineHost := r.addHost(owner, nil, "0.11.0")
	other := r.addHost(friend, nil, "0.11.0")
	policy := r.run.Core.Policy
	w, err := policy.Watch(ctx, accountCaller(owner))
	if err != nil {
		t.Fatal(err)
	}
	if !w.Sees(mineHost.id) || w.Sees(other.id) {
		t.Fatal("初始可见集合")
	}
	w.Refresh(grants.AccessChanged{SourceID: other.id, AccountID: owner.accountID, Visible: true})
	if !w.Sees(other.id) {
		t.Fatal("点名账号的放宽应生效")
	}
	w.Refresh(core.NewSourceRevoked(mineHost.id))
	if w.Sees(mineHost.id) {
		t.Fatal("撤销的主机应移除")
	}
	w.Close()
	if w.Sees(other.id) {
		t.Fatal("关闭后不可见")
	}
	w.Refresh(grants.AccessChanged{SourceID: other.id, AccountID: owner.accountID, Visible: true})
	if w.Sees(other.id) {
		t.Fatal("关闭后 Refresh 无效")
	}
}
