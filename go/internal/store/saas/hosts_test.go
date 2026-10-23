package saas_test

import (
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas"
)

func TestHostCanManage(t *testing.T) {
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
		{"团队主机的所有者降成 billing 管不了", team("me", saas.RoleBilling), false},
		{"团队主机的所有者降成 guest 管不了", team("me", saas.RoleGuest), false},
		{"团队 owner", team("x", saas.RoleOwner), true},
		{"团队 admin", team("x", saas.RoleAdmin), true},
		{"团队 member 管不了别人的", team("x", saas.RoleMember), false},
		{"billing", team("x", saas.RoleBilling), false},
		{"guest", team("x", saas.RoleGuest), false},
		{"没有角色", team("x", ""), false},
	}
	for _, c := range cases {
		if got := c.host.CanManage("me"); got != c.want {
			t.Errorf("%s：%v，应为 %v", c.name, got, c.want)
		}
	}
}

func TestHostCursorRoundTrip(t *testing.T) {
	cursor := saas.HostCursor{RegisteredAtMs: 1700000000123, SourceID: "abc"}
	got, err := saas.DecodeHostCursor(cursor.Encode())
	if err != nil || *got != cursor {
		t.Fatalf("往返：%+v %v", got, err)
	}
	for _, bad := range []string{"", "***", "e30"} {
		if _, err := saas.DecodeHostCursor(bad); err == nil {
			t.Fatalf("%q 不该解出来", bad)
		}
	}
}
