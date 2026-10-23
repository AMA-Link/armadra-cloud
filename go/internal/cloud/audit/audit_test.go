package audit

import (
	"strings"
	"testing"
)

func TestIPPrefix(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.77":                     "203.0.113.0/24",
		"::ffff:203.0.113.77":              "203.0.113.0/24",
		"2001:db8:1234:5678:9abc:def0:1:2": "2001:db8:1234::/48",
		"[2001:db8::1]":                    "2001:db8::/48",
		"not-an-ip":                        "",
		"":                                 "",
	} {
		if got := IPPrefix(in); got != want {
			t.Errorf("IPPrefix(%q) = %q，应为 %q", in, got, want)
		}
	}
}

func TestTruncateKeepsWholeRunes(t *testing.T) {
	long := strings.Repeat("机", 200)
	got := truncate(long)
	if len([]rune(got)) != deviceNameMax || !strings.HasPrefix(long, got) {
		t.Fatalf("按码点截到 %d：%d", deviceNameMax, len([]rune(got)))
	}
	if truncate("短") != "短" {
		t.Fatal("不超长原样")
	}
}
