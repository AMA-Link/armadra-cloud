package coretest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/identity"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

const (
	password = "plover-quartz-lantern-41"
	sourceID = "0123456789abcdef0123456789abcdef"
)

var (
	device       = core.DeviceInput{Platform: "desktop", Name: "test"}
	hashed       identity.PasswordRecord
	hashedOnce   sync.Once
	hashedFailed error
)

func hashedPassword(t testing.TB) identity.PasswordRecord {
	t.Helper()
	hashedOnce.Do(func() { hashed, hashedFailed = identity.HashPassword(password) })
	if hashedFailed != nil {
		t.Fatal(hashedFailed)
	}
	return hashed
}

func account(t testing.TB, name, accountID string) store.AccountRecord {
	return store.AccountRecord{AccountID: accountID, Name: name, Password: hashedPassword(t)}
}

// payloadOf 拆令牌的载荷（只拆不验，验签由别处负责）。
func payloadOf(t testing.TB, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("不是 JWS：%q", token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// codeOf 把一次调用的结果换成错误码；成功是 "ok"。
func codeOf(err error) string {
	if err == nil {
		return "ok"
	}
	if code, ok := core.CodeOf(err); ok {
		return string(code)
	}
	return err.Error()
}

type env struct {
	t   *testing.T
	ctx context.Context
	h   *Harness
}

func (e *env) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) login(name, secret string) (core.Session, error) {
	return core.NewAccounts(e.h.Context).Login(e.ctx, core.LoginInput{Account: name, Password: secret, Device: device}, "198.51.100.1")
}

func (e *env) callerOf(accessToken string) (core.Caller, error) {
	return core.NewAuthenticator(e.h.Context).Authenticate(e.ctx, "Bearer "+accessToken)
}

// ownerOf 放一个账号 alice（u-1），登录并认证，答调用方。
func (e *env) ownerOf() core.Caller {
	e.t.Helper()
	e.must(e.h.Backend.AddAccount(e.ctx, e.h.Context.TenantID, account(e.t, "alice", "u-1")))
	session, err := e.login("alice", password)
	e.must(err)
	caller, err := e.callerOf(session.AccessToken)
	e.must(err)
	return caller
}

func (e *env) registered(owner core.Caller) (*core.Sources, core.RegisterResult) {
	e.t.Helper()
	source := sourceKey(e.t)
	sources := core.NewSources(e.h.Context)
	token, err := sources.RegistrationToken(e.ctx, owner)
	e.must(err)
	result, err := sources.Register(e.ctx, core.RegisterInput{
		RegistrationToken: token.RegistrationToken, SourceID: sourceID,
		PublicKey: gen.Ed25519PublicJWK{Kty: "OKP", Crv: "Ed25519", X: base64.RawURLEncoding.EncodeToString(source)},
		Name:      "mac", Kind: "desktop", CoreVersion: "0.2.0-test", Capabilities: []string{},
		ProtocolMajor: 1, ProtocolMinor: 0,
	}, "ip")
	e.must(err)
	return sources, result
}

func fresh(t *testing.T, create func(t *testing.T) store.Backend, options Options) *env {
	t.Helper()
	backend := create(t)
	t.Cleanup(func() { _ = backend.Close() })
	return &env{t: t, ctx: context.Background(), h: NewHarness(t, backend, options)}
}

func eq(t *testing.T, got, want any, what string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s：\n得到 %+v\n应为 %+v", what, got, want)
	}
}

func scopeOf(ws, sess string, readOnly *bool) *store.LinkScope {
	s := &store.LinkScope{ReadOnly: readOnly}
	if ws != "" {
		s.WorkspaceID = &ws
	}
	if sess != "" {
		s.SessionID = &sess
	}
	return s
}

func ptr[T any](v T) *T { return &v }

var (
	sourceKeyOnce sync.Once
	sourcePublic  ed25519.PublicKey
)

