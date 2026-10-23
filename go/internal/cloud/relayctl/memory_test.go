package relayctl_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/relayctl"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
)

var ctx = context.Background()

type clock struct{ ms int64 }

func (c *clock) now() time.Time { return time.UnixMilli(c.ms) }

func newMemory() (*relayctl.Memory, *clock) {
	c := &clock{ms: 1_000_000}
	return relayctl.NewMemory(c.now), c
}

func issue(t *testing.T, m *relayctl.Memory, subject, source, device, jti string, exp int64) {
	t.Helper()
	if err := m.IndexIssued(ctx, relayctl.Issued{Subject: subject, SourceID: source, DeviceID: device, JTI: jti, ExpMs: exp}); err != nil {
		t.Fatal(err)
	}
}

func jtis(m *relayctl.Memory) []string {
	var out []string
	for _, r := range m.RevokedTokens() {
		out = append(out, r.JTI)
	}
	return out
}

func TestDeviceRevokedRevokesOnlyThatDeviceAndKicksEachSource(t *testing.T) {
	m, _ := newMemory()
	issue(t, m, "acct-1", "srcA", "dev1", "s.dev1.srcA.x", 9_000_000)
	issue(t, m, "acct-1", "srcA", "dev1", "r.dev1.srcA.x", 9_000_000)
	issue(t, m, "acct-1", "srcB", "dev1", "r.dev1.srcB.x", 9_000_000)
	issue(t, m, "acct-1", "srcA", "dev2", "r.dev2.srcA.x", 9_000_000)
	issue(t, m, "acct-2", "srcA", "dev1", "r.dev1.srcA.y", 9_000_000)
	m.Revocations().DeviceRevoked(ctx, "acct-1", "dev1")
	if got, want := jtis(m), []string{"r.dev1.srcA.x", "r.dev1.srcB.x", "s.dev1.srcA.x"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("撤销的令牌：%v", got)
	}
	want := []relayctl.Command{
		{Type: relayctl.CommandKick, SourceID: "srcA", Subject: "acct-1", DeviceID: "dev1", Reason: "device_revoked"},
		{Type: relayctl.CommandKick, SourceID: "srcB", Subject: "acct-1", DeviceID: "dev1", Reason: "device_revoked"},
	}
	if got := m.Commands(); !reflect.DeepEqual(got, want) {
		t.Fatalf("命令：%+v", got)
	}
	// 别的设备、别的账号的令牌不受影响，再撤一次是空操作。
	m.Revocations().DeviceRevoked(ctx, "acct-1", "dev1")
	if len(m.Commands()) != 2 || len(jtis(m)) != 3 {
		t.Fatal("重复撤销应是空操作")
	}
}

func TestDevicesRevokedKeepsCurrentDevice(t *testing.T) {
	m, _ := newMemory()
	issue(t, m, "acct-1", "srcA", "cur", "r.cur.srcA.x", 9_000_000)
	issue(t, m, "acct-1", "srcA", "old", "r.old.srcA.x", 9_000_000)
	m.Revocations().DevicesRevoked(ctx, "acct-1", "cur")
	if got := jtis(m); !reflect.DeepEqual(got, []string{"r.old.srcA.x"}) {
		t.Fatalf("应留下当前设备：%v", got)
	}
	cmds := m.Commands()
	if len(cmds) != 1 || cmds[0].DeviceID != "old" || cmds[0].SourceID != "srcA" || cmds[0].Subject != "acct-1" {
		t.Fatalf("只踢被撤的设备：%+v", cmds)
	}
	m.Revocations().DevicesRevoked(ctx, "acct-1", "")
	if got := jtis(m); len(got) != 2 {
		t.Fatalf("keep 为空撤全部：%v", got)
	}
}

func TestLinkAndSourceRevoked(t *testing.T) {
	m, _ := newMemory()
	guest := core.GuestPrefix("0123456789abcdef") + "aa"
	other := core.GuestPrefix("fedcba9876543210") + "bb"
	issue(t, m, guest, "srcA", "g1", "r.g1.srcA.x", 9_000_000)
	issue(t, m, other, "srcA", "g2", "r.g2.srcA.x", 9_000_000)
	issue(t, m, "acct-1", "srcA", "d1", "r.d1.srcA.x", 9_000_000)
	issue(t, m, relayctl.NoAccount, "srcA", "", "t.srcA.x", 9_000_000)
	issue(t, m, "acct-1", "srcB", "d1", "r.d1.srcB.x", 9_000_000)
	m.Revocations().LinkRevoked(ctx, "srcA", "0123456789abcdef")
	if got := jtis(m); !reflect.DeepEqual(got, []string{"r.g1.srcA.x"}) {
		t.Fatalf("只撤这条链接的访客：%v", got)
	}
	if len(m.Commands()) != 0 {
		t.Fatalf("链接撤销的踢人由 core 的 Disconnect 做：%+v", m.Commands())
	}
	m.Revocations().SourceRevoked(ctx, "srcA")
	if got := jtis(m); !reflect.DeepEqual(got, []string{"r.d1.srcA.x", "r.g1.srcA.x", "r.g2.srcA.x", "t.srcA.x"}) {
		t.Fatalf("源撤销撤该源全部令牌（含隧道令牌），不动别的源：%v", got)
	}
}

