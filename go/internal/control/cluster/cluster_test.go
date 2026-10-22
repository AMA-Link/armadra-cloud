package cluster_test

import (
	"context"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/control/cluster"
)

// TypeScript 节点发的广播（JSON.stringify 的输出）Go 要认得。
func TestParseBroadcastAcceptsTypeScriptMessages(t *testing.T) {
	m, err := cluster.ParseBroadcast([]byte(`{"origin":"abc","kind":"presence","event":{"type":"sourceOnline","sourceId":"s","node":"n","tunnelId":"t","coreVersion":"0.3.0","atMs":5}}`))
	if err != nil || m.Presence != (control.PresenceEvent{Type: control.PresenceSourceOnline, SourceID: "s", Node: "n", TunnelID: "t", CoreVersion: "0.3.0", AtMs: 5}) {
		t.Fatalf("%+v %v", m, err)
	}
	m, err = cluster.ParseBroadcast([]byte(`{"origin":"abc","kind":"command","command":{"type":"kick","sourceId":"s","reason":"replaced"}}`))
	if err != nil || m.Command != (control.NodeCommand{Type: control.CommandKick, SourceID: "s", Reason: "replaced"}) {
		t.Fatalf("%+v %v", m, err)
	}
	m, err = cluster.ParseBroadcast([]byte(`{"origin":"abc","kind":"revoked","jti":"j","expMs":1760000000000}`))
	if err != nil || m.JTI != "j" || m.ExpMs != 1760000000000 {
		t.Fatalf("%+v %v", m, err)
	}
}

func TestParseBroadcastIgnoresUnknownShapes(t *testing.T) {
	for _, text := range []string{
		``, `x`, `[]`, `{}`, `{"origin":"a"}`, `{"origin":"a","kind":"other"}`,
		`{"origin":"a","kind":"presence","event":{"type":"weird","sourceId":"s"}}`,
		`{"origin":"a","kind":"presence"}`,
		`{"origin":"a","kind":"command","command":{"type":"closeTunnel"}}`,
		`{"origin":"a","kind":"revoked","jti":"j"}`,
		`{"origin":"a","kind":"revoked","jti":"j","expMs":"soon"}`,
	} {
		if _, err := cluster.ParseBroadcast([]byte(text)); err == nil {
			t.Errorf("应忽略：%q", text)
		}
	}
}

func TestRouteJSONRoundTrip(t *testing.T) {
	r := control.Route{Node: "n", TunnelID: "t", SinceMs: 9, CoreVersion: "0.3.0"}
	got, err := cluster.DecodeRoute(cluster.EncodeRoute(r))
	if err != nil || got != r {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := cluster.DecodeRoute([]byte("nope")); err == nil {
		t.Fatal("坏 JSON 应报错")
	}
}

// 全量重读是并集：重读期间收到的广播不会被冲掉；过期的会被清掉。
func TestMergeRevokedKeepsBroadcastsReceivedDuringReload(t *testing.T) {
	now := time.UnixMilli(1_000_000)
	b := cluster.NewBase(cluster.Options{Now: func() time.Time { return now }})
	tokens := cluster.Tokens{B: b}
	b.ReceiveRevoked("from-broadcast", now.Add(time.Minute).UnixMilli())
	b.ReceiveRevoked("soon-gone", now.Add(time.Second).UnixMilli())
	b.MergeRevoked(map[string]int64{"from-scan": now.Add(time.Minute).UnixMilli(), "expired": now.Add(-time.Second).UnixMilli()})
	ctx := context.Background()
	for jti, want := range map[string]bool{"from-broadcast": true, "from-scan": true, "expired": false, "soon-gone": true} {
		if got, _ := tokens.IsRevoked(ctx, jti); got != want {
			t.Errorf("%s：%v", jti, got)
		}
	}
	now = now.Add(2 * time.Second)
	if got, _ := tokens.IsRevoked(ctx, "soon-gone"); got {
		t.Error("到期之后不再拒绝")
	}
	if b.RevokedCount() != 2 {
		t.Errorf("未过期的撤销数：%d", b.RevokedCount())
	}
}

func TestRevokedHookAlsoCounts(t *testing.T) {
	b := cluster.NewBase(cluster.Options{Revoked: func(_ context.Context, jti string) (bool, error) { return jti == "by-store", nil }})
	tokens := cluster.Tokens{B: b}
	if got, _ := tokens.IsRevoked(context.Background(), "by-store"); !got {
		t.Fatal("撤销表之外的判断也要算")
	}
	if got, _ := tokens.IsRevoked(context.Background(), "other"); got {
		t.Fatal("没撤销的放行")
	}
}

func TestLimitIDHashesTheKey(t *testing.T) {
	id := cluster.LimitID(control.LimitLoginIP, "198.51.100.7")
	if len(id) != len("login.ip:")+64 || id[:9] != "login.ip:" {
		t.Fatalf("%q", id)
	}
	if cluster.LimitID(control.LimitLoginIP, "198.51.100.7") != id || cluster.LimitID(control.LimitLoginAcct, "198.51.100.7") == id {
		t.Fatal("同键同种类同桶，种类不同不同桶")
	}
}
