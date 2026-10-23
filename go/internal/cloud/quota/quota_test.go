package quota_test

import (
	"encoding/json"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/quota"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
)

func TestFreeLimits(t *testing.T) {
	personal := quota.ForPersonal("free", nil)
	if personal.Teams != 1 || personal.Hosts != 2 || personal.TrafficBytes != 5<<30 {
		t.Fatalf("个人空间：%+v", personal)
	}
	team := quota.ForTeam("free", nil)
	if team.Hosts != 3 || team.Members != 3 || team.Guests != 3 || team.Concurrent != 10 || team.TrafficBytes != 5<<30 {
		t.Fatalf("团队空间：%+v", team)
	}
	// 未知计划按 free（只会更严）。
	if quota.ForTeam("platinum", nil) != team {
		t.Fatal("未知计划应按 free")
	}
}

func TestOverrides(t *testing.T) {
	team := quota.ForTeam("free", json.RawMessage(`{"members": 10, "guests": -1, "concurrent": 50, "bogus": 1}`))
	if team.Members != 10 || team.Guests != quota.Unlimited || team.Concurrent != 50 || team.Hosts != 3 {
		t.Fatalf("overrides：%+v", team)
	}
	// 坏了的 overrides 当作没有：不会放宽。
	for _, raw := range []string{`not json`, `[1]`, `{"members": "many"}`, ``} {
		if got := quota.ForTeam("free", json.RawMessage(raw)); got != quota.ForTeam("free", nil) {
			t.Fatalf("%q 应被忽略：%+v", raw, got)
		}
	}
	if quota.ForTeam("free", json.RawMessage(`{"members": -5}`)).Members != 3 {
		t.Fatal("小于 -1 的值不算数")
	}
}

func TestCheck(t *testing.T) {
	if err := quota.Check(quota.Members, 2, 3); err != nil {
		t.Fatal(err)
	}
	if err := quota.Check(quota.Members, 99, quota.Unlimited); err != nil {
		t.Fatal("不限永不越过")
	}
	err := quota.Check(quota.Guests, 3, 3)
	coreErr, ok := err.(*core.Error)
	if !ok || coreErr.Code != "limit_reached" {
		t.Fatalf("到上限应答 limit_reached：%v", err)
	}
	want := map[string]any{"quota": "guests", "used": 3, "limit": 3}
	for key, value := range want {
		if coreErr.Details[key] != value {
			t.Fatalf("details[%s] = %v，应为 %v", key, coreErr.Details[key], value)
		}
	}
}
