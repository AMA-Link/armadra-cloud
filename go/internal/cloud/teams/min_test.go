package teams

import (
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/store/saas/teamscope"
)

// 计划与用量：读门槛放开 admin，写门槛（MinBilling）不放开；member 与 guest 两者都不行。
func TestMinBillingReadAllowsAdminButWriteDoesNot(t *testing.T) {
	roles := []struct {
		role        teamscope.Role
		read, write bool
	}{
		{teamscope.Owner, true, true},
		{teamscope.Billing, true, true},
		{teamscope.Admin, true, false},
		{teamscope.Member, false, false},
		{teamscope.Guest, false, false},
	}
	for _, c := range roles {
		if got := MinBillingRead.allows(c.role); got != c.read {
			t.Errorf("%s 读：%v", c.role, got)
		}
		if got := MinBilling.allows(c.role); got != c.write {
			t.Errorf("%s 写：%v", c.role, got)
		}
	}
}
