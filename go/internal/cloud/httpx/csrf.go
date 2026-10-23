package httpx

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"io"

	"golang.org/x/crypto/hkdf"
)

// CSRF 值（设计 §5.2）：csrfToken = base64url(HMAC-SHA256(K_csrf, "armadra/csrf\0" + deviceId))，
// K_csrf = HKDF-SHA256(主密钥, info "armadra-cloud/csrf")。无状态，同一台设备永远是同一个值，
// auth.refresh 照常返回它。

const csrfInfo = "armadra-cloud/csrf"

// CSRF 算与核对 CSRF 值。
type CSRF struct{ key []byte }

// NewCSRF 从主密钥派生 CSRF 钥。
func NewCSRF(masterKey []byte) (*CSRF, error) {
	key := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, masterKey, nil, []byte(csrfInfo)), key); err != nil {
		return nil, err
	}
	return &CSRF{key: key}, nil
}

// Token 是设备的 CSRF 值。
func (c *CSRF) Token(deviceID string) string {
	mac := hmac.New(sha256.New, c.key)
	mac.Write([]byte("armadra/csrf\x00" + deviceID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Check 常量时间比较请求带来的值与该设备的 CSRF 值。
func (c *CSRF) Check(deviceID, given string) bool {
	return subtle.ConstantTimeCompare([]byte(c.Token(deviceID)), []byte(given)) == 1
}
