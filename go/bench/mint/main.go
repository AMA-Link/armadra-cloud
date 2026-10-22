// mint 签发压测用的凭据：每个源一把 Ed25519 源钥、一枚隧道令牌（cnf = 源公钥）、一枚中继令牌，
// 用本次随机生成的签发者钥签，同时写出 `armadra-relay dev --jwks` 要读的 JWKS 文件。
//
// 用法: mint <源数> <creds.json> <jwks.json>
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/jose"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

const (
	issuer = "https://cloud.armadra.test"
	kid    = "bench-issuer"
)

var b64 = base64.RawURLEncoding

type source struct {
	SourceID    string `json:"sourceId"`
	Seed        string `json:"seed"`
	TunnelToken string `json:"tunnelToken"`
	RelayToken  string `json:"relayToken"`
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func must[T any](v T, err error) T {
	if err != nil {
		fmt.Fprintln(os.Stderr, "签发失败：", err)
		os.Exit(1)
	}
	return v
}

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "用法: mint <源数> <creds.json> <jwks.json>")
		os.Exit(64)
	}
	count := must(strconv.Atoi(os.Args[1]))
	issuerPub, issuerKey := must2(ed25519.GenerateKey(rand.Reader))
	iat := time.Now().Unix()
	exp := iat + 6*3600
	sign := func(typ string, claims map[string]any) string {
		return must(jose.Sign(issuerKey, jose.Header{Kid: kid, Typ: typ}, claims))
	}
	sources := make([]source, 0, count)
	for i := 0; i < count; i++ {
		id := randHex(16)
		pub, priv := must2(ed25519.GenerateKey(rand.Reader))
		sources = append(sources, source{
			SourceID: id,
			Seed:     b64.EncodeToString(priv.Seed()),
			TunnelToken: sign(gen.TokenTypeTunnel, map[string]any{
				"iss": issuer, "sub": id, "aud": gen.TokenAudienceTunnel, "iat": iat, "exp": exp,
				"jti": "t-" + randHex(8), "ver": 1,
				"cnf": map[string]any{"jwk": map[string]any{"kty": "OKP", "crv": "Ed25519", "x": b64.EncodeToString(pub)}},
			}),
			RelayToken: sign(gen.TokenTypeRelay, map[string]any{
				"iss": issuer, "sub": "acct_" + strconv.Itoa(i), "aud": gen.TokenAudienceRelay, "src": id,
				"iat": iat, "exp": exp, "jti": "r-" + randHex(8), "ver": 1,
			}),
		})
	}
	jwk := map[string]any{"kty": "OKP", "crv": "Ed25519", "x": b64.EncodeToString(issuerPub), "kid": kid, "alg": "EdDSA", "use": "sig"}
	write(os.Args[2], map[string]any{"issuer": issuer, "issuerJwk": jwk, "sources": sources})
	write(os.Args[3], map[string]any{"keys": []any{jwk}})
	fmt.Printf("已签发：%d 个源 → %s\n", count, os.Args[2])
}

func must2(pub ed25519.PublicKey, priv ed25519.PrivateKey, err error) (ed25519.PublicKey, ed25519.PrivateKey) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "生成密钥失败：", err)
		os.Exit(1)
	}
	return pub, priv
}

func write(path string, v any) {
	b := must(json.Marshal(v))
	if err := os.WriteFile(path, b, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "写文件失败：", err)
		os.Exit(1)
	}
}
