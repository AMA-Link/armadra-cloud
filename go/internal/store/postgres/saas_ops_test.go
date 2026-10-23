package postgres_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

func TestAdminListsSearchAndPaging(t *testing.T) {
	e := openSaaS(t)
	for i, id := range []string{"a1", "a2", "a3"} {
		e.account(id)
		e.must("UPDATE accounts SET display_name = $2, created_at_ms = $3 WHERE account_id = $1", id, "用户"+id, int64(100+i))
	}
	e.must("UPDATE accounts SET status = 'disabled' WHERE account_id = 'a2'")
	e.team("t1", "a1")
	e.team("t2", "a2")
	e.must("UPDATE teams SET name = '甲_队', created_at_ms = 200 WHERE team_id = 't1'")
	e.must("UPDATE teams SET name = '乙队', status = 'suspended', created_at_ms = 150 WHERE team_id = 't2'")
	e.must(`INSERT INTO entitlements (tenant_id, space_kind, space_id, plan, status, updated_at_ms) VALUES ($1, 'team', 't1', 'pro', 'active', 1)`, saasTenant)
	e.source("s1")
	e.source("s2")
	e.must(`INSERT INTO source_spaces (tenant_id, source_id, team_id, owner_account_id, created_at_ms, updated_at_ms) VALUES ($1, 's1', 't1', 'a1', 1, 1), ($1, 's2', NULL, 'a1', 1, 1)`, saasTenant)
	e.must(`INSERT INTO devices (tenant_id, device_id, subject, platform, name, refresh_hash, rotation, created_at_ms, expires_at_ms, last_seen_at_ms)
	  VALUES ($1, 'd1', 'a1', 'web', 'x', 'h', 1, 1, 9, 77)`, saasTenant)
	s := e.db.SaaS().Read()

	accounts, err := s.AdminAccounts(e.ctx, saasTenant, saas.AdminQuery{})
	if err != nil || len(accounts) != 3 || accounts[0].AccountID != "a3" || accounts[2].AccountID != "a1" {
		t.Fatalf("按创建时间倒序：%+v %v", accounts, err)
	}
	a1 := accounts[2]
	if a1.HostCount != 2 || a1.TeamCount != 1 || a1.Plan != "free" || a1.LastSeenAtMs == nil || *a1.LastSeenAtMs != 77 || a1.Email == nil || *a1.Email != "a1@x.test" {
		t.Fatalf("a1 的聚合：%+v", a1)
	}
	if got, _ := s.AdminAccounts(e.ctx, saasTenant, saas.AdminQuery{Status: "disabled"}); len(got) != 1 || got[0].AccountID != "a2" {
		t.Fatalf("按状态：%+v", got)
	}
	if got, _ := s.AdminAccounts(e.ctx, saasTenant, saas.AdminQuery{Q: "A3@X"}); len(got) != 1 || got[0].AccountID != "a3" {
		t.Fatalf("邮箱包含、不分大小写：%+v", got)
	}
	if got, _ := s.AdminAccounts(e.ctx, saasTenant, saas.AdminQuery{Q: "%"}); len(got) != 0 {
		t.Fatalf("通配符按字面量：%+v", got)
	}
	page1, _ := s.AdminAccounts(e.ctx, saasTenant, saas.AdminQuery{Limit: 2})
	page2, _ := s.AdminAccounts(e.ctx, saasTenant, saas.AdminQuery{Limit: 2, After: &saas.AdminCursor{CreatedAtMs: page1[1].CreatedAtMs, ID: page1[1].AccountID}})
	if len(page1) != 2 || len(page2) != 1 || page2[0].AccountID != "a1" {
		t.Fatalf("游标翻页：%+v %+v", page1, page2)
	}
	cursor := saas.AdminCursor{CreatedAtMs: 5, ID: "x"}
	if back, err := saas.DecodeAdminCursor(cursor.Encode()); err != nil || *back != cursor {
		t.Fatalf("游标往返：%v %v", back, err)
	}
	if _, err := saas.DecodeAdminCursor("??"); err == nil {
		t.Fatal("坏游标要拒绝")
	}

	e.must(`INSERT INTO relay_traffic_buckets (tenant_id, bucket_start_ms, node, source_id, subject, kind, team_id, bytes_to_source, bytes_to_client)
	  VALUES ($1, 3600000, 'n', 's1', 'a', 'e2e', 't1', 5, 6), ($1, 3600000, 'n', 's2', 'a', 'e2e', '', 2, 2)`, saasTenant)
	detail, err := s.AdminAccount(e.ctx, saasTenant, "a1", 0, 86400000)
	if err != nil || detail == nil || detail.RelayBytesMonth != 4 {
		t.Fatalf("账号详情带本月流量：%+v %v", detail, err)
	}
	if none, err := s.AdminAccount(e.ctx, saasTenant, "nobody", 0, 1); err != nil || none != nil {
		t.Fatalf("没有的账号答 nil：%+v %v", none, err)
	}

	teams, err := s.AdminTeams(e.ctx, saasTenant, saas.AdminQuery{})
	if err != nil || len(teams) != 2 || teams[0].TeamID != "t1" || teams[0].Plan != "pro" || teams[0].HostCount != 1 || teams[0].MemberCount != 1 || teams[1].Status != "suspended" {
		t.Fatalf("团队：%+v %v", teams, err)
	}
	if got, _ := s.AdminTeams(e.ctx, saasTenant, saas.AdminQuery{Q: "甲_"}); len(got) != 1 || got[0].TeamID != "t1" {
		t.Fatalf("下划线按字面量：%+v", got)
	}
	if got, _ := s.AdminTeams(e.ctx, saasTenant, saas.AdminQuery{Status: "suspended"}); len(got) != 1 || got[0].TeamID != "t2" {
		t.Fatalf("团队按状态：%+v", got)
	}
	team, err := s.AdminTeam(e.ctx, saasTenant, "t1", 0, 86400000)
	if err != nil || team == nil || team.Seats != 1 || team.RelayBytesMonth != 11 {
		t.Fatalf("团队详情：%+v %v", team, err)
	}

	tenants, err := s.AdminTenants(e.ctx, saasTenant, saas.AdminQuery{})
	var kinds []string
	for _, row := range tenants {
		kinds = append(kinds, row.Kind+":"+row.ID)
	}
	if err != nil || strings.Join(kinds, ",") != "team:t1,team:t2,account:a3,account:a2,account:a1" {
		t.Fatalf("账号与团队混排：%v %v", kinds, err)
	}
	if got, _ := s.AdminTenants(e.ctx, saasTenant, saas.AdminQuery{Q: "t-t2"}); len(got) != 1 || got[0].ID != "t2" || got[0].Status != "suspended" {
		t.Fatalf("按 slug 搜：%+v", got)
	}
	if got, _ := s.AdminTenants(e.ctx, saasTenant, saas.AdminQuery{Limit: 2}); len(got) != 2 {
		t.Fatalf("limit：%+v", got)
	}
	// 别的租户的行不出现。
	e.must("INSERT INTO tenants (tenant_id, issuer, allow_guests, trusted_origins_json, revision) VALUES ('other', 'https://o.test', false, '[]', 0)")
	if got, _ := s.AdminTenants(e.ctx, "other", saas.AdminQuery{}); len(got) != 0 {
		t.Fatalf("别的租户看不到：%+v", got)
	}
}

