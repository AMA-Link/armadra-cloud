package httpx

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// Gate 是凭据的闸（设计 §5.1、§5.2）：同名 Cookie 重复、凭据混用、传输绑定、运营隔离、Origin 与 CSRF。
// api.Options.Before 与 api.Options.Authenticate 接的就是它。
type Gate struct {
	appOrigin string
	csrf      *CSRF
	auth      *core.Authenticator
	now       func() time.Time
}

// NewGate 建闸；appOrigin 是控制台来源（= ISSUER）。
func NewGate(appOrigin string, masterKey []byte, auth *core.Authenticator, now func() time.Time) (*Gate, error) {
	csrf, err := NewCSRF(masterKey)
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &Gate{appOrigin: appOrigin, csrf: csrf, auth: auth, now: now}, nil
}

// CSRFToken 是设备的 CSRF 值（me.get、登录类答案的 csrfToken）。
func (g *Gate) CSRFToken(deviceID string) string { return g.csrf.Token(deviceID) }

func isAdmin(procedure string) bool { return strings.HasPrefix(procedure, "admin.") }

// scopedSessionProcedure：auth.refresh 与 auth.logout 带 scope: "operator" 时作用于运营会话。
func scopedSessionProcedure(procedure string) bool {
	return procedure == "auth.refresh" || procedure == "auth.logout"
}

func operatorInput(call *api.Call) bool {
	scope, _ := call.Input["scope"].(string)
	return scope == "operator"
}

func mutating(r *http.Request) bool {
	return r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions
}

// Before 在入参校验之后、处理器之前：
//   - 同名 Cookie 出现两次以上（Cookie 投放攻击）→ 401 unauthenticated，并在答复里清掉这些 Cookie；
//   - 请求既带会话 Cookie 又带 Authorization → 400 bad_request（不挑其中一个用）。
func (g *Gate) Before(call *api.Call) error {
	r := call.Request
	cookies := ParseCookies(r)
	if duplicated := cookies.Duplicated(); len(duplicated) > 0 {
		for _, name := range duplicated {
			call.Header.Add("Set-Cookie", Clear(name))
		}
		return core.Fail(gen.ErrUnauthenticated)
	}
	if cookies.Any() && r.Header.Get("Authorization") != "" {
		return core.Fail(gen.ErrBadRequest)
	}
	return nil
}

var bearerHeader = regexp.MustCompile(`(?i)^Bearer\s+(\S+)$`)

// CheckOrigin 要求 Origin 头恰好出现一次且等于控制台来源；挡住同站的兄弟子域（*.src.<域>）。
func (g *Gate) CheckOrigin(r *http.Request) error {
	origin, ok := OriginOnce(r)
	if !ok || origin != g.appOrigin {
		return core.Fail(gen.ErrForbidden)
	}
	return nil
}

// TransportFor 定登录类过程建的设备走哪种传输：带 Origin = 控制台来源（恰好一次）且不带
// Authorization → Cookie；否则 Bearer。
func (g *Gate) TransportFor(r *http.Request) core.Transport {
	if r.Header.Get("Authorization") != "" {
		return core.TransportBearer
	}
	if origin, ok := OriginOnce(r); ok && origin == g.appOrigin {
		return core.TransportCookie
	}
	return core.TransportBearer
}

