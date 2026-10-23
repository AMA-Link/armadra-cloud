package server_test

import (
	"strings"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// 启用的过程集合 = 设计 §1.1 到本批（S5b）为止的累计；其余照旧 501 not_implemented。
// 往后每批把自己实现的过程加进这张表，漏加或多开都会在这里失败。
var enabledThroughS5b = map[string]bool{
	"platform.info": true, "platform.jwks": true,
	"auth.register": true, "auth.login": true, "auth.refresh": true, "auth.logout": true,
	"auth.changePassword": true, "auth.stepUp": true,
	"auth.deviceStart": true, "auth.devicePoll": true, "auth.deviceApprove": true, "auth.deviceGet": true, "auth.deviceDeny": true,
	"auth.emailVerify": true, "auth.emailVerifyResend": true, "auth.passwordResetStart": true, "auth.passwordResetFinish": true,
	"me.emailChange": true,
	"me.get":         true, "me.update": true, "me.devices": true, "me.deviceRevoke": true,
	"me.spaces":    true,
	"teams.create": true, "teams.list": true, "teams.get": true, "teams.update": true, "teams.delete": true,
	"teams.restore": true, "teams.transferOwnership": true, "teams.members": true, "teams.memberRole": true,
	"teams.memberRemove": true, "teams.leave": true, "teams.settings": true, "teams.settingsUpdate": true, "teams.usage": true,
	"invitations.create": true, "invitations.list": true, "invitations.revoke": true, "invitations.resend": true,
	"invitations.get": true, "invitations.preview": true, "invitations.accept": true, "invitations.decline": true,
	"sources.registrationToken": true, "sources.register": true, "sources.heartbeatHint": true, "sources.list": true,
	"sources.get": true, "sources.update": true, "sources.revoke": true, "me.sources": true,
	"sources.assertion": true, "sources.accessList": true, "sources.accessRevoke": true,
	"grants.list": true, "grants.create": true, "grants.update": true, "grants.revoke": true, "grants.effective": true,
}

// enabledS7 是 S7 实现的过程（单独一张表，各批并行时少冲突）。
var enabledS7 = map[string]bool{
	"audit.list": true, "me.securityLog": true, "billing.plan": true, "billing.usage": true,
	"auth.mfaVerify": true,
	"admin.tenants":  true, "admin.accounts": true, "admin.account": true, "admin.accountStatus": true,
	"admin.teams": true, "admin.team": true, "admin.teamStatus": true, "admin.relayNodes": true, "admin.relayNodeDrain": true,
	"admin.incident": true, "admin.incidentSet": true,
}

func TestEnabledProceduresAreExactlyThisBatch(t *testing.T) {
	r := newRig(t, rigOptions{})
	seen := 0
	for _, route := range gen.Routes {
		if route.Procedure == "platform.health" {
			continue // 由外层的 /health 答，不进路由表
		}
		path := route.Path
		for strings.Contains(path, "{") {
			start := strings.Index(path, "{")
			end := strings.Index(path, "}")
			path = path[:start] + "x" + path[end+1:]
		}
		method := route.Method
		var body any
		if method != "GET" && method != "DELETE" {
			body = "{}"
		}
		res := r.do(method, path, body, nil)
		is501 := res.status == 501 && res.json["code"] == "not_implemented"
		if enabledThroughS5b[route.Procedure] || enabledS7[route.Procedure] {
			seen++
			if is501 {
				t.Errorf("%s 应已启用", route.Procedure)
			}
		} else if !is501 {
			t.Errorf("%s 还没实现，应答 501 not_implemented：%d %s", route.Procedure, res.status, res.body)
		}
	}
	if seen != len(enabledThroughS5b)+len(enabledS7) {
		t.Fatalf("启用表里有契约里没有的过程：%d / %d", seen, len(enabledThroughS5b))
	}
}
