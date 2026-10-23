package server_test

import (
	"strings"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
)

// 启用的过程集合 = 设计 §1.1 到本批（S3）为止的累计；其余照旧 501 not_implemented。
// 往后每批把自己实现的过程加进这张表，漏加或多开都会在这里失败。
var enabledThroughS3 = map[string]bool{
	"platform.info": true, "platform.jwks": true,
	"auth.register": true, "auth.login": true, "auth.refresh": true, "auth.logout": true,
	"auth.changePassword": true, "auth.stepUp": true,
	"auth.deviceStart": true, "auth.devicePoll": true, "auth.deviceApprove": true, "auth.deviceGet": true, "auth.deviceDeny": true,
	"me.get": true, "me.update": true, "me.devices": true, "me.deviceRevoke": true,
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
		if enabledThroughS3[route.Procedure] {
			seen++
			if is501 {
				t.Errorf("%s 应已启用", route.Procedure)
			}
		} else if !is501 {
			t.Errorf("%s 还没实现，应答 501 not_implemented：%d %s", route.Procedure, res.status, res.body)
		}
	}
	if seen != len(enabledThroughS3) {
		t.Fatalf("启用表里有契约里没有的过程：%d / %d", seen, len(enabledThroughS3))
	}
}
