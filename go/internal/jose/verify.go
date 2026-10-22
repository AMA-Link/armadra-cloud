package jose

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/schema"
)

// Failure 是验证失败的原因，与 TypeScript 一侧的 JwtFailure 同名。
// 它是本包内部的枚举，不是线上错误码；调用方映射到 gen 里登记的码。
type Failure string

// 验证失败的原因。
const (
	FailMalformed        Failure = "malformed"
	FailWrongType        Failure = "wrong_type"
	FailUnknownKey       Failure = "unknown_key"
	FailBadSignature     Failure = "bad_signature"
	FailBadClaims        Failure = "bad_claims"
	FailAudienceMismatch Failure = "audience_mismatch"
	FailIssuerMismatch   Failure = "issuer_mismatch"
	FailExpired          Failure = "expired"
	FailNotYetValid      Failure = "not_yet_valid"
	FailRevoked          Failure = "revoked"
)

// Error 是 Verify 的失败。Err 是底层原因（取钥失败、撤销查询失败等），可为空。
type Error struct {
	Failure Failure
	Err     error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return "令牌验证失败：" + string(e.Failure) + "：" + e.Err.Error()
	}
	return "令牌验证失败：" + string(e.Failure)
}

func (e *Error) Unwrap() error { return e.Err }

func fail(f Failure, err error) error { return &Error{Failure: f, Err: err} }

// FailureOf 取错误的失败原因；不是 *Error 时返回空串。
func FailureOf(err error) Failure {
	var e *Error
	if errors.As(err, &e) {
		return e.Failure
	}
	return ""
}

// Keys 按 kid 取验签公钥。没有这把钥返回 (nil, false, nil)；取不到（网络等）返回 error。
type Keys interface {
	Key(ctx context.Context, kid string) (ed25519.PublicKey, bool, error)
}

// StaticKeys 是固定的一组钥。
type StaticKeys map[string]ed25519.PublicKey

// Key 实现 Keys。
func (s StaticKeys) Key(_ context.Context, kid string) (ed25519.PublicKey, bool, error) {
	key, ok := s[kid]
	return key, ok, nil
}

// Options 是一次验证的参数。
type Options struct {
	// Typ 是头里 typ 必须等于的值，取自 gen.TokenType*。
	Typ string
	// Schema 校验声明，取自 gen.*ClaimsSchema。
	Schema *schema.Schema
	// ExpectedAud 与 ExpectedIss 逐字相等才过。
	ExpectedAud string
	ExpectedIss string
	Now         time.Time
	// Skew 是时钟偏差容忍，零值取协议的 5 分钟。
	Skew time.Duration
	Keys Keys
	// IsRevoked 查 jti 是否已撤销。必须给：与 TypeScript 一侧一样，缺了就报错而不是悄悄跳过。
	IsRevoked func(ctx context.Context, jti string) (bool, error)
	// SkipRevocation 明确不查撤销（例如断言走重放表而不是撤销表）。
	// 只有 IsRevoked 为 nil 时才看它。
	SkipRevocation bool
}

// 选项缺必填项时 Verify 返回的错误（不是 *Error，不对应令牌的失败原因）。
var (
	ErrNoKeys       = errors.New("Options.Keys 不能为空")
	ErrNoSchema     = errors.New("Options.Schema 不能为空")
	ErrNoRevocation = errors.New("Options.IsRevoked 不能为空；确实不查撤销要显式设 SkipRevocation")
)

func (o Options) check() error {
	switch {
	case o.Keys == nil:
		return ErrNoKeys
	case o.Schema == nil:
		return ErrNoSchema
	case o.IsRevoked == nil && !o.SkipRevocation:
		return ErrNoRevocation
	}
	return nil
}

// Registered 是注册声明。
type Registered struct {
	Iss string
	Aud string
	Jti string
	Iat int64
	Exp int64
	Nbf *int64
}

// Result 是验证通过的令牌。
type Result struct {
	Header     Header
	Registered Registered
	// Payload 是声明的 JSON 文本。
	Payload []byte
	schema  *schema.Schema
}

// Into 把声明解进结构体（gen.*Claims）。
func (r Result) Into(out any) error {
	return schema.Decode(r.schema, r.Payload, out)
}

func registered(claims map[string]any) Registered {
	str := func(key string) string { s, _ := claims[key].(string); return s }
	num := func(key string) int64 { f, _ := claims[key].(float64); return int64(f) }
	r := Registered{Iss: str("iss"), Aud: str("aud"), Jti: str("jti"), Iat: num("iat"), Exp: num("exp")}
	if _, ok := claims["nbf"]; ok {
		nbf := num("nbf")
		r.Nbf = &nbf
	}
	return r
}

// Verify 验一个令牌，顺序见包注释。失败时返回 *Error。
func Verify(ctx context.Context, token string, o Options) (Result, error) {
	if err := o.check(); err != nil {
		return Result{}, err
	}
	parts, err := Split(token)
	if err != nil {
		return Result{}, fail(FailMalformed, err)
	}
	if parts.Header.Typ != o.Typ {
		return Result{}, fail(FailWrongType, nil)
	}
	key, ok, err := o.Keys.Key(ctx, parts.Header.Kid)
	if err != nil {
		return Result{}, fail(FailUnknownKey, err)
	}
	if !ok {
		return Result{}, fail(FailUnknownKey, nil)
	}
	if len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, parts.SigningInput, parts.Signature) {
		return Result{}, fail(FailBadSignature, nil)
	}
	var claims any
	if err := json.Unmarshal(parts.Payload, &claims); err != nil {
		return Result{}, fail(FailBadClaims, err)
	}
	if err := o.Schema.Validate(claims); err != nil {
		return Result{}, fail(FailBadClaims, err)
	}
	reg := registered(claims.(map[string]any))
	if reg.Aud != o.ExpectedAud {
		return Result{}, fail(FailAudienceMismatch, nil)
	}
	if reg.Iss != o.ExpectedIss {
		return Result{}, fail(FailIssuerMismatch, nil)
	}
	if f := checkTimes(reg, o); f != "" {
		return Result{}, fail(f, nil)
	}
	if !o.SkipRevocation || o.IsRevoked != nil {
		revoked, err := o.IsRevoked(ctx, reg.Jti)
		if err != nil {
			return Result{}, fail(FailRevoked, err)
		}
		if revoked {
			return Result{}, fail(FailRevoked, nil)
		}
	}
	return Result{Header: parts.Header, Registered: reg, Payload: parts.Payload, schema: o.Schema}, nil
}

// checkTimes：exp / iat / nbf 是整数秒。过期是 now - skew >= exp；
// 未生效是 iat 或 nbf 晚于 now + skew。
func checkTimes(r Registered, o Options) Failure {
	skew := o.Skew
	if skew == 0 {
		skew = time.Duration(gen.IdentityAssertionSkewMs) * time.Millisecond
	}
	now := o.Now.UnixMilli()
	skewMs := skew.Milliseconds()
	if now-skewMs >= r.Exp*1000 {
		return FailExpired
	}
	if r.Iat*1000 > now+skewMs {
		return FailNotYetValid
	}
	if r.Nbf != nil && *r.Nbf*1000 > now+skewMs {
		return FailNotYetValid
	}
	return ""
}
