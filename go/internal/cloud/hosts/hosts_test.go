package hosts

import (
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

func TestAtLeast(t *testing.T) {
	cases := []struct {
		version, min string
		want         bool
	}{
		{"0.10.0", "0.10.0", true},
		{"0.10.1", "0.10.0", true},
		{"0.11.0", "0.10.0", true},
		{"1.0.0", "0.10.0", true},
		{"v0.10.0", "0.10.0", true},
		{"0.10.0+build.5", "0.10.0", true},
		{"0.9.2", "0.10.0", false},
		{"0.9.99", "0.10.0", false},
		{"0.10.0-rc.1", "0.10.0", false}, // 预发布版低于同号的正式版
		{"0.10.1-rc.1", "0.10.0", true},
		{"0.10.0-rc.2", "0.10.0-rc.1", true},
		{"", "0.10.0", false},
		{"dev", "0.10.0", false},
		{"0.10", "0.10.0", false},
		{"0.10.0.1", "0.10.0", false},
		{"0.10.x", "0.10.0", false},
		{"00.10.0", "0.10.0", false},
		{"0.10.0-", "0.10.0", false},
		{"-1.10.0", "0.10.0", false},
		{"0.10.0", "dev", false},
	}
	for _, c := range cases {
		if got := AtLeast(c.version, c.min); got != c.want {
			t.Errorf("AtLeast(%q, %q) = %v，应为 %v", c.version, c.min, got, c.want)
		}
	}
}

func TestCanManage(t *testing.T) {
	team := func(owner string, role saas.TeamRole) saas.Host {
		return saas.Host{TeamID: "t1", OwnerAccountID: owner, ViewerRole: role}
	}
	cases := []struct {
		name string
		host saas.Host
		want bool
	}{
		{"个人空间的所有者", saas.Host{OwnerAccountID: "me"}, true},
		{"个人空间的别人", saas.Host{OwnerAccountID: "x"}, false},
		{"团队主机的所有者（member）", team("me", saas.RoleMember), true},
		{"团队 owner", team("x", saas.RoleOwner), true},
		{"团队 admin", team("x", saas.RoleAdmin), true},
		{"团队 member 管不了别人的", team("x", saas.RoleMember), false},
		{"billing", team("x", saas.RoleBilling), false},
		{"guest", team("x", saas.RoleGuest), false},
		{"没有角色", team("x", ""), false},
	}
	for _, c := range cases {
		if got := canManage(c.host, "me"); got != c.want {
			t.Errorf("%s：%v，应为 %v", c.name, got, c.want)
		}
	}
}

func TestSettingsOfDefaultsAndOverlay(t *testing.T) {
	if got := settingsOf(saas.Host{}); got != (hostSettings{LinkCreators: "owner_admin", AllowGuests: true, DefaultRole: "viewer"}) {
		t.Fatalf("缺省：%+v", got)
	}
	// 团队的 defaultHostRole 只作 defaultRole 的预选值；非法值忽略；主机自己写过的项优先。
	if got := settingsOf(saas.Host{TeamDefaultRole: "editor"}); got.DefaultRole != "editor" {
		t.Fatalf("团队预选：%+v", got)
	}
	if got := settingsOf(saas.Host{TeamDefaultRole: "root"}); got.DefaultRole != "viewer" {
		t.Fatalf("非法的团队预选：%+v", got)
	}
	got := settingsOf(saas.Host{TeamDefaultRole: "editor", Settings: []byte(`{"allowGuests":false,"defaultRole":"driver"}`)})
	if got != (hostSettings{LinkCreators: "owner_admin", AllowGuests: false, DefaultRole: "driver"}) {
		t.Fatalf("主机自己的设置：%+v", got)
	}
	if got := settingsOf(saas.Host{Settings: []byte(`not json`)}); got.LinkCreators != "owner_admin" {
		t.Fatalf("坏掉的库内值保持缺省：%+v", got)
	}
}
