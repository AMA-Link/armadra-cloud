// Package identity 是身份算法：口令的 scrypt 派生与校验、令牌哈希、口令策略。
// 参数全部取自协议包的生成物（gen.Identity*），与 packages/control-core 的实现逐字对应。
package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"golang.org/x/crypto/scrypt"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// PasswordRecord 是存进库里的口令记录。校验按记录里的参数，换参数后旧记录照样能验。
type PasswordRecord struct {
	KDF      string `json:"kdf"`
	Cost     int    `json:"cost"`
	Block    int    `json:"block"`
	Parallel int    `json:"parallel"`
	Length   int    `json:"length"`
	// Salt 与 Hash 是标准 base64（带填充），与 TypeScript 一侧的存法相同。
	Salt string `json:"salt"`
	Hash string `json:"hash"`
}

// Derive 按协议参数（N=2^15、r=8、p=1、32 字节）派生密钥。
func Derive(password string, salt []byte) ([]byte, error) {
	return scrypt.Key([]byte(password), salt,
		scryptCost(), gen.IdentityScryptBlock, gen.IdentityScryptParallel, gen.IdentityScryptLength)
}

// HashPassword 用随机盐派生口令记录。
func HashPassword(password string) (PasswordRecord, error) {
	salt := make([]byte, gen.IdentityScryptSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return PasswordRecord{}, err
	}
	return HashPasswordWithSalt(password, salt)
}

// HashPasswordWithSalt 用给定的盐派生口令记录（测试与迁移用）。
func HashPasswordWithSalt(password string, salt []byte) (PasswordRecord, error) {
	hash, err := Derive(password, salt)
	if err != nil {
		return PasswordRecord{}, err
	}
	return PasswordRecord{
		KDF:      "scrypt",
		Cost:     scryptCost(),
		Block:    gen.IdentityScryptBlock,
		Parallel: gen.IdentityScryptParallel,
		Length:   gen.IdentityScryptLength,
		Salt:     base64.StdEncoding.EncodeToString(salt),
		Hash:     base64.StdEncoding.EncodeToString(hash),
	}, nil
}

// VerifyPassword 按记录里的参数派生并定时安全地比较。超过 maxBytes 的口令直接判否。
func VerifyPassword(password string, record PasswordRecord) bool {
	if len(password) > gen.IdentityPasswordPolicyMaxBytes || record.KDF != "scrypt" {
		return false
	}
	salt, err := base64.StdEncoding.DecodeString(record.Salt)
	if err != nil {
		return false
	}
	expected, err := base64.StdEncoding.DecodeString(record.Hash)
	if err != nil || len(expected) != record.Length {
		return false
	}
	actual, err := scrypt.Key([]byte(password), salt, record.Cost, record.Block, record.Parallel, record.Length)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(expected, actual) == 1
}

// BurnPasswordCheck 在账号名不对时也做一次同样成本的派生，别让响应时间泄露账号名。
func BurnPasswordCheck(password string) {
	if len(password) > gen.IdentityPasswordPolicyMaxBytes {
		password = password[:gen.IdentityPasswordPolicyMaxBytes]
	}
	_, _ = Derive(password, make([]byte, gen.IdentityScryptSaltBytes))
}

// TokenHash 是令牌哈希：sha256("armadra/identity/v1/" + kind + "\0" + value) 的小写 hex。
// kind 必须是协议登记的用途之一。存储里只存哈希，明文只在签发那一刻返回一次。
func TokenHash(kind, value string) (string, error) {
	known := false
	for _, k := range gen.IdentityTokenHashKinds {
		if k == kind {
			known = true
			break
		}
	}
	if !known {
		return "", fmt.Errorf("未登记的令牌用途：%q", kind)
	}
	sum := sha256.Sum256([]byte(gen.IdentityTokenHashPrefix + kind + "\x00" + value))
	return hex.EncodeToString(sum[:]), nil
}
