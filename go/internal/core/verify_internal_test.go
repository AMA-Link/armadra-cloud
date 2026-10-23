package core

import (
	"context"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/identity"
)

// 未知账号走等时派生：VerifyPassword(nil) 与 Login 的未知账号路径都调用它，答同样的 credentials_invalid。
func TestUnknownAccountBurnsAPasswordDerivation(t *testing.T) {
	real := burnPasswordCheck
	defer func() { burnPasswordCheck = real }()
	var burned []string
	burnPasswordCheck = func(password string) { burned = append(burned, password); identity.BurnPasswordCheck(password) }

	err := VerifyPassword(context.Background(), &Context{}, nil, "whatever-password", 0)
	if code, _ := CodeOf(err); code != "credentials_invalid" {
		t.Fatalf("未知账号：%v", err)
	}
	if len(burned) != 1 || burned[0] != "whatever-password" {
		t.Fatalf("应调用等时派生：%v", burned)
	}
}
