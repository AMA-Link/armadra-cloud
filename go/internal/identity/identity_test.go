package identity_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/identity"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil"
)

type vectors struct {
	Scrypt []struct {
		Password string `json:"password"`
		Salt     string `json:"salt"`
		Hex      string `json:"hex"`
	} `json:"scrypt"`
	TokenHash []struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
		Hex   string `json:"hex"`
	} `json:"tokenHash"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(testutil.ProtocolDir(t), "src", "identity-vectors", "vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := jsonUnmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// identity-vectors/vectors.json 全过：scrypt 与令牌哈希。
func TestScryptVectors(t *testing.T) {
	v := loadVectors(t)
	if len(v.Scrypt) != 3 {
		t.Fatalf("scrypt 向量应有 3 条，实际 %d", len(v.Scrypt))
	}
	for _, vec := range v.Scrypt {
		t.Run(vec.Password, func(t *testing.T) {
			salt, err := hex.DecodeString(vec.Salt)
			if err != nil || len(salt) != gen.IdentityScryptSaltBytes {
				t.Fatalf("盐应是 %d 字节：%v", gen.IdentityScryptSaltBytes, err)
			}
			got, err := identity.Derive(vec.Password, salt)
			if err != nil {
				t.Fatal(err)
			}
			if hex.EncodeToString(got) != vec.Hex {
				t.Fatalf("scrypt 不同：\n得到 %x\n应为 %s", got, vec.Hex)
			}
		})
	}
}

func TestTokenHashVectors(t *testing.T) {
	v := loadVectors(t)
	kinds := map[string]bool{}
	for _, vec := range v.TokenHash {
		got, err := identity.TokenHash(vec.Kind, vec.Value)
		if err != nil {
			t.Fatal(err)
		}
		if got != vec.Hex {
			t.Errorf("%s(%s)：得到 %s，应为 %s", vec.Kind, vec.Value, got, vec.Hex)
		}
		kinds[vec.Kind] = true
	}
	// 每种用途都有向量。
	for _, kind := range gen.IdentityTokenHashKinds {
		if !kinds[kind] {
			t.Errorf("没有 %s 的向量", kind)
		}
	}
	if len(kinds) != len(gen.IdentityTokenHashKinds) {
		t.Errorf("向量的用途 %v 与登记的 %v 不一致", kinds, gen.IdentityTokenHashKinds)
	}
	if _, err := identity.TokenHash("nope", "x"); err == nil {
		t.Error("没登记的用途应报错")
	}
}

func TestScryptParametersAreTheProtocolOnes(t *testing.T) {
	if gen.IdentityScryptCost != 1<<15 || gen.IdentityScryptBlock != 8 || gen.IdentityScryptParallel != 1 ||
		gen.IdentityScryptLength != 32 || gen.IdentityScryptSaltBytes != 16 {
		t.Fatal("scrypt 参数应为 N=2^15、r=8、p=1、32 字节、16 字节盐")
	}
	if gen.IdentityTokenHashPrefix != "armadra/identity/v1/" {
		t.Fatal("令牌哈希前缀不对")
	}
}

func TestPasswordRecordRoundTrip(t *testing.T) {
	salt, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f")
	rec, err := identity.HashPasswordWithSalt("correct horse battery staple", salt)
	if err != nil {
		t.Fatal(err)
	}
	// 与协议向量里同一盐、同一口令的结果一致。
	if want := loadVectors(t).Scrypt[0].Hex; hex.EncodeToString(mustDecode(t, rec.Hash)) != want {
		t.Fatal("记录里的哈希应等于向量")
	}
	if rec.KDF != "scrypt" || rec.Cost != gen.IdentityScryptCost || rec.Length != gen.IdentityScryptLength {
		t.Fatalf("记录：%+v", rec)
	}
	if !identity.VerifyPassword("correct horse battery staple", rec) {
		t.Fatal("正确口令应通过")
	}
	for _, bad := range []string{"", "correct horse battery stapl", "Correct horse battery staple"} {
		if identity.VerifyPassword(bad, rec) {
			t.Errorf("%q 不该通过", bad)
		}
	}
	// 超长口令直接判否，不去派生。
	if identity.VerifyPassword(strings.Repeat("a", gen.IdentityPasswordPolicyMaxBytes+1), rec) {
		t.Error("超过 1024 字节的口令不该通过")
	}

	// 随机盐：同一口令两次的记录不同，各自都能验。
	a, err := identity.HashPassword("another password 12345")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := identity.HashPassword("another password 12345")
	if a.Salt == b.Salt || a.Hash == b.Hash {
		t.Error("随机盐应使两次的记录不同")
	}
	if !identity.VerifyPassword("another password 12345", a) || !identity.VerifyPassword("another password 12345", b) {
		t.Error("两条记录都应通过")
	}

	// 校验按记录里的参数：旧参数的记录照样能验。
	oldSalt := []byte("0123456789abcdef")
	oldHash := mustScrypt(t, "legacy password!", oldSalt, 1<<10, 8, 1, 32)
	legacy := identity.PasswordRecord{
		KDF: "scrypt", Cost: 1 << 10, Block: 8, Parallel: 1, Length: 32,
		Salt: base64.StdEncoding.EncodeToString(oldSalt), Hash: base64.StdEncoding.EncodeToString(oldHash),
	}
	if !identity.VerifyPassword("legacy password!", legacy) || identity.VerifyPassword("legacy password?", legacy) {
		t.Error("应按记录里的参数校验")
	}

	// 坏记录一律判否，不 panic。
	for name, broken := range map[string]identity.PasswordRecord{
		"kdf 不是 scrypt": {KDF: "bcrypt", Cost: 1 << 15, Block: 8, Parallel: 1, Length: 32, Salt: rec.Salt, Hash: rec.Hash},
		"盐不是 base64":    {KDF: "scrypt", Cost: 1 << 15, Block: 8, Parallel: 1, Length: 32, Salt: "!!", Hash: rec.Hash},
		"哈希不是 base64":   {KDF: "scrypt", Cost: 1 << 15, Block: 8, Parallel: 1, Length: 32, Salt: rec.Salt, Hash: "!!"},
		"长度对不上":         {KDF: "scrypt", Cost: 1 << 15, Block: 8, Parallel: 1, Length: 16, Salt: rec.Salt, Hash: rec.Hash},
		"cost 不是 2 的幂":  {KDF: "scrypt", Cost: 1000, Block: 8, Parallel: 1, Length: 32, Salt: rec.Salt, Hash: rec.Hash},
	} {
		if identity.VerifyPassword("correct horse battery staple", broken) {
			t.Errorf("%s：不该通过", name)
		}
	}
	identity.BurnPasswordCheck("whatever")
	identity.BurnPasswordCheck(strings.Repeat("x", 5000))
}

func TestPolicy(t *testing.T) {
	cases := []struct {
		name     string
		password string
		names    []string
		want     gen.ErrorCode
	}{
		{"十二个字符刚好够", "zq-vw-xy-ptk", nil, ""},
		{"十一个字符太短", "zq-vw-xy-pt", nil, gen.ErrPasswordTooShort},
		{"按码点数计：十二个汉字够", "一二三四五六七八九十甲乙", nil, ""},
		{"按码点数计：十一个汉字不够", "一二三四五六七八九十甲", nil, gen.ErrPasswordTooShort},
		{"一个 emoji 算一个码点", "🔐🔐🔐🔐🔐🔐🔐🔐🔐🔐🔐🔐", nil, ""},
		{"1024 字节刚好", strings.Repeat("a", 1024), nil, ""},
		{"1025 字节太长", strings.Repeat("a", 1025), nil, gen.ErrPasswordTooLong},
		{"按字节计长度上限", strings.Repeat("好", 342), nil, gen.ErrPasswordTooLong},
		{"含账号名", "my-dev@armadra-pass", []string{"DEV@ARMADRA"}, gen.ErrPasswordContainsName},
		{"含账号名（名字前后有空格）", "hello-owner-hello", []string{"  Owner "}, gen.ErrPasswordContainsName},
		{"两个字符的名字不算", "xxxxabxxxxxxxx", []string{"ab"}, ""},
		{"三个字符的名字算", "xxxxabcxxxxxxx", []string{"abc"}, gen.ErrPasswordContainsName},
		{"空名字不算", "zq-vw-xy-ptk", []string{"", "  "}, ""},
		{"常见口令", "password1234", nil, gen.ErrPasswordTooCommon},
		{"常见口令不分大小写", "IloveYou1234", nil, gen.ErrPasswordTooCommon},
		{"不是精确匹配就不算常见", "password1234x", nil, ""},
		{"在表里但先被最短长度挡住", "letmein.123", nil, gen.ErrPasswordTooShort},
		{"太短先于太长之外的规则", "qwerty", []string{"qwerty"}, gen.ErrPasswordTooShort},
		{"含账号名先于常见口令", "0000000000000", []string{"0000"}, gen.ErrPasswordContainsName},
		{"全是零的常见口令", "0000000000000", nil, gen.ErrPasswordTooCommon},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := identity.Violation(c.password, c.names, nil); got != c.want {
				t.Fatalf("得到 %q，应为 %q", got, c.want)
			}
		})
	}
	custom := identity.ParseCommonPasswords("# 注释\n\n  Secret-Phrase-1  \nOTHER\n")
	if identity.Violation("SECRET-phrase-1", nil, custom) != gen.ErrPasswordTooCommon {
		t.Error("自备的常见口令表应按小写比对")
	}
	if identity.Violation("secret-phrase-2", nil, custom) != "" {
		t.Error("不在自备的表里")
	}
	if len(custom) != 2 {
		t.Errorf("注释与空行不算条目：%v", custom)
	}
}

// 随包的常见口令表与 control-core 里那份逐字相同。
func TestCommonPasswordsMatchControlCore(t *testing.T) {
	original, err := os.ReadFile(filepath.Join(testutil.RepoRoot(t), "packages", "control-core", "src", "common-passwords.txt"))
	if err != nil {
		t.Fatal(err)
	}
	embedded, err := os.ReadFile("common-passwords.txt")
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(original) != sha256.Sum256(embedded) {
		t.Fatal("go/internal/identity/common-passwords.txt 与 packages/control-core/src/common-passwords.txt 不同：两边一起改")
	}
	if n := len(identity.CommonPasswords()); n != 10000 {
		t.Errorf("常见口令表应有 10000 条，实际 %d", n)
	}
}

// 降成本只影响之后新写的记录；之前的记录按自带的成本照样能验，恢复协议成本后也一样。
func TestUseScryptCostForTests(t *testing.T) {
	t.Cleanup(func() { identity.UseScryptCostForTests(gen.IdentityScryptCost) })
	protocol, err := identity.HashPassword("plover-quartz-lantern-41")
	if err != nil {
		t.Fatal(err)
	}
	if protocol.Cost != gen.IdentityScryptCost {
		t.Fatalf("缺省应取协议成本：%d", protocol.Cost)
	}
	identity.UseScryptCostForTests(1 << 4)
	fast, err := identity.HashPassword("plover-quartz-lantern-41")
	if err != nil {
		t.Fatal(err)
	}
	if fast.Cost != 1<<4 || !identity.VerifyPassword("plover-quartz-lantern-41", fast) || identity.VerifyPassword("wrong-password", fast) {
		t.Fatalf("降成本的记录应按自带成本校验：%+v", fast)
	}
	if !identity.VerifyPassword("plover-quartz-lantern-41", protocol) {
		t.Fatal("协议成本的旧记录仍应能验")
	}
}
