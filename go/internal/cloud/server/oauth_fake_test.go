package server_test

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/config"
	"github.com/AMA-Link/armadra-cloud/go/internal/cloud/oauth"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
)

// 假的第三方登录提供方（httptest）：授权码 + PKCE、令牌端点、GitHub 的 /user 与 /user/emails、Google 的 userinfo。
// 控制面的地址指向它；授权页不用访问，测试直接拿授权地址里的 code_challenge 给身份发 code。

const (
	fakeGitHubID     = "gh-client-id"
	fakeGitHubSecret = "gh-client-secret-value"
	fakeGoogleID     = "g-client-id"
	fakeGoogleSecret = "g-client-secret-value"
)

// fakeIdentity 是提供方此刻给出的用户。
type fakeIdentity struct {
	subject, login, name, email string
	verified                    bool
	// emails 非空时原样作为 GitHub 的 /user/emails 答复（测试 primary / verified 的筛选）。
	emails []map[string]any
}

type fakeGrant struct {
	provider, challenge, redirect string
	ident                         fakeIdentity
	used                          bool
}

type fakeCall struct{ provider, path, code, verifier, redirect string }

type fakeOAuth struct {
	t         *testing.T
	srv       *httptest.Server
	providers []string

	mu     sync.Mutex
	codes  map[string]*fakeGrant
	tokens map[string]*fakeGrant
	calls  []fakeCall
	// tokenStatus 强制令牌端点答这个状态码（提供方故障）。
	tokenStatus map[string]int
	seq         int
}

func newFakeOAuth(t *testing.T, providers ...string) *fakeOAuth {
	t.Helper()
	f := &fakeOAuth{t: t, providers: providers, codes: map[string]*fakeGrant{}, tokens: map[string]*fakeGrant{}, tokenStatus: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// configure 把提供方的 ID 与密钥文件写进配置。
func (f *fakeOAuth) configure(t *testing.T, env config.MapEnv) {
	t.Helper()
	dir := t.TempDir()
	for _, provider := range f.providers {
		id, secret := fakeGitHubID, fakeGitHubSecret
		name := "GITHUB"
		if provider == oauth.Google {
			id, secret, name = fakeGoogleID, fakeGoogleSecret, "GOOGLE"
		}
		path := filepath.Join(dir, provider+"-secret")
		if err := os.WriteFile(path, []byte(secret+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		env["ARMADRA_CLOUD_OAUTH_"+name+"_CLIENT_ID"] = id
		env["ARMADRA_CLOUD_OAUTH_"+name+"_CLIENT_SECRET_FILE"] = path
	}
}

func (f *fakeOAuth) endpoints() map[string]oauth.Endpoints {
	return map[string]oauth.Endpoints{
		oauth.GitHub: {Token: f.srv.URL + "/github/token", User: f.srv.URL + "/github/user", Emails: f.srv.URL + "/github/emails"},
		oauth.Google: {Token: f.srv.URL + "/google/token", UserInfo: f.srv.URL + "/google/userinfo"},
	}
}

// grant 对授权地址（oauthStart 答的 authorizationUrl）发一个 code：核对地址里的客户端、PKCE 与回调，
// 答 code 与地址里的 state。
func (f *fakeOAuth) grant(provider, authorizationURL string, ident fakeIdentity) (code, state string) {
	f.t.Helper()
	u, err := url.Parse(authorizationURL)
	if err != nil {
		f.t.Fatal(err)
	}
	q := u.Query()
	wantHost, wantID := "github.com", fakeGitHubID
	if provider == oauth.Google {
		wantHost, wantID = "accounts.google.com", fakeGoogleID
	}
	if u.Scheme != "https" || u.Host != wantHost || q.Get("client_id") != wantID || q.Get("code_challenge_method") != "S256" ||
		q.Get("code_challenge") == "" || q.Get("state") == "" || q.Get("redirect_uri") != appOrigin+"/login/oauth/"+provider {
		f.t.Fatalf("授权地址不对：%s", authorizationURL)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	code = provider + "-code-" + strconv.Itoa(f.seq) + "-" + core.RandomToken(6)
	f.codes[code] = &fakeGrant{provider: provider, challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri"), ident: ident}
	return code, q.Get("state")
}

// callsTo 是对某个路径后缀的请求次数。
func (f *fakeOAuth) callsTo(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.path == path {
			n++
		}
	}
	return n
}

func (f *fakeOAuth) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	provider, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	switch {
	case rest == "token" && r.Method == http.MethodPost:
		f.token(w, r, provider)
	case rest == "user" || rest == "emails" || rest == "userinfo":
		f.profile(w, r, provider, rest)
	default:
		http.NotFound(w, r)
	}
}

func writeFakeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fakeOAuth) token(w http.ResponseWriter, r *http.Request, provider string) {
	if err := r.ParseForm(); err != nil || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		writeFakeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	code, verifier := r.PostForm.Get("code"), r.PostForm.Get("code_verifier")
	f.calls = append(f.calls, fakeCall{provider: provider, path: "token", code: code, verifier: verifier, redirect: r.PostForm.Get("redirect_uri")})
	if status := f.tokenStatus[provider]; status != 0 {
		writeFakeJSON(w, status, map[string]string{"error": "server_error"})
		return
	}
	id, secret := fakeGitHubID, fakeGitHubSecret
	if provider == oauth.Google {
		id, secret = fakeGoogleID, fakeGoogleSecret
	}
	// 客户端凭据、授权码一次性、回调地址与 PKCE 都要对得上。
	sum := sha256.Sum256([]byte(verifier))
	grant := f.codes[code]
	ok := grant != nil && !grant.used && grant.provider == provider &&
		r.PostForm.Get("client_id") == id && subtle.ConstantTimeCompare([]byte(r.PostForm.Get("client_secret")), []byte(secret)) == 1 &&
		r.PostForm.Get("redirect_uri") == grant.redirect && base64.RawURLEncoding.EncodeToString(sum[:]) == grant.challenge &&
		(provider != oauth.Google || r.PostForm.Get("grant_type") == "authorization_code")
	if !ok {
		if provider == oauth.Google {
			writeFakeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		} else {
			writeFakeJSON(w, http.StatusOK, map[string]string{"error": "bad_verification_code"})
		}
		return
	}
	grant.used = true
	token := "at-" + code
	f.tokens[token] = grant
	writeFakeJSON(w, http.StatusOK, map[string]string{"access_token": token, "token_type": "bearer"})
}

func (f *fakeOAuth) profile(w http.ResponseWriter, r *http.Request, provider, which string) {
	f.calls = append(f.calls, fakeCall{provider: provider, path: which})
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	grant := f.tokens[token]
	if grant == nil || grant.provider != provider {
		writeFakeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
		return
	}
	ident := grant.ident
	switch which {
	case "user":
		var name any
		if ident.name != "" {
			name = ident.name
		}
		n, _ := strconv.ParseInt(ident.subject, 10, 64)
		writeFakeJSON(w, http.StatusOK, map[string]any{"id": n, "login": ident.login, "name": name, "email": nil})
	case "emails":
		emails := ident.emails
		if emails == nil {
			emails = []map[string]any{
				{"email": "other-" + ident.email, "primary": false, "verified": true},
				{"email": ident.email, "primary": true, "verified": ident.verified},
			}
		}
		writeFakeJSON(w, http.StatusOK, emails)
	case "userinfo":
		writeFakeJSON(w, http.StatusOK, map[string]any{"sub": ident.subject, "email": ident.email, "email_verified": ident.verified, "name": ident.name})
	}
}
