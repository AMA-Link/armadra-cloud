package postgres_test

import (
	"context"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/core/coretest"
	"github.com/AMA-Link/armadra-cloud/go/internal/identity"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// 访问令牌只认设备建立时的传输；传输、范围与提权时刻在 PostgreSQL 里往返不丢。
func TestAuthenticateTokenBindsTransport(t *testing.T) {
	ctx := context.Background()
	h := coretest.NewHarness(t, create(t), coretest.Options{})
	c := h.Context
	hash, err := identity.HashPassword("plover-quartz-lantern-41")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Backend.AddAccount(ctx, c.TenantID, store.AccountRecord{AccountID: "acct:alice", Name: "alice", Password: hash}); err != nil {
		t.Fatal(err)
	}
	nowMs := h.Clock.Ms()
	device, refresh := core.NewDevice("acct:alice", core.DeviceInput{Platform: "web", Name: "浏览器"}, core.TransportCookie, core.SessionScopeOperator, nowMs, core.OperatorSessionTTL)
	if err := c.Ports.Devices.Insert(ctx, c.TenantID, device, nowMs); err != nil {
		t.Fatal(err)
	}
	elevated := nowMs + 600_000
	if err := c.Ports.Devices.Update(ctx, c.TenantID, device.DeviceID, func(d *store.DeviceRecord) error {
		d.ElevatedUntilMs = &elevated
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	session, err := core.SessionFor(ctx, c, device, refresh)
	if err != nil {
		t.Fatal(err)
	}
	auth := core.NewAuthenticator(c)
	caller, err := auth.AuthenticateToken(ctx, session.AccessToken, core.TransportCookie)
	if err != nil {
		t.Fatal(err)
	}
	if caller.Transport != core.TransportCookie || caller.Scope != core.SessionScopeOperator || caller.ElevatedUntilMs != elevated || caller.AccountID != "acct:alice" {
		t.Fatalf("调用方：%+v", caller)
	}
	// Cookie 设备的令牌放进 Authorization 头：对不上传输。
	if _, err := auth.Authenticate(ctx, "Bearer "+session.AccessToken); err == nil {
		t.Fatal("cookie 设备的令牌走 bearer 应被拒绝")
	} else if got, _ := core.CodeOf(err); got != "unauthenticated" {
		t.Fatalf("错误码：%v", err)
	}
	// bearer 设备的令牌当 Cookie 用同样被拒绝。
	bearer, bearerRefresh, err := core.CreateDevice(ctx, c, "acct:alice", core.DeviceInput{Platform: "desktop", Name: "d"}, nowMs, 0)
	if err != nil {
		t.Fatal(err)
	}
	bearerSession, err := core.SessionFor(ctx, c, bearer, bearerRefresh)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.AuthenticateToken(ctx, bearerSession.AccessToken, core.TransportBearer); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.AuthenticateToken(ctx, bearerSession.AccessToken, core.TransportCookie); err == nil {
		t.Fatal("bearer 设备的令牌走 cookie 应被拒绝")
	}
	// 读回来：bearer / session 在记录里是空串。
	got, err := c.Ports.Devices.Get(ctx, c.TenantID, bearer.DeviceID)
	if err != nil || got == nil || got.Transport != "" || got.Scope != "" {
		t.Fatalf("bearer 设备往返：%+v %v", got, err)
	}
}
