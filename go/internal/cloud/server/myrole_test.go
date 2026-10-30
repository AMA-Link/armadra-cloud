package server_test

import (
	"strings"
	"testing"
	"time"
)

// absent 是行里没有 myRole 这个键（区别于 null）。
const absent = "<absent>"

// myRolesOf 取目录答复里每行的 myRole：值为字符串、nil（null）或 absent。
func myRolesOf(t *testing.T, res response) map[string]any {
	t.Helper()
	out := map[string]any{}
	for _, row := range res.json["sources"].([]any) {
		m := row.(map[string]any)
		role, ok := m["myRole"]
		if !ok {
			role = absent
		}
		out[m["sourceId"].(string)] = role
	}
	return out
}

// 目录行的 myRole（Closes #157）：成员、团队 admin、所有者、团队 guest、个人空间的被授予者各看到自己的有效角色；
// sources.list、me.sources、sources.get 三处一致；只反映调用者本人（别人更高的授予不漏进来）；授予到期后不再算；
// 链接访客的 me.sources 不报。
func TestDirectoryMyRole(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner, admin, member, member2, guest, friend := r.verified(), r.verified(), r.verified(), r.verified(), r.verified(), r.verified()
	team := r.seedTeam("myrole", map[string]string{
		owner.accountID: "owner", admin.accountID: "admin", member.accountID: "member", member2.accountID: "member", guest.accountID: "guest",
	})
	hOwner := r.addHost(owner, teamSpace(team), "0.11.0")   // owner 登记，成员靠授予
	hMember := r.addHost(member, teamSpace(team), "0.11.0") // member 登记，团队授予 viewer
	hPersonal := r.addHost(owner, nil, "0.11.0")

	expires := r.clock.Ms() + 600_000 // 访问令牌 15 分钟，到期前后都不用重新登录
	memberGrant := r.mustGrant(owner, hOwner.id, memberOf(member.accountID), "editor", map[string]any{"expiresAtMs": expires})
	otherGrant := r.mustGrant(owner, hOwner.id, memberOf(member2.accountID), "driver", nil)
	r.mustGrant(owner, hOwner.id, memberOf(admin.accountID), "operator", map[string]any{"expiresAtMs": expires})
	r.mustGrant(owner, hOwner.id, guestOf(guest.accountID), "operator", map[string]any{"workspaceIds": []string{"w1"}})
	r.mustGrant(owner, hMember.id, teamOf(team), "viewer", nil)
	r.mustGrant(owner, hPersonal.id, guestOf(friend.accountID), "editor", nil)

	check := func(name string, s session, want map[string]any) {
		t.Helper()
		list := r.do("GET", "/v1/sources", nil, s.bearer())
		r.expect(list, 200, "")
		me := r.do("GET", "/v1/me/sources", nil, s.bearer())
		r.expect(me, 200, "")
		for label, res := range map[string]response{"sources.list": list, "me.sources": me} {
			got := myRolesOf(t, res)
			if len(got) != len(want) {
				t.Errorf("%s 的 %s：%v，应为 %v", name, label, got, want)
				continue
			}
			for id, role := range want {
				if got[id] != role {
					t.Errorf("%s 的 %s 对 %s：myRole = %v，应为 %v", name, label, id, got[id], role)
				}
			}
		}
		for id, role := range want {
			res := r.do("GET", sourcePath(id), nil, s.bearer())
			r.expect(res, 200, "")
			got, ok := res.json["myRole"]
			if !ok {
				got = absent
			}
			if got != role {
				t.Errorf("%s 的 sources.get 对 %s：myRole = %v，应为 %v", name, id, got, role)
			}
		}
		// 不泄露别人的授予：答复里没有任何授予 id，也没有别人的账号 id。
		for _, leak := range []string{memberGrant, otherGrant, member2.accountID} {
			if s.accountID != member2.accountID && strings.Contains(string(list.body)+string(me.body), leak) {
				t.Errorf("%s 的目录泄露了 %q", name, leak)
			}
		}
	}

	// 所有者：自己的主机 driver（owner = true 另表）；成员登记的团队主机经团队授予 viewer。
	check("owner", owner, map[string]any{hOwner.id: "driver", hMember.id: "viewer", hPersonal.id: "driver"})
	// 团队 admin：看得到全部团队主机；hOwner 有自己的 member 授予 operator，hMember 经团队授予 viewer。
	check("admin", admin, map[string]any{hOwner.id: "operator", hMember.id: "viewer"})
	// 成员：自己的 editor，不是同一台主机上 member2 的 driver；自己登记的主机 driver。
	check("member", member, map[string]any{hOwner.id: "editor", hMember.id: "driver"})
	check("member2", member2, map[string]any{hOwner.id: "driver", hMember.id: "viewer"})
	// 团队 guest：只有被授予的那台，限工作空间的 operator；团队授予不给 guest。
	check("guest", guest, map[string]any{hOwner.id: "operator"})
	// 个人空间的被授予者（来路 link）：editor。
	check("friend", friend, map[string]any{hPersonal.id: "editor"})

	// 授予到期后不再算：member 的 editor 到期，hOwner 从目录里消失；admin 仍看得到全部团队主机，但 hOwner 上没有
	// 有效权限了，myRole = null（不是到期前的 operator）。
	r.clock.Advance(11 * time.Minute)
	check("member（到期后）", member, map[string]any{hMember.id: "driver"})
	check("admin（到期后）", admin, map[string]any{hOwner.id: nil, hMember.id: "viewer"})

	// 团队授予撤掉之后，admin 在两台上都没有有效权限，仍看得到（null）。
	res := r.do("GET", "/v1/grants?sourceId="+hMember.id, nil, owner.bearer())
	r.expect(res, 200, "")
	for _, row := range res.json["grants"].([]any) {
		r.expect(r.do("DELETE", "/v1/grants/"+row.(map[string]any)["grantId"].(string), nil, owner.bearer()), 200, "")
	}
	check("admin（撤团队授予后）", admin, map[string]any{hOwner.id: nil, hMember.id: nil})

	// 链接访客：me.sources 只有链接那一台，不报 myRole（权限由链接签，不经授予合成）。
	link := r.mustLink(owner, hPersonal.id, nil)
	accepted := r.acceptLink(link, nil)
	r.expect(accepted, 200, "")
	visitor := r.guestOfAccept(accepted)
	res = r.do("GET", "/v1/me/sources", nil, visitor.bearer())
	r.expect(res, 200, "")
	if got := myRolesOf(t, res); len(got) != 1 || got[hPersonal.id] != absent {
		t.Fatalf("链接访客的 me.sources：%s", res.body)
	}
}
