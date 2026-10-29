package server_test

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// ---- 链接访客的 auth.refresh 与 me.sources（#154）----

// guestOf 是接受答复里的访客会话，连同刷新令牌。
func (r *rig) guestOf(res response) session {
	r.t.Helper()
	s := r.guestOfAccept(res)
	s.refresh, _ = res.json["guestSession"].(map[string]any)["refreshToken"].(string)
	if s.refresh == "" {
		r.t.Fatalf("访客会话没有刷新令牌：%s", res.body)
	}
	return s
}

// refreshGuest 用访客的刷新令牌换新会话；成功时答新的访客会话。
func (r *rig) refreshGuest(g session) (response, session) {
	r.t.Helper()
	res := r.do("POST", "/v1/auth/refresh", map[string]any{"refreshToken": g.refresh}, nil)
	if res.status != 200 {
		return res, session{}
	}
	body := res.json["session"].(map[string]any)
	next := session{
		access: body["accessToken"].(string), refresh: body["refreshToken"].(string),
		deviceID: body["device"].(map[string]any)["deviceId"].(string),
	}
	return res, next
}

func (r *rig) liveGuestDevices(linkID string) int {
	return r.queryInt("SELECT count(*)::int FROM devices WHERE subject LIKE $1 AND revoked_at_ms IS NULL", "guest:"+linkID+":%")
}

