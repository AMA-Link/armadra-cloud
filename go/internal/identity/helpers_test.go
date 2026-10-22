package identity_test

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"golang.org/x/crypto/scrypt"
)

func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

func mustDecode(t *testing.T, s string) []byte {
	t.Helper()
	out, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func mustScrypt(t *testing.T, password string, salt []byte, n, r, p, length int) []byte {
	t.Helper()
	out, err := scrypt.Key([]byte(password), salt, n, r, p, length)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
