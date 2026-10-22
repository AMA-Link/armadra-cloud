package store

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// KeyRetireGraceMs 是旧钥退役的宽限：最长令牌寿命（中继令牌、隧道令牌都是 1 小时）。
const KeyRetireGraceMs = int64(max(gen.IdentityRelayTokenTTLMs, gen.IdentityTunnelTokenTTLMs))

// GenerateSigningKey 生成一把 Ed25519 签名钥：私钥 PKCS#8 DER 的 base64，公钥是 JWK。
func GenerateSigningKey(kid string, nowMs int64) (SigningKeyRecord, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return SigningKeyRecord{}, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return SigningKeyRecord{}, err
	}
	alg, use := "EdDSA", "sig"
	return SigningKeyRecord{
		Kid:             kid,
		PrivateKeyPKCS8: base64.StdEncoding.EncodeToString(der),
		PublicJWK: gen.Ed25519PublicJWK{
			Kty: "OKP", Crv: "Ed25519", X: base64.RawURLEncoding.EncodeToString(pub),
			Kid: &kid, Alg: &alg, Use: &use,
		},
		ActiveFromMs: nowMs,
	}, nil
}

// ParsePrivateKey 把 PKCS#8 base64 解成 Ed25519 私钥。
func ParsePrivateKey(pkcs8 string) (ed25519.PrivateKey, error) {
	der, err := base64.StdEncoding.DecodeString(pkcs8)
	if err != nil {
		return nil, fmt.Errorf("签名钥不是合法的 base64")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("签名钥不是合法的 PKCS#8")
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("签名钥不是 Ed25519")
	}
	return key, nil
}

var kidPattern = regexp.MustCompile(`^k(\d+)$`)

// NextKid 是下一把钥的 kid：现有最大编号 + 1。
func NextKid(keys []SigningKeyRecord) string {
	highest := 0
	for _, key := range keys {
		if m := kidPattern.FindStringSubmatch(key.Kid); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				highest = max(highest, n)
			}
		}
	}
	return "k" + strconv.Itoa(highest+1)
}

// RotateSigningKeys 在副本上轮换：其余未退役的钥定在 now + 宽限退役，早已退役的删掉，
// 追加新钥。答新的钥列表与新 kid。
func RotateSigningKeys(keys []SigningKeyRecord, nowMs int64) ([]SigningKeyRecord, string, error) {
	kid := NextKid(keys)
	grace := nowMs + KeyRetireGraceMs
	next := make([]SigningKeyRecord, 0, len(keys)+1)
	for _, key := range keys {
		if key.RetiredAtMs == nil || *key.RetiredAtMs > grace {
			retired := grace
			key.RetiredAtMs = &retired
		}
		if key.RetiredAtMs == nil || *key.RetiredAtMs > nowMs {
			next = append(next, key)
		}
	}
	fresh, err := GenerateSigningKey(kid, nowMs)
	if err != nil {
		return nil, "", err
	}
	return append(next, fresh), kid, nil
}
