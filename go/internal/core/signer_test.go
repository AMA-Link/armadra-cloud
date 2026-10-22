package core_test

import (
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/core/coretest"
	"github.com/AMA-Link/armadra-cloud/go/internal/jose"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

func TestSignedTokensVerifyAgainstTheJWKS(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	signer := e.h.Context.Signer
	token, err := signer.Sign(e.ctx, gen.TokenTypeAccess, core.Claims{
		{Key: "sub", Value: "acct:alice"}, {Key: "aud", Value: gen.TokenAudienceAccess}, {Key: "sid", Value: "d1"},
	}, core.SignOptions{TTL: 15 * time.Minute, JTIPrefix: "a.d1"})
	e.must(err)

	result, err := jose.Verify(e.ctx, token.Token, jose.Options{
		Typ: gen.TokenTypeAccess, Schema: gen.AccessClaimsSchema, ExpectedAud: gen.TokenAudienceAccess,
		ExpectedIss: "https://cloud.armadra.test", Now: e.h.Clock.Now(), Keys: signer,
	})
	if err != nil {
		t.Fatalf("自己签的令牌应验得过：%v", err)
	}
	if result.Header.Kid != "k1" || result.Header.Typ != gen.TokenTypeAccess {
		t.Fatalf("头：%+v", result.Header)
	}
	if !regexp.MustCompile(`^a\.d1\.[0-9a-f]{24}$`).MatchString(token.JTI) {
		t.Fatalf("jti 形如 <前缀>.<24 位 hex>：%q", token.JTI)
	}
	if token.ExpiresAtMs != (e.h.Clock.Ms()/1000+900)*1000 {
		t.Fatalf("exp 是整数秒：%d", token.ExpiresAtMs)
	}

	// 声明的字段顺序与 TypeScript 一致：iss、传入的、iat、exp、jti、ver。
	raw, _ := base64.RawURLEncoding.DecodeString(strings.Split(token.Token, ".")[1])
	order := regexp.MustCompile(`"([a-z]+)":`).FindAllStringSubmatch(string(raw), -1)
	var keys []string
	for _, m := range order {
		keys = append(keys, m[1])
	}
	eq(t, strings.Join(keys, ","), "iss,sub,aud,sid,iat,exp,jti,ver", "声明顺序")

	// 没有前缀时 jti 是 32 位 hex；NoVer 时不写 ver。
	plain, err := signer.Sign(e.ctx, gen.TokenTypeAssertion, core.Claims{{Key: "sub", Value: "x"}}, core.SignOptions{TTL: time.Minute, NoVer: true})
	e.must(err)
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(plain.JTI) {
		t.Fatalf("jti：%q", plain.JTI)
	}
	if _, present := claimsOf(t, plain.Token)["ver"]; present {
		t.Fatal("NoVer 时不该有 ver")
	}
	// 声明里的特殊字符原样，不转成 <。
	special, err := signer.Sign(e.ctx, gen.TokenTypeAssertion, core.Claims{{Key: "name", Value: "<a&b>"}}, core.SignOptions{TTL: time.Minute})
	e.must(err)
	rawSpecial, _ := base64.RawURLEncoding.DecodeString(strings.Split(special.Token, ".")[1])
	if !strings.Contains(string(rawSpecial), `"name":"<a&b>"`) {
		t.Fatalf("特殊字符应原样：%s", rawSpecial)
	}
}

// 轮换：旧钥一小时内仍在 JWKS 里、仍验得过，之后退役；新钥签发。
func TestRotationKeepsOldKeyForAnHour(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	signer := e.h.Context.Signer
	old, err := signer.Sign(e.ctx, gen.TokenTypeAssertion, core.Claims{{Key: "sub", Value: "x"}}, core.SignOptions{TTL: time.Hour})
	e.must(err)
	kid, err := e.h.Backend.RotateKey(e.ctx, "t1", e.h.Clock.Ms())
	e.must(err)
	eq(t, kid, "k2", "新 kid")

	jwks, err := signer.JWKS(e.ctx)
	e.must(err)
	if len(jwks.Keys) != 2 {
		t.Fatalf("轮换后两把钥都在 JWKS 里：%d", len(jwks.Keys))
	}
	fresh, err := signer.Sign(e.ctx, gen.TokenTypeAssertion, core.Claims{{Key: "sub", Value: "x"}}, core.SignOptions{TTL: time.Minute})
	e.must(err)
	header := func(token string) string {
		parts, err := jose.Split(token)
		e.must(err)
		return parts.Header.Kid
	}
	eq(t, header(old.Token), "k1", "旧令牌用旧钥")
	eq(t, header(fresh.Token), "k2", "新令牌用新钥")

	keyFor := func(kid string) bool {
		_, ok, err := signer.Key(e.ctx, kid)
		e.must(err)
		return ok
	}
	if !keyFor("k1") || !keyFor("k2") {
		t.Fatal("一小时内旧钥仍可用来验签")
	}
	e.h.Clock.Set(e.h.Clock.Ms() + int64(time.Hour/time.Millisecond) + 1)
	if keyFor("k1") || !keyFor("k2") {
		t.Fatal("一小时后旧钥退役，新钥仍在")
	}
	jwks, _ = signer.JWKS(e.ctx)
	if len(jwks.Keys) != 1 || jwks.Keys[0].Kid == nil || *jwks.Keys[0].Kid != "k2" {
		t.Fatalf("JWKS 只剩 k2：%+v", jwks.Keys)
	}
}

func TestSignerWithoutKeysFailsClosed(t *testing.T) {
	e := newEnv(t, coretest.Options{})
	if err := e.h.Backend.CreateTenant(e.ctx, "empty", store.Tenant{TenantSettings: store.TenantSettings{Issuer: "https://x.test", TrustedOrigins: []string{}}}); err != nil {
		t.Fatal(err)
	}
	signer := core.NewSigner(e.h.Backend.Keys("empty"), e.h.Clock.Now)
	if _, err := signer.Sign(e.ctx, gen.TokenTypeAccess, nil, core.SignOptions{TTL: time.Minute}); err == nil {
		t.Fatal("没有签名钥不能签发")
	}
}