// sourceKey 是测试源登记的 Ed25519 公钥（进程内生成一次）。
func sourceKey(t testing.TB) ed25519.PublicKey {
	t.Helper()
	sourceKeyOnce.Do(func() {
		var err error
		if sourcePublic, _, err = ed25519.GenerateKey(rand.Reader); err != nil {
			t.Fatal(err)
		}
	})
	return sourcePublic
}

// ServicesContract 对一个存储实现跑服务合同（16 个用例）。
func ServicesContract(t *testing.T, create func(t *testing.T) store.Backend) {
	t.Run("账号与会话", func(t *testing.T) {
		t.Run("账号 id 是存储给的不透明 sub；刷新旋转、重放作废整台设备", func(t *testing.T) { // 例 1
			e := fresh(t, create, Options{})
			owner := e.ownerOf()
			eq(t, [3]any{owner.Subject, owner.Role, owner.LinkID}, [3]any{"u-1", core.RoleOwner, ""}, "调用方")
			first, err := e.login("alice", password)
			e.must(err)
			eq(t, [2]string{first.Account.AccountID, first.Account.DisplayName}, [2]string{"u-1", "alice"}, "会话里的账号")
			second, err := core.Refresh(e.ctx, e.h.Context, first.RefreshToken)
			e.must(err)
			if second.Device.DeviceID != first.Device.DeviceID {
				t.Fatal("刷新不换设备")
			}
			_, err = core.Refresh(e.ctx, e.h.Context, first.RefreshToken)
			eq(t, codeOf(err), "session_revoked", "旧令牌重放")
			_, err = core.Refresh(e.ctx, e.h.Context, second.RefreshToken)
			eq(t, codeOf(err), "session_revoked", "重放之后整台设备作废")
		})

		t.Run("租户隔离：别的租户里同名账号登录不上", func(t *testing.T) { // 例 2
			e := fresh(t, create, Options{TenantID: "t1"})
			e.must(e.h.Backend.CreateTenant(e.ctx, "t2", store.Tenant{TenantSettings: store.TenantSettings{
				Issuer: "https://cloud.armadra.test", AllowGuests: true, TrustedOrigins: []string{},
			}}))
			e.must(e.h.Backend.AddAccount(e.ctx, "t2", account(t, "alice", "u-2")))
			_, err := core.NewAccounts(e.h.Context).Login(e.ctx, core.LoginInput{Account: "alice", Password: password, Device: device}, "198.51.100.2")
			eq(t, codeOf(err), "credentials_invalid", "别的租户的账号")
		})

		t.Run("改口令原子地撤销账号本人的全部设备；口令策略由装配方给", func(t *testing.T) { // 例 3
			e := fresh(t, create, Options{Policy: shortPolicy})
			owner := e.ownerOf()
			eq(t, codeOf(core.ChangePasswordByName(e.ctx, e.h.Context, "alice", "short", 1)), "password_too_short", "策略")
			e.must(core.ChangePasswordByName(e.ctx, e.h.Context, "alice", "saffron-glacier-meadow-77", 1))
			device, err := e.h.Context.Ports.Devices.Get(e.ctx, "t1", owner.DeviceID)
			e.must(err)
			if device.RevokedAtMs == nil || *device.RevokedAtMs != 1 {
				t.Fatalf("设备应在 1 撤销：%v", device.RevokedAtMs)
			}
			eq(t, codeOf(core.ChangePasswordByName(e.ctx, e.h.Context, "nobody", password, 1)), "not_found", "没有这个账号")
		})

		t.Run("改口令（cloud-api §18）", func(t *testing.T) {
			const next = "saffron-glacier-meadow-77"
			change := func(e *env, caller core.Caller, old, new string) (core.Session, error) {
				return core.NewAccounts(e.h.Context).ChangePassword(e.ctx, caller,
					core.ChangePasswordInput{Password: old, NewPassword: new}, "ip")
			}

			t.Run("旧口令不对：credentials_invalid，计入锁定计数，连错 5 次锁定", func(t *testing.T) { // 例 4
				e := fresh(t, create, Options{Policy: shortPolicy})
				owner := e.ownerOf()
				_, err := change(e, owner, "wrong-old-password", next)
				eq(t, codeOf(err), "credentials_invalid", "旧口令不对")
				got, _ := e.h.Context.Ports.Accounts.Get(e.ctx, "t1", "u-1")
				if got.Failures != 1 {
					t.Fatalf("failures = %d", got.Failures)
				}
				for i := 0; i < 4; i++ {
					_, _ = change(e, owner, "wrong-old-password", next)
				}
				_, err = change(e, owner, password, next)
				eq(t, codeOf(err), "account_locked", "连错 5 次后锁定")
				// 口令没换：锁着时新口令也登不上。
				_, err = e.login("alice", next)
				eq(t, codeOf(err), "account_locked", "锁着登录")
				got, _ = e.h.Context.Ports.Accounts.Get(e.ctx, "t1", "u-1")
				eq(t, got.Password.Hash, hashedPassword(t).Hash, "口令记录没换")
			})

			t.Run("新口令不合策略：答策略码，什么也不改", func(t *testing.T) { // 例 5
				e := fresh(t, create, Options{Policy: shortPolicy})
				owner := e.ownerOf()
				_, err := change(e, owner, password, "short")
				eq(t, codeOf(err), "password_too_short", "策略码")
				_, err = e.login("alice", password)
				eq(t, codeOf(err), "ok", "旧口令照样能登")
				device, _ := e.h.Context.Ports.Devices.Get(e.ctx, "t1", owner.DeviceID)
				if device.RevokedAtMs != nil {
					t.Fatal("设备不该被撤销")
				}
			})

			t.Run("成功：其它设备与刷新令牌失效，当前设备保留并换新刷新令牌，旧口令登不上", func(t *testing.T) { // 例 6
				e := fresh(t, create, Options{Policy: shortPolicy})
				e.must(e.h.Backend.AddAccount(e.ctx, "t1", account(t, "alice", "u-1")))
				current, err := e.login("alice", password)
				e.must(err)
				other, err := e.login("alice", password)
				e.must(err)
				caller, err := e.callerOf(current.AccessToken)
				e.must(err)
				session, err := change(e, caller, password, next)
				e.must(err)
				if session.Device.DeviceID != current.Device.DeviceID {
					t.Fatal("当前设备应保留")
				}
				if session.RefreshToken == current.RefreshToken {
					t.Fatal("当前设备的刷新令牌应换新")
				}
				// 当前设备：访问令牌照样认，新刷新令牌能用；旧刷新令牌作废但不撤销这台。
				_, err = e.callerOf(current.AccessToken)
				eq(t, codeOf(err), "ok", "当前设备的访问令牌")
				_, err = core.Refresh(e.ctx, e.h.Context, current.RefreshToken)
				eq(t, codeOf(err), "session_revoked", "旧刷新令牌")
				renewed, err := core.Refresh(e.ctx, e.h.Context, session.RefreshToken)
				e.must(err)
				if renewed.Device.DeviceID != current.Device.DeviceID {
					t.Fatal("新刷新令牌应属于当前设备")
				}
				// 其它设备：访问令牌与刷新令牌都失效。
				_, err = e.callerOf(other.AccessToken)
				eq(t, codeOf(err), "unauthenticated", "其它设备的访问令牌")
				_, err = core.Refresh(e.ctx, e.h.Context, other.RefreshToken)
				eq(t, codeOf(err), "session_revoked", "其它设备的刷新令牌")
				// 旧口令登不上，新口令能登。
				_, err = e.login("alice", password)
				eq(t, codeOf(err), "credentials_invalid", "旧口令")
				_, err = e.login("alice", next)
				eq(t, codeOf(err), "ok", "新口令")
			})

			t.Run("访客不能改口令", func(t *testing.T) { // 例 7
				e := fresh(t, create, Options{Policy: shortPolicy})
				e.ownerOf()
				guest := core.Caller{Subject: "guest:" + strings.Repeat("a", 16) + ":x", DeviceID: "d", Role: core.RoleGuest, LinkID: strings.Repeat("a", 16)}
				_, err := change(e, guest, password, next)
				eq(t, codeOf(err), "forbidden", "访客")
			})
		})

		t.Run("限流来自 RateLimiter 端口", func(t *testing.T) { // 例 8
			e := fresh(t, create, Options{Limit: func(core.LimitKind, string) core.LimitResult {
				return core.LimitResult{Allowed: false, RetryAfter: 1234 * time.Millisecond}
			}})
			e.must(e.h.Backend.AddAccount(e.ctx, "t1", account(t, "alice", "u-1")))
			_, err := core.NewAccounts(e.h.Context).Login(e.ctx, core.LoginInput{Account: "alice", Password: password, Device: device}, "ip")
			var failure *core.Error
			if !asError(err, &failure) {
				t.Fatalf("应是控制核的错误：%v", err)
			}
			eq(t, [2]any{failure.Code, failure.RetryAfter}, [2]any{gen.ErrRateLimited, 1234 * time.Millisecond}, "限流")
		})
	})

	t.Run("源、链接与撤销", func(t *testing.T) {
		t.Run("登记的源归签发注册令牌的账号；在线状态来自中继端口", func(t *testing.T) { // 例 9
			e := fresh(t, create, Options{})
			owner := e.ownerOf()
			sources, result := e.registered(owner)
			eq(t, result.OwnerAccountID, "u-1", "归属")
			list, err := sources.List(e.ctx, owner)
			e.must(err)
			if list[0].Online {
				t.Fatal("还没上线")
			}
			e.h.SetOnline(sourceID, "0.3.0")
			list, err = sources.List(e.ctx, owner)
			e.must(err)
			eq(t, [2]any{list[0].Online, list[0].CoreVersion}, [2]any{true, "0.3.0"}, "在线")
		})

		t.Run("隧道节点地址：缺省 issuer 的 wss /t/v1；装配方给 tunnelUrl 时原样答（可带 query，§17）", func(t *testing.T) { // 例 10
			e := fresh(t, create, Options{})
			owner := e.ownerOf()
			sources, _ := e.registered(owner)
			source, err := e.h.Context.Ports.Sources.Get(e.ctx, "t1", sourceID)
			e.must(err)
			info, err := sources.RelayFor(e.ctx, *source)
			e.must(err)
			eq(t, info.Nodes, []core.TunnelNode{{URL: "wss://cloud.armadra.test/t/v1", Weight: 1}}, "缺省节点")
			e.h.Context.TunnelNodes = func(_ context.Context, id string) ([]core.TunnelNode, error) {
				return []core.TunnelNode{{URL: "wss://relay.armadra.test/t/v1?src=" + id, Weight: 1}}, nil
			}
			info, err = core.NewSources(e.h.Context).RelayFor(e.ctx, *source)
			e.must(err)
			eq(t, info.Nodes, []core.TunnelNode{{URL: "wss://relay.armadra.test/t/v1?src=" + sourceID, Weight: 1}}, "装配方给的节点")
		})

		t.Run("访客 accept → 断言；次数用完 link_exhausted；撤销链接连同访客设备与中继令牌一起失效", func(t *testing.T) { // 例 11
			e := fresh(t, create, Options{})
			owner := e.ownerOf()
			sources, _ := e.registered(owner)
			links := core.NewLinks(e.h.Context, sources)
			created, err := links.Create(e.ctx, owner, core.CreateLinkInput{
				SourceID: sourceID, InvitationID: "inv", Role: "viewer",
				ExpiresAtMs: e.h.Clock.Ms() + 60_000, MaxUses: ptr(int64(1)),
			})
			e.must(err)
			guest, err := links.Accept(e.ctx, core.AcceptInput{LinkID: created.LinkID, Secret: created.Secret}, "ip")
			e.must(err)
			eq(t, guest.GuestSession.Account.DisplayName, "guest", "访客显示名")
			// cloud-api §20：源登记的公钥的指纹（访客按链接片段钉扎，这里只是提示）。
			eq(t, guest.IdentityFp, core.IdentityFingerprint(sourceKey(t)), "身份指纹")
			_, err = links.Accept(e.ctx, core.AcceptInput{LinkID: created.LinkID, Secret: created.Secret}, "ip")
			eq(t, codeOf(err), "link_exhausted", "次数用完")
			relayJTI := payloadOf(t, guest.RelayToken)["jti"].(string)
			revoked, err := core.IsRevokedJTI(e.ctx, e.h.Context, relayJTI, e.h.Clock.Ms())
			e.must(err)
			if revoked {
				t.Fatal("撤销前中继令牌有效")
			}
			e.must(links.Revoke(e.ctx, owner, created.LinkID))
			revoked, err = core.IsRevokedJTI(e.ctx, e.h.Context, relayJTI, e.h.Clock.Ms())
			e.must(err)
			if !revoked {
				t.Fatal("撤销链接后中继令牌应失效")
			}
			disconnects := e.h.Disconnects()
			eq(t, disconnects[len(disconnects)-1], core.ConnectionFilter{SourceID: sourceID, SubjectPrefix: "guest:" + created.LinkID + ":"}, "断开访客（带 sourceId，节点命令必填）")
			_, err = e.callerOf(guest.GuestSession.AccessToken)
			eq(t, codeOf(err), "unauthenticated", "访客的访问令牌")
		})

		t.Run("分享范围：访客断言带 scp、只读时中继令牌带 ro；links.get / list 报范围；sources.assertion 沿用", func(t *testing.T) { // 例 12
			e := fresh(t, create, Options{})
			owner := e.ownerOf()
			sources, _ := e.registered(owner)
			links := core.NewLinks(e.h.Context, sources)
			base := core.CreateLinkInput{SourceID: sourceID, InvitationID: "inv", Role: "viewer", ExpiresAtMs: e.h.Clock.Ms() + 60_000}
			with := func(scope *store.LinkScope) core.CreateLinkInput {
				input := base
				input.Scope = scope
				return input
			}
			scoped, err := links.Create(e.ctx, owner, with(scopeOf("ws_01", "", ptr(true))))
			e.must(err)
			info, err := links.Get(e.ctx, scoped.LinkID)
			e.must(err)
			eq(t, info.Scope, scopeOf("ws_01", "", ptr(true)), "links.get 的范围")
			accepted, err := links.Accept(e.ctx, core.AcceptInput{LinkID: scoped.LinkID, Secret: scoped.Secret}, "ip")
			e.must(err)
			eq(t, payloadOf(t, accepted.Assertion)["scp"], []any{"ws:ws_01", "ro"}, "断言的 scp")
			eq(t, payloadOf(t, accepted.RelayToken)["ro"], true, "中继令牌的 ro")
			guest, err := e.callerOf(accepted.GuestSession.AccessToken)
			e.must(err)
			again, err := sources.Assertion(e.ctx, guest, sourceID, nil)
			e.must(err)
			eq(t, payloadOf(t, again.Assertion)["scp"], []any{"ws:ws_01", "ro"}, "沿用范围")
			eq(t, payloadOf(t, again.RelayToken)["ro"], true, "沿用只读")

			session, err := links.Create(e.ctx, owner, with(scopeOf("ws_01", "sess_01", nil)))
			e.must(err)
			viaSession, err := links.Accept(e.ctx, core.AcceptInput{LinkID: session.LinkID, Secret: session.Secret}, "ip")
			e.must(err)
			eq(t, payloadOf(t, viaSession.Assertion)["scp"], []any{"ws:ws_01", "sess:sess_01", "ro"}, "会话范围")
			listed, err := links.List(e.ctx, owner, "")
			e.must(err)
			eq(t, []*store.LinkScope{listed[0].Scope, listed[1].Scope},
				[]*store.LinkScope{scopeOf("ws_01", "", ptr(true)), scopeOf("ws_01", "sess_01", ptr(true))}, "list 的范围")

			// owner 的断言与令牌没有范围。
			mine, err := sources.Assertion(e.ctx, owner, sourceID, nil)
			e.must(err)
			if _, present := payloadOf(t, mine.Assertion)["scp"]; present {
				t.Fatal("owner 的断言不该有 scp")
			}
			if _, present := payloadOf(t, mine.RelayToken)["ro"]; present {
				t.Fatal("owner 的中继令牌不该有 ro")
			}
			// 协议 1.3：中继令牌的 dev 就是断言里的设备 id（owner 与访客都带）。
			deviceOf := func(assertion string) any { return payloadOf(t, assertion)["device"].(map[string]any)["deviceId"] }
			eq(t, payloadOf(t, mine.RelayToken)["dev"], deviceOf(mine.Assertion), "owner 的 dev")
			eq(t, payloadOf(t, again.RelayToken)["dev"], deviceOf(again.Assertion), "访客的 dev")
		})

		t.Run("缺省 scope = 旧行为：断言没有 scp、中继令牌没有 ro，摘要没有 scope；可写的工作空间范围不带 ro", func(t *testing.T) { // 例 13
			e := fresh(t, create, Options{})
			owner := e.ownerOf()
			sources, _ := e.registered(owner)
			links := core.NewLinks(e.h.Context, sources)
			base := core.CreateLinkInput{SourceID: sourceID, InvitationID: "inv", Role: "editor", ExpiresAtMs: e.h.Clock.Ms() + 60_000}
			for _, scope := range []*store.LinkScope{nil, {}, {ReadOnly: ptr(false)}} {
				input := base
				input.Scope = scope
				created, err := links.Create(e.ctx, owner, input)
				e.must(err)
				info, err := links.Get(e.ctx, created.LinkID)
				e.must(err)
				if info.Scope != nil {
					t.Fatalf("缺省范围不该有 scope：%+v", info.Scope)
				}
				accepted, err := links.Accept(e.ctx, core.AcceptInput{LinkID: created.LinkID, Secret: created.Secret}, "ip")
				e.must(err)
				assertion := payloadOf(t, accepted.Assertion)
				for _, key := range []string{"scp", "role"} {
					if _, present := assertion[key]; present {
						t.Fatalf("断言不该有 %s", key)
					}
				}
				if _, present := payloadOf(t, accepted.RelayToken)["ro"]; present {
					t.Fatal("中继令牌不该有 ro")
				}
			}
			input := base
			input.Scope = scopeOf("ws_02", "", ptr(false))
			writable, err := links.Create(e.ctx, owner, input)
			e.must(err)
			accepted, err := links.Accept(e.ctx, core.AcceptInput{LinkID: writable.LinkID, Secret: writable.Secret}, "ip")
			e.must(err)
			eq(t, payloadOf(t, accepted.Assertion)["scp"], []any{"ws:ws_02"}, "可写的工作空间范围")
			if _, present := payloadOf(t, accepted.RelayToken)["ro"]; present {
				t.Fatal("可写范围的中继令牌不该有 ro")
			}
			listed, err := links.List(e.ctx, owner, "")
			e.must(err)
			eq(t, listed[len(listed)-1].Scope, scopeOf("ws_02", "", nil), "list 里的可写范围（readOnly: false 不存）")
		})

		t.Run("会话范围要工作空间、不能可写：bad_request", func(t *testing.T) { // 例 14
			e := fresh(t, create, Options{})
			owner := e.ownerOf()
			sources, _ := e.registered(owner)
			links := core.NewLinks(e.h.Context, sources)
			base := core.CreateLinkInput{SourceID: sourceID, InvitationID: "inv", Role: "viewer", ExpiresAtMs: e.h.Clock.Ms() + 60_000}
			for name, scope := range map[string]*store.LinkScope{
				"只有 sessionId": {SessionID: ptr("s")},
				"会话范围显式可写":     {WorkspaceID: ptr("w"), SessionID: ptr("s"), ReadOnly: ptr(false)},
			} {
				input := base
				input.Scope = scope
				_, err := links.Create(e.ctx, owner, input)
				eq(t, codeOf(err), "bad_request", name)
			}
		})

		t.Run("撤销源：关隧道、断开、发事件", func(t *testing.T) { // 例 15
			e := fresh(t, create, Options{})
			owner := e.ownerOf()
			sources, _ := e.registered(owner)
			e.must(sources.Revoke(e.ctx, owner, sourceID))
			eq(t, e.h.ClosedTunnels(), []string{sourceID}, "关掉的隧道")
			eq(t, e.h.Disconnects(), []core.ConnectionFilter{{SourceID: sourceID}}, "断开的连接")
			events := e.h.Events()
			eq(t, events[len(events)-1], core.Event(core.NewSourceRevoked(sourceID)), "最后一个事件")
			revoked, err := core.IsRevokedJTI(e.ctx, e.h.Context, "t."+sourceID+".x", 0)
			e.must(err)
			if !revoked {
				t.Fatal("撤销源之后它的隧道令牌应失效")
			}
		})
	})

	t.Run("me-stream 扇出", func(t *testing.T) {
		t.Run("票一次性；owner 收全部、访客只收链接的源；在线事件只转仍登记的源", func(t *testing.T) { // 例 16
			e := fresh(t, create, Options{})
			owner := e.ownerOf()
			sources, _ := e.registered(owner)
			stream := core.NewMeStream(e.h.Context)
			ticket := stream.Ticket(owner)
			got, ok, err := stream.Redeem(e.ctx, ticket.Ticket)
			e.must(err)
			eq(t, [2]any{got, ok}, [2]any{owner, true}, "兑换")
			if _, ok, _ := stream.Redeem(e.ctx, ticket.Ticket); ok {
				t.Fatal("票只能用一次")
			}
			if _, ok, _ := stream.Redeem(e.ctx, ""); ok {
				t.Fatal("空票不能兑换")
			}

			links := core.NewLinks(e.h.Context, sources)
			created, err := links.Create(e.ctx, owner, core.CreateLinkInput{
				SourceID: sourceID, InvitationID: "inv", Role: "viewer", ExpiresAtMs: e.h.Clock.Ms() + 60_000,
			})
			e.must(err)
			accepted, err := links.Accept(e.ctx, core.AcceptInput{LinkID: created.LinkID, Secret: created.Secret}, "ip")
			e.must(err)
			guest, err := e.callerOf(accepted.GuestSession.AccessToken)
			e.must(err)

			var mu sync.Mutex
			var toOwner, toGuest []string
			record := func(into *[]string) func(core.Event) {
				return func(event core.Event) {
					name := reflect.ValueOf(event).FieldByName("Type").String()
					mu.Lock()
					*into = append(*into, name)
					mu.Unlock()
				}
			}
			_, _ = stream.Attach(owner, record(&toOwner), nil)
			_, _ = stream.Attach(guest, record(&toGuest), nil)
			e.h.Context.Emit(core.NewSourceRevoked(strings.Repeat("f", 32)), nil)
			e.h.Publish(core.PresenceEvent{Online: true, SourceID: sourceID, CoreVersion: "1", AtMs: 1})
			e.h.Publish(core.PresenceEvent{Online: true, SourceID: strings.Repeat("e", 32), CoreVersion: "1", AtMs: 1})
			// 事件在各连接自己的协程里送达：等 owner 收齐（顺序不变）再核对。
			deadline := time.Now().Add(3 * time.Second)
			for {
				mu.Lock()
				done := len(toOwner) >= 2 && len(toGuest) >= 1
				mu.Unlock()
				if done || time.Now().After(deadline) {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			time.Sleep(20 * time.Millisecond) // 多余的事件若有，也该到了
			mu.Lock()
			defer mu.Unlock()
			eq(t, toOwner, []string{"sourceRevoked", "sourceOnline"}, "owner 收到的")
			eq(t, toGuest, []string{"sourceOnline"}, "访客收到的")
			valid, err := stream.StillValid(e.ctx, guest)
			e.must(err)
			if !valid {
				t.Fatal("访客的设备仍有效")
			}
		})
	})
}

func shortPolicy(password string, _ []string) gen.ErrorCode {
	if len([]rune(password)) < 12 {
		return gen.ErrPasswordTooShort
	}
	return ""
}
