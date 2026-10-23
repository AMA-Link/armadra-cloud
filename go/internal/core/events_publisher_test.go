package core_test

import (
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
)

type capture struct{ got []core.Event }

func (c *capture) Publish(event core.Event, _ *core.Audience) { c.got = append(c.got, event) }

// 设了 publisher 之后 Emit 交给它（本机分发由它回头调 Deliver）；Deliver 永远只在本机分发。
func TestEventBusPublisherTakesOverEmit(t *testing.T) {
	bus := core.NewEventBus()
	var local []core.Event
	bus.Subscribe(func(e core.Event, _ *core.Audience) { local = append(local, e) })
	bus.Emit(core.NewSourceRevoked("a"), nil)
	if len(local) != 1 {
		t.Fatalf("没有 publisher 时 Emit 直接分发：%d", len(local))
	}
	pub := &capture{}
	bus.SetPublisher(pub)
	bus.Emit(core.NewSourceRevoked("b"), nil)
	if len(local) != 1 || len(pub.got) != 1 {
		t.Fatalf("有 publisher 时 Emit 只交给它：本机 %d，publisher %d", len(local), len(pub.got))
	}
	bus.Deliver(core.NewSourceRevoked("c"), nil)
	if len(local) != 2 || len(pub.got) != 1 {
		t.Fatalf("Deliver 只在本机分发：本机 %d，publisher %d", len(local), len(pub.got))
	}
	bus.SetPublisher(nil)
	bus.Emit(core.NewSourceRevoked("d"), nil)
	if len(local) != 3 {
		t.Fatalf("取消 publisher 后恢复直接分发：%d", len(local))
	}
}

// 受众：链接访客按 LinkID，账号按 AccountIDs；空受众不投给任何人；访客不在账号受众里。
func TestAudienceIncludes(t *testing.T) {
	owner := core.Caller{AccountID: "a1", Role: core.RoleOwner}
	guest := core.Caller{LinkID: "l1", Role: core.RoleGuest}
	cases := []struct {
		name     string
		audience core.Audience
		caller   core.Caller
		want     bool
	}{
		{"账号在受众里", core.Audience{AccountIDs: []string{"a0", "a1"}}, owner, true},
		{"账号不在受众里", core.Audience{AccountIDs: []string{"a0"}}, owner, false},
		{"访客不在账号受众里", core.Audience{AccountIDs: []string{"a1"}}, guest, false},
		{"链接受众里的访客", core.Audience{LinkID: "l1"}, guest, true},
		{"别的链接", core.Audience{LinkID: "l2"}, guest, false},
		{"账号不是链接受众", core.Audience{LinkID: "l1"}, owner, false},
		{"空受众不投给任何人", core.Audience{}, owner, false},
	}
	for _, c := range cases {
		if got := c.audience.Includes(c.caller); got != c.want {
			t.Errorf("%s：%v", c.name, got)
		}
	}
}
