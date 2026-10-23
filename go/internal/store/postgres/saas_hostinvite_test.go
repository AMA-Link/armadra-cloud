package postgres_test

import (
	"errors"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

// 主机邀请仓储：同主机同邮箱只能有一条 pending、过期的先改 expired 才能重新邀请、收件箱不列已撤销主机的邀请、
// 按主机列邀请与链接只含给定的主机。
func TestHostInvitationRepo(t *testing.T) {
	e := openSaaS(t)
	for _, id := range []string{"owner", "u1"} {
		e.account(id)
	}
	e.team("t1", "owner")
	e.seedHost("h1", "t1", "owner", 10)
	e.seedHost("h2", "", "owner", 20)
	s := e.db.SaaS()
	grantExpiry := int64(9_000)
	host := func(id, source, team, email string, createdAt, expiresAt int64) saas.Invitation {
		return saas.Invitation{
			InvitationID: id, Kind: saas.InvitationHost, Email: email, TeamID: team, SourceID: source, HostRole: "editor",
			WorkspaceIDs: []string{"w1"}, InvitedBy: "owner", CreatedAtMs: createdAt, ExpiresAtMs: expiresAt, GrantExpiresAtMs: &grantExpiry,
		}
	}
	insert := func(inv saas.Invitation) error {
		return s.Tx(e.ctx, func(tx saas.Tx) error { return tx.Invitations().InsertHost(e.ctx, saasTenant, inv) })
	}
	if err := insert(host("i1", "h1", "t1", "Ann@X.test", 100, 1000)); err != nil {
		t.Fatal(err)
	}
	if err := insert(host("i2", "h1", "t1", "ann@x.test", 101, 2000)); !errors.Is(err, saas.ErrConflict) {
		t.Fatalf("同主机同邮箱（大小写不同）已有 pending 应答冲突：%v", err)
	}
	if err := insert(host("i3", "h2", "", "ann@x.test", 102, 2000)); err != nil {
		t.Fatalf("别的主机不冲突：%v", err)
	}
	_ = s.Tx(e.ctx, func(tx saas.Tx) error {
		has, err := tx.Invitations().HasPendingHost(e.ctx, saasTenant, "h1", "ANN@x.test", 999)
		if err != nil || !has {
			t.Fatalf("pending：%v %v", has, err)
		}
		if has, _ := tx.Invitations().HasPendingHost(e.ctx, saasTenant, "h1", "ann@x.test", 1000); has {
			t.Fatal("到期（expires_at_ms ≤ now）不算待处理")
		}
		if err := tx.Invitations().ExpireStaleHost(e.ctx, saasTenant, "h1", "ann@x.test", 1000); err != nil {
			t.Fatal(err)
		}
		got, err := tx.Invitation(e.ctx, saasTenant, "i1")
		if err != nil || got.State != saas.InvitationExpired || got.GrantExpiresAtMs == nil || *got.GrantExpiresAtMs != grantExpiry ||
			got.HostRole != "editor" || got.SourceID != "h1" || got.TeamID != "t1" || len(got.WorkspaceIDs) != 1 {
			t.Fatalf("过期后：%+v %v", got, err)
		}
		return tx.Invitations().InsertHost(e.ctx, saasTenant, host("i4", "h1", "t1", "ann@x.test", 1100, 5000))
	})
	// 不过期的授予到期（NULL）原样存取。
	forever := host("i5", "h1", "t1", "bob@x.test", 1200, 5000)
	forever.GrantExpiresAtMs = nil
	if err := insert(forever); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Read().Invitation(e.ctx, saasTenant, "i5"); got == nil || got.GrantExpiresAtMs != nil {
		t.Fatalf("NULL 的授予到期：%+v", got)
	}

	r := s.Read()
	listed, err := r.HostInvitations(e.ctx, saasTenant, []string{"h1"}, 10)
	if err != nil || len(listed) != 3 || listed[0].InvitationID != "i5" {
		t.Fatalf("按主机列：%+v %v", listed, err)
	}
	if listed, _ := r.HostInvitations(e.ctx, saasTenant, nil, 10); len(listed) != 0 {
		t.Fatal("空主机集合不列")
	}

	// 收件箱：主机邀请列出来；主机撤销后不列。
	received, _ := r.ReceivedInvitations(e.ctx, saasTenant, "ann@x.test", 1500)
	if len(received) != 2 {
		t.Fatalf("收件箱：%+v", received)
	}
	e.must("UPDATE sources SET revoked_at_ms = 7 WHERE source_id = 'h2'")
	received, _ = r.ReceivedInvitations(e.ctx, saasTenant, "ann@x.test", 1500)
	if len(received) != 1 || received[0].InvitationID != "i4" {
		t.Fatalf("主机撤销后收件箱：%+v", received)
	}

	// 链接：只列给定主机的，含已撤销，按创建顺序，受 limit 限制。
	ports := e.db.SaaSPorts()
	for i, link := range []struct{ id, source string }{{"l1", "h1"}, {"l2", "h2"}, {"l3", "h1"}} {
		if err := ports.Links.Insert(e.ctx, saasTenant, store.LinkRecord{
			LinkID: link.id, SourceID: link.source, InvitationID: "inv", Label: "x", Role: "viewer", SecretHash: "h",
			CreatedAtMs: int64(i), ExpiresAtMs: 99,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := ports.Links.Revoke(e.ctx, saasTenant, "l1", 5); err != nil {
		t.Fatal(err)
	}
	links, err := r.LinksForSources(e.ctx, saasTenant, []string{"h1"}, 10)
	if err != nil || len(links) != 2 || links[0].LinkID != "l1" || links[0].RevokedAtMs == nil || links[1].LinkID != "l3" {
		t.Fatalf("链接：%+v %v", links, err)
	}
	if links, _ := r.LinksForSources(e.ctx, saasTenant, []string{"h1", "h2"}, 2); len(links) != 2 {
		t.Fatalf("limit：%+v", links)
	}
}
