package server_test

import (
	"fmt"
	"net/url"
	"testing"
	"time"
)

func spaceQuery(kind, team string) string {
	q := url.Values{"space[kind]": {kind}}
	if team != "" {
		q.Set("space[teamId]", team)
	}
	return q.Encode()
}

func (r *rig) traffic(at int64, sourceID, team, kind string, bytes int64) {
	r.t.Helper()
	r.exec(`INSERT INTO relay_traffic_buckets (tenant_id, bucket_start_ms, node, source_id, subject, kind, team_id, bytes_to_source, bytes_to_client)
	  VALUES ('cloud', $1, 'n', $2, 'a', $3, $4, $5, $5)`, at/3600000*3600000, sourceID, kind, team, bytes)
}

func usageMap(t *testing.T, res response) map[string][2]any {
	t.Helper()
	out := map[string][2]any{}
	for _, e := range res.json["usage"].([]any) {
		entry := e.(map[string]any)
		out[entry["metric"].(string)] = [2]any{entry["value"], entry["limit"]}
	}
	return out
}

// billing.plan：个人空间本人看；团队空间只有 owner 与 billing，别的角色 403、非成员 404。
func TestBillingPlanSpacesAndRoles(t *testing.T) {
	r := newRig(t, rigOptions{})
	owner, billing, admin, member, outsider := r.verified(), r.verified(), r.verified(), r.verified(), r.verified()
	team := r.seedTeam("bill", map[string]string{owner.accountID: "owner", billing.accountID: "billing", admin.accountID: "admin", member.accountID: "member"})
	r.exec(`INSERT INTO entitlements (tenant_id, space_kind, space_id, plan, status, updated_at_ms) VALUES ('cloud', 'team', $1, 'pro', 'trialing', 1)`, team)
	monthEnd := time.Date(r.clock.Now().UTC().Year(), r.clock.Now().UTC().Month()+1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()

	// 个人空间：没有 entitlements 行 = free / active，席位 1，周期到本月底（UTC）。
	res := r.do("GET", "/v1/billing/plan?"+spaceQuery("personal", ""), nil, owner.bearer())
	r.expect(res, 200, "")
	if res.json["plan"] != "free" || res.json["status"] != "active" || res.json["seats"] != float64(1) || res.json["periodEndsAtMs"] != float64(monthEnd) {
		t.Fatalf("个人计划：%s", res.body)
	}
	// 团队空间：计划与状态来自 entitlements，席位 = 非 guest 成员。
	for _, who := range []session{owner, billing} {
		res = r.do("GET", "/v1/billing/plan?"+spaceQuery("team", team), nil, who.bearer())
		r.expect(res, 200, "")
		if res.json["plan"] != "pro" || res.json["status"] != "trialing" || res.json["seats"] != float64(4) {
			t.Fatalf("团队计划：%s", res.body)
		}
		if sp := res.json["space"].(map[string]any); sp["kind"] != "team" || sp["teamId"] != team {
			t.Fatalf("space：%s", res.body)
		}
	}
	for _, who := range []session{admin, member} {
		r.expect(r.do("GET", "/v1/billing/plan?"+spaceQuery("team", team), nil, who.bearer()), 403, "team_role_insufficient")
	}
	r.expect(r.do("GET", "/v1/billing/plan?"+spaceQuery("team", team), nil, outsider.bearer()), 404, "not_found")
	r.expect(r.do("GET", "/v1/billing/plan?"+spaceQuery("team", "nope"), nil, owner.bearer()), 404, "not_found")
	r.expect(r.do("GET", "/v1/billing/plan?"+spaceQuery("personal", ""), nil, nil), 401, "unauthenticated")
	// 别人的个人空间读不到：personal 永远是调用者自己的。
	other := r.do("GET", "/v1/billing/plan?"+spaceQuery("personal", ""), nil, outsider.bearer())
	r.expect(other, 200, "")
	// 其余 billing 过程仍是 501。
	r.expect(r.do("POST", "/v1/billing/portal", map[string]any{"space": map[string]any{"kind": "personal"}}, owner.bearer()), 501, "not_implemented")
}

// billing.usage：个人与团队的用量对照额度，按天序列补零，period 取别的月份；团队的与 teams.usage 同一个统计。
func TestBillingUsageMonthAndDay(t *testing.T) {
	r := newRig(t, rigOptions{})
	owner, billing, member := r.verified(), r.verified(), r.verified()
	team := r.seedTeam("usage", map[string]string{owner.accountID: "owner", billing.accountID: "billing", member.accountID: "member"})
	personal := r.addHost(owner, nil, "0.10.0")
	teamHost := r.addHost(owner, teamSpace(team), "0.10.0")

	now := r.clock.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	days := int(monthStart.AddDate(0, 1, 0).Sub(monthStart) / (24 * time.Hour))
	day := func(i int) int64 { return monthStart.AddDate(0, 0, i).UnixMilli() }
	// 个人主机：第 0 天 3+3，第 2 天 4+4；http 不计；团队流量不计入个人。
	r.traffic(day(0), personal.id, "", "e2e", 3)
	r.traffic(day(0)+7200000, personal.id, "", "e2e", 0)
	r.traffic(day(2), personal.id, "", "e2e", 4)
	r.traffic(day(2), personal.id, "", "http", 99)
	r.traffic(day(1), teamHost.id, team, "e2e", 100)
	prev := monthStart.AddDate(0, -1, 0)
	r.traffic(prev.UnixMilli(), personal.id, "", "e2e", 50)

	res := r.do("GET", "/v1/billing/usage?"+spaceQuery("personal", ""), nil, owner.bearer())
	r.expect(res, 200, "")
	got := usageMap(t, res)
	if got["hosts"] != [2]any{float64(1), float64(2)} || got["teams"] != [2]any{float64(1), float64(1)} || got["traffic"] != [2]any{float64(14), float64(5 << 30)} {
		t.Fatalf("个人用量：%v", got)
	}
	if res.json["period"] != monthStart.Format("2006-01") || res.json["series"] != nil {
		t.Fatalf("月粒度没有 series：%s", res.body)
	}

	res = r.do("GET", "/v1/billing/usage?"+spaceQuery("personal", "")+"&granularity=day", nil, owner.bearer())
	r.expect(res, 200, "")
	series := res.json["series"].([]any)
	if len(series) != days {
		t.Fatalf("本周期每天一条：%d / %d", len(series), days)
	}
	want := map[int]float64{0: 6, 2: 8}
	var sum float64
	for i, p := range series {
		point := p.(map[string]any)
		if point["startMs"] != float64(day(i)) || point["relayBytes"] != want[i] {
			t.Fatalf("第 %d 天：%v", i, point)
		}
		sum += point["relayBytes"].(float64)
	}
	if sum != 14 {
		t.Fatalf("按天之和等于月用量：%v", sum)
	}

	// 上个月：只有那一条。
	res = r.do("GET", "/v1/billing/usage?"+spaceQuery("personal", "")+"&period="+prev.Format("2006-01"), nil, owner.bearer())
	r.expect(res, 200, "")
	if usageMap(t, res)["traffic"][0] != float64(100) || res.json["period"] != prev.Format("2006-01") {
		t.Fatalf("上月用量：%s", res.body)
	}
	r.expect(r.do("GET", "/v1/billing/usage?"+spaceQuery("personal", "")+"&period=2026-13", nil, owner.bearer()), 400, "bad_request")

	// 团队：与 teams.usage 同一组统计；按天只含本团队的流量。
	for _, who := range []session{owner, billing} {
		res = r.do("GET", "/v1/billing/usage?"+spaceQuery("team", team)+"&granularity=day", nil, who.bearer())
		r.expect(res, 200, "")
		got = usageMap(t, res)
		if got["hosts"] != [2]any{float64(1), float64(3)} || got["members"] != [2]any{float64(3), float64(3)} || got["traffic"] != [2]any{float64(200), float64(5 << 30)} || got["concurrent"][1] != float64(10) {
			t.Fatalf("团队用量：%v", got)
		}
		teamSeries := res.json["series"].([]any)
		if len(teamSeries) != days || teamSeries[1].(map[string]any)["relayBytes"] != float64(200) || teamSeries[0].(map[string]any)["relayBytes"] != float64(0) {
			t.Fatalf("团队按天：%s", res.body)
		}
	}
	teamUsage := r.do("GET", teamPath(team, "usage"), nil, owner.bearer())
	r.expect(teamUsage, 200, "")
	billingUsage := r.do("GET", "/v1/billing/usage?"+spaceQuery("team", team), nil, owner.bearer())
	if fmt.Sprint(teamUsage.json["usage"]) != fmt.Sprint(billingUsage.json["usage"]) {
		t.Fatalf("teams.usage 与 billing.usage 同一组统计：%v / %v", teamUsage.json["usage"], billingUsage.json["usage"])
	}
	r.expect(r.do("GET", "/v1/billing/usage?"+spaceQuery("team", team), nil, member.bearer()), 403, "team_role_insufficient")
}
