package protocol

import (
	"fmt"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/schema"
)

// 隧道握手是四条文本帧（JSON），ready 之后只许二进制帧：
//
//  1. core → relay  hello      带控制面签的隧道令牌与 nonce
//  2. relay → core  challenge  relay 已验隧道令牌，回 nonce2 与自己的节点名
//  3. core → relay  auth       源私钥对 SigningInput 的 Ed25519 签名
//  4. relay → core  ready 或 reject
//
// schema 非 strict：更高 minor 加的字段被忽略。

// SigningInput 是 auth 签名的输入：sourceId、nonce、nonce2、relayNode，以换行连接。
func SigningInput(sourceID, nonce, nonce2, relayNode string) []byte {
	return []byte(sourceID + "\n" + nonce + "\n" + nonce2 + "\n" + relayNode)
}

func parse[T any](data []byte, s *schema.Schema, what string) (T, error) {
	var out T
	if err := schema.Decode(s, data, &out); err != nil {
		var zero T
		return zero, fmt.Errorf("%s 不符合 schema：%w", what, err)
	}
	return out, nil
}

// ParseHello 解并校验 hello。
func ParseHello(data []byte) (gen.Hello, error) {
	return parse[gen.Hello](data, gen.HelloSchema, "hello")
}

// ParseChallenge 解并校验 challenge。
func ParseChallenge(data []byte) (gen.Challenge, error) {
	return parse[gen.Challenge](data, gen.ChallengeSchema, "challenge")
}

// ParseAuth 解并校验 auth。
func ParseAuth(data []byte) (gen.Auth, error) {
	return parse[gen.Auth](data, gen.AuthSchema, "auth")
}

// ParseReady 解并校验 ready。
func ParseReady(data []byte) (gen.Ready, error) {
	return parse[gen.Ready](data, gen.ReadySchema, "ready")
}

// ParseReject 解并校验 reject。
func ParseReject(data []byte) (gen.Reject, error) {
	return parse[gen.Reject](data, gen.RejectSchema, "reject")
}

// ParseE2EAccept 解并校验端到端通道的 accept。
func ParseE2EAccept(data []byte) (gen.E2EAccept, error) {
	return parse[gen.E2EAccept](data, gen.E2EAcceptSchema, "accept")
}
