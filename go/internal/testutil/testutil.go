// Package testutil 给测试读协议包的夹具与密钥。只在 _test.go 里用。
package testutil

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// FixedNow 是夹具里令牌签发用的固定时刻（毫秒），与协议包的 FIXED_NOW_MS 相同。
const FixedNow = 1_760_000_000_000

// ProtocolDir 返回 packages/platform-protocol 的路径：从当前目录往上找。
func ProtocolDir(t testing.TB) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		candidate := filepath.Join(dir, "packages", "platform-protocol")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("找不到 packages/platform-protocol")
		}
		dir = parent
	}
}

// RepoRoot 返回仓库根目录。
func RepoRoot(t testing.TB) string {
	t.Helper()
	return filepath.Dir(filepath.Dir(ProtocolDir(t)))
}

// Fixture 读 fixtures/ 下的一个文件。
func Fixture(t testing.TB, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(ProtocolDir(t), "fixtures", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// FixtureJSON 读 fixtures/ 下的 JSON 文件并解到 out。
func FixtureJSON(t testing.TB, name string, out any) {
	t.Helper()
	if err := json.Unmarshal(Fixture(t, name), out); err != nil {
		t.Fatalf("%s：%v", name, err)
	}
}

// Fixtures 列出 fixtures/<dir>/ 下扩展名为 ext 的文件名（排序后）。
func Fixtures(t testing.TB, dir, ext string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(ProtocolDir(t), "fixtures", dir))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ext {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// Key 是 keys.json 里的一把测试钥（私钥公开在仓库里，只用于测试）。
type Key struct {
	Kid     string
	Public  ed25519.PublicKey
	Private ed25519.PrivateKey
	// Issuer 只有签发者的钥才有。
	Issuer string
}

type keyFile struct {
	Issuer struct {
		Kid        string `json:"kid"`
		Issuer     string `json:"issuer"`
		PrivateJWK struct {
			D string `json:"d"`
		} `json:"privateJwk"`
		PublicJWK struct {
			X string `json:"x"`
		} `json:"publicJwk"`
	} `json:"issuer"`
	Source struct {
		Kid        string `json:"kid"`
		SourceID   string `json:"sourceId"`
		PrivateJWK struct {
			D string `json:"d"`
		} `json:"privateJwk"`
		PublicJWK struct {
			X string `json:"x"`
		} `json:"publicJwk"`
	} `json:"source"`
}

func decodeKey(t testing.TB, kid, x, d string) Key {
	t.Helper()
	pub, err := base64.RawURLEncoding.DecodeString(x)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := base64.RawURLEncoding.DecodeString(d)
	if err != nil {
		t.Fatal(err)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	if string(priv.Public().(ed25519.PublicKey)) != string(pub) {
		t.Fatalf("%s：私钥与公钥不配", kid)
	}
	return Key{Kid: kid, Public: pub, Private: priv}
}

// Keys 读 assertion/keys.json，返回签发者的钥与源的钥。
func Keys(t testing.TB) (issuer, source Key) {
	t.Helper()
	var file keyFile
	FixtureJSON(t, "assertion/keys.json", &file)
	issuer = decodeKey(t, file.Issuer.Kid, file.Issuer.PublicJWK.X, file.Issuer.PrivateJWK.D)
	issuer.Issuer = file.Issuer.Issuer
	source = decodeKey(t, file.Source.Kid, file.Source.PublicJWK.X, file.Source.PrivateJWK.D)
	return issuer, source
}
