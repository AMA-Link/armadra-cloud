package core_test

import (
	"context"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/core/coretest"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

func guestOf(t *testing.T, e *env, sources *core.Sources, owner core.Caller) (core.Caller, string) {
	t.Helper()
	links := core.NewLinks(e.h.Context, sources)
	created, err := links.Create(e.ctx, owner, core.CreateLinkInput{
		SourceID: sourceID, InvitationID: "inv", Label: "朋友", Role: "viewer", ExpiresAtMs: e.h.Clock.Ms() + 60_000,
	})
	e.must(err)
	accepted, err := links.Accept(e.ctx, core.AcceptInput{LinkID: created.LinkID, Secret: created.Secret}, "ip")
	e.must(err)
	caller, err := core.NewAuthenticator(e.h.Context).Authenticate(e.ctx, "Bearer "+accepted.GuestSession.AccessToken)
	e.must(err)
	return caller, created.LinkID
}

func TestPersonalPolicy(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	owner := e.owner()
	sources := e.register(owner, sourceID)
	guest, linkID := guestOf(t, e, sources, owner)
	policy := core.NewPersonalPolicy(e.h.Context)

	t.Run("Visible：owner 看全部未撤销的，访客只看链接指向的源，来路与所有者标记", func(t *testing.T) {
		page, next, err := policy.Visible(e.ctx, owner, core.SourceFilter{})
		e.must(err)
		eq(t, next, "", "不分页")
		eq(t, len(page), 1, "owner 数量")
		eq(t, [2]any{page[0].Via, page[0].Owner}, [2]any{"owner", true}, "owner 来路")
		page, _, err = policy.Visible(e.ctx, guest, core.SourceFilter{})
		e.must(err)
		eq(t, len(page), 1, "访客数量")
		eq(t, [2]any{page[0].Via, page[0].Owner}, [2]any{"link", false}, "访客来路")
		page, _, err = policy.Visible(e.ctx, core.Caller{Role: core.RoleGuest}, core.SourceFilter{})
		e.must(err)
		eq(t, len(page), 0, "没有链接的访客")
	})

	t.Run("Intent：owner 带账号名，访客带链接、备注与范围", func(t *testing.T) {
		page, _, _ := policy.Visible(e.ctx, owner, core.SourceFilter{})
		extra, err := policy.Intent(e.ctx, owner, page[0].Source)
		e.must(err)
		eq(t, [2]any{extra.Name, extra.Link == nil}, [2]any{"alice", true}, "owner 意图")
		extra, err = policy.Intent(e.ctx, guest, page[0].Source)
		e.must(err)
		eq(t, [2]any{extra.Name, extra.Link != nil && extra.Link.LinkID == linkID}, [2]any{"朋友", true}, "访客意图")
	})

	t.Run("CanManage：只有 owner", func(t *testing.T) {
		for _, action := range []core.ManageAction{core.ManageSourceRevoke, core.ManageLinkCreate, core.ManageLinkList, core.ManageLinkUpdate, core.ManageLinkRevoke} {
			eq(t, code(policy.CanManage(e.ctx, owner, sourceID, action)), "ok", string(action)+" owner")
			eq(t, code(policy.CanManage(e.ctx, guest, sourceID, action)), "forbidden", string(action)+" 访客")
		}
	})

	t.Run("Watch：owner 看全部，访客只看链接指向的源", func(t *testing.T) {
		sees, err := policy.Watch(e.ctx, owner)
		e.must(err)
		eq(t, sees("anything"), true, "owner")
		sees, err = policy.Watch(e.ctx, guest)
		e.must(err)
		eq(t, [2]bool{sees(sourceID), sees("other")}, [2]bool{true, false}, "访客")
		sees, err = policy.Watch(e.ctx, core.Caller{Role: core.RoleGuest})
		e.must(err)
		eq(t, sees(sourceID), false, "没有链接")
	})
}

// denyAll 是一个注入的策略：证明服务确实经 Context.Policy 判定。
type denyAll struct{ managed []core.ManageAction }

func (d *denyAll) Visible(context.Context, core.Caller, core.SourceFilter) ([]core.VisibleSource, string, error) {
	return nil, "", nil
}

func (d *denyAll) Intent(context.Context, core.Caller, store.SourceRecord) (core.GrantExtra, error) {
	return core.GrantExtra{}, core.Fail(gen.ErrSourceAccessDenied)
}

func (d *denyAll) CanManage(_ context.Context, _ core.Caller, _ string, action core.ManageAction) error {
	d.managed = append(d.managed, action)
	return core.Fail(gen.ErrForbidden)
}

func (d *denyAll) Watch(context.Context, core.Caller) (func(string) bool, error) {
	return func(string) bool { return false }, nil
}

func TestServicesGoThroughPolicy(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	owner := e.owner()
	sources := e.register(owner, sourceID)
	deny := &denyAll{}
	e.h.Context.Policy = deny
	links := core.NewLinks(e.h.Context, sources)

	list, err := sources.List(e.ctx, owner)
	e.must(err)
	eq(t, len(list), 0, "List 经策略")
	_, err = sources.Get(e.ctx, owner, sourceID)
	eq(t, code(err), "not_found", "Get 经策略")
	_, err = sources.Assertion(e.ctx, owner, sourceID, nil)
	eq(t, code(err), "source_access_denied", "Assertion 经策略")
	eq(t, code(sources.Revoke(e.ctx, owner, sourceID)), "forbidden", "Revoke")
	_, err = links.Create(e.ctx, owner, core.CreateLinkInput{SourceID: sourceID, ExpiresAtMs: e.h.Clock.Ms() + 1000})
	eq(t, code(err), "forbidden", "Create")
	_, err = links.List(e.ctx, owner, "")
	eq(t, code(err), "forbidden", "List 链接")
	_, err = links.Update(e.ctx, owner, "x", "y")
	eq(t, code(err), "forbidden", "Update")
	eq(t, code(links.Revoke(e.ctx, owner, "x")), "forbidden", "Revoke 链接")
	eq(t, len(deny.managed), 5, "管理判定都经策略")

	var seen []core.Event
	detach, err := core.NewMeStream(e.h.Context).Attach(owner, func(ev core.Event) { seen = append(seen, ev) }, nil)
	e.must(err)
	e.h.Context.Emit(core.NewSourceRevoked(sourceID), nil)
	detach()
	eq(t, len(seen), 0, "MeStream 经策略过滤")
}

func TestSourceAddressFunctions(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	owner := e.owner()
	sources := e.register(owner, sourceID)

	list, err := sources.List(e.ctx, owner)
	e.must(err)
	grant, err := sources.Assertion(e.ctx, owner, sourceID, nil)
	e.must(err)
	eq(t, [3]string{list[0].RelayOrigin, grant.RelayOrigin, grant.RelayBaseURL},
		[3]string{"https://cloud.armadra.test", "https://cloud.armadra.test", "https://cloud.armadra.test/s/" + sourceID}, "缺省来源")

	e.h.Context.RelayOrigin = func(id string) string { return "https://" + id + ".src.example.test" }
	e.h.Context.RelayBaseURL = func(id string) string { return "https://" + id + ".src.example.test/" }
	list, err = sources.List(e.ctx, owner)
	e.must(err)
	grant, err = sources.Assertion(e.ctx, owner, sourceID, nil)
	e.must(err)
	eq(t, [3]string{list[0].RelayOrigin, grant.RelayOrigin, grant.RelayBaseURL},
		[3]string{"https://" + sourceID + ".src.example.test", "https://" + sourceID + ".src.example.test", "https://" + sourceID + ".src.example.test/"}, "装配方给的来源")
	token, err := sources.RegistrationToken(e.ctx, owner)
	e.must(err)
	result, err := sources.Register(e.ctx, registerInput(t, token.RegistrationToken, sourceID, 1), "ip")
	e.must(err)
	eq(t, result.RelayOrigins[0], "https://"+sourceID+".src.example.test", "登记答复的 relayOrigins")
}

func TestBuildSourceRecord(t *testing.T) {
	input := registerInput(t, "tok", sourceID, 1)
	fresh, err := core.BuildSourceRecord(nil, input, 100)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, [3]any{fresh.RegisteredAtMs, fresh.Name, len(fresh.TrustedOrigins)}, [3]any{int64(100), "mac", 0}, "新记录")

	seen := int64(50)
	existing := store.SourceRecord{
		SourceID: sourceID, PublicJWK: fresh.PublicJWK, RegisteredAtMs: 10, LastSeenAtMs: &seen, TrustedOrigins: []string{"https://a"},
	}
	merged, err := core.BuildSourceRecord(&existing, input, 100)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, [3]any{merged.RegisteredAtMs, *merged.LastSeenAtMs, merged.TrustedOrigins[0]}, [3]any{int64(10), int64(50), "https://a"}, "同钥合并")

	other := existing
	other.PublicJWK.X = "different"
	_, err = core.BuildSourceRecord(&other, input, 100)
	eq(t, code(err), "source_taken", "别人的钥")

	revokedAt := int64(60)
	other.RevokedAtMs = &revokedAt
	reborn, err := core.BuildSourceRecord(&other, input, 100)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, [2]any{reborn.RegisteredAtMs, reborn.RevokedAtMs == nil}, [2]any{int64(100), true}, "撤销后重登记")
}
