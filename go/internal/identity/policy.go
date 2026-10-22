package identity

import (
	_ "embed"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// commonPasswords 是随包的常见口令表，与 packages/control-core/src/common-passwords.txt
// 逐字相同（测试比 sha256），两边一起改。
//
//go:embed common-passwords.txt
var commonPasswords string

var (
	defaultCommon     map[string]struct{}
	defaultCommonOnce sync.Once
)

// ParseCommonPasswords 把常见口令表的文本（# 注释、空行忽略）解成小写集合。
func ParseCommonPasswords(text string) map[string]struct{} {
	entries := map[string]struct{}{}
	for _, line := range strings.Split(text, "\n") {
		value := strings.TrimSpace(line)
		if value == "" || strings.HasPrefix(value, "#") {
			continue
		}
		entries[strings.ToLower(value)] = struct{}{}
	}
	return entries
}

// CommonPasswords 返回随包的常见口令集合（小写）。调用方不得修改。
func CommonPasswords() map[string]struct{} {
	defaultCommonOnce.Do(func() { defaultCommon = ParseCommonPasswords(commonPasswords) })
	return defaultCommon
}

// Violation 返回第一条不过的口令策略，全过时返回空串。错误码与 core 同名。顺序：
//
//  1. 码点数 ≥ 12（password_too_short）；
//  2. UTF-8 ≤ 1024 字节（password_too_long）；
//  3. 不含账号名，三个字符以下的名字不算（password_contains_name）；
//  4. 不在常见口令表里，小写精确比对（password_too_common）。
//
// common 为 nil 时用随包的表。
func Violation(password string, names []string, common map[string]struct{}) gen.ErrorCode {
	if utf8.RuneCountInString(password) < gen.IdentityPasswordPolicyMinLengthDefault {
		return gen.ErrPasswordTooShort
	}
	if len(password) > gen.IdentityPasswordPolicyMaxBytes {
		return gen.ErrPasswordTooLong
	}
	lowered := strings.ToLower(password)
	for _, name := range names {
		value := strings.ToLower(strings.TrimSpace(name))
		if utf8.RuneCountInString(value) < gen.IdentityPasswordPolicyMinNameLength {
			continue
		}
		if strings.Contains(lowered, value) {
			return gen.ErrPasswordContainsName
		}
	}
	if common == nil {
		common = CommonPasswords()
	}
	if _, ok := common[lowered]; ok {
		return gen.ErrPasswordTooCommon
	}
	return ""
}
