package accounts

import (
	"regexp"
	"strings"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

func TestNewUserCodeFormatAndAlphabet(t *testing.T) {
	pattern := regexp.MustCompile(`^[BCDFGHJKLMNPQRSTVWXZ]{4}-[BCDFGHJKLMNPQRSTVWXZ]{4}$`)
	seen := map[byte]int{}
	for i := 0; i < 4000; i++ {
		code := newUserCode()
		if !pattern.MatchString(code) {
			t.Fatalf("格式：%q", code)
		}
		for j := 0; j < len(code); j++ {
			if j != 4 {
				seen[code[j]]++
			}
		}
	}
	if len(seen) != len(gen.IdentityDeviceCodeUserCodeAlphabet) {
		t.Fatalf("字母表没用全：%d", len(seen))
	}
	// 无偏：每个字母期望 1600 次（32000 / 20），偏离过大说明取模偏置或坏的随机源。
	for letter, count := range seen {
		if count < 1300 || count > 1900 {
			t.Fatalf("%c 出现 %d 次，分布不均", letter, count)
		}
	}
}

func TestNormalizeUserCode(t *testing.T) {
	for input, want := range map[string]string{
		"BCDF-GHJK": "BCDF-GHJK", "bcdf-ghjk": "BCDF-GHJK", " bcdfghjk ": "BCDF-GHJK", "BCDF GHJK": "BCDF-GHJK",
	} {
		got, ok := normalizeUserCode(input)
		if !ok || got != want {
			t.Errorf("%q → %q %v，应为 %q", input, got, ok, want)
		}
	}
	for _, bad := range []string{"", "BCDF-GHJ", "BCDF-GHJKL", "BCDF_GHJK", "ACDF-GHJK", "BCDF-GHJ0", "BCDFG-HJK", strings.Repeat("B", 20)} {
		if got, ok := normalizeUserCode(bad); ok {
			t.Errorf("%q 不该通过：%q", bad, got)
		}
	}
}

func TestDeviceCodeHashIsDomainSeparated(t *testing.T) {
	if deviceCodeHash("a") == deviceCodeHash("b") || len(deviceCodeHash("a")) != 64 || deviceCodeHash("a") == "a" {
		t.Fatal("哈希")
	}
}
