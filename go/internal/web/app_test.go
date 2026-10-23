package web

import "testing"

func TestIsAppRoute(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]bool{
		"/": true, "/login": true, "/verify": true, "/reset": true, "/device": true,
		"/t/x": true, "/t": true, "/h/x/y": true, "/j/abc": true, "/invite/i1": true, "/ops/teams": true, "/ops": true,
		"/register": true, "/me": true, "/me/": true, "/me/hosts": true, "/me/hosts/h1": true, "/me/links": true, "/me/account": true, "/me/tokens": true,
		"/t/x/hosts/h1": true, "/t/x/members": true, "/t/x/billing": true, "/ops/tenants/t1": true, "/ops/nodes": true,
		"/v1/me": false, "/v1/auth/login": false, "/v1/unknown": false, "/.well-known/armadra-platform": false, "/me2": false, "/register/x": false, "/assets/a.js": false, "/login/x": false, "/team": false, "/tx": false, "/health": false,
	} {
		if got := IsAppRoute(path); got != want {
			t.Errorf("IsAppRoute(%q) = %v，应为 %v", path, got, want)
		}
	}
}

func TestConsoleCSP(t *testing.T) {
	t.Parallel()
	want := "default-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self' wss://*.src.example.com https://challenges.cloudflare.com; " +
		"script-src 'self' https://challenges.cloudflare.com; frame-src https://challenges.cloudflare.com; frame-ancestors 'none'; base-uri 'none'; object-src 'none'; form-action 'self'"
	if got := ConsoleCSP("example.com"); got != want {
		t.Fatalf("ConsoleCSP = %q", got)
	}
}
