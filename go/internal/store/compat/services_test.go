package compat_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/core/coretest"
	"github.com/AMA-Link/armadra-cloud/go/internal/jose"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/schema"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/pgtest"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/postgres"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/sqlite"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil"
)

// 控制核服务跑在冻结夹具（已部署的库）上：夹具里的 scrypt 口令记录、刷新令牌哈希、链接秘密哈希、
// 注册令牌哈希、PKCS#8 签名钥，Go 的服务都认；签出的令牌按 JWKS 用 internal/jose 逐项验
// （typ、kid、签名、协议 schema、aud、iss、时间）。

func fixtureBackends(t *testing.T) map[string]func(t *testing.T) store.Backend {
	return map[string]func(t *testing.T) store.Backend{
		"SQLite": func(t *testing.T) store.Backend {
			db, err := sqlite.Open(context.Background(), copyFixtureDB(t), sqlite.Options{})
			if err != nil {
				t.Fatal(err)
			}
			return db
		},
		"PostgreSQL": func(t *testing.T) store.Backend {
			url, schema := restoreDump(t)
			db, err := postgres.Open(context.Background(), postgres.Options{URL: url, Schema: schema})
			if err != nil {
				t.Fatal(err)
			}
			return db
		},
	}
}

type verified struct {
	OK      bool
	Failure string
	Claims  map[string]any
}

var tokenKinds = map[string]struct {
	typ    string
	schema *schema.Schema
}{
	"access":    {gen.TokenTypeAccess, gen.AccessClaimsSchema},
	"assertion": {gen.TokenTypeAssertion, gen.AssertionClaimsSchema},
	"relay":     {gen.TokenTypeRelay, gen.RelayTokenClaimsSchema},
	"tunnel":    {gen.TokenTypeTunnel, gen.TunnelTokenClaimsSchema},
}

// verifyTokens 按发布出去的 JWKS 验一批令牌（只信 JWKS 里的公钥，不碰签名私钥）。
func verifyTokens(t *testing.T, issuer string, nowMs int64, jwks gen.JWKSet, tokens []map[string]string) []verified {
	t.Helper()
	data, err := json.Marshal(jwks)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := jose.ParseJWKS(data)
	if err != nil {
		t.Fatal(err)
	}
	results := make([]verified, 0, len(tokens))
	for _, token := range tokens {
		kind, ok := tokenKinds[token["kind"]]
		if !ok {
			t.Fatalf("未知的令牌类型：%s", token["kind"])
		}
		result, err := jose.Verify(context.Background(), token["token"], jose.Options{
			Typ: kind.typ, Schema: kind.schema, ExpectedAud: token["aud"], ExpectedIss: issuer,
			Now: time.UnixMilli(nowMs), Keys: jose.StaticKeys(keys), SkipRevocation: true,
		})
		if err != nil {
			results = append(results, verified{Failure: string(jose.FailureOf(err))})
			continue
		}
		var claims map[string]any
		if err := json.Unmarshal(result.Payload, &claims); err != nil {
			t.Fatal(err)
		}
		results = append(results, verified{OK: true, Claims: claims})
	}
	return results
}

