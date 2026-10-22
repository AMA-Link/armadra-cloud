package core_test

import (
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/core/coretest"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

func linkInput(e *env, mutate ...func(*core.CreateLinkInput)) core.CreateLinkInput {
	in := core.CreateLinkInput{SourceID: sourceID, InvitationID: "inv", Label: "朋友", Role: "viewer", ExpiresAtMs: e.h.Clock.Ms() + 3_600_000}
	for _, m := range mutate {
		m(&in)
	}
	return in
}

func TestCreateLinkValidation(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	owner := e.owner()
	sources := e.register(owner, sourceID)
	links := core.NewLinks(e.h.Context, sources)
	const thirtyDays = int64(30 * 24 * 3600 * 1000)

	for name, in := range map[string]core.CreateLinkInput{
		"已过期":          linkInput(e, func(in *core.CreateLinkInput) { in.ExpiresAtMs = e.h.Clock.Ms() }),
		"超过 30 天":      linkInput(e, func(in *core.CreateLinkInput) { in.ExpiresAtMs = e.h.Clock.Ms() + thirtyDays + 1 }),
		"org_join 不支持": linkInput(e, func(in *core.CreateLinkInput) { in.OrgJoin = true }),
	} {
		_, err := links.Create(e.ctx, owner, in)
		eq(t, code(err), "bad_request", name)
	}
	_, err := links.Create(e.ctx, owner, linkInput(e, func(in *core.CreateLinkInput) { in.ExpiresAtMs = e.h.Clock.Ms() + thirtyDays }))
	eq(t, code(err), "ok", "恰好 30 天")
	_, err = links.Create(e.ctx, owner, linkInput(e, func(in *core.CreateLinkInput) { in.SourceID = "ffffffffffffffffffffffffffffffff" }))
	eq(t, code(err), "source_access_denied", "没登记的源")
	guest := core.Caller{Subject: "guest:1111111111111111:ab", DeviceID: "d", Role: core.RoleGuest, LinkID: "1111111111111111"}
	_, err = links.Create(e.ctx, guest, linkInput(e))
	eq(t, code(err), "forbidden", "访客")

	created, err := links.Create(e.ctx, owner, linkInput(e))
	e.must(err)
	eq(t, created.URL, "https://cloud.armadra.test/j/"+created.LinkID, "url 不带片段")
	if len(created.LinkID) != 16 || len(created.Secret) != 43 {
		t.Fatalf("linkId 16 位 hex、secret 32 字节 base64url：%q %q", created.LinkID, created.Secret)
	}
	// 库里只有秘密的哈希。
	record, _ := e.h.Context.Ports.Links.Get(e.ctx, "t1", created.LinkID)
	if record.SecretHash == created.Secret || len(record.SecretHash) != 64 {
		t.Fatalf("库里只该有哈希：%q", record.SecretHash)
	}
}

func TestCreateLinkRefusedWhenGuestsDisallowed(t *testing.T) {
	e := newEnv(t, coretest.Options{DisallowGuests: true})
	owner := e.owner()
	sources := e.register(owner, sourceID)
	links := core.NewLinks(e.h.Context, sources)
	_, err := links.Create(e.ctx, owner, linkInput(e))
	eq(t, code(err), "forbidden", "不允许访客")
}

func TestAcceptRules(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	owner := e.owner()
	sources := e.register(owner, sourceID)
	links := core.NewLinks(e.h.Context, sources)
	created, err := links.Create(e.ctx, owner, linkInput(e))
	e.must(err)
	accept := func(id, secret string) error {
		_, err := links.Accept(e.ctx, core.AcceptInput{LinkID: id, Secret: secret}, "ip")
		return err
	}

	eq(t, code(accept("ffffffffffffffff", created.Secret)), "link_invalid", "没有这条链接")
	eq(t, code(accept(created.LinkID, "")), "link_secret_invalid", "缺秘密")
	eq(t, code(accept(created.LinkID, "错的")), "link_secret_invalid", "秘密不对")
	accepted, err := links.Accept(e.ctx, core.AcceptInput{LinkID: created.LinkID, Secret: created.Secret}, "ip")
	e.must(err)
	// 访客主体 guest:<linkId>:<8 hex>，设备 30 天，名字缺省 guest。
	subject := accepted.GuestSession.Account.AccountID
	if len(subject) != len("guest:")+16+1+8 || subject[:6] != "guest:" {
		t.Fatalf("访客主体：%q", subject)
	}
	device, _ := e.h.Context.Ports.Devices.Get(e.ctx, "t1", accepted.GuestSession.Device.DeviceID)
	eq(t, device.ExpiresAtMs-device.CreatedAtMs, int64(30*24*3600*1000), "访客设备 30 天")
	eq(t, device.Name, "guest", "缺省设备名")
	eq(t, claimsOf(t, accepted.Assertion)["name"], any("朋友"), "断言带链接备注")
	link := claimsOf(t, accepted.Assertion)["link"].(map[string]any)
	eq(t, link["linkId"], any(created.LinkID), "断言的 link")
	// 计数。
	record, _ := e.h.Context.Ports.Links.Get(e.ctx, "t1", created.LinkID)
	eq(t, record.Uses, int64(1), "uses")

	// 过期。
	e.h.Clock.Set(record.ExpiresAtMs)
	eq(t, code(accept(created.LinkID, created.Secret)), "link_expired", "过期")
	// 撤销后：link_invalid，落地页也看不到。
	e.h.Clock.Set(coretest.DefaultNow)
	e.must(links.Revoke(e.ctx, owner, created.LinkID))
	eq(t, code(accept(created.LinkID, created.Secret)), "link_invalid", "已撤销")
	_, err = links.Get(e.ctx, created.LinkID)
	eq(t, code(err), "link_invalid", "落地页")
	eq(t, code(links.Revoke(e.ctx, owner, created.LinkID)), "not_found", "再撤一次")
}

func TestAcceptFailsWhenSourceIsRevoked(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	owner := e.owner()
	sources := e.register(owner, sourceID)
	links := core.NewLinks(e.h.Context, sources)
	created, err := links.Create(e.ctx, owner, linkInput(e))
	e.must(err)
	e.must(sources.Revoke(e.ctx, owner, sourceID))
	_, err = links.Accept(e.ctx, core.AcceptInput{LinkID: created.LinkID, Secret: created.Secret}, "ip")
	eq(t, code(err), "link_invalid", "源已撤销")
	_, err = links.Get(e.ctx, created.LinkID)
	eq(t, code(err), "link_invalid", "落地页")
}

func TestAcceptEmitsLinkUsedAndRevokeNotifiesGuests(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	owner := e.owner()
	sources := e.register(owner, sourceID)
	links := core.NewLinks(e.h.Context, sources)
	created, err := links.Create(e.ctx, owner, linkInput(e))
	e.must(err)
	_, err = links.Accept(e.ctx, core.AcceptInput{LinkID: created.LinkID, Secret: created.Secret}, "ip")
	e.must(err)
	events := e.h.Events()
	used, ok := events[len(events)-1].(core.LinkUsed)
	if !ok || used.LinkID != created.LinkID || used.SourceID != sourceID || used.By.Guest == nil || !*used.By.Guest || used.Type != "linkUsed" {
		t.Fatalf("linkUsed 事件：%+v", events[len(events)-1])
	}
	e.must(links.Revoke(e.ctx, owner, created.LinkID))
	events = e.h.Events()
	if revoked, ok := events[len(events)-1].(core.AccessRevoked); !ok || revoked.SourceID != sourceID {
		t.Fatalf("accessRevoked 事件：%+v", events[len(events)-1])
	}
}

func TestUpdateLabel(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	owner := e.owner()
	sources := e.register(owner, sourceID)
	links := core.NewLinks(e.h.Context, sources)
	created, err := links.Create(e.ctx, owner, linkInput(e))
	e.must(err)
	updated, err := links.Update(e.ctx, owner, created.LinkID, "　\t 新备注 \n")
	e.must(err)
	eq(t, updated.Label, "新备注", "去首尾空白（含全角空格）")
	_, err = links.Update(e.ctx, owner, "ffffffffffffffff", "x")
	eq(t, code(err), "not_found", "没有这条链接")
	// 已撤销的也能改，且不动秘密、次数与期限。
	e.must(links.Revoke(e.ctx, owner, created.LinkID))
	before, _ := e.h.Context.Ports.Links.Get(e.ctx, "t1", created.LinkID)
	updated, err = links.Update(e.ctx, owner, created.LinkID, "撤销后改")
	e.must(err)
	after, _ := e.h.Context.Ports.Links.Get(e.ctx, "t1", created.LinkID)
	if updated.RevokedAtMs == nil || after.SecretHash != before.SecretHash || after.Uses != before.Uses || after.ExpiresAtMs != before.ExpiresAtMs {
		t.Fatalf("已撤销的链接改备注：%+v", after)
	}
}

func TestNormalizeLinkScope(t *testing.T) {
	str := func(s string) *string { return &s }
	yes, no := true, false
	cases := []struct {
		name string
		in   *store.LinkScope
		want *store.LinkScope
		err  string
	}{
		{"nil", nil, nil, ""},
		{"空对象", &store.LinkScope{}, nil, ""},
		{"readOnly false 等同缺省", &store.LinkScope{ReadOnly: &no}, nil, ""},
		{"只读整台", &store.LinkScope{ReadOnly: &yes}, &store.LinkScope{ReadOnly: &yes}, ""},
		{"工作空间可写", &store.LinkScope{WorkspaceID: str("w"), ReadOnly: &no}, &store.LinkScope{WorkspaceID: str("w")}, ""},
		{"会话范围总是只读", &store.LinkScope{WorkspaceID: str("w"), SessionID: str("s")}, &store.LinkScope{WorkspaceID: str("w"), SessionID: str("s"), ReadOnly: &yes}, ""},
		{"会话范围显式只读", &store.LinkScope{WorkspaceID: str("w"), SessionID: str("s"), ReadOnly: &yes}, &store.LinkScope{WorkspaceID: str("w"), SessionID: str("s"), ReadOnly: &yes}, ""},
		{"会话缺工作空间", &store.LinkScope{SessionID: str("s")}, nil, "bad_request"},
		{"会话范围显式可写", &store.LinkScope{WorkspaceID: str("w"), SessionID: str("s"), ReadOnly: &no}, nil, "bad_request"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := core.NormalizeLinkScope(c.in)
			if c.err != "" {
				eq(t, code(err), c.err, "错误码")
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(store.Marshal(got)) != string(store.Marshal(c.want)) {
				t.Fatalf("得到 %s，应为 %s", store.Marshal(got), store.Marshal(c.want))
			}
		})
	}
	// scp 的顺序固定：ws:、sess:、ro。
	scp := core.ScopeClaims(&store.LinkScope{WorkspaceID: str("w"), SessionID: str("s"), ReadOnly: &yes})
	eq(t, join(scp), "ws:w,sess:s,ro", "scp 顺序")
	eq(t, join(core.ScopeClaims(nil)), "", "整台没有 scp")
}

func join(list []string) string {
	out := ""
	for i, item := range list {
		if i > 0 {
			out += ","
		}
		out += item
	}
	return out
}
