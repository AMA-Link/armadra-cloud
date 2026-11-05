package platform_test

import (
	"reflect"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/platform"
)

// 认证相关的能力位按配置组装；其余能力不随配置变。
func TestCapabilitiesAssembledFromConfig(t *testing.T) {
	t.Parallel()
	rest := []string{
		"auth.device-code", "links.source-invite", "links.update", "links.scope", "me.stream", "teams", "invitations",
		"grants", "audit", "billing",
	}
	with := func(auth ...string) []string { return append(append([]string{}, auth...), rest...) }
	cases := []struct {
		name         string
		passwordAuth bool
		providers    []string
		want         []string
		absent       []string
	}{
		{"缺省：口令开、没有第三方", true, nil, with("auth.password", "auth.password-change"), []string{"auth.oauth", "auth.oauth.github", "auth.oauth.google"}},
		{"只配 GitHub", true, []string{"github"}, with("auth.password", "auth.password-change", "auth.oauth", "auth.oauth.github", "auth.step-up-oauth", "auth.oauth-link"), []string{"auth.oauth.google"}},
		{"只配 Google", true, []string{"google"}, with("auth.password", "auth.password-change", "auth.oauth", "auth.oauth.google", "auth.step-up-oauth", "auth.oauth-link"), []string{"auth.oauth.github"}},
		{"两个都配", true, []string{"github", "google"}, with("auth.password", "auth.password-change", "auth.oauth", "auth.oauth.github", "auth.oauth.google", "auth.step-up-oauth", "auth.oauth-link"), nil},
		{"口令关、配了 Google", false, []string{"google"}, with("auth.oauth", "auth.oauth.google", "auth.step-up-oauth", "auth.oauth-link"), []string{"auth.password", "auth.password-change", "auth.oauth.github"}},
		{"口令关、没有第三方", false, nil, with(), []string{"auth.password", "auth.password-change", "auth.oauth"}},
	}
	for _, c := range cases {
		got := platform.Capabilities(c.passwordAuth, c.providers)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s：\n got %v\nwant %v", c.name, got, c.want)
		}
		for _, absent := range c.absent {
			for _, have := range got {
				if have == absent {
					t.Errorf("%s：不该报 %s", c.name, absent)
				}
			}
		}
	}
}