func TestServicesOnFixtureDatabase(t *testing.T) {
	for name, create := range fixtureBackends(t) {
		t.Run(name, func(t *testing.T) {
			if name == "PostgreSQL" {
				pgtest.URL(t)
			}
			want := loadExpected(t)
			ctx := context.Background()
			backend := create(t)
			t.Cleanup(func() { _ = backend.Close() })
			h := coretest.NewHarness(t, backend, coretest.Options{TenantID: tenant, NowMs: want.NowMs + 60_000, Existing: true})
			issuer := "https://relay.armadra.test"

			// 口令：夹具里的 scrypt 记录，Go 验得过；失败计数被清零。
			session, err := core.NewAccounts(h.Context).Login(ctx, core.LoginInput{
				Account: "owner", Password: want.Password, Device: core.DeviceInput{Platform: "desktop", Name: "go"},
			}, "198.51.100.1")
			if err != nil {
				t.Fatalf("登录夹具账号：%v", err)
			}
			if session.Account.AccountID != "acct:owner" || session.Account.DisplayName != "owner" {
				t.Fatalf("会话里的账号：%+v", session.Account)
			}
			account, _ := h.Context.Ports.Accounts.Get(ctx, tenant, "acct:owner")
			if account.Failures != 0 {
				t.Fatalf("登录成功应清零失败计数：%d", account.Failures)
			}
			if _, err := core.NewAccounts(h.Context).Login(ctx, core.LoginInput{
				Account: "owner", Password: want.Password + "x", Device: core.DeviceInput{Platform: "desktop", Name: "go"},
			}, "198.51.100.1"); err == nil {
				t.Fatal("错口令不该通过")
			}

			// 刷新令牌：哈希拼法与夹具一致，所以能用夹具里的刷新令牌换新会话并旋转。
			refreshed, err := core.Refresh(ctx, h.Context, want.Secrets.OwnerRefreshToken)
			if err != nil {
				t.Fatalf("刷新夹具里的刷新令牌：%v", err)
			}
			if refreshed.Device.DeviceID != want.IDs.OwnerDevice || refreshed.RefreshToken == want.Secrets.OwnerRefreshToken {
				t.Fatalf("刷新应旋转令牌：%+v", refreshed.Device)
			}
			if _, err := core.Refresh(ctx, h.Context, want.Secrets.OwnerRefreshToken); err == nil {
				t.Fatal("旧令牌重放应被拒绝")
			}

			// 链接：夹具里的秘密哈希，Go 认；范围随断言走。
			sources := core.NewSources(h.Context)
			links := core.NewLinks(h.Context, sources)
			accepted, err := links.Accept(ctx, core.AcceptInput{LinkID: want.IDs.Link, Secret: "fixture-link-secret"}, "198.51.100.2")
			if err != nil {
				t.Fatalf("接受夹具里的链接：%v", err)
			}
			if got := claimsOf(t, accepted.Assertion)["scp"]; len(got.([]any)) != 2 {
				t.Fatalf("断言的 scp：%v", got)
			}
			link, _ := h.Context.Ports.Links.Get(ctx, tenant, want.IDs.Link)
			if link.Uses != 2 {
				t.Fatalf("uses = %d", link.Uses)
			}

			// 注册令牌：夹具里的哈希，Go 认，且一次性。
			_, source := testutil.Keys(t)
			register := func() error {
				_, err := sources.Register(ctx, core.RegisterInput{
					RegistrationToken: "fixture-registration-token", SourceID: strings.Repeat("ab", 16),
					PublicKey: gen.Ed25519PublicJWK{Kty: "OKP", Crv: "Ed25519", X: base64.RawURLEncoding.EncodeToString(source.Public)},
					Name:      "go source", Kind: "server", CoreVersion: "0.10.0", Capabilities: []string{}, ProtocolMajor: 1,
				}, "198.51.100.3")
				return err
			}
			if err := register(); err != nil {
				t.Fatalf("用夹具里的注册令牌登记：%v", err)
			}
			if err := register(); err == nil {
				t.Fatal("注册令牌一次性")
			}

			// 断言、中继令牌、隧道令牌、访问令牌：Go 用夹具里的签名钥签出，按 JWKS 验得过。
			caller, err := core.NewAuthenticator(h.Context).Authenticate(ctx, "Bearer "+session.AccessToken)
			if err != nil {
				t.Fatalf("认证访问令牌：%v", err)
			}
			grant, err := sources.Assertion(ctx, caller, want.IDs.Source, nil)
			if err != nil {
				t.Fatal(err)
			}
			fixtureSource, _ := h.Context.Ports.Sources.Get(ctx, tenant, want.IDs.Source)
			tunnel, err := sources.RelayFor(ctx, *fixtureSource)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := backend.RotateKey(ctx, tenant, want.NowMs+60_000); err != nil {
				t.Fatal(err)
			}
			if rk, ok := backend.(interface {
				RefreshKeys(context.Context, string) error
			}); ok {
				_ = rk.RefreshKeys(ctx, tenant)
			}
			afterRotation, err := h.Context.Signer.Sign(ctx, gen.TokenTypeAssertion, core.Claims{
				{Key: "sub", Value: "acct:owner"}, {Key: "aud", Value: want.IDs.Source},
				{Key: "device", Value: gen.AssertionClaimsDevice{DeviceID: want.IDs.OwnerDevice, Platform: "desktop", Name: "x"}},
			}, core.SignOptions{TTL: 5 * time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			jwks, err := h.Context.Signer.JWKS(ctx)
			if err != nil || len(jwks.Keys) != 3 {
				t.Fatalf("JWKS 应有 k1、k2（夹具里的）和 Go 轮换出的 k3：%+v %v", jwks.Keys, err)
			}
			results := verifyTokens(t, issuer, want.NowMs+60_000, jwks, []map[string]string{
				{"kind": "access", "token": session.AccessToken, "aud": "cloud"},
				{"kind": "assertion", "token": grant.Assertion, "aud": want.IDs.Source},
				{"kind": "relay", "token": grant.RelayToken, "aud": "relay"},
				{"kind": "tunnel", "token": tunnel.TunnelToken, "aud": "relay-tunnel"},
				{"kind": "assertion", "token": afterRotation.Token, "aud": want.IDs.Source},
				{"kind": "access", "token": accepted.GuestSession.AccessToken, "aud": "cloud"},
				{"kind": "relay", "token": accepted.RelayToken, "aud": "relay"},
			})
			for i, result := range results {
				if !result.OK {
					t.Errorf("验第 %d 个令牌失败：%s", i, result.Failure)
				}
			}
			if results[2].Claims["dev"] != caller.DeviceID {
				t.Errorf("中继令牌的 dev：%v", results[2].Claims["dev"])
			}
			if results[6].Claims["ro"] != true {
				t.Errorf("只读链接的中继令牌应带 ro：%v", results[6].Claims)
			}
		})
	}
}

func claimsOf(t *testing.T, token string) map[string]any {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[1])
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
