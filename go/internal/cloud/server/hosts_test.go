package server_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
	"github.com/AMA-Link/armadra-cloud/go/internal/jose"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// ---- 测试用的主机与登记 ----

type testHost struct {
	id   string
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newTestHost(t *testing.T) testHost {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	return testHost{id: hex.EncodeToString(id), pub: pub, priv: priv}
}

func teamSpace(id string) map[string]any { return map[string]any{"kind": "team", "teamId": id} }

// hostToken 要一枚注册令牌；space 为 nil = 缺省（个人空间）。
func (r *rig) hostToken(s session, space any) response {
	r.t.Helper()
	body := map[string]any{}
	if space != nil {
		body["space"] = space
	}
	return r.do("POST", "/v1/sources/registration-tokens", body, s.bearer())
}

// tokenOf 要一枚令牌并断言成功。
func (r *rig) tokenOf(s session, space any) string {
	r.t.Helper()
	res := r.hostToken(s, space)
	r.expect(res, 200, "")
	return res.json["registrationToken"].(string)
}

func (r *rig) registerHost(token string, h testHost, version string) response {
	r.t.Helper()
	return r.do("POST", "/v1/sources/register", map[string]any{
		"registrationToken": token, "sourceId": h.id,
		"publicKey": map[string]any{"kty": "OKP", "crv": "Ed25519", "x": base64.RawURLEncoding.EncodeToString(h.pub)},
		"name":      "主机 " + h.id[:6], "kind": "desktop", "coreVersion": version, "capabilities": []string{"terminal"},
		"protocol": map[string]any{"major": gen.ProtocolMajor, "minor": gen.ProtocolMinor},
	}, nil)
}

// addHost 要令牌并登记一台新主机，断言成功。
func (r *rig) addHost(s session, space any, version string) testHost {
	r.t.Helper()
	h := newTestHost(r.t)
	res := r.registerHost(r.tokenOf(s, space), h, version)
	r.expect(res, 200, "")
	if res.json["ownerAccountId"] != s.accountID || res.json["sourceId"] != h.id {
		r.t.Fatalf("登记答复：%s", res.body)
	}
	return h
}

func (r *rig) sourceAuth(h testHost) headers {
	r.t.Helper()
	nowSec := r.clock.Ms() / 1000
	jws, err := jose.Sign(h.priv, jose.Header{Kid: h.id}, map[string]any{
		"iss": h.id, "aud": appOrigin, "iat": nowSec, "exp": nowSec + 60, "jti": "j-" + hex.EncodeToString(randBytes(8)),
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return hdr("Authorization", "Source "+jws)
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func sourcePath(id string) string { return "/v1/sources/" + id }

func ids(res response) []string {
	out := []string{}
	for _, row := range res.json["sources"].([]any) {
		out = append(out, row.(map[string]any)["sourceId"].(string))
	}
	return out
}

func sortedIDs(hosts ...testHost) []string {
	out := []string{}
	for _, h := range hosts {
		out = append(out, h.id)
	}
	sort.Strings(out)
	return out
}

func sortedCopy(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

// 个人空间：登记写源与空间行、审计一行；Free 额度 2 台，令牌端快速失败，登记端权威检查。
func TestHostsRegisterPersonalAndQuota(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	raw := r.registerBearer(r.email())
	r.expect(r.hostToken(raw, nil), 403, "email_unverified")

	a := r.verified()
	h1 := r.addHost(a, nil, "0.9.2") // 个人空间不设版本门
	var team any
	if err := r.run.DB.Pool().QueryRow(ctxBackground,
		"SELECT team_id FROM source_spaces WHERE source_id = $1 AND owner_account_id = $2", h1.id, a.accountID).Scan(&team); err != nil || team != nil {
		t.Fatalf("个人空间的空间行：%v %v", team, err)
	}
	if n := r.queryInt("SELECT count(*)::int FROM audit_log WHERE action = 'host.register' AND source_id = $1 AND actor_id = $2 AND team_id IS NULL", h1.id, a.accountID); n != 1 {
		t.Fatal("应有 host.register 审计")
	}
	res := r.do("GET", sourcePath(h1.id), nil, a.bearer())
	r.expect(res, 200, "")
	if res.json["owner"] != true || res.json["via"] != "owner" || res.json["ownerName"] != "测试用户" ||
		!reflect.DeepEqual(res.json["space"], map[string]any{"kind": "personal"}) ||
		res.json["relayOrigin"] != "https://"+h1.id+".src.src.localhost" || res.json["online"] != false ||
		!reflect.DeepEqual(res.json["settings"], map[string]any{"linkCreators": "owner_admin", "allowGuests": true, "defaultRole": "viewer"}) {
		t.Fatalf("sources.get：%s", res.body)
	}

	// 令牌拿在手里时再登记第二台，之后第三台的令牌被快速拒绝；先拿到的令牌在登记端被权威检查挡下。
	spare := r.tokenOf(a, nil)
	r.addHost(a, nil, "0.9.2")
	res = r.hostToken(a, nil)
	r.expect(res, 429, "limit_reached")
	details := res.json["details"].(map[string]any)
	if details["quota"] != "hosts" || details["used"] != float64(2) || details["limit"] != float64(2) {
		t.Fatalf("limit_reached 的 details：%s", res.body)
	}
	late := newTestHost(t)
	r.expect(r.registerHost(spare, late, "0.9.2"), 429, "limit_reached")
	// 失败回滚：没有留下源与空间行，令牌也没被消耗（它自己十分钟后过期）。
	if n := r.queryInt("SELECT count(*)::int FROM sources WHERE source_id = $1", late.id); n != 0 {
		t.Fatal("失败的登记不该留下源")
	}
	if n := r.queryInt("SELECT count(*)::int FROM registration_tokens WHERE account_id = $1", a.accountID); n != 1 {
		t.Fatalf("回滚后令牌应还在：%d", n)
	}
	// 撤销一台后又有名额；已撤销的不占额度。
	r.expect(r.do("DELETE", sourcePath(h1.id), nil, a.bearer()), 200, "")
	r.expect(r.registerHost(spare, late, "0.9.2"), 200, "")
}

// 令牌与登记的错误：令牌失效、一次性、协议主版本、source_taken、同钥重登记幂等。
func TestHostsRegisterErrorsAndReRegister(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	a, b := r.verified(), r.verified()
	h := newTestHost(t)
	r.expect(r.registerHost("nope", h, "0.10.0"), 401, "registration_token_invalid")

	token := r.tokenOf(a, nil)
	bad := r.do("POST", "/v1/sources/register", map[string]any{
		"registrationToken": token, "sourceId": h.id,
		"publicKey": map[string]any{"kty": "OKP", "crv": "Ed25519", "x": base64.RawURLEncoding.EncodeToString(h.pub)},
		"name":      "x", "kind": "desktop", "coreVersion": "0.10.0", "capabilities": []string{},
		"protocol": map[string]any{"major": gen.ProtocolMajor + 1, "minor": 0},
	}, nil)
	r.expect(bad, 426, "protocol_unsupported")
	r.expect(r.registerHost(token, h, "0.10.0"), 200, "") // 回滚了，令牌还能用
	r.expect(r.registerHost(token, newTestHost(t), "0.10.0"), 401, "registration_token_invalid")

	// 同一个人用同一把钥重登记：幂等，不占第二个名额，空间行与标签保留。
	r.expect(r.do("PATCH", sourcePath(h.id), map[string]any{"tags": []string{"gpu"}}, a.bearer()), 200, "")
	r.expect(r.registerHost(r.tokenOf(a, nil), h, "0.10.0"), 200, "")
	if n := r.queryInt("SELECT count(*)::int FROM source_spaces WHERE source_id = $1 AND tags = '{gpu}'", h.id); n != 1 {
		t.Fatal("重登记应保留标签")
	}
	// 另一把钥、别的账号、别的空间：source_taken。
	other := h
	other.pub, other.priv, _ = ed25519.GenerateKey(rand.Reader)
	r.expect(r.registerHost(r.tokenOf(a, nil), other, "0.10.0"), 409, "source_taken")
	r.expect(r.registerHost(r.tokenOf(b, nil), h, "0.10.0"), 409, "source_taken")
	team := r.seedTeam("taken", map[string]string{a.accountID: "owner"})
	r.expect(r.registerHost(r.tokenOf(a, teamSpace(team)), h, "0.10.0"), 409, "source_taken")
	// 撤销之后别人拿同一把钥可以登记到自己的空间。
	r.expect(r.do("DELETE", sourcePath(h.id), nil, a.bearer()), 200, "")
	r.expect(r.registerHost(r.tokenOf(b, nil), h, "0.10.0"), 200, "")
	if n := r.queryInt("SELECT count(*)::int FROM source_spaces WHERE source_id = $1 AND owner_account_id = $2", h.id, b.accountID); n != 1 {
		t.Fatal("撤销后的重登记应归新账号")
	}
}

// 团队空间：谁能要令牌（角色矩阵）、停用 / 删除、成员登记开关、版本门、配额。
func TestHostsRegisterTeamRolesAndGates(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	people := map[string]session{}
	members := map[string]string{}
	for _, role := range []string{"owner", "admin", "member", "billing", "guest"} {
		people[role] = r.verified()
		members[people[role].accountID] = role
	}
	outsider := r.verified()
	team := r.seedTeam("gate", members)

	for role, want := range map[string]struct {
		status int
		code   string
	}{
		"owner": {200, ""}, "admin": {200, ""}, "member": {200, ""},
		"billing": {403, "team_role_insufficient"}, "guest": {403, "team_role_insufficient"},
	} {
		r.expect(r.hostToken(people[role], teamSpace(team)), want.status, want.code)
	}
	r.expect(r.hostToken(outsider, teamSpace(team)), 404, "not_found")
	r.expect(r.hostToken(outsider, teamSpace("no-such-team")), 404, "not_found")

	// 成员登记开关：关掉后只挡 member，owner / admin 照常。
	r.exec("UPDATE teams SET settings = jsonb_set(settings, '{membersCanRegisterHosts}', 'false') WHERE team_id = $1", team)
	r.expect(r.hostToken(people["member"], teamSpace(team)), 403, "host_registration_disabled")
	r.expect(r.hostToken(people["admin"], teamSpace(team)), 200, "")
	r.exec("UPDATE teams SET settings = jsonb_set(settings, '{membersCanRegisterHosts}', 'true') WHERE team_id = $1", team)

	// 版本门：不到 MinCoreVersionTeamAssertion 的 core 进不了团队，也不留痕；预发布版低于同号正式版。
	for _, version := range []string{"0.9.2", "0.10.0-rc.1", "dev", ""} {
		h := newTestHost(t)
		token := r.tokenOf(people["member"], teamSpace(team))
		r.expect(r.registerHost(token, h, version), 426, "protocol_unsupported")
		if n := r.queryInt("SELECT count(*)::int FROM sources WHERE source_id = $1", h.id); n != 0 {
			t.Fatalf("版本 %q 被拒后不该留下源", version)
		}
		// 同一枚令牌换个够新的版本还能用（失败回滚没消耗它）。
		r.expect(r.registerHost(token, h, gen.MinCoreVersionTeamAssertion), 200, "")
		r.expect(r.do("DELETE", sourcePath(h.id), nil, people["member"].bearer()), 200, "")
	}
	// 同一台 0.9.2 的主机登记到个人空间则成功。
	r.addHost(people["member"], nil, "0.9.2")

	// 团队停用：令牌与登记都挡（登记时团队已停用：令牌拿在手里也一样）。
	token := r.tokenOf(people["admin"], teamSpace(team))
	r.exec("UPDATE teams SET status = 'suspended' WHERE team_id = $1", team)
	r.expect(r.hostToken(people["admin"], teamSpace(team)), 403, "team_suspended")
	r.expect(r.registerHost(token, newTestHost(t), "0.10.0"), 403, "team_suspended")
	r.exec("UPDATE teams SET status = 'active' WHERE team_id = $1", team)
	// 令牌签发之后成员被降成 billing、或被移出：令牌作废。
	r.exec("UPDATE team_members SET role = 'billing' WHERE team_id = $1 AND account_id = $2", team, people["member"].accountID)
	token = r.tokenOf(people["admin"], teamSpace(team))
	r.exec("UPDATE registration_tokens SET account_id = $1 WHERE token_hash = (SELECT token_hash FROM registration_tokens WHERE team_id = $2 ORDER BY seq DESC LIMIT 1)", people["member"].accountID, team)
	r.expect(r.registerHost(token, newTestHost(t), "0.10.0"), 401, "registration_token_invalid")
	// 团队进入恢复期。
	r.exec("UPDATE teams SET deleted_at_ms = 1, purge_at_ms = 2 WHERE team_id = $1", team)
	r.expect(r.hostToken(people["owner"], teamSpace(team)), 410, "team_deleted")
}

// 团队主机配额（Free 3 台）：令牌端快速失败；并发登记恰好 3 个成功；个人空间同理（2 台）。
func TestHostsRegisterConcurrentRespectsQuota(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner := r.verified()
	a, b := r.verified(), r.verified()
	team := r.seedTeam("race", map[string]string{owner.accountID: "owner", a.accountID: "member", b.accountID: "member"})

	run := func(space any, who []session, wantOK int, quota string) {
		t.Helper()
		type job struct {
			token string
			host  testHost
		}
		jobs := []job{}
		for _, s := range who {
			for i := 0; i < 3; i++ {
				jobs = append(jobs, job{token: r.tokenOf(s, space), host: newTestHost(t)})
			}
		}
		statuses := make([]response, len(jobs))
		var wg sync.WaitGroup
		for i := range jobs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				statuses[i] = r.registerHost(jobs[i].token, jobs[i].host, "0.10.0")
			}()
		}
		wg.Wait()
		ok := 0
		for _, res := range statuses {
			switch res.status {
			case 200:
				ok++
			case 429:
				if res.json["code"] != "limit_reached" || res.json["details"].(map[string]any)["quota"] != quota {
					t.Fatalf("超额的答复：%s", res.body)
				}
			default:
				t.Fatalf("意外的答复：%d %s", res.status, res.body)
			}
		}
		if ok != wantOK {
			t.Fatalf("应恰好 %d 个成功，实际 %d", wantOK, ok)
		}
	}
	run(teamSpace(team), []session{owner, a, b}, 3, "hosts")
	if n := r.queryInt("SELECT count(*)::int FROM source_spaces WHERE team_id = $1", team); n != 3 {
		t.Fatalf("团队里应有 3 台：%d", n)
	}
	r.expect(r.hostToken(a, teamSpace(team)), 429, "limit_reached")
	run(nil, []session{owner}, 2, "hosts") // 个人空间 Free 2 台
}

// 目录：可见性（所有者、团队 admin、成员只看自己的、billing / guest 与外人看不到）与跨团队读不到。
func TestHostsDirectoryVisibilityAndCrossTeam(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner, admin, member, other, billing, guest := r.verified(), r.verified(), r.verified(), r.verified(), r.verified(), r.verified()
	t1 := r.seedTeam("dir-1", map[string]string{
		owner.accountID: "owner", admin.accountID: "admin", member.accountID: "member", other.accountID: "member",
		billing.accountID: "billing", guest.accountID: "guest",
	})
	b := r.verified()
	t2 := r.seedTeam("dir-2", map[string]string{b.accountID: "owner"})

	hOwner := r.addHost(owner, teamSpace(t1), "0.10.0")
	hMember := r.addHost(member, teamSpace(t1), "0.10.0")
	hOther := r.addHost(other, teamSpace(t1), "0.10.0")
	hPersonal := r.addHost(owner, nil, "0.10.0")
	hB := r.addHost(b, teamSpace(t2), "0.10.0")

	list := func(s session, query string) response {
		t.Helper()
		res := r.do("GET", "/v1/sources"+query, nil, s.bearer())
		r.expect(res, 200, "")
		return res
	}
	for _, c := range []struct {
		who  session
		name string
		want []testHost
	}{
		{owner, "owner", []testHost{hOwner, hMember, hOther, hPersonal}},
		{admin, "admin", []testHost{hOwner, hMember, hOther}},
		{member, "member", []testHost{hMember}},
		{billing, "billing", nil},
		{guest, "guest", nil},
		{b, "t2 owner", []testHost{hB}},
	} {
		if got := sortedCopy(ids(list(c.who, ""))); !reflect.DeepEqual(got, sortedIDs(c.want...)) {
			t.Errorf("%s 看到 %v，应为 %v", c.name, got, sortedIDs(c.want...))
		}
	}
	// 视角：admin 看别人的团队主机 owner = false、via = team；me.sources 与 sources.list 同一批。
	res := list(admin, "?space[kind]=team&space[teamId]="+t1)
	for _, row := range res.json["sources"].([]any) {
		m := row.(map[string]any)
		if m["owner"] != false || m["via"] != "team" || !reflect.DeepEqual(m["space"], teamSpace(t1)) || m["ownerName"] != "测试用户" {
			t.Fatalf("admin 的视角：%s", res.body)
		}
	}
	res = r.do("GET", "/v1/me/sources", nil, owner.bearer())
	r.expect(res, 200, "")
	if got := sortedCopy(ids(res)); !reflect.DeepEqual(got, sortedIDs(hOwner, hMember, hOther, hPersonal)) {
		t.Fatalf("me.sources：%v", got)
	}

	// 跨团队读不到：T2 的 owner 看 T1 的主机——get / update / revoke 都是 404，列表筛 T1 为空，T1 的主机原封不动。
	for _, h := range []testHost{hOwner, hMember, hPersonal} {
		r.expect(r.do("GET", sourcePath(h.id), nil, b.bearer()), 404, "not_found")
		r.expect(r.do("PATCH", sourcePath(h.id), map[string]any{"name": "劫持"}, b.bearer()), 404, "not_found")
		r.expect(r.do("DELETE", sourcePath(h.id), nil, b.bearer()), 404, "not_found")
	}
	if got := ids(list(b, "?space[kind]=team&space[teamId]="+t1)); len(got) != 0 {
		t.Fatalf("T2 的人筛 T1：%v", got)
	}
	// 同团队但管不到：member 看不到 owner 的，billing / guest 什么都看不到。
	for _, s := range []session{member, billing, guest} {
		r.expect(r.do("GET", sourcePath(hOwner.id), nil, s.bearer()), 404, "not_found")
		r.expect(r.do("PATCH", sourcePath(hOwner.id), map[string]any{"name": "x"}, s.bearer()), 404, "not_found")
		r.expect(r.do("DELETE", sourcePath(hOwner.id), nil, s.bearer()), 404, "not_found")
	}
	r.expect(r.do("GET", sourcePath(hMember.id), nil, member.bearer()), 200, "")
	if n := r.queryInt("SELECT count(*)::int FROM sources s JOIN source_spaces p USING (tenant_id, source_id) WHERE s.revoked_at_ms IS NULL"); n != 5 {
		t.Fatalf("没有一台被动过：%d", n)
	}
	// 个人空间的主机，团队 owner 以外的人也看不到；个人空间筛选只列自己的。
	if got := ids(list(owner, "?space[kind]=personal")); !reflect.DeepEqual(got, []string{hPersonal.id}) {
		t.Fatalf("个人空间：%v", got)
	}
	// 外人的令牌不能把主机登记进别人的团队（上面 gate 用例覆盖）；访客会话不是账号。
	r.expect(r.do("GET", "/v1/sources", nil, nil), 401, "unauthenticated")
}

// 目录筛选与游标：status、tag（且）、q（通配符按字面）、翻页不漏不重、坏游标。
func TestHostsDirectoryFiltersAndCursor(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{env: map[string]string{}})
	owner := r.verified()
	team := r.seedTeam("filters", map[string]string{owner.accountID: "owner"})
	// 直接写库造 7 台团队主机（Free 额度只限登记路径）；registered_at 有同值，靠 source_id 定序。
	hosts := []string{}
	for i := 0; i < 7; i++ {
		id := fmt.Sprintf("%032x", i+1)
		hosts = append(hosts, id)
		r.seedHost(id, team, owner.accountID)
		r.exec("UPDATE sources SET registered_at_ms = $1, name = $2 WHERE source_id = $3", int64(i/2*1000+1), fmt.Sprintf("机器 %d", i), id)
	}
	r.exec("UPDATE sources SET name = '100%_c:\\机' WHERE source_id = $1", hosts[3])
	r.exec("UPDATE source_spaces SET tags = '{gpu,prod}' WHERE source_id = ANY($1)", hosts[:2])
	r.exec("UPDATE source_spaces SET tags = '{gpu}' WHERE source_id = $1", hosts[2])
	r.exec("UPDATE sources SET revoked_at_ms = 5 WHERE source_id = $1", hosts[6])
	r.run.Relay.SetRoute(hosts[1], "0.10.0")
	r.run.Relay.SetRoute(hosts[5], "0.10.1")
	get := func(query string) response {
		t.Helper()
		res := r.do("GET", "/v1/sources?"+query, nil, owner.bearer())
		r.expect(res, 200, "")
		return res
	}
	if got := ids(get("")); !reflect.DeepEqual(got, hosts[:6]) {
		t.Fatalf("缺省不含已撤销：%v", got)
	}
	cases := map[string][]string{
		"status=online":                                  {hosts[1], hosts[5]},
		"status=offline":                                 {hosts[0], hosts[2], hosts[3], hosts[4]},
		"status=revoked":                                 {hosts[6]},
		"tag=gpu":                                        hosts[:3],
		"tag=gpu&tag=prod":                               hosts[:2],
		"tag[]=gpu&tag[]=prod":                           hosts[:2],
		"tag[0]=gpu&tag[1]=prod":                         hosts[:2],
		"tag=prod&tag=missing":                           {},
		"status=online&tag=gpu":                          {hosts[1]},
		"q=" + url.QueryEscape("100%"):                   {hosts[3]},
		"q=" + url.QueryEscape("%"):                      {hosts[3]},
		"q=" + url.QueryEscape("_c:"):                    {hosts[3]},
		"q=" + url.QueryEscape("\\机"):                    {hosts[3]},
		"q=" + url.QueryEscape("机器 4"):                   {hosts[4]},
		"q=" + url.QueryEscape("机器") + "&status=offline": {hosts[0], hosts[2], hosts[4]},
	}
	for query, want := range cases {
		if got := ids(get(query)); !reflect.DeepEqual(got, want) && !(len(got) == 0 && len(want) == 0) {
			t.Errorf("%s：%v，应为 %v", query, got, want)
		}
	}
	// 在线状态的在线信息进答复：在线的 online = true、coreVersion 取隧道上报的。
	res := get("status=online")
	rows := res.json["sources"].([]any)
	if rows[0].(map[string]any)["online"] != true || rows[1].(map[string]any)["coreVersion"] != "0.10.1" {
		t.Fatalf("在线的主机：%s", res.body)
	}
	// 游标：每页 2 条翻到底，不漏不重；最后一页 nextCursor 为 null。
	for _, extra := range []string{"", "&status=offline"} {
		all, cursor, pages := []string{}, "", 0
		for {
			query := "limit=2" + extra
			if cursor != "" {
				query += "&cursor=" + url.QueryEscape(cursor)
			}
			res := get(query)
			all = append(all, ids(res)...)
			pages++
			next, _ := res.json["nextCursor"].(string)
			if next == "" {
				break
			}
			cursor = next
		}
		want := hosts[:6]
		if extra != "" {
			want = []string{hosts[0], hosts[2], hosts[3], hosts[4]}
		}
		if !reflect.DeepEqual(all, want) || pages != (len(want)+1)/2 {
			t.Fatalf("翻页%q：%v（%d 页），应为 %v", extra, all, pages, want)
		}
	}
	r.expect(r.do("GET", "/v1/sources?cursor=%21%21%21", nil, owner.bearer()), 400, "bad_request")
	r.expect(r.do("GET", "/v1/sources?cursor="+base64.RawURLEncoding.EncodeToString([]byte("{}")), nil, owner.bearer()), 400, "bad_request")
	r.expect(r.do("GET", "/v1/sources?limit=201", nil, owner.bearer()), 400, "bad_request")
	r.expect(r.do("GET", "/v1/sources?status=bogus", nil, owner.bearer()), 400, "bad_request")
}

// 按在线状态筛选要跨过库里的批次（200 行一批）：450 台里只有最后几台在线，翻页仍然正确。
func TestHostsOnlineFilterSpansBatches(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner := r.verified()
	team := r.seedTeam("batches", map[string]string{owner.accountID: "owner"})
	r.exec(`INSERT INTO sources (tenant_id, source_id, name, kind, public_jwk_json, core_version, capabilities_json, registered_at_ms, trusted_origins_json)
	  SELECT 'cloud', lpad(to_hex(n), 32, '0'), 'h' || n, 'desktop', '{}', '0.10.0', '[]', n, '[]' FROM generate_series(1, 450) n`)
	r.exec(`INSERT INTO source_spaces (tenant_id, source_id, team_id, owner_account_id, created_at_ms, updated_at_ms)
	  SELECT 'cloud', lpad(to_hex(n), 32, '0'), $1, $2, 1, 1 FROM generate_series(1, 450) n`, team, owner.accountID)
	online := []string{}
	for _, n := range []int{250, 430, 449, 450} {
		id := fmt.Sprintf("%032x", n)
		online = append(online, id)
		r.run.Relay.SetRoute(id, "0.10.0")
	}
	got, cursor := []string{}, ""
	for pages := 0; pages < 5; pages++ {
		query := "/v1/sources?status=online&limit=3"
		if cursor != "" {
			query += "&cursor=" + url.QueryEscape(cursor)
		}
		res := r.do("GET", query, nil, owner.bearer())
		r.expect(res, 200, "")
		got = append(got, ids(res)...)
		if cursor, _ = res.json["nextCursor"].(string); cursor == "" {
			break
		}
	}
	if !reflect.DeepEqual(got, online) {
		t.Fatalf("在线的主机：%v，应为 %v", got, online)
	}
}

// 改名 / 标签 / 设置 / 可信来源：管理权、校验、局部合并、审计。
func TestHostsUpdate(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner, admin, member, other := r.verified(), r.verified(), r.verified(), r.verified()
	team := r.seedTeam("upd", map[string]string{owner.accountID: "owner", admin.accountID: "admin", member.accountID: "member", other.accountID: "member"})
	// 团队默认角色影响主机设置的预选值。
	r.exec("UPDATE teams SET settings = jsonb_set(settings, '{defaultHostRole}', '\"editor\"') WHERE team_id = $1", team)
	h := r.addHost(member, teamSpace(team), "0.10.0")

	res := r.do("PATCH", sourcePath(h.id), map[string]any{"name": " 新名字 ", "tags": []string{"gpu", "prod", "gpu"}}, member.bearer())
	r.expect(res, 200, "")
	if res.json["name"] != "新名字" || !reflect.DeepEqual(res.json["tags"], []any{"gpu", "prod"}) ||
		!reflect.DeepEqual(res.json["settings"], map[string]any{"linkCreators": "owner_admin", "allowGuests": true, "defaultRole": "editor"}) {
		t.Fatalf("改名与标签：%s", res.body)
	}
	// 设置逐项改，没给的项保持；团队 admin 可以改成员登记的主机。
	res = r.do("PATCH", sourcePath(h.id), map[string]any{"settings": map[string]any{"allowGuests": false}}, admin.bearer())
	r.expect(res, 200, "")
	res = r.do("PATCH", sourcePath(h.id), map[string]any{"settings": map[string]any{"linkCreators": "driver"}}, owner.bearer())
	r.expect(res, 200, "")
	if !reflect.DeepEqual(res.json["settings"], map[string]any{"linkCreators": "driver", "allowGuests": false, "defaultRole": "editor"}) {
		t.Fatalf("设置合并：%s", res.body)
	}
	res = r.do("PATCH", sourcePath(h.id), map[string]any{"trustedOrigins": []string{"https://App.Example.com/", "https://app.example.com", "http://localhost:3000"}}, member.bearer())
	r.expect(res, 200, "")
	if !reflect.DeepEqual(res.json["trustedOrigins"], []any{"https://app.example.com", "http://localhost:3000"}) {
		t.Fatalf("可信来源：%s", res.body)
	}
	// 空体不改任何东西；整组替换标签可清空。
	r.expect(r.do("PATCH", sourcePath(h.id), map[string]any{}, member.bearer()), 200, "")
	res = r.do("PATCH", sourcePath(h.id), map[string]any{"tags": []string{}}, member.bearer())
	r.expect(res, 200, "")
	if !reflect.DeepEqual(res.json["tags"], []any{}) {
		t.Fatalf("清标签：%s", res.body)
	}
	// 校验：非法标签、超 16 个、非法来源、空名字；失败不改任何东西。
	tooMany := []string{}
	for i := 0; i < 17; i++ {
		tooMany = append(tooMany, fmt.Sprintf("t%d", i))
	}
	for _, body := range []map[string]any{
		{"tags": []string{"Has Space"}}, {"tags": []string{"-bad"}}, {"tags": tooMany},
		{"trustedOrigins": []string{"https://x.com/path"}}, {"trustedOrigins": []string{"ftp://x.com"}},
		{"trustedOrigins": []string{"https://u:p@x.com"}}, {"trustedOrigins": []string{"not a url"}}, {"name": "  "},
		{"settings": map[string]any{"linkCreators": "everyone"}}, {"settings": map[string]any{"defaultRole": "root"}},
	} {
		r.expect(r.do("PATCH", sourcePath(h.id), body, member.bearer()), 400, "bad_request")
	}
	// 别的成员看不到这台：404；审计有 host.update，team_id 记团队。
	r.expect(r.do("PATCH", sourcePath(h.id), map[string]any{"name": "x"}, other.bearer()), 404, "not_found")
	if n := r.queryInt("SELECT count(*)::int FROM audit_log WHERE action = 'host.update' AND source_id = $1 AND team_id = $2", h.id, team); n < 5 {
		t.Fatalf("host.update 审计：%d", n)
	}
	// 已撤销的主机改不了。
	r.expect(r.do("DELETE", sourcePath(h.id), nil, member.bearer()), 200, "")
	r.expect(r.do("PATCH", sourcePath(h.id), map[string]any{"name": "x"}, member.bearer()), 404, "not_found")
}

func payloadOf(t *testing.T, jws string) map[string]any {
	t.Helper()
	parts := strings.Split(jws, ".")
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(data, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

// 隧道令牌：团队主机带 team、个人空间不带；节点来自节点表（rendezvous 前三、权重 3/2/1，排空的去掉），
// 表为空答 503；jti 记进签发索引。
func TestHostsHeartbeatHint(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner := r.verified()
	team := r.seedTeam("hint", map[string]string{owner.accountID: "owner"})
	hTeam := r.addHost(owner, teamSpace(team), "0.10.0")
	hPersonal := r.addHost(owner, nil, "0.10.0")

	r.expect(r.do("GET", "/v1/sources/me/relay", nil, r.sourceAuth(hTeam)), 503, "unavailable")
	for i, id := range []string{"a", "b", "c", "d", "e"} {
		r.run.Relay.SetNode(relayctl.Node{ID: id, URL: "wss://" + id + ".edge.example.com/t/v1", Weight: 1, Draining: id == "e"})
		_ = i
	}
	res := r.do("GET", "/v1/sources/me/relay", nil, r.sourceAuth(hTeam))
	r.expect(res, 200, "")
	nodes := res.json["nodes"].([]any)
	if len(nodes) != 3 {
		t.Fatalf("应答前三个节点：%s", res.body)
	}
	for i, n := range nodes {
		m := n.(map[string]any)
		if m["weight"] != float64(3-i) || strings.Contains(m["url"].(string), "//e.") {
			t.Fatalf("节点：%s", res.body)
		}
	}
	claims := payloadOf(t, res.json["tunnelToken"].(string))
	if claims["team"] != team || claims["aud"] != gen.TokenAudienceTunnel || claims["sub"] != hTeam.id {
		t.Fatalf("团队主机的隧道令牌：%v", claims)
	}
	if jti, _ := claims["jti"].(string); !strings.HasPrefix(jti, "t."+hTeam.id) {
		t.Fatalf("jti：%v", claims["jti"])
	}
	res = r.do("GET", "/v1/sources/me/relay", nil, r.sourceAuth(hPersonal))
	r.expect(res, 200, "")
	if _, has := payloadOf(t, res.json["tunnelToken"].(string))["team"]; has {
		t.Fatal("个人空间的隧道令牌不带 team")
	}
	// 同一个源两次的节点顺序一致。
	again := r.do("GET", "/v1/sources/me/relay", nil, r.sourceAuth(hPersonal))
	if !reflect.DeepEqual(res.json["nodes"], again.json["nodes"]) {
		t.Fatal("同一个源的节点顺序应稳定")
	}
	// 没有或伪造的源认证。
	r.expect(r.do("GET", "/v1/sources/me/relay", nil, nil), 401, "unauthenticated")
	forged := newTestHost(t)
	forged.id = hTeam.id
	r.expect(r.do("GET", "/v1/sources/me/relay", nil, r.sourceAuth(forged)), 401, "unauthenticated")
}

// 撤销：置撤销时刻、该源已签令牌（含隧道令牌）进撤销表、关隧道并踢人、审计；之后各处都看不到。
func TestHostsRevoke(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	owner, admin, member, other := r.verified(), r.verified(), r.verified(), r.verified()
	team := r.seedTeam("rev", map[string]string{owner.accountID: "owner", admin.accountID: "admin", member.accountID: "member", other.accountID: "member"})
	r.run.Relay.SetNode(relayctl.Node{ID: "a", URL: "wss://a.edge.example.com/t/v1", Weight: 1})
	h := r.addHost(member, teamSpace(team), "0.10.0")
	untouched := r.addHost(other, teamSpace(team), "0.10.0")
	var jtis []string
	for _, host := range []testHost{h, untouched} {
		res := r.do("GET", "/v1/sources/me/relay", nil, r.sourceAuth(host))
		r.expect(res, 200, "")
		jtis = append(jtis, payloadOf(t, res.json["tunnelToken"].(string))["jti"].(string))
	}
	// 别的成员动不了。
	r.expect(r.do("DELETE", sourcePath(h.id), nil, other.bearer()), 404, "not_found")
	if len(r.run.Relay.RevokedTokens()) != 0 {
		t.Fatal("没权限的撤销不该撤任何令牌")
	}
	r.run.Relay.SetRoute(h.id, "0.10.0")
	// 团队 admin 撤销成员登记的主机。
	r.expect(r.do("DELETE", sourcePath(h.id), nil, admin.bearer()), 200, "")
	if !r.run.Relay.IsRevoked(jtis[0]) || r.run.Relay.IsRevoked(jtis[1]) {
		t.Fatalf("只撤销这台的令牌：%+v", r.run.Relay.RevokedTokens())
	}
	var closed, kicked bool
	for _, c := range r.run.Relay.Commands() {
		closed = closed || (c.Type == relayctl.CommandCloseTunnel && c.SourceID == h.id)
		kicked = kicked || (c.Type == relayctl.CommandKick && c.SourceID == h.id && c.Subject == "")
		if c.SourceID != h.id {
			t.Fatalf("不该动别的主机：%+v", c)
		}
	}
	if !closed || !kicked {
		t.Fatalf("应关隧道并踢人：%+v", r.run.Relay.Commands())
	}
	if n := r.queryInt("SELECT count(*)::int FROM audit_log WHERE action = 'host.revoke' AND source_id = $1 AND actor_id = $2 AND team_id = $3", h.id, admin.accountID, team); n != 1 {
		t.Fatal("应有 host.revoke 审计")
	}
	// 撤销后：get / 再撤 / 改都是 404，心跳 410，默认列表没有、status=revoked 里有（所有者与 admin 看得到）。
	r.expect(r.do("GET", sourcePath(h.id), nil, member.bearer()), 404, "not_found")
	r.expect(r.do("DELETE", sourcePath(h.id), nil, member.bearer()), 404, "not_found")
	r.expect(r.do("GET", "/v1/sources/me/relay", nil, r.sourceAuth(h)), 410, "source_revoked")
	res := r.do("GET", "/v1/sources?status=revoked", nil, member.bearer())
	r.expect(res, 200, "")
	if got := ids(res); !reflect.DeepEqual(got, []string{h.id}) {
		t.Fatalf("已撤销列表：%v", got)
	}
	// 撤销释放名额：Free 团队 3 台，撤一台后又能登记。
	for i := 0; i < 2; i++ {
		r.addHost(owner, teamSpace(team), "0.10.0")
	}
	r.expect(r.hostToken(owner, teamSpace(team)), 429, "limit_reached")
	r.expect(r.do("DELETE", sourcePath(untouched.id), nil, other.bearer()), 200, "")
	r.expect(r.hostToken(owner, teamSpace(team)), 200, "")
}

// 个人空间的撤销：所有者本人；别的账号 404。
func TestHostsRevokePersonal(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	a, b := r.verified(), r.verified()
	r.run.Relay.SetNode(relayctl.Node{ID: "a", URL: "wss://a.edge.example.com/t/v1", Weight: 1})
	h := r.addHost(a, nil, "0.10.0")
	res := r.do("GET", "/v1/sources/me/relay", nil, r.sourceAuth(h))
	r.expect(res, 200, "")
	jti := payloadOf(t, res.json["tunnelToken"].(string))["jti"].(string)
	r.expect(r.do("DELETE", sourcePath(h.id), nil, b.bearer()), 404, "not_found")
	r.expect(r.do("DELETE", sourcePath(h.id), nil, a.bearer()), 200, "")
	if !r.run.Relay.IsRevoked(jti) {
		t.Fatal("撤销后隧道令牌应进撤销表")
	}
	if n := r.queryInt("SELECT count(*)::int FROM audit_log WHERE action = 'host.revoke' AND source_id = $1 AND team_id IS NULL", h.id); n != 1 {
		t.Fatal("个人空间撤销的审计 team_id 为空")
	}
}

// 每个账号同时有效的注册令牌有上限，按账号计、不互相挤占。
func TestHostsRegistrationTokenCapPerAccount(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOptions{})
	a, b := r.verified(), r.verified()
	r.exec(`INSERT INTO registration_tokens (tenant_id, token_hash, account_id, expires_at_ms)
	  SELECT 'cloud', 'h' || n, $1, $2 FROM generate_series(1, 20) n`, a.accountID, r.clock.Ms()+600000)
	r.expect(r.hostToken(a, nil), 429, "limit_reached")
	r.expect(r.hostToken(b, nil), 200, "")
	// 过期的不占名额。
	r.exec("UPDATE registration_tokens SET expires_at_ms = 1 WHERE token_hash = 'h1'")
	r.expect(r.hostToken(a, nil), 200, "")
}
