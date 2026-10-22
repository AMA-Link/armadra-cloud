package api

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

func noop(context.Context, *Call) (any, error) { return nil, nil }

// 路由表取自 gen.Routes：静态段多的优先，启用的先于预留的，方法不同不命中。
func TestRouterPrefersStaticSegments(t *testing.T) {
	router, err := NewRouter(map[string]Handler{
		"sources.heartbeatHint": noop,
		"sources.get":           noop,
		"sources.revoke":        noop,
	})
	if err != nil {
		t.Fatal(err)
	}

	hit := router.Match("GET", "/v1/sources/me/relay")
	if hit == nil || hit.Route.Procedure != "sources.heartbeatHint" || hit.Handler == nil {
		t.Fatalf("/v1/sources/me/relay 应命中 heartbeatHint：%+v", hit)
	}
	hit = router.Match("GET", "/v1/sources/abc")
	if hit == nil || hit.Route.Procedure != "sources.get" || hit.Params["sourceId"] != "abc" {
		t.Fatalf("/v1/sources/abc 应命中 sources.get：%+v", hit)
	}
	if hit = router.Match("DELETE", "/v1/sources/abc"); hit == nil || hit.Route.Procedure != "sources.revoke" {
		t.Fatalf("DELETE 应命中 sources.revoke：%+v", hit)
	}
	// 路径参数做百分号解码。
	if hit = router.Match("GET", "/v1/sources/a%2Fb"); hit == nil || hit.Params["sourceId"] != "a/b" {
		t.Fatalf("路径参数应解码：%+v", hit)
	}
	// 契约里有、没给处理器的是预留的：命中但 Handler 为空。
	hit = router.Match("PATCH", "/v1/sources/abc")
	if hit == nil || hit.Route.Procedure != "sources.update" || hit.Handler != nil {
		t.Fatalf("sources.update 应是预留的：%+v", hit)
	}
	// 契约外的路径、空参数、多余的段都不命中。
	for _, miss := range [][2]string{{"GET", "/v1/nope"}, {"GET", "/v1/sources/"}, {"GET", "/v1/sources/a/b"}, {"POST", "/v1/sources/me/relay"}} {
		if hit := router.Match(miss[0], miss[1]); hit != nil && hit.Handler != nil {
			t.Fatalf("%s %s 不该命中启用的路由：%+v", miss[0], miss[1], hit)
		}
	}
}

// 匹配先只看启用的路由，都不中再看预留的：预留的静态路由不会遮住启用的带参数路由。
func TestRouterEnabledShadowsReserved(t *testing.T) {
	router := &Router{
		enabled:  []compiled{compile(Route{Procedure: "x.get", Method: "GET", Path: "/v1/x/{id}"})},
		reserved: []compiled{compile(Route{Procedure: "x.special", Method: "GET", Path: "/v1/x/special"})},
		handlers: map[string]Handler{"x.get": noop},
	}
	hit := router.Match("GET", "/v1/x/special")
	if hit == nil || hit.Route.Procedure != "x.get" || hit.Handler == nil {
		t.Fatalf("启用的先于预留的：%+v", hit)
	}
	if hit := router.Match("POST", "/v1/x/special"); hit != nil {
		t.Fatalf("方法不同不命中：%+v", hit)
	}
}

func TestRouterRejectsUnknownAndUnsupported(t *testing.T) {
	if _, err := NewRouter(map[string]Handler{"nope.nope": noop}); err == nil || !strings.Contains(err.Error(), "nope.nope") {
		t.Fatalf("契约里没有的过程应报装配错误：%v", err)
	}
	for procedure := range gen.UnsupportedInputs {
		if _, err := NewRouter(map[string]Handler{procedure: noop}); err == nil {
			t.Fatalf("入参 schema 不支持的过程 %s 不能启用", procedure)
		}
	}
	// exclude 的过程不进路由表。
	router, err := NewRouter(map[string]Handler{}, "platform.health")
	if err != nil {
		t.Fatal(err)
	}
	if hit := router.Match("GET", "/health"); hit != nil {
		t.Fatalf("exclude 的过程不该命中：%+v", hit)
	}
	if len(router.Enabled()) != 0 || len(router.Reserved()) != len(gen.Routes)-1 {
		t.Fatalf("启用 %d、预留 %d", len(router.Enabled()), len(router.Reserved()))
	}
}

// 每条路由都有入参 schema，UnsupportedInputs 为空（S0a 之后没有例外）。
func TestEveryRouteHasAnInputSchema(t *testing.T) {
	if len(gen.UnsupportedInputs) != 0 {
		t.Errorf("UnsupportedInputs 应为空：%v", gen.UnsupportedInputs)
	}
	for _, route := range gen.Routes {
		if _, has := gen.RouteInputs[route.Procedure]; !has {
			t.Errorf("%s 没有入参 schema", route.Procedure)
		}
	}
}

func TestRouterOrderingIsStable(t *testing.T) {
	router, err := NewRouter(map[string]Handler{})
	if err != nil {
		t.Fatal(err)
	}
	got := router.Reserved()
	// 静态段更多的排在前面：/v1/sources/me/relay（4 段静态）排在 /v1/sources/{sourceId}（3 段静态）之前。
	index := func(name string) int {
		for i, entry := range got {
			if entry == name {
				return i
			}
		}
		return -1
	}
	if index("sources.heartbeatHint") > index("sources.get") {
		t.Fatalf("heartbeatHint 应排在 sources.get 之前：%v", got)
	}
	again, _ := NewRouter(map[string]Handler{})
	if !reflect.DeepEqual(got, again.Reserved()) {
		t.Fatal("两次建表的顺序应一致")
	}
}
