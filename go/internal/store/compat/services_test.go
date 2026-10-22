package compat_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/core/coretest"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/pgtest"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/postgres"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/sqlite"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil"
)

// 控制核服务跑在 TypeScript 写出来的库上：TypeScript 的 scrypt 口令记录、刷新令牌哈希、链接秘密哈希、
// 注册令牌哈希、PKCS#8 签名钥，Go 的服务都认；Go 签出的令牌再交给 TypeScript 的校验（verifyJwt +
// 协议包的 zod schema）验。

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
	OK      bool           `json:"ok"`
	Failure string         `json:"failure"`
	Claims  map[string]any `json:"claims"`
}

// verifyWithTypeScript 让 TypeScript 的令牌校验验一批令牌。
func verifyWithTypeScript(t *testing.T, issuer string, nowMs int64, jwks gen.JWKSet, tokens []map[string]string) []verified {
	t.Helper()
	probe, _ := runNodeAvailable(t)
	if !probe {
		return nil
	}
	input, err := json.Marshal(map[string]any{"issuer": issuer, "nowMs": nowMs, "jwks": jwks, "tokens": tokens})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("node", filepath.Join(compatDir(t), "verify-tokens.mjs"))
	cmd.Dir = testutil.RepoRoot(t)
	cmd.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("TypeScript 验令牌失败：%v\n%s", err, stderr.String())
	}
	var results []verified
	if err := json.Unmarshal(out, &results); err != nil {
		t.Fatal(err)
	}
	return results
}

// runNodeAvailable 判断 TypeScript 一侧能不能跑；不能就跳过那一半（返回 false 而不是让整个测试跳过）。
func runNodeAvailable(t *testing.T) (bool, string) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Log("没有 node：跳过 TypeScript 验令牌")
		return false, ""
	}
	root := testutil.RepoRoot(t)
	for _, dir := range []string{"packages/cloud-shared/node_modules", "packages/platform-protocol/dist"} {
		if _, err := os.Stat(filepath.Join(root, dir)); err != nil {
			t.Logf("%s 不存在（pnpm install / libs:build）：跳过 TypeScript 验令牌", dir)
			return false, ""
		}
	}
	out, err := exec.Command("node", "--version").Output()
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(out)), "v24") && !strings.HasPrefix(strings.TrimSpace(string(out)), "v25") && !strings.HasPrefix(strings.TrimSpace(string(out)), "v26") {
		t.Logf("node %s 低于 24：跳过 TypeScript 验令牌", strings.TrimSpace(string(out)))
		return false, ""
	}
	return true, strings.TrimSpace(string(out))
}

func TestServicesOnTypeScriptWrittenDatabase(t *testing.T) {
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

			// 口令：TypeScript 派生的 scrypt 记录，Go 验得过；失败计数被清零。
			session, err := core.NewAccounts(h.Context).Login(ctx, core.LoginInput{
				Account: "owner", Password: want.Password, Device: core.DeviceInput{Platform: "desktop", Name: "go"},
			}, "198.51.100.1")
			if err != nil {
				t.Fatalf("登录 TypeScript 写的账号：%v", err)
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

			// 刷新令牌：哈希拼法与 TypeScript 一致，所以能用 TypeScript 签发的刷新令牌换新会话并旋转。
			refreshed, err := core.Refresh(ctx, h.Context, want.Secrets.OwnerRefreshToken)
			if err != nil {
				t.Fatalf("刷新 TypeScript 发的刷新令牌：%v", err)
			}
			if refreshed.Device.DeviceID != want.IDs.OwnerDevice || refreshed.RefreshToken == want.Secrets.OwnerRefreshToken {
				t.Fatalf("刷新应旋转令牌：%+v", refreshed.Device)
			}
			if _, err := core.Refresh(ctx, h.Context, want.Secrets.OwnerRefreshToken); err == nil {
				t.Fatal("旧令牌重放应被拒绝")
			}

			// 链接：TypeScript 存的秘密哈希，Go 认；范围随断言走。
			sources := core.NewSources(h.Context)
			links := core.NewLinks(h.Context, sources)
			accepted, err := links.Accept(ctx, core.AcceptInput{LinkID: want.IDs.Link, Secret: "fixture-link-secret"}, "198.51.100.2")
			if err != nil {
				t.Fatalf("接受 TypeScript 建的链接：%v", err)
			}
			if got := claimsOf(t, accepted.Assertion)["scp"]; len(got.([]any)) != 2 {
				t.Fatalf("断言的 scp：%v", got)
			}
			link, _ := h.Context.Ports.Links.Get(ctx, tenant, want.IDs.Link)
			if link.Uses != 2 {
				t.Fatalf("uses = %d", link.Uses)
			}

			// 注册令牌：TypeScript 存的哈希，Go 认，且一次性。
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
				t.Fatalf("用 TypeScript 签发的注册令牌登记：%v", err)
			}
			if err := register(); err == nil {
				t.Fatal("注册令牌一次性")
			}

			// 断言、中继令牌、隧道令牌、访问令牌：Go 用 TypeScript 写的签名钥签出，TypeScript 验得过。
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
				t.Fatalf("JWKS 应有 k1、k2（TypeScript 写的）和 Go 轮换出的 k3：%+v %v", jwks.Keys, err)
			}
			results := verifyWithTypeScript(t, issuer, want.NowMs+60_000, jwks, []map[string]string{
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
					t.Errorf("TypeScript 验第 %d 个令牌失败：%s", i, result.Failure)
				}
			}
			if len(results) > 0 {
				if results[2].Claims["dev"] != caller.DeviceID {
					t.Errorf("中继令牌的 dev：%v", results[2].Claims["dev"])
				}
				if results[6].Claims["ro"] != true {
					t.Errorf("只读链接的中继令牌应带 ro：%v", results[6].Claims)
				}
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
