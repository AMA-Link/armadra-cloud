package tunnel_test

import (
	"context"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/control"
	"github.com/AMA-Link/armadra-cloud/go/internal/tunnel"
)

// slowRoutes 的 Delete 卡住直到放行：隧道关闭不能被它拖住。
type slowPlane struct {
	control.Plane
	gate chan struct{}
}

type slowRoutes struct {
	control.RouteTable
	gate chan struct{}
}

func (p slowPlane) Routes() control.RouteTable {
	return slowRoutes{RouteTable: p.Plane.Routes(), gate: p.gate}
}

func (r slowRoutes) Delete(ctx context.Context, sourceID, tunnelID string) error {
	<-r.gate
	return r.RouteTable.Delete(ctx, sourceID, tunnelID)
}

func TestHubClosedDoesNotBlockOnControlPlane(t *testing.T) {
	gate := make(chan struct{})
	r := newHubRig(t, func(o *tunnel.HubOptions) { o.Control = slowPlane{Plane: o.Control, gate: gate} })
	sock := newFakeSocket()
	done := r.serve(sock)
	sock.handshake(t, r.fx)
	if e := r.event(t); e.Type != control.PresenceSourceOnline {
		t.Fatalf("%+v", e)
	}
	sock.hangup()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("关闭被删路由拖住了")
	}
	// 路由还在、下线事件还没发（同一个源按序：先删路由再发事件）。
	select {
	case e := <-r.events:
		t.Fatalf("删路由完成前不该发下线事件：%+v", e)
	case <-time.After(100 * time.Millisecond):
	}
	close(gate)
	if e := r.event(t); e.Type != control.PresenceSourceOffline {
		t.Fatalf("%+v", e)
	}
	rt, _ := r.env.Control.Routes().Get(context.Background(), r.env.SourceID)
	if rt != nil {
		t.Fatalf("路由应已删：%+v", rt)
	}
}
