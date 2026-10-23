package postgres_test

import (
	"errors"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

func (e *saasEnv) trust(id, source, pending, team, account, device string, requestedAt, expiresAt int64) {
	e.t.Helper()
	err := e.db.SaaS().Tx(e.ctx, func(tx saas.Tx) error {
		return tx.Trust().Insert(e.ctx, saasTenant, saas.TrustRequest{
			RequestID: id, SourceID: source, PendingID: pending, TeamID: team, AccountID: account, DeviceID: device,
			DeviceName: "笔记本", Platform: "desktop", RequestedAtMs: requestedAt, ExpiresAtMs: expiresAt,
		})
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

// 待信任请求仓储：幂等键、同设备唯一的 pending、过期与取代、拒绝的冷却查询、联表视图只含团队成员与未撤销的主机。
func TestTrustRepo(t *testing.T) {
	e := openSaaS(t)
	for _, id := range []string{"owner", "u1", "u2", "ext"} {
		e.account(id)
	}
	e.team("t1", "owner")
	e.members("t1", map[string]string{"u1": "member", "u2": "guest"})
	e.seedHost("h1", "t1", "owner", 10)
	s := e.db.SaaS()
	r := s.Read()

	e.trust("r1", "h1", "p1", "t1", "u1", "d1", 100, 1000)
	// 同 (源, 条目 id) 与同 (源, 设备) 的 pending 都只能有一条。
	for _, dup := range []saas.TrustRequest{
		{RequestID: "r2", SourceID: "h1", PendingID: "p1", TeamID: "t1", AccountID: "u1", DeviceID: "d2", DeviceName: "x", Platform: "desktop", RequestedAtMs: 1, ExpiresAtMs: 2},
		{RequestID: "r3", SourceID: "h1", PendingID: "p3", TeamID: "t1", AccountID: "u1", DeviceID: "d1", DeviceName: "x", Platform: "desktop", RequestedAtMs: 1, ExpiresAtMs: 2},
	} {
		err := s.Tx(e.ctx, func(tx saas.Tx) error { return tx.Trust().Insert(e.ctx, saasTenant, dup) })
		if !errors.Is(err, saas.ErrConflict) {
			t.Fatalf("重复的 pending 应答冲突：%v", err)
		}
	}
	// 24 小时上限由 CHECK 守住。
	err := s.Tx(e.ctx, func(tx saas.Tx) error {
		return tx.Trust().Insert(e.ctx, saasTenant, saas.TrustRequest{
			RequestID: "r4", SourceID: "h1", PendingID: "p4", TeamID: "t1", AccountID: "u1", DeviceID: "d4", DeviceName: "x", Platform: "desktop",
			RequestedAtMs: 100, ExpiresAtMs: 100 + 86_400_001,
		})
	})
	if err == nil {
		t.Fatal("超过 24 小时的请求应被拒")
	}

	// 视图：查询者是团队成员才看得到；请求者不再是成员则消失；主机撤销则消失。
	views, err := r.PendingTrust(e.ctx, saasTenant, "owner", "", 500, 10)
	if err != nil || len(views) != 1 || views[0].RequestID != "r1" || views[0].SourceName == "" ||
		views[0].ViewerRole != saas.RoleOwner || views[0].SourceOwnerID != "owner" || len(views[0].TeamSettings) == 0 {
		t.Fatalf("视图：%+v %v", views, err)
	}
	if views, _ := r.PendingTrust(e.ctx, saasTenant, "ext", "", 500, 10); len(views) != 0 {
		t.Fatalf("外人看不到：%+v", views)
	}
	if views, _ := r.PendingTrust(e.ctx, saasTenant, "owner", "t2", 500, 10); len(views) != 0 {
		t.Fatalf("别的团队：%+v", views)
	}
	if views, _ := r.PendingTrust(e.ctx, saasTenant, "owner", "", 1000, 10); len(views) != 0 {
		t.Fatalf("到期（expires_at_ms ≤ now）不列：%+v", views)
	}
	if v, _ := r.TrustView(e.ctx, saasTenant, "r1", "ext"); v == nil || v.ViewerRole != "" {
		t.Fatalf("外人读单条：角色应为空：%+v", v)
	}
	e.must("DELETE FROM team_members WHERE account_id = 'u1'")
	if views, _ := r.PendingTrust(e.ctx, saasTenant, "owner", "", 500, 10); len(views) != 0 {
		t.Fatalf("请求者离开团队后不再列：%+v", views)
	}
	e.must("INSERT INTO team_members (tenant_id, team_id, account_id, role, joined_at_ms) VALUES ($1, 't1', 'u1', 'member', 1)", saasTenant)
	e.must("UPDATE sources SET revoked_at_ms = 5 WHERE source_id = 'h1'")
	if v, _ := r.TrustView(e.ctx, saasTenant, "r1", "owner"); v != nil {
		t.Fatalf("主机撤销后读不到：%+v", v)
	}
	e.must("UPDATE sources SET revoked_at_ms = NULL WHERE source_id = 'h1'")

	// 取代与过期：先过期的清掉，再把剩下的 pending 取代。
	err = s.Tx(e.ctx, func(tx saas.Tx) error {
		if err := tx.Trust().ExpireStale(e.ctx, saasTenant, "h1", "d1", 999); err != nil { // 还没到期
			return err
		}
		got, err := tx.Trust().ByPending(e.ctx, saasTenant, "h1", "p1")
		if err != nil || got == nil || got.State != saas.TrustPending {
			t.Fatalf("还没到期应仍是 pending：%+v %v", got, err)
		}
		if err := tx.Trust().ExpireStale(e.ctx, saasTenant, "h1", "d1", 1000); err != nil {
			return err
		}
		got, _ = tx.Trust().ByPending(e.ctx, saasTenant, "h1", "p1")
		if got.State != saas.TrustExpired || got.ResolvedBy != saas.TrustResolvedBySystem {
			t.Fatalf("到期应改 expired：%+v", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	e.trust("r5", "h1", "p5", "t1", "u1", "d1", 2000, 3000) // 同设备过期后可以重新上报
	err = s.Tx(e.ctx, func(tx saas.Tx) error {
		ids, err := tx.Trust().Supersede(e.ctx, saasTenant, "h1", "d1", 2500)
		if err != nil || len(ids) != 1 || ids[0] != "r5" {
			t.Fatalf("取代：%v %v", ids, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// 解决只作用于 pending；拒绝后能查到最近一次被拒绝的；GC 只删早于界限的已解决请求。
	e.trust("r6", "h1", "p6", "t1", "u2", "d6", 4000, 5000)
	_ = s.Tx(e.ctx, func(tx saas.Tx) error {
		if ok, err := tx.Trust().Resolve(e.ctx, saasTenant, "r6", saas.TrustRejected, "owner", 4500); err != nil || !ok {
			t.Fatalf("拒绝：%v %v", ok, err)
		}
		if ok, _ := tx.Trust().Resolve(e.ctx, saasTenant, "r6", saas.TrustTrusted, "source", 4600); ok {
			t.Fatal("已解决的不能再解决")
		}
		last, err := tx.Trust().LastRejected(e.ctx, saasTenant, "h1", "d6")
		if err != nil || last == nil || last.RequestID != "r6" || last.ResolvedAtMs == nil || *last.ResolvedAtMs != 4500 || last.ResolvedBy != "owner" {
			t.Fatalf("最近被拒：%+v %v", last, err)
		}
		if last, _ := tx.Trust().LastRejected(e.ctx, saasTenant, "h1", "d1"); last != nil {
			t.Fatalf("没被拒过：%+v", last)
		}
		return nil
	})
	e.trust("r7", "h1", "p7", "t1", "u1", "d7", 6000, 6500)
	_ = s.Tx(e.ctx, func(tx saas.Tx) error {
		due, err := tx.Trust().ExpireDue(e.ctx, saasTenant, 6500)
		if err != nil || len(due) != 1 || due[0].RequestID != "r7" {
			t.Fatalf("到期作业：%+v %v", due, err)
		}
		n, err := tx.Trust().GC(e.ctx, saasTenant, 4501)
		if err != nil || n != 3 { // r1（system 1000）、r5（superseded 2500）、r6（rejected 4500）
			t.Fatalf("GC：%d %v", n, err)
		}
		return nil
	})
}
