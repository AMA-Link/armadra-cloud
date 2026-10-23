package core

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"github.com/AMA-Link/armadra-cloud/go/internal/identity"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// 控制面的随机值与哈希。存储里只存哈希，明文只在签发那一刻返回一次。
//
//   - 刷新令牌：sha256("armadra/identity/v1/refresh\0" + 值)，与 core 的令牌哈希同一拼法；
//   - 注册令牌、分享链接秘密、流票据：同一拼法，前缀换成本仓自己的 armadra/relay/v1/。

// SecretKind 是中继自己的秘密种类。
type SecretKind string

// 秘密种类。
const (
	SecretRegistration SecretKind = "registration"
	SecretLink         SecretKind = "link"
	SecretTicket       SecretKind = "ticket"
)

const relayHashPrefix = "armadra/relay/v1/"

func mustRandom(n int) []byte {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// 系统随机源不可用时继续签发等于签出可预测的令牌：直接停下。
		panic("core: 系统随机源不可用：" + err.Error())
	}
	return buf
}

// RandomToken 是 n 字节随机数的 base64url（无填充）。
func RandomToken(n int) string { return base64.RawURLEncoding.EncodeToString(mustRandom(n)) }

// NewUUID 是 UUIDv4 小写文本（团队、邀请等业务 id）。
func NewUUID() string {
	b := mustRandom(16)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// RandomHex 是 n 字节随机数的小写 hex。
func RandomHex(n int) string { return hex.EncodeToString(mustRandom(n)) }

// RefreshHash 是刷新令牌的哈希。
func RefreshHash(value string) string {
	hash, err := identity.TokenHash("refresh", value)
	if err != nil {
		panic(err) // refresh 是登记过的用途
	}
	return hash
}

// SecretHash 是注册令牌、分享链接秘密、流票据的哈希。
func SecretHash(kind SecretKind, value string) string {
	sum := sha256.Sum256([]byte(relayHashPrefix + string(kind) + "\x00" + value))
	return hex.EncodeToString(sum[:])
}

// SameHex 定时安全地比较两个 hex 摘要；长度不同直接假。
func SameHex(left, right string) bool { return store.SameHex(left, right) }