func TestOpsWritesAndIncidentMerge(t *testing.T) {
	e := openSaaS(t)
	e.account("a1")
	e.team("t1", "a1")
	e.source("s1")
	e.source("s2")
	e.must(`INSERT INTO source_spaces (tenant_id, source_id, team_id, owner_account_id, created_at_ms, updated_at_ms) VALUES ($1, 's1', 't1', 'a1', 1, 1), ($1, 's2', 't1', 'a1', 1, 1)`, saasTenant)
	e.must("UPDATE sources SET revoked_at_ms = 5 WHERE source_id = 's2'")
	for _, id := range []string{"d1", "d2"} {
		e.must(`INSERT INTO devices (tenant_id, device_id, subject, platform, name, refresh_hash, rotation, created_at_ms, expires_at_ms)
		  VALUES ($1, $2, 'a1', 'web', 'x', 'h', 1, 1, 9)`, saasTenant, id)
	}
	s := e.db.SaaS()
	yes, no := true, false
	err := s.Tx(e.ctx, func(tx saas.Tx) error {
		ops := tx.Ops()
		if ok, err := ops.SetTeamStatus(e.ctx, saasTenant, "t1", "suspended", strPtr("违规"), 10); err != nil || !ok {
			t.Fatalf("停用团队：%v %v", ok, err)
		}
		if ok, _ := ops.SetTeamStatus(e.ctx, saasTenant, "nope", "suspended", nil, 10); ok {
			t.Fatal("没有的团队答 false")
		}
		if hosts, err := ops.TeamHostIDs(e.ctx, saasTenant, "t1"); err != nil || fmt.Sprint(hosts) != "[s1]" {
			t.Fatalf("未撤销的团队主机：%v %v", hosts, err)
		}
		if ok, err := ops.SetAccountStatus(e.ctx, saasTenant, "a1", "disabled", strPtr("滥用"), 11); err != nil || !ok {
			t.Fatalf("停用账号：%v %v", ok, err)
		}
		if n, err := ops.RevokeDevices(e.ctx, saasTenant, "a1", 12); err != nil || n != 2 {
			t.Fatalf("撤销设备：%d %v", n, err)
		}
		if n, _ := ops.RevokeDevices(e.ctx, saasTenant, "a1", 13); n != 0 {
			t.Fatal("已撤销的不重复撤")
		}
		for i := 0; i < 2; i++ { // 重复加不重复
			if ok, err := ops.AddScope(e.ctx, saasTenant, "a1", "operator", 14); err != nil || !ok {
				t.Fatalf("加范围：%v %v", ok, err)
			}
		}
		if ok, _ := ops.AddScope(e.ctx, saasTenant, "nobody", "operator", 14); ok {
			t.Fatal("没有的账号答 false")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.count("SELECT count(*)::int FROM accounts WHERE account_id = 'a1' AND status = 'disabled' AND status_reason = '滥用' AND scopes = '{operator}'") != 1 ||
		e.count("SELECT count(*)::int FROM teams WHERE team_id = 't1' AND status = 'suspended' AND status_reason = '违规'") != 1 {
		t.Fatal("状态、原因与范围落库")
	}

	// 事故开关：部分更新，两项各改一项互不覆盖。
	if _, at, err := s.Read().IncidentAt(e.ctx, saasTenant); err != nil || at != nil {
		t.Fatalf("没写过：%v %v", at, err)
	}
	apply := func(patch saas.IncidentPatch, now int64) saas.Incident {
		var got saas.Incident
		if err := s.Tx(e.ctx, func(tx saas.Tx) (err error) {
			got, err = tx.Ops().UpdateIncident(e.ctx, saasTenant, patch, "a1", now)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := apply(saas.IncidentPatch{AssertionsPaused: &yes}, 20); got != (saas.Incident{AssertionsPaused: true}) {
		t.Fatalf("只开断言：%+v", got)
	}
	if got := apply(saas.IncidentPatch{RejectNewConnections: &yes}, 21); got != (saas.Incident{AssertionsPaused: true, RejectNewConnections: true}) {
		t.Fatalf("再开拒绝新连接，不覆盖前一项：%+v", got)
	}
	if got := apply(saas.IncidentPatch{AssertionsPaused: &no, RegistrationsPaused: &yes}, 22); got != (saas.Incident{RegistrationsPaused: true, RejectNewConnections: true}) {
		t.Fatalf("一次改两项：%+v", got)
	}
	incident, at, err := s.Read().IncidentAt(e.ctx, saasTenant)
	if err != nil || at == nil || *at != 22 || !incident.RegistrationsPaused {
		t.Fatalf("读回：%+v %v %v", incident, at, err)
	}
	if plain, err := s.Read().Incident(e.ctx, saasTenant); err != nil || plain != incident {
		t.Fatalf("Incident 与 IncidentAt 一致：%+v %v", plain, err)
	}
}

func strPtr(s string) *string { return &s }

func TestTOTPStepIsAcceptedOnce(t *testing.T) {
	e := openSaaS(t)
	e.account("a1")
	s := e.db.SaaS()
	if c, err := s.Read().TOTP(e.ctx, saasTenant, "a1"); err != nil || c != nil {
		t.Fatalf("没登记：%+v %v", c, err)
	}
	accept := func(step int64) bool {
		var ok bool
		if err := s.Tx(e.ctx, func(tx saas.Tx) (err error) {
			ok, err = tx.Ops().AcceptTOTPStep(e.ctx, saasTenant, "a1", step, 1)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if accept(10) {
		t.Fatal("没登记不能接受")
	}
	if err := s.Tx(e.ctx, func(tx saas.Tx) error {
		return tx.Ops().PutTOTP(e.ctx, saasTenant, "a1", "c1", "aes256gcm-v1:xxx", 1)
	}); err != nil {
		t.Fatal(err)
	}
	cred, err := s.Read().TOTP(e.ctx, saasTenant, "a1")
	if err != nil || cred == nil || cred.CredentialID != "c1" || cred.SecretWrap != "aes256gcm-v1:xxx" || cred.LastStep != nil {
		t.Fatalf("登记：%+v %v", cred, err)
	}
	if ok, err := s.Read().HasCredential(e.ctx, saasTenant, "a1", "totp"); err != nil || !ok {
		t.Fatalf("HasCredential：%v %v", ok, err)
	}
	if !accept(100) || accept(100) || accept(99) || !accept(101) {
		t.Fatal("同一步、更早的步都不再接受，更晚的接受")
	}
	// 重新登记（换种子）清掉旧的步。
	if err := s.Tx(e.ctx, func(tx saas.Tx) error {
		return tx.Ops().PutTOTP(e.ctx, saasTenant, "a1", "c2", "aes256gcm-v1:yyy", 2)
	}); err != nil {
		t.Fatal(err)
	}
	if e.count("SELECT count(*)::int FROM account_credentials WHERE kind = 'totp'") != 1 || !accept(50) {
		t.Fatal("一个账号至多一个 TOTP，重新登记后步数重新计")
	}
}

func TestOperatorActionsAreScopedToTarget(t *testing.T) {
	e := openSaaS(t)
	auditSeed(t, e, 1_700_000_000_000)
	e.must(`INSERT INTO audit_log (at_ms, event_id, tenant_id, actor_kind, actor_id, action, target_kind, target_id, reason)
	  VALUES (1700000000100, 'o2', $1, 'operator', 'op', 'account.enable', 'account', 'a1', '复核'), (1700000000200, 'o3', $1, 'operator', 'op', 'account.disable', 'account', 'a9', '别人')`, saasTenant)
	got, err := e.db.SaaS().Read().OperatorActions(e.ctx, saasTenant, "account", "a1", 5)
	if err != nil || ids(got) != "o2,e08," {
		t.Fatalf("a1 的运营动作（新的在前，只含运营）：%s %v", ids(got), err)
	}
}