func TestRelayPortAdapter(t *testing.T) {
	m, _ := newMemory()
	relay := m.Relay()
	if _, ok, _ := relay.Route(ctx, "srcA"); ok {
		t.Fatal("没上线")
	}
	m.SetRoute("srcA", "0.10.1")
	if version, ok, err := relay.Route(ctx, "srcA"); err != nil || !ok || version != "0.10.1" {
		t.Fatalf("路由取隧道上报的版本：%q %v %v", version, ok, err)
	}
	relay.CloseTunnel("srcA")
	relay.Disconnect(core.ConnectionFilter{SourceID: "srcA", SubjectPrefix: "guest:x:"})
	want := []relayctl.Command{
		{Type: relayctl.CommandCloseTunnel, SourceID: "srcA"},
		{Type: relayctl.CommandKick, SourceID: "srcA", SubjectPrefix: "guest:x:", Reason: "disconnect"},
	}
	if got := m.Commands(); !reflect.DeepEqual(got, want) {
		t.Fatalf("命令：%+v", got)
	}
	m.SetRoute("srcA", "")
	if _, ok, _ := relay.Route(ctx, "srcA"); ok {
		t.Fatal("下线")
	}
}

func TestExpiredEntriesDropped(t *testing.T) {
	m, c := newMemory()
	issue(t, m, "acct-1", "srcA", "d1", "r.d1.srcA.x", 1_500_000)
	m.Revocations().DeviceRevoked(ctx, "acct-1", "d1")
	if !m.IsRevoked("r.d1.srcA.x") {
		t.Fatal("应在撤销表里")
	}
	c.ms = 1_600_000
	if m.IsRevoked("r.d1.srcA.x") || len(m.RevokedTokens()) != 0 {
		t.Fatal("令牌到期后撤销记录可以丢")
	}
	issue(t, m, "acct-1", "srcA", "d2", "r.d2.srcA.x", 1_700_000)
	c.ms = 1_800_000
	n, err := m.RevokeWhere(ctx, relayctl.Filter{Subject: "acct-1"})
	if err != nil || n != 0 {
		t.Fatalf("到期的索引不再选中：%d %v", n, err)
	}
}

func TestRevokeWhereEmptyFilterSelectsNothing(t *testing.T) {
	m, _ := newMemory()
	issue(t, m, "acct-1", "srcA", "d1", "r.d1.srcA.x", 9_000_000)
	if n, err := m.RevokeWhere(ctx, relayctl.Filter{}); err != nil || n != 0 {
		t.Fatalf("空过滤不选任何令牌：%d %v", n, err)
	}
	if n, _ := m.RevokeWhere(ctx, relayctl.Filter{SourceID: "srcA", DeviceID: "nope"}); n != 0 {
		t.Fatal("过滤取交集")
	}
	if n, _ := m.RevokeWhere(ctx, relayctl.Filter{SourceID: "srcA", DeviceID: "d1"}); n != 1 {
		t.Fatal("交集命中")
	}
}

func TestNodesDrainQuotaIncident(t *testing.T) {
	m, _ := newMemory()
	m.SetNode(relayctl.Node{ID: "b", URL: "wss://b.edge.example/t/v1", Weight: 1})
	m.SetNode(relayctl.Node{ID: "a", URL: "wss://a.edge.example/t/v1", Weight: 1})
	nodes, _ := m.Nodes(ctx)
	if len(nodes) != 2 || nodes[0].ID != "a" {
		t.Fatalf("节点按 id 排序：%+v", nodes)
	}
	if err := m.Drain(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	nodes, _ = m.Nodes(ctx)
	if len(nodes) != 1 || nodes[0].ID != "b" {
		t.Fatalf("排空的节点从列表去掉：%+v", nodes)
	}
	if got := m.Commands(); len(got) != 1 || got[0].Type != relayctl.CommandDrain || got[0].Node != "a" {
		t.Fatalf("drain 命令：%+v", got)
	}
	_ = m.SetTeamQuota(ctx, "t1", relayctl.Quota{Concurrent: 10})
	if q, ok := m.Quota("t1"); !ok || q.Concurrent != 10 {
		t.Fatalf("额度：%+v", q)
	}
	_ = m.SetIncident(ctx, relayctl.Incident{RejectNewConnections: true, AtMs: 5})
	if !m.CurrentIncident().RejectNewConnections {
		t.Fatal("事故开关")
	}
}

// 作为 core 的撤销钩子端到端：撤销设备之后按索引撤令牌（用 core.RevokeDevice 触发）。
var _ core.RevocationSink = relayctl.NewMemory(nil).Revocations()
