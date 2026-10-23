// Package admin 是运营后台（设计 §4.9）：运营账号的两步验证（TOTP）、登录挑战、admin.* 的过程与事故开关。
package admin

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 的 TOTP 用 HMAC-SHA1，认证器 App 都按它算
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// TOTP 的参数（RFC 6238：SHA-1、6 位、30 秒、前后各容 1 步）。
const (
	TOTPDigits   = 6
	TOTPPeriod   = 30 * time.Second
	TOTPSeedSize = 20
	// TOTPSkew 是前后容的步数（时钟漂移）。
	TOTPSkew = 1
)

// NewTOTPSeed 生成 20 字节的随机种子。
func NewTOTPSeed() []byte {
	seed := make([]byte, TOTPSeedSize)
	if _, err := rand.Read(seed); err != nil {
		panic("admin: 系统随机源不可用：" + err.Error())
	}
	return seed
}

// TOTPStep 是时刻 t 所在的时间步。
func TOTPStep(t time.Time) int64 { return t.Unix() / int64(TOTPPeriod/time.Second) }

// TOTPCode 是某个时间步的验证码（RFC 4226 的动态截断，6 位，前导零补齐）。
func TOTPCode(seed []byte, step int64) string {
	mac := hmac.New(sha1.New, seed)
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step))
	mac.Write(counter[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := (binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff) % 1_000_000
	return leftPad(strconv.FormatUint(uint64(value), 10), TOTPDigits)
}

func leftPad(text string, width int) string {
	if len(text) >= width {
		return text
	}
	return strings.Repeat("0", width-len(text)) + text
}

// VerifyTOTP 校验验证码：当前步和前后各 TOTPSkew 步都比（不因命中提前退出，常量时间比较），
// 答命中的时间步。命中的步要交给存储记下（只接受比上次更大的步），防止同一个码重用。
func VerifyTOTP(seed []byte, code string, now time.Time) (step int64, ok bool) {
	code = strings.TrimSpace(code)
	if len(code) != TOTPDigits {
		return 0, false
	}
	current := TOTPStep(now)
	matched := int64(-1)
	for delta := -int64(TOTPSkew); delta <= TOTPSkew; delta++ {
		candidate := current + delta
		if candidate < 0 {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(TOTPCode(seed, candidate)), []byte(code)) == 1 {
			matched = candidate
		}
	}
	return matched, matched >= 0
}

// OTPAuthURI 是认证器 App 扫描 / 导入的地址（只在 operators totp 的终端里打印一次）。
func OTPAuthURI(issuer, account string, seed []byte) string {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(seed)
	label := url.PathEscape(issuer + ":" + account)
	query := url.Values{
		"secret": {secret}, "issuer": {issuer}, "algorithm": {"SHA1"},
		"digits": {strconv.Itoa(TOTPDigits)}, "period": {strconv.Itoa(int(TOTPPeriod / time.Second))},
	}
	return "otpauth://totp/" + label + "?" + query.Encode()
}

// TOTPAAD 是 TOTP 种子包裹时绑定的附加数据：用途、租户与账号，抄到别的账号 / 租户解不开。
func TOTPAAD(tenantID, accountID string) string { return "totp\x00" + tenantID + "\x00" + accountID }
