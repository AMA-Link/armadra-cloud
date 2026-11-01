package oauth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/oauth"
)

// RFC 7636 附录 B 的向量。
func TestChallengeVector(t *testing.T) {
	t.Parallel()
	if got := oauth.Challenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatalf("S256 挑战：%s", got)
	}
}

func TestAuthorizationURL(t *testing.T) {
	t.Parallel()
	c := oauth.New(oauth.Options{Providers: map[string]oauth.Provider{
		oauth.GitHub: {ClientID: "gh"}, oauth.Google: {ClientID: "g"}, "gitlab": {ClientID: "x"},
	}})
	if !c.Configured("github") || !c.Configured("google") || c.Configured("gitlab") {
		t.Fatal("不认识的提供方被忽略")
	}
	if got := c.Names(); len(got) != 2 || got[0] != "github" || got[1] != "google" {
		t.Fatalf("名字顺序：%v", got)
	}
	raw, ok := c.AuthorizationURL("github", "st", "verifier", "https://app.test/login/oauth/github")
	u, _ := url.Parse(raw)
	if !ok || u.Host != "github.com" || u.Query().Get("scope") != "read:user user:email" || u.Query().Get("code_challenge") != oauth.Challenge("verifier") {
		t.Fatalf("GitHub：%s", raw)
	}
	if strings.Contains(raw, "verifier") && !strings.Contains(raw, oauth.Challenge("verifier")) {
		t.Fatal("授权地址里只能有挑战")
	}
	if _, ok := c.AuthorizationURL("gitlab", "st", "v", "x"); ok {
		t.Fatal("没配的提供方没有授权地址")
	}
}

// 令牌端点与资料端点的各种答复：被拒绝、不可用、字段不合格。
func TestExchangeFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		token   func(w http.ResponseWriter)
		profile func(w http.ResponseWriter)
		want    error
	}{
		{"GitHub 200 带 error", func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"error":"bad_verification_code"}`)) }, nil, oauth.ErrRejected},
		{"4xx", func(w http.ResponseWriter) { w.WriteHeader(400); _, _ = w.Write([]byte(`{"error":"invalid_grant"}`)) }, nil, oauth.ErrRejected},
		{"4xx 不是 JSON", func(w http.ResponseWriter) { w.WriteHeader(400); _, _ = w.Write([]byte(`nope`)) }, nil, oauth.ErrRejected},
		{"5xx", func(w http.ResponseWriter) { w.WriteHeader(502); _, _ = w.Write([]byte(`{"error":"x"}`)) }, nil, oauth.ErrUnavailable},
		{"200 不是 JSON", func(w http.ResponseWriter) { _, _ = w.Write([]byte(`<html>`)) }, nil, oauth.ErrUnavailable},
		{"200 没有令牌", func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{}`)) }, nil, oauth.ErrUnavailable},
		{"/user 答 500", func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"access_token":"t"}`)) }, func(w http.ResponseWriter) { w.WriteHeader(500) }, oauth.ErrUnavailable},
		{"/user 的 id 带冒号", func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"access_token":"t"}`)) }, func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"id":"a:b","login":"x"}`)) }, oauth.ErrUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/token" {
					c.token(w)
					return
				}
				if c.profile == nil {
					t.Errorf("不该走到 %s", r.URL.Path)
					return
				}
				c.profile(w)
			}))
			defer srv.Close()
			client := oauth.New(oauth.Options{Providers: map[string]oauth.Provider{oauth.GitHub: {
				ClientID: "id", ClientSecret: "secret", Endpoints: oauth.Endpoints{Token: srv.URL + "/token", User: srv.URL + "/user", Emails: srv.URL + "/emails"},
			}}})
			_, err := client.Exchange(context.Background(), oauth.GitHub, "code", "verifier", "https://app.test/cb")
			if !errors.Is(err, c.want) {
				t.Fatalf("应是 %v：%v", c.want, err)
			}
			if err != nil && (strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "code")) {
				t.Fatalf("错误文字不该带秘密或授权码：%v", err)
			}
		})
	}
}

// 不跟随重定向：令牌端点把请求（带着客户端密钥）转去别处，当作失败。
func TestExchangeDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()
	hit := false
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	client := oauth.New(oauth.Options{Providers: map[string]oauth.Provider{oauth.Google: {
		ClientID: "id", ClientSecret: "secret", Endpoints: oauth.Endpoints{Token: srv.URL, UserInfo: srv.URL},
	}}})
	if _, err := client.Exchange(context.Background(), oauth.Google, "c", "v", "https://app.test/cb"); err == nil {
		t.Fatal("重定向应当失败")
	}
	if hit {
		t.Fatal("不该跟随重定向")
	}
}

// Google 的 email_verified：布尔 true 或字符串 "true" 才算，其余都当没验证。
func TestGoogleEmailVerifiedForms(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]bool{`true`: true, `"true"`: true, `false`: false, `"false"`: false, `null`: false, `1`: false, `"yes"`: false} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/token" {
				_, _ = w.Write([]byte(`{"access_token":"t"}`))
				return
			}
			_, _ = w.Write([]byte(`{"sub":"s1","email":"a@example.com","email_verified":` + value + `,"name":" Ann "}`))
		}))
		client := oauth.New(oauth.Options{Providers: map[string]oauth.Provider{oauth.Google: {
			ClientID: "id", ClientSecret: "secret", Endpoints: oauth.Endpoints{Token: srv.URL + "/token", UserInfo: srv.URL + "/userinfo"},
		}}})
		id, err := client.Exchange(context.Background(), oauth.Google, "c", "v", "https://app.test/cb")
		srv.Close()
		if err != nil || id.EmailVerified != want || (want && id.Email != "a@example.com") || (!want && id.Email != "") || id.Subject != "s1" || id.Name != "Ann" {
			t.Errorf("email_verified=%s：%+v %v", value, id, err)
		}
	}
}
