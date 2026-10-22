package edge

import "testing"

const testID = "0123456789abcdef0123456789abcdef"

// 对应 edge.test.ts「addressing / personal：/s/<id> 前缀剥掉后转发，query 保留」。
func TestAddressingPath(t *testing.T) {
	a := PathAddressing()
	at := func(url string) (Target, bool) { return a.Resolve(url, "relay") }
	for url, want := range map[string]Target{
		"/s/" + testID + "/api/x?y=1": {testID, "/api/x?y=1"},
		"/s/" + testID:                {testID, "/"},
		"/s/" + testID + "?t=1":       {testID, "/?t=1"},
	} {
		if got, ok := at(url); !ok || got != want {
			t.Errorf("%s: %+v %v", url, got, ok)
		}
	}
	for _, url := range []string{
		"/s/" + testID + "x/api",
		"/s/" + "0123456789ABCDEF0123456789abcdef" + "/api",
		"/s/abc/api",
		"/health",
		"",
	} {
		if got, ok := at(url); ok {
			t.Errorf("%s 不应匹配：%+v", url, got)
		}
	}
}

// 对应 edge.test.ts「addressing / saas：<id>.src.<域>，路径原样」。
func TestAddressingHost(t *testing.T) {
	a := HostAddressing("relay.armadra.test")
	if got, ok := a.Resolve("/api/x", testID+".src.relay.armadra.test:443"); !ok || got != (Target{testID, "/api/x"}) {
		t.Errorf("%+v %v", got, ok)
	}
	if _, ok := a.Resolve("/api/x", testID+".src.relay.armadra.test"); !ok {
		t.Error("无端口应匹配")
	}
	for _, c := range []struct{ url, host string }{
		{"/api/x", testID + ".src.relayXarmadra.test"},
		{"/api/x", "relay.armadra.test"},
		{"/api/x", ""},
		{"api/x", testID + ".src.relay.armadra.test"},
		{"/api/x", testID + ".src.relay.armadra.test:abc"},
		{"/api/x", "x" + testID + ".src.relay.armadra.test"},
	} {
		if _, ok := a.Resolve(c.url, c.host); ok {
			t.Errorf("%+v 不应匹配", c)
		}
	}
	if _, ok := a.Resolve("/", testID+".SRC.Relay.Armadra.TEST"); !ok {
		t.Error("主机名应忽略大小写")
	}
	if _, ok := HostAddressing("Relay.Armadra.Test").Resolve("/", testID+".src.relay.armadra.test"); !ok {
		t.Error("域名应忽略大小写")
	}
}