// 访客刷新：轮换出新令牌（设备不变、仍是访客主体）；新访问令牌照常要断言；旧令牌重放撤整台设备；
// scope 对不上答 session_revoked。me.sources 只有链接指向的那一台，字段是访客可见的最小集合；
// 账号接口（me.* 其余、sources.*、links.*）对刷新出来的访客会话仍答 403。
func TestGuestRefreshAndMeSources(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner := r.verified()
	h := r.addHost(owner, nil, "0.11.0")
	other := r.addHost(owner, nil, "0.11.0")
	r.exec("UPDATE source_spaces SET tags = ARRAY['secret-tag'] WHERE source_id = $1", h.id)
	r.exec("UPDATE accounts SET display_name = '爱丽丝' WHERE account_id = $1", owner.accountID)
	link := r.mustLink(owner, h.id, nil)
	res := r.acceptLink(link, nil)
	r.expect(res, 200, "")
	guest := r.guestOf(res)

	res, next := r.refreshGuest(guest)
	r.expect(res, 200, "")
	account := res.json["session"].(map[string]any)["account"].(map[string]any)
	if next.deviceID != guest.deviceID || next.refresh == guest.refresh || next.refresh == "" ||
		!strings.HasPrefix(account["accountId"].(string), "guest:"+link.id+":") || account["email"] != nil || res.json["csrfToken"] != nil {
		t.Fatalf("访客刷新：%s", res.body)
	}
	if strings.Contains(string(res.body), owner.accountID) {
		t.Fatalf("刷新答复不该带主人的账号：%s", res.body)
	}
	r.expect(r.do("POST", "/v1/sources/"+h.id+"/assertion", map[string]any{}, next.bearer()), 200, "")

	// me.sources：只有链接指向的那一台，只有九个字段（没有 ownerName / space / tags）。
	res = r.do("GET", "/v1/me/sources", nil, next.bearer())
	r.expect(res, 200, "")
	rows := res.json["sources"].([]any)
	if len(rows) != 1 {
		t.Fatalf("访客的 me.sources：%s", res.body)
	}
	row := rows[0].(map[string]any)
	var keys []string
	for key := range row {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	want := []string{"coreVersion", "kind", "lastSeenAtMs", "name", "online", "owner", "relayOrigin", "sourceId", "via"}
	if !slices.Equal(keys, want) || row["sourceId"] != h.id || row["owner"] != false || row["via"] != "link" || row["relayOrigin"] == "" {
		t.Fatalf("访客的摘要字段：%s", res.body)
	}
	for _, leak := range []string{"爱丽丝", "secret-tag", other.id, owner.accountID} {
		if strings.Contains(string(res.body), leak) {
			t.Fatalf("访客的 me.sources 泄露了 %q：%s", leak, res.body)
		}
	}
	// 所有者的 me.sources 不变：两台，带 ownerName 与 tags。
	res = r.do("GET", "/v1/me/sources", nil, owner.bearer())
	r.expect(res, 200, "")
	if len(res.json["sources"].([]any)) != 2 || !strings.Contains(string(res.body), "secret-tag") {
		t.Fatalf("所有者的 me.sources：%s", res.body)
	}

	// 刷新出来的仍是访客会话：账号接口一律 403。
	for _, call := range []struct{ method, path string }{
		{"GET", "/v1/me"}, {"PATCH", "/v1/me"}, {"GET", "/v1/me/devices"}, {"DELETE", "/v1/me/devices/" + guest.deviceID},
		{"GET", "/v1/me/spaces"}, {"GET", "/v1/me/security-log"}, {"POST", "/v1/me/email"},
		{"GET", "/v1/sources"}, {"GET", sourcePath(h.id)}, {"GET", "/v1/links"}, {"POST", "/v1/auth/step-up"},
	} {
		var body any
		if call.method != "GET" && call.method != "DELETE" {
			body = map[string]any{}
		}
		// 账号接口在认证之后就拒绝访客（403）；少数先校验入参的答 400，都不会成功，也不是 401（会话本身有效）。
		if got := r.do(call.method, call.path, body, next.bearer()); got.status != 403 && got.status != 400 {
			t.Fatalf("%s %s 对访客应拒绝：%d %s", call.method, call.path, got.status, got.body)
		}
	}
	r.expect(r.do("GET", "/v1/me", nil, next.bearer()), 403, "forbidden")
	r.expect(r.do("GET", "/v1/sources", nil, next.bearer()), 403, "forbidden")
	r.expect(r.do("POST", "/v1/sources/"+other.id+"/assertion", map[string]any{}, next.bearer()), 403, "source_access_denied")
	r.expect(r.do("POST", "/v1/auth/refresh", map[string]any{"refreshToken": next.refresh, "scope": "operator"}, nil), 401, "session_revoked")

	// 旧令牌重放：撤整台设备，新令牌也作废。
	r.expect(r.do("POST", "/v1/auth/refresh", map[string]any{"refreshToken": guest.refresh}, nil), 401, "session_revoked")
	r.expect(r.do("POST", "/v1/auth/refresh", map[string]any{"refreshToken": next.refresh}, nil), 401, "session_revoked")
	if n := r.liveGuestDevices(link.id); n != 0 {
		t.Fatalf("重放之后访客设备应作废：%d", n)
	}
}

// 访客站不住时刷新答 session_revoked、me.sources 答空：撤链接、撤主机、团队停用、团队进恢复期、团队或主机关 guest。
// 可恢复的条件（停用、关 guest）恢复之后同一枚刷新令牌还能用（站不住时不轮换）。
func TestGuestRefreshRevokedWhenGuestNoLongerStands(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		apply    func(r *rig, owner session, team, sourceID, linkID string)
		undo     func(r *rig, owner session, team, sourceID string)
		disabled bool // 访客设备随之作废（me.sources 也进不来）
		// guestsOff 为真时访客再要断言也答 403 source_access_denied（关 guest 的两种）。
		guestsOff bool
	}{
		{name: "撤链接", disabled: true, apply: func(r *rig, owner session, _, _, linkID string) {
			r.expect(r.do("DELETE", linkPathOf(linkID), nil, owner.bearer()), 200, "")
		}},
		{name: "撤主机", apply: func(r *rig, owner session, _, sourceID, _ string) {
			r.expect(r.do("DELETE", sourcePath(sourceID), nil, owner.bearer()), 200, "")
		}},
		{name: "团队停用", apply: func(r *rig, _ session, team, _, _ string) {
			r.exec("UPDATE teams SET status = 'suspended' WHERE team_id = $1", team)
		}, undo: func(r *rig, _ session, team, _ string) {
			r.exec("UPDATE teams SET status = 'active' WHERE team_id = $1", team)
		}},
		{name: "团队恢复期", apply: func(r *rig, _ session, team, _, _ string) {
			r.exec("UPDATE teams SET deleted_at_ms = 1, purge_at_ms = 2 WHERE team_id = $1", team)
		}, undo: func(r *rig, _ session, team, _ string) {
			r.exec("UPDATE teams SET deleted_at_ms = NULL, purge_at_ms = NULL WHERE team_id = $1", team)
		}},
		{name: "团队关 guest", guestsOff: true, apply: func(r *rig, owner session, team, _, _ string) {
			r.expect(r.do("PATCH", teamPath(team, "settings"), map[string]any{"allowGuests": false}, owner.bearer()), 200, "")
		}, undo: func(r *rig, owner session, team, _ string) {
			r.expect(r.do("PATCH", teamPath(team, "settings"), map[string]any{"allowGuests": true}, owner.bearer()), 200, "")
		}},
		{name: "主机关 guest", guestsOff: true, apply: func(r *rig, owner session, _, sourceID, _ string) {
			r.expect(r.do("PATCH", sourcePath(sourceID), map[string]any{"settings": map[string]any{"allowGuests": false}}, owner.bearer()), 200, "")
		}, undo: func(r *rig, owner session, _, sourceID string) {
			r.expect(r.do("PATCH", sourcePath(sourceID), map[string]any{"settings": map[string]any{"allowGuests": true}}, owner.bearer()), 200, "")
		}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, rigOptions{})
			owner := r.verified()
			team := r.seedTeam("gr-"+string(rune('a'+i)), map[string]string{owner.accountID: "owner"})
			r.expect(r.do("PATCH", teamPath(team, "settings"), map[string]any{"allowAnonymousLinks": true}, owner.bearer()), 200, "")
			h := r.addHost(owner, teamSpace(team), "0.11.0")
			link := r.mustLink(owner, h.id, nil)
			res := r.acceptLink(link, nil)
			r.expect(res, 200, "")
			guest := r.guestOf(res)
			res, guest = r.refreshGuest(guest)
			r.expect(res, 200, "")

			tc.apply(r, owner, team, h.id, link.id)
			r.expect(r.do("POST", "/v1/auth/refresh", map[string]any{"refreshToken": guest.refresh}, nil), 401, "session_revoked")
			if tc.guestsOff {
				r.expect(r.do("POST", "/v1/sources/"+h.id+"/assertion", map[string]any{}, guest.bearer()), 403, "source_access_denied")
			}
			sources := r.do("GET", "/v1/me/sources", nil, guest.bearer())
			if tc.disabled {
				r.expect(sources, 401, "unauthenticated")
				if n := r.liveGuestDevices(link.id); n != 0 {
					t.Fatalf("访客设备应作废：%d", n)
				}
				return
			}
			r.expect(sources, 200, "")
			if n := len(sources.json["sources"].([]any)); n != 0 {
				t.Fatalf("站不住的访客 me.sources 应为空：%s", sources.body)
			}
			if tc.undo == nil {
				return
			}
			tc.undo(r, owner, team, h.id)
			res, _ = r.refreshGuest(guest)
			r.expect(res, 200, "")
		})
	}
}

