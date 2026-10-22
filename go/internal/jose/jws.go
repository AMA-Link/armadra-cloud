// Package jose 实现协议用的 EdDSA compact JWS：签名、验签与声明校验，以及 JWKS 缓存。
//
// 验证顺序与 packages/cloud-shared/src/jwt.ts 一致，签名没过之前不看声明内容：
// typ → kid 查钥 → 签名 → schema → aud → iss → 时间 → jti 撤销回调。
package jose

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

// 拆包失败的两种原因。验证时它们都算 malformed（与 TypeScript 一侧相同）。
var (
	ErrMalformed      = errors.New("不是合法的 compact JWS")
	ErrAlgUnsupported = errors.New("只接受 alg = EdDSA")
)

// Header 是 JWS 头。
type Header struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ,omitempty"`
}

// Parts 是拆开的 compact JWS，签名还没验。
type Parts struct {
	Header Header
	// TypPresent 为真：头里写了 typ（哪怕是空串）。Header.Typ 为空时靠它区分「没写」与「写了空串」，
	// 源 JWS 的 typ 是可选的，但写了就必须等于 armadra-source（空串要拒）。
	TypPresent bool
	// Payload 是载荷的 JSON 文本（base64url 解码后，一定是 JSON 对象）。
	Payload []byte
	// SigningInput 是 "<头>.<载荷>" 的原文字节：验签的输入。
	SigningInput []byte
	Signature    []byte
}

const b64Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

// decodeB64 严格解码 base64url：只收字母表、无填充、无多余位。
// encoding/base64 会悄悄跳过换行，所以字符要先自己查一遍。
func decodeB64(s string) ([]byte, error) {
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(b64Alphabet, s[i]) < 0 {
			return nil, ErrMalformed
		}
	}
	out, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil {
		return nil, ErrMalformed
	}
	return out, nil
}

func decodeObject(segment string) (map[string]any, []byte, error) {
	raw, err := decodeB64(segment)
	if err != nil {
		return nil, nil, err
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil, nil, ErrMalformed
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, nil, ErrMalformed
	}
	return obj, raw, nil
}

// Split 拆一个 compact JWS，不验签。
func Split(token string) (Parts, error) {
	segments := strings.Split(token, ".")
	if len(segments) != 3 {
		return Parts{}, ErrMalformed
	}
	for _, s := range segments {
		if s == "" {
			return Parts{}, ErrMalformed
		}
	}
	rawHeader, _, err := decodeObject(segments[0])
	if err != nil {
		return Parts{}, err
	}
	alg, ok := rawHeader["alg"].(string)
	if !ok {
		return Parts{}, ErrMalformed
	}
	if alg != "EdDSA" {
		return Parts{}, ErrAlgUnsupported
	}
	kid, ok := rawHeader["kid"].(string)
	if !ok || kid == "" {
		return Parts{}, ErrMalformed
	}
	header := Header{Alg: alg, Kid: kid}
	typPresent := false
	if typ, present := rawHeader["typ"]; present {
		typPresent = true
		if header.Typ, ok = typ.(string); !ok {
			return Parts{}, ErrMalformed
		}
	}
	_, payload, err := decodeObject(segments[1])
	if err != nil {
		return Parts{}, err
	}
	signature, err := decodeB64(segments[2])
	if err != nil || len(signature) != ed25519.SignatureSize {
		return Parts{}, ErrMalformed
	}
	return Parts{
		Header:       header,
		TypPresent:   typPresent,
		Payload:      payload,
		SigningInput: []byte(segments[0] + "." + segments[1]),
		Signature:    signature,
	}, nil
}

// Sign 用私钥签一个 compact JWS。头的字段顺序固定为 alg、kid、typ；claims 用 encoding/json 序列化。
func Sign(key ed25519.PrivateKey, header Header, claims any) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	return SignPayload(key, header, payload)
}

// SignPayload 签一个已经序列化好的载荷（JSON 对象文本，原样进令牌，不再转义）：声明的字段顺序
// 与转义由调用方定（控制核按 TypeScript 的顺序、不转义 <>& 写出载荷）。
func SignPayload(key ed25519.PrivateKey, header Header, payload []byte) (string, error) {
	header.Alg = "EdDSA"
	if header.Kid == "" {
		return "", errors.New("头里缺 kid")
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString(headerJSON) + "." + enc.EncodeToString(payload)
	return signingInput + "." + enc.EncodeToString(ed25519.Sign(key, []byte(signingInput))), nil
}
