// Package api 是 /v1 路由与 HTTP 外壳：路由表取自协议包的生成物 gen.Routes（方法、路径）与
// gen.RouteInputs（入参 schema），个人中转给处理器的过程接上控制核服务，其余答 404（personal）
// 或 501（cloud）。形状与 已删除的 TypeScript 版 apps/relay/src/personal/api.ts 一致：
//
//   - 输入：路径参数 + （GET / DELETE 的 query | 其余的 JSON 体），按入参 schema 校验，不过答
//     400 bad_request；JSON 体上限 64 KiB；
//   - 认证：session（Authorization: Bearer <访问令牌>）、source（Authorization: Source <jws>）、
//     anonymous，由处理器自己要，惰性、只认证一次；
//   - 错误：{ code, message, requestId, details? }，状态码查注册表；account_locked / rate_limited
//     带 details.retryAfterMs 与 Retry-After 头；
//   - 跨源：/v1/* 与 /.well-known/* 允许任意来源（只认 Bearer / Source 头，不用 Cookie）。
package api

import (
	"sort"
	"strings"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/schema"
)

// Route 是路由表里的一条：方法、路径模板与入参 schema。
type Route struct {
	Procedure string
	Method    string
	// Path 是路径模板，参数写成 {name}。
	Path string
	// Input 是入参 schema；生成器还不认识它的关键字的过程为 nil（这些过程没有启用的处理器）。
	Input *schema.Schema
}

// Match 是一次命中：启用的命中带处理器；只命中预留的 Handler 为 nil。
type Match struct {
	Route   Route
	Params  map[string]string
	Handler Handler
}

type compiled struct {
	route    Route
	segments []string
	statics  int
}

func compile(route Route) compiled {
	segments := strings.Split(route.Path, "/")
	statics := 0
	for _, segment := range segments {
		if !isParam(segment) {
			statics++
		}
	}
	return compiled{route: route, segments: segments, statics: statics}
}

func isParam(segment string) bool {
	return len(segment) > 2 && segment[0] == '{' && segment[len(segment)-1] == '}'
}

// Router 是 /v1 的路由表：启用的在前，预留的在后，同一组里静态段多的优先
// （/v1/sources/me/relay 先于 /v1/sources/{sourceId}），同样多的保持契约声明顺序。
type Router struct {
	enabled  []compiled
	reserved []compiled
	handlers map[string]Handler
}

// NewRouter 用 gen.Routes 建路由表。handlers 里列出的过程是「启用」的，其余是「预留」的，
// 由装配方决定怎么答（personal：404 not_found；cloud：501 not_implemented）。exclude 里的过程
// 不进路由表（例如 SaaS 的 platform.health 由外层直接答）。handlers 里有契约里没有的过程是装配错误。
func NewRouter(handlers map[string]Handler, exclude ...string) (*Router, error) {
	skip := map[string]bool{}
	for _, name := range exclude {
		skip[name] = true
	}
	known := map[string]bool{}
	var enabled, reserved []compiled
	for _, entry := range gen.Routes {
		if skip[entry.Procedure] {
			continue
		}
		known[entry.Procedure] = true
		route := Route{Procedure: entry.Procedure, Method: entry.Method, Path: entry.Path, Input: gen.RouteInputs[entry.Procedure]}
		if _, ok := handlers[entry.Procedure]; ok {
			if route.Input == nil {
				return nil, errUnsupportedInput(entry.Procedure)
			}
			enabled = append(enabled, compile(route))
		} else {
			reserved = append(reserved, compile(route))
		}
	}
	for name := range handlers {
		if !known[name] {
			return nil, errUnknownProcedure(name)
		}
	}
	byStatics := func(table []compiled) {
		sort.SliceStable(table, func(i, j int) bool { return table[i].statics > table[j].statics })
	}
	byStatics(enabled)
	byStatics(reserved)
	return &Router{enabled: enabled, reserved: reserved, handlers: handlers}, nil
}

func find(table []compiled, method, path string) (Route, map[string]string, bool) {
	segments := strings.Split(path, "/")
	for _, entry := range table {
		if entry.route.Method != method || len(entry.segments) != len(segments) {
			continue
		}
		params := map[string]string{}
		ok := true
		for i, want := range entry.segments {
			if isParam(want) {
				if segments[i] == "" {
					ok = false
					break
				}
				params[want[1:len(want)-1]] = unescape(segments[i])
			} else if want != segments[i] {
				ok = false
				break
			}
		}
		if ok {
			return entry.route, params, true
		}
	}
	return Route{}, nil, false
}

// Match 先只看启用的路由，都不中再看预留的——预留路由不会遮住启用的路由。都不中答 nil。
func (r *Router) Match(method, path string) *Match {
	if route, params, ok := find(r.enabled, method, path); ok {
		return &Match{Route: route, Params: params, Handler: r.handlers[route.Procedure]}
	}
	if route, params, ok := find(r.reserved, method, path); ok {
		return &Match{Route: route, Params: params}
	}
	return nil
}

// Enabled 是启用的过程（按路由表的匹配顺序）。
func (r *Router) Enabled() []string { return names(r.enabled) }

// Reserved 是预留的过程。
func (r *Router) Reserved() []string { return names(r.reserved) }

func names(table []compiled) []string {
	out := make([]string, 0, len(table))
	for _, entry := range table {
		out = append(out, entry.route.Procedure)
	}
	return out
}
