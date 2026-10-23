package grants_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/grants"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/pgtest"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/postgres"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

const tenant = "cloud"

// 同一批数据（主体 × 团队角色 × 范围 × 到期 × 撤销 × 恢复期）：对每个查看者分别问目录（ListHosts 的 SQL）与
// AccessBasis + Compose。非团队 owner / admin 时「看得到 == 有效权限非空」；团队 owner / admin 时「看得到 ⊇ 有效权限非空」
// （他们看得到全部团队主机，拿断言仍要有效权限）。
func TestDirectoryVisibilityMatchesEffectivePermissions(t *testing.T) {
	url, schema := pgtest.Schema(t)
	ctx := context.Background()
	db, err := postgres.Open(ctx, postgres.Options{URL: url, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.CreateTenant(ctx, tenant, store.Tenant{TenantSettings: store.TenantSettings{Issuer: "https://x.test", TrustedOrigins: []string{}}}); err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Pool().Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	// 账号：团队角色各一个，加第二个 member、陌生人。
	roles := map[string]string{"owner": "owner", "admin": "admin", "member": "member", "member2": "member", "billing": "billing", "guest": "guest"}
	accounts := []string{"stranger"}
	for account := range roles {
		accounts = append(accounts, account)
	}
	for _, id := range accounts {
		exec(`INSERT INTO accounts (tenant_id, account_id, name, password_json, failures, locked_until_ms, last_failure_at_ms, created_at_ms, updated_at_ms, email)
		  VALUES ($1, $2, $2, '{}', 0, 0, 0, 1, 1, $2 || '@x.test')`, tenant, id)
	}
	for _, team := range []string{"live", "gone"} {
		tx, err := db.Pool().Begin(ctx) // 团队与 owner 成员要同事务（约束在提交时检查）
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO teams (tenant_id, team_id, slug, name, settings, created_by, created_at_ms, updated_at_ms) VALUES ($1, $2, $2, $2, '{}', 'x', 1, 1)`, tenant, team); err != nil {
			t.Fatal(err)
		}
		for account, role := range roles {
			if _, err := tx.Exec(ctx, `INSERT INTO team_members (tenant_id, team_id, account_id, role, joined_at_ms) VALUES ($1, $2, $3, $4, 1)`, tenant, team, account, role); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	exec("UPDATE teams SET deleted_at_ms = 1, purge_at_ms = 2 WHERE team_id = 'gone'") // 恢复期
	const nowMs, future, past = int64(1_000_000), int64(2_000_000), int64(500_000)

	hostN := 0
	var hostIDs []string
	newHost := func(teamID, owner string, revoked bool) string {
		hostN++
		id := fmt.Sprintf("h%04d", hostN)
		err := db.SaaS().Tx(ctx, func(tx saas.Tx) error {
			_, err := tx.Ports().Sources.Put(ctx, tenant, id, func(*store.SourceRecord) (store.SourceRecord, error) {
				rec := store.SourceRecord{SourceID: id, Name: id, Kind: "desktop", CoreVersion: "0.11.0", Capabilities: []string{}, RegisteredAtMs: int64(hostN), TrustedOrigins: []string{}}
				if revoked {
					at := int64(5)
					rec.RevokedAtMs = &at
				}
				return rec, nil
			})
			if err != nil {
				return err
			}
			return tx.Hosts().UpsertSpace(ctx, tenant, saas.HostSpace{SourceID: id, TeamID: teamID, OwnerAccountID: owner}, 1)
		})
		if err != nil {
			t.Fatal(err)
		}
		hostIDs = append(hostIDs, id)
		return id
	}
	grantN := 0
	addGrant := func(source, teamID, kind, subject string, ws []string, expires *int64, revoked bool) {
		grantN++
		var revokedAt *int64
		if revoked {
			at := int64(7)
			revokedAt = &at
		}
		err := db.SaaS().Tx(ctx, func(tx saas.Tx) error {
			if err := tx.Grants().Insert(ctx, tenant, saas.Grant{
				GrantID: fmt.Sprintf("g%05d", grantN), SourceID: source, TeamID: teamID, SubjectKind: kind, SubjectID: subject, Role: "viewer",
				WorkspaceIDs: ws, ExpiresAtMs: expires, GrantedBy: "x", Origin: saas.GrantOriginGrant, CreatedAtMs: 1,
			}); err != nil {
				return err
			}
			if revoked {
				_, err := tx.Grants().Revoke(ctx, tenant, fmt.Sprintf("g%05d", grantN), "x", *revokedAt)
				return err
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	expiries := map[string]*int64{"never": nil, "future": ptr(future), "past": ptr(past)}
	scopes := [][]string{nil, {"w1"}, {"w1", "w2"}}

	i := 0
	for _, teamID := range []string{"live", "gone"} {
		// 登记者各角色：所有者路径。
		for _, owner := range accounts {
			newHost(teamID, owner, false)
		}
		newHost("live", "member", true) // 已撤销的主机
		// 每个账号 × 授予种类 × 到期 × 撤销 × 范围：一台主机一条授予。
		for _, subject := range accounts {
			for _, kind := range []string{"member", "guest"} {
				for name, exp := range expiries {
					for _, revoked := range []bool{false, true} {
						if teamID == "gone" && (revoked || name == "past") {
							continue
						}
						h := newHost(teamID, "member2", false)
						addGrant(h, teamID, kind, subject, scopes[i%len(scopes)], exp, revoked)
						i++
					}
				}
			}
		}
		// team 授予（全员）× 到期 × 撤销。
		for name, exp := range expiries {
			for _, revoked := range []bool{false, true} {
				h := newHost(teamID, "member2", false)
				addGrant(h, teamID, "team", teamID, scopes[i%len(scopes)], exp, revoked)
				i++
				_ = name
			}
		}
	}
	// 个人空间。
	for _, owner := range accounts {
		newHost("", owner, false)
	}
	for _, subject := range accounts {
		for name, exp := range expiries {
			for _, revoked := range []bool{false, true} {
				h := newHost("", "owner", false)
				addGrant(h, "", "guest", subject, scopes[i%len(scopes)], exp, revoked)
				i++
				_ = name
			}
		}
	}

	reader := db.SaaS().Read()
	checked, visibleCount, effectiveCount := 0, 0, 0
	for _, viewer := range accounts {
		listed := map[string]bool{}
		for after := (*saas.HostCursor)(nil); ; {
			rows, err := reader.ListHosts(ctx, tenant, saas.HostFilter{ViewerID: viewer, NowMs: nowMs, After: after, Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			for _, h := range rows {
				listed[h.SourceID] = true
			}
			if len(rows) < 100 {
				break
			}
			last := rows[len(rows)-1]
			after = &saas.HostCursor{RegisteredAtMs: last.RegisteredAtMs, SourceID: last.SourceID}
		}
		for _, id := range hostIDs {
			basis, err := reader.AccessBasis(ctx, tenant, viewer, id)
			if err != nil {
				t.Fatal(err)
			}
			eff := grants.Compose(basis, viewer, nowMs)
			staff := basis.TeamID != "" && !basis.TeamDeleted && !basis.SourceRevoked && (basis.TeamRole == saas.RoleOwner || basis.TeamRole == saas.RoleAdmin)
			visible := listed[id]
			checked++
			if visible {
				visibleCount++
			}
			if !eff.None() {
				effectiveCount++
			}
			switch {
			case staff && !eff.None() && !visible:
				t.Errorf("%s 对 %s：有有效权限却看不到", viewer, id)
			case !staff && visible != !eff.None():
				t.Errorf("%s（团队角色 %q）对 %s：看得到 = %v，有效权限 = %q", viewer, basis.TeamRole, id, visible, eff.Role)
			case staff && visible != !basis.SourceRevoked:
				// 团队 owner / admin 看得到全部未撤销的团队主机。
				t.Errorf("%s 是团队 %q，对 %s：看得到 = %v", viewer, basis.TeamRole, id, visible)
			}
		}
	}
	if checked < 1000 || visibleCount == 0 || effectiveCount == 0 {
		t.Fatalf("数据量或覆盖不够：检查 %d 对，看得到 %d，有效权限非空 %d", checked, visibleCount, effectiveCount)
	}
}
