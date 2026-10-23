package relayctl_test

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/core/coretest"
	"github.com/AMA-Link/armadra-cloud/go/internal/identity"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/pgtest"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/postgres"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/sqlite"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil"
)

const sourceID = "0123456789abcdef0123456789abcdef"

// 链接撤销从 core.Links.Revoke 一路走到节点命令：kick 必带 sourceId（设计 §2.4，节点会丢弃不带的）。
func TestLinkRevokeReachesNodeCommandsWithSourceID(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "relay.db"), sqlite.Options{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h := coretest.NewHarness(t, db, coretest.Options{})
	mem := relayctl.NewMemory(h.Clock.Now)
	h.Context.Relay, h.Context.Revocations = mem.Relay(), mem.Revocations()

	hashed, err := identity.HashPassword("plover-quartz-lantern-41")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddAccount(ctx, h.Context.TenantID, store.AccountRecord{AccountID: "acct:alice", Name: "alice", Password: hashed}); err != nil {
		t.Fatal(err)
	}
	owner := core.Caller{Subject: "acct:alice", AccountID: "acct:alice", Role: core.RoleOwner}
	sources := core.NewSources(h.Context)
	token, err := sources.RegistrationToken(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	_, source := testutil.Keys(t)
	if _, err := sources.Register(ctx, core.RegisterInput{
		RegistrationToken: token.RegistrationToken, SourceID: sourceID,
		PublicKey: gen.Ed25519PublicJWK{Kty: "OKP", Crv: "Ed25519", X: base64.RawURLEncoding.EncodeToString(source.Public)},
		Name:      "mac", Kind: "desktop", CoreVersion: "0.9.2", Capabilities: []string{}, ProtocolMajor: 1,
	}, "ip"); err != nil {
		t.Fatal(err)
	}
	links := core.NewLinks(h.Context, sources)
	created, err := links.Create(ctx, owner, core.CreateLinkInput{SourceID: sourceID, Role: "editor", Label: "x", ExpiresAtMs: h.Clock.Ms() + 3_600_000})
	if err != nil {
		t.Fatal(err)
	}
	prefix := core.GuestPrefix(created.LinkID)
	_ = mem.IndexIssued(ctx, relayctl.Issued{Subject: prefix + "aa", SourceID: sourceID, DeviceID: "g1", JTI: "r.g1." + sourceID + ".x", ExpMs: h.Clock.Ms() + 3_600_000})
	if err := links.Revoke(ctx, owner, created.LinkID); err != nil {
		t.Fatal(err)
	}
	cmds := mem.Commands()
	if len(cmds) != 1 || cmds[0].Type != relayctl.CommandKick || cmds[0].SourceID != sourceID || cmds[0].SubjectPrefix != prefix {
		t.Fatalf("kick 要带 sourceId 与访客前缀：%+v", cmds)
	}
	if len(mem.Problems()) != 0 || !mem.IsRevoked("r.g1."+sourceID+".x") {
		t.Fatalf("没有被丢弃的命令，访客令牌已撤：%v", mem.Problems())
	}
	// 不带 sourceId 的踢人不发，记问题。
	mem.Relay().Disconnect(core.ConnectionFilter{SubjectPrefix: prefix})
	if len(mem.Commands()) != 1 || len(mem.Problems()) != 1 {
		t.Fatalf("缺 sourceId 不该发：%+v %v", mem.Commands(), mem.Problems())
	}
}

// 绑定事务里钩子经 AfterCommit 投递：提交前不投，回滚不投，提交后按序投。
func TestDeferredSinkDeliversAfterCommitOnly(t *testing.T) {
	ctx := context.Background()
	url, schema := pgtest.Schema(t)
	db, err := postgres.Open(ctx, postgres.Options{URL: url, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h := coretest.NewHarness(t, db, coretest.Options{})
	mem := relayctl.NewMemory(h.Clock.Now)
	exp := h.Clock.Ms() + 3_600_000
	_ = mem.IndexIssued(ctx, relayctl.Issued{Subject: "acct:a", SourceID: "srcA", DeviceID: "dev", JTI: "r.dev.srcA.x", ExpMs: exp})
	base := h.Context
	nowMs := h.Clock.Ms()
	device, _ := core.NewDevice("acct:a", core.DeviceInput{Platform: "browser", Name: "n"}, core.TransportBearer, core.SessionScopeSession, nowMs, 0)
	if err := base.Ports.Devices.Insert(ctx, base.TenantID, device, nowMs); err != nil {
		t.Fatal(err)
	}
	// 索引里的设备 id 要与库里的对上。
	_ = mem.IndexIssued(ctx, relayctl.Issued{Subject: "acct:a", SourceID: "srcA", DeviceID: device.DeviceID, JTI: "r." + device.DeviceID + ".srcA.x", ExpMs: exp})

	boom := errors.New("boom")
	err = db.SaaS().Tx(ctx, func(tx saas.Tx) error {
		bound := base.Bound(tx.Ports(), relayctl.Deferred(tx, mem.Revocations()))
		if ok, err := core.RevokeDevice(ctx, bound, device.DeviceID); err != nil || !ok {
			t.Fatalf("撤销：%v %v", ok, err)
		}
		if mem.IsRevoked("r." + device.DeviceID + ".srcA.x") {
			t.Fatal("事务还没提交，令牌不该先被撤")
		}
		return boom
	})
	if !errors.Is(err, boom) || mem.IsRevoked("r."+device.DeviceID+".srcA.x") || len(mem.Commands()) != 0 {
		t.Fatalf("回滚不投递：%v", err)
	}
	if got, _ := base.Ports.Devices.Get(ctx, base.TenantID, device.DeviceID); got == nil || got.RevokedAtMs != nil {
		t.Fatal("回滚后设备仍有效")
	}
	err = db.SaaS().Tx(ctx, func(tx saas.Tx) error {
		bound := base.Bound(tx.Ports(), relayctl.Deferred(tx, mem.Revocations()))
		_, err := core.RevokeDevice(ctx, bound, device.DeviceID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !mem.IsRevoked("r."+device.DeviceID+".srcA.x") || len(mem.Commands()) != 1 {
		t.Fatalf("提交后投递：%+v", mem.Commands())
	}
}
