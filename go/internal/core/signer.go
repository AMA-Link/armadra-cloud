package core

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/jose"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// 签名钥与令牌签发（中继设计 §6.3，协议包 §4.2）。Ed25519，kid 递增（k1、k2…）。
//
//   - JWKS 答所有未退役（retiredAtMs 为空或在未来）的公钥；
//   - 签发用最新的那把未退役钥；
//   - rotate-key 追加新钥，旧钥 retiredAtMs = now + 1h（最长令牌寿命），期间旧令牌仍验得过。
//
// 时间声明是整数秒。私钥只在 store.KeyStore 背后，不进日志与响应。

// Claim 是一个声明；Claims 保持写入顺序（签出的 JSON 字段顺序与 TypeScript 一致）。
type Claim struct {
	Key   string
	Value any
}

// Claims 是有序的声明列表。
type Claims []Claim

// SignOptions 是签发选项。
type SignOptions struct {
	TTL time.Duration
	// JTIPrefix 非空时 jti 是 <前缀>.<24 位 hex>，否则是 32 位 hex。
	JTIPrefix string
	// NoVer 为真时不写 ver 声明。
	NoVer bool
}

// SignedToken 是签出的令牌。
type SignedToken struct {
	Token       string
	JTI         string
	ExpiresAtMs int64
}

// Signer 用租户的签名钥签发令牌。
type Signer struct {
	keys store.KeyStore
	now  func() time.Time

	mu      sync.Mutex
	private map[string]ed25519.PrivateKey
}

// NewSigner 建签发器；now 缺省 time.Now。
func NewSigner(keys store.KeyStore, now func() time.Time) *Signer {
	if now == nil {
		now = time.Now
	}
	return &Signer{keys: keys, now: now, private: map[string]ed25519.PrivateKey{}}
}

// Issuer 是令牌的 iss。
func (s *Signer) Issuer(ctx context.Context) (string, error) { return s.keys.Issuer(ctx) }

func live(key store.SigningKeyRecord, nowMs int64) bool {
	return key.RetiredAtMs == nil || *key.RetiredAtMs > nowMs
}

// JWKS 答所有未退役的公钥。
func (s *Signer) JWKS(ctx context.Context) (gen.JWKSet, error) {
	keys, err := s.keys.SigningKeys(ctx)
	if err != nil {
		return gen.JWKSet{}, err
	}
	nowMs := s.now().UnixMilli()
	set := gen.JWKSet{Keys: []gen.Ed25519PublicJWK{}}
	for _, key := range keys {
		if live(key, nowMs) {
			set.Keys = append(set.Keys, key.PublicJWK)
		}
	}
	return set, nil
}

// Key 实现 jose.Keys：按 kid 取未退役的验签公钥。
func (s *Signer) Key(ctx context.Context, kid string) (ed25519.PublicKey, bool, error) {
	set, err := s.JWKS(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, jwk := range set.Keys {
		if jwk.Kid != nil && *jwk.Kid == kid {
			raw, err := base64.RawURLEncoding.DecodeString(jwk.X)
			if err != nil || len(raw) != ed25519.PublicKeySize {
				return nil, false, nil
			}
			return ed25519.PublicKey(raw), true, nil
		}
	}
	return nil, false, nil
}

// current 是签发用的钥：最新的那把未退役钥（activeFrom 允许比现在晚 1 秒内）。
func (s *Signer) current(ctx context.Context) (store.SigningKeyRecord, error) {
	keys, err := s.keys.SigningKeys(ctx)
	if err != nil {
		return store.SigningKeyRecord{}, err
	}
	nowMs := s.now().UnixMilli()
	var pool []store.SigningKeyRecord
	for _, key := range keys {
		if key.RetiredAtMs == nil && key.ActiveFromMs <= nowMs+1000 {
			pool = append(pool, key)
		}
	}
	if len(pool) == 0 {
		for _, key := range keys {
			if live(key, nowMs) {
				pool = append(pool, key)
			}
		}
	}
	if len(pool) == 0 {
		return store.SigningKeyRecord{}, errors.New("没有可用的签名钥")
	}
	best := pool[0]
	for _, key := range pool[1:] {
		if key.ActiveFromMs >= best.ActiveFromMs {
			best = key
		}
	}
	return best, nil
}

func (s *Signer) privateKey(record store.SigningKeyRecord) (ed25519.PrivateKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key, ok := s.private[record.Kid]; ok {
		return key, nil
	}
	key, err := store.ParsePrivateKey(record.PrivateKeyPKCS8)
	if err != nil {
		return nil, err
	}
	s.private[record.Kid] = key
	return key, nil
}

// Sign 签一个令牌：iss、iat、exp、jti（给了前缀就是 <前缀>.<随机>）与 ver 由这里补。
func (s *Signer) Sign(ctx context.Context, typ string, claims Claims, options SignOptions) (SignedToken, error) {
	key, err := s.current(ctx)
	if err != nil {
		return SignedToken{}, err
	}
	issuer, err := s.Issuer(ctx)
	if err != nil {
		return SignedToken{}, err
	}
	nowMs := s.now().UnixMilli()
	iat := nowMs / 1000
	exp := (nowMs + options.TTL.Milliseconds()) / 1000
	jti := RandomHex(16)
	if options.JTIPrefix != "" {
		jti = options.JTIPrefix + "." + RandomHex(12)
	}
	payload := append(Claims{{"iss", issuer}}, claims...)
	payload = append(payload, Claim{"iat", iat}, Claim{"exp", exp}, Claim{"jti", jti})
	if !options.NoVer {
		payload = append(payload, Claim{"ver", gen.ClaimsVersion})
	}
	body, err := payload.marshal()
	if err != nil {
		return SignedToken{}, err
	}
	private, err := s.privateKey(key)
	if err != nil {
		return SignedToken{}, err
	}
	token, err := jose.Sign(private, jose.Header{Kid: key.Kid, Typ: typ}, json.RawMessage(body))
	if err != nil {
		return SignedToken{}, err
	}
	return SignedToken{Token: token, JTI: jti, ExpiresAtMs: exp * 1000}, nil
}

// marshal 按写入顺序序列化成 JSON 对象。
func (c Claims) marshal() ([]byte, error) {
	out := []byte{'{'}
	for i, claim := range c {
		if i > 0 {
			out = append(out, ',')
		}
		key, err := json.Marshal(claim.Key)
		if err != nil {
			return nil, err
		}
		var value bytes.Buffer
		encoder := json.NewEncoder(&value)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(claim.Value); err != nil {
			return nil, err
		}
		out = append(append(append(out, key...), ':'), bytes.TrimRight(value.Bytes(), "\n")...)
	}
	return append(out, '}'), nil
}