// 刷新与撤链接交错：停在「核过访客、轮换之前」或「已轮换、复核之前」时撤销链接，刷新答 session_revoked，
// 不留下活着的访客设备，旧令牌也换不出会话。停在轮换之后时团队停用（不撤设备的条件）同样撤掉这台访客设备。
func TestGuestRefreshRacesRevoke(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ stage, change string }{
		{"refresh.checked", "link"}, {"refresh.rotated", "link"}, {"refresh.rotated", "suspend"},
	} {
		t.Run(tc.stage+"/"+tc.change, func(t *testing.T) {
			t.Parallel()
			g := newGate(tc.stage)
			r := newRig(t, rigOptions{grantsHook: g.hook})
			owner := r.verified()
			team := r.seedTeam("grr-"+tc.change+strings.TrimPrefix(tc.stage, "refresh."), map[string]string{owner.accountID: "owner"})
			r.expect(r.do("PATCH", teamPath(team, "settings"), map[string]any{"allowAnonymousLinks": true}, owner.bearer()), 200, "")
			h := r.addHost(owner, teamSpace(team), "0.11.0")
			link := r.mustLink(owner, h.id, nil)
			res := r.acceptLink(link, nil)
			r.expect(res, 200, "")
			guest := r.guestOf(res)

			done := make(chan response, 1)
			go func() {
				res, _ := r.refreshGuest(guest)
				done <- res
			}()
			select {
			case <-g.reached:
			case res := <-done:
				t.Fatalf("刷新没停在 %s：%d %s", tc.stage, res.status, res.body)
			case <-time.After(30 * time.Second):
				t.Fatalf("刷新没走到 %s", tc.stage)
			}
			if tc.change == "link" {
				r.expect(r.do("DELETE", linkPathOf(link.id), nil, owner.bearer()), 200, "")
			} else {
				r.exec("UPDATE teams SET status = 'suspended' WHERE team_id = $1", team)
			}
			close(g.resume)
			res = <-done
			if res.status != 401 || res.json["code"] != "session_revoked" {
				t.Fatalf("刷新与撤销交错：%d %s", res.status, res.body)
			}
			if n := r.liveGuestDevices(link.id); n != 0 {
				t.Fatalf("不该留下活着的访客设备：%d", n)
			}
			r.expect(r.do("POST", "/v1/auth/refresh", map[string]any{"refreshToken": guest.refresh}, nil), 401, "session_revoked")
			if code := r.do("GET", "/v1/me/sources", nil, guest.bearer()); code.status != 401 {
				t.Fatalf("访客的访问令牌应失效：%d %s", code.status, code.body)
			}
		})
	}
}
