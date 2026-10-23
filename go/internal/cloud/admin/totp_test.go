package admin_test

import (
	"strings"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/admin"
)

// RFC 6238 附录 B 的 SHA-1 向量（种子 "12345678901234567890"，取 8 位里的后 6 位）。
func TestTOTPRFC6238Vectors(t *testing.T) {
	seed := []byte("12345678901234567890")
	for unix, want := range map[int64]string{
		59: "287082", 1111111109: "081804", 1111111111: "050471", 1234567890: "005924", 2000000000: "279037", 20000000000: "353130",
	} {
		step := admin.TOTPStep(time.Unix(unix, 0))
		if got := admin.TOTPCode(seed, step); got != want {
			t.Fatalf("T=%d：%s，应为 %s", unix, got, want)
		}
		if matched, ok := admin.VerifyTOTP(seed, want, time.Unix(unix, 0)); !ok || matched != step {
			t.Fatalf("T=%d 校验：%d %v", unix, matched, ok)
		}
	}
}

func TestVerifyTOTPWindowAndShape(t *testing.T) {
	seed := admin.NewTOTPSeed()
	if len(seed) != 20 {
		t.Fatalf("种子 20 字节：%d", len(seed))
	}
	now := time.Unix(1_800_000_000, 0)
	step := admin.TOTPStep(now)
	code := func(delta int64) string { return admin.TOTPCode(seed, step+delta) }
	for _, delta := range []int64{-1, 0, 1} {
		if matched, ok := admin.VerifyTOTP(seed, code(delta), now); !ok || matched != step+delta {
			t.Fatalf("±1 步内都认，答命中的步：delta=%d %d %v", delta, matched, ok)
		}
	}
	for _, delta := range []int64{-2, 2} {
		if _, ok := admin.VerifyTOTP(seed, code(delta), now); ok {
			t.Fatalf("±2 步不认：delta=%d", delta)
		}
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef", "      ", code(0) + "0"} {
		if _, ok := admin.VerifyTOTP(seed, bad, now); ok {
			t.Fatalf("形状不对不认：%q", bad)
		}
	}
	if _, ok := admin.VerifyTOTP(seed, " "+code(0)+" ", now); !ok {
		t.Fatal("首尾空白容忍")
	}
	if _, ok := admin.VerifyTOTP(admin.NewTOTPSeed(), code(0), now); ok {
		t.Fatal("别的种子不认（1e-6 的碰撞概率可忽略）")
	}
}

func TestOTPAuthURI(t *testing.T) {
	uri := admin.OTPAuthURI("Armadra", "ops@example.com", []byte("12345678901234567890"))
	for _, part := range []string{
		"otpauth://totp/Armadra:ops@example.com?", "secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", "issuer=Armadra",
		"algorithm=SHA1", "digits=6", "period=30",
	} {
		if !strings.Contains(uri, part) {
			t.Fatalf("%q 里缺 %q", uri, part)
		}
	}
}
