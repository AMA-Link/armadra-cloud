package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
)

// 主密钥包裹（SaaS 实现方案 §2.2、§3.4）：SaaS 的签名私钥、TOTP 种子用主密钥（32 字节）做
// AES-256-GCM 包裹后才落库，明文从不落库。密文 = base64(12 字节随机数 ‖ 密文 ‖ 16 字节标签)，
// 附加数据绑定用途与对象（例如租户与 kid），换个位置抄过去解不开。
// 签名钥列 private_key_wrap 写 KeyWrapAESGCM，TOTP 种子的 secret_wrap 写 "aes256gcm-v1:<base64>"。

// KeyWrapAESGCM 是包裹方式的名字（signing_keys.private_key_wrap）。
const KeyWrapAESGCM = "aes256gcm-v1"

// MasterKeySize 是主密钥长度（字节）。
const MasterKeySize = 32

// ErrWrapped 表示库里是包裹过的值而没有给主密钥。
var ErrWrapped = errors.New("库里的密钥是主密钥包裹的，需要主密钥")

// ParseMasterKey 把 64 位十六进制解成主密钥。
func ParseMasterKey(text string) ([]byte, error) {
	key, err := hex.DecodeString(strings.TrimSpace(text))
	if err != nil || len(key) != MasterKeySize {
		return nil, errors.New("主密钥要是 64 位十六进制（32 字节）")
	}
	return key, nil
}

// Wrapper 用主密钥包裹与解开秘密。
type Wrapper struct{ aead cipher.AEAD }

// NewWrapper 用主密钥建包裹器。
func NewWrapper(masterKey []byte) (*Wrapper, error) {
	if len(masterKey) != MasterKeySize {
		return nil, errors.New("主密钥要是 32 字节")
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Wrapper{aead: aead}, nil
}

// Seal 包裹 plain；aad 绑定用途与对象。答 base64。
func (w *Wrapper) Seal(plain []byte, aad string) string {
	nonce := make([]byte, w.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic("store: 系统随机源不可用：" + err.Error())
	}
	return base64.StdEncoding.EncodeToString(w.aead.Seal(nonce, nonce, plain, []byte(aad)))
}

// Open 解开 Seal 的结果；密文被改、aad 不符或主密钥不对都答错误。
func (w *Wrapper) Open(sealed, aad string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil || len(raw) < w.aead.NonceSize()+w.aead.Overhead() {
		return nil, errors.New("包裹的密钥不合格式")
	}
	nonce, body := raw[:w.aead.NonceSize()], raw[w.aead.NonceSize():]
	plain, err := w.aead.Open(nil, nonce, body, []byte(aad))
	if err != nil {
		return nil, errors.New("包裹的密钥解不开：主密钥不对或密文被改")
	}
	return plain, nil
}

// SealSecret 包成 "aes256gcm-v1:<base64>"（account_credentials.secret_wrap）。
func (w *Wrapper) SealSecret(plain []byte, aad string) string {
	return KeyWrapAESGCM + ":" + w.Seal(plain, aad)
}

// OpenSecret 解开 SealSecret 的结果。
func (w *Wrapper) OpenSecret(sealed, aad string) ([]byte, error) {
	body, ok := strings.CutPrefix(sealed, KeyWrapAESGCM+":")
	if !ok {
		return nil, errors.New("包裹方式不认识")
	}
	return w.Open(body, aad)
}