// Authenticate 是 api.Options.Authenticate：按过程与传输选凭据并认证。
//
//   - 带 Authorization → Bearer 访问令牌，设备必须是 bearer 传输；
//   - 否则取会话 Cookie：admin.*、以及带 scope: "operator" 的 auth.refresh / auth.logout 用运营那枚，
//     其余用普通那枚；设备必须是 cookie 传输；写请求先验 Origin，再验 CSRF（auth.refresh 例外）；
//   - 运营会话只能调 admin.* 与自己的 auth.refresh / auth.logout，普通会话不能调 admin.*，都答 401。
func (g *Gate) Authenticate(ctx context.Context, call *api.Call) (core.Caller, error) {
	r := call.Request
	wantOps := isAdmin(call.Procedure) || (scopedSessionProcedure(call.Procedure) && operatorInput(call))
	var (
		caller    core.Caller
		err       error
		viaCookie bool
	)
	if header := r.Header.Get("Authorization"); header != "" {
		match := bearerHeader.FindStringSubmatch(header)
		if match == nil {
			return core.Caller{}, core.Fail(gen.ErrUnauthenticated)
		}
		caller, err = g.auth.AuthenticateToken(ctx, match[1], core.TransportBearer)
	} else {
		name := CookieAccess
		if wantOps {
			name = CookieOpsAccess
		}
		token := ParseCookies(r).Value(name)
		if token == "" {
			return core.Caller{}, core.Fail(gen.ErrUnauthenticated)
		}
		if mutating(r) {
			if err := g.CheckOrigin(r); err != nil {
				return core.Caller{}, err
			}
		}
		viaCookie = true
		caller, err = g.auth.AuthenticateToken(ctx, token, core.TransportCookie)
	}
	if err != nil {
		return core.Caller{}, err
	}
	// 运营隔离。
	operator := caller.Scope == core.SessionScopeOperator
	switch {
	case operator && !(isAdmin(call.Procedure) || scopedSessionProcedure(call.Procedure)):
		return core.Caller{}, core.Fail(gen.ErrUnauthenticated)
	case !operator && isAdmin(call.Procedure):
		return core.Caller{}, core.Fail(gen.ErrUnauthenticated)
	}
	if viaCookie {
		// 运营 Cookie 里放的必须是运营设备的令牌，反之亦然。
		if wantOps != operator {
			return core.Caller{}, core.Fail(gen.ErrUnauthenticated)
		}
		if mutating(r) && call.Procedure != "auth.refresh" && !g.csrf.Check(caller.DeviceID, r.Header.Get(HeaderCSRF)) {
			return core.Caller{}, core.Fail(gen.ErrForbidden)
		}
	}
	if scopedSessionProcedure(call.Procedure) && operatorInput(call) && !operator {
		return core.Caller{}, core.Fail(gen.ErrForbidden)
	}
	return caller, nil
}

// RefreshToken 取 auth.refresh 的刷新凭据：请求体给了就用它（Bearer 客户端）；没给就取对应范围的
// 刷新 Cookie，并要求 Origin 等于控制台来源（auth.refresh 免 CSRF，刷新 Cookie 是 SameSite=Strict）。
// 两处都没有答 session_revoked（与令牌无效同码）。
func (g *Gate) RefreshToken(call *api.Call, given *string) (token string, viaCookie bool, err error) {
	if given != nil && *given != "" {
		return *given, false, nil
	}
	_, name := CookieNames(core.SessionScopeSession)
	if operatorInput(call) {
		_, name = CookieNames(core.SessionScopeOperator)
	}
	value := ParseCookies(call.Request).Value(name)
	if value == "" {
		return "", false, core.Fail(gen.ErrSessionRevoked)
	}
	if err := g.CheckOrigin(call.Request); err != nil {
		return "", false, err
	}
	return value, true, nil
}

// secondsUntil 是 atMs 离现在还有几秒，封顶 capSeconds，至少 1。
func (g *Gate) secondsUntil(atMs int64, capSeconds int) int {
	seconds := int((atMs - g.now().UnixMilli()) / 1000)
	if seconds > capSeconds {
		seconds = capSeconds
	}
	return max(seconds, 1)
}

// SetSession 把 Cookie 会话的两枚 Cookie 写进答复头：访问令牌（寿命同令牌）与刷新令牌（寿命同设备，
// Max-Age 不超过会话剩余寿命）。
func (g *Gate) SetSession(h http.Header, scope core.SessionScope, session core.Session, refreshToken string) {
	access, refresh := CookieNames(scope)
	refreshCap := int(core.RefreshTTL / time.Second)
	h.Add("Set-Cookie", Set(access, session.AccessToken, g.secondsUntil(min(session.AccessExpiresAtMs, session.ExpiresAtMs), int(core.AccessTTL/time.Second))))
	h.Add("Set-Cookie", Set(refresh, refreshToken, g.secondsUntil(session.ExpiresAtMs, refreshCap)))
}

// ClearSession 清掉某个范围的两枚 Cookie。
func (g *Gate) ClearSession(h http.Header, scope core.SessionScope) {
	access, refresh := CookieNames(scope)
	h.Add("Set-Cookie", Clear(access))
	h.Add("Set-Cookie", Clear(refresh))
}
