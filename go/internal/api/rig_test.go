package api_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/api"
	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/core/coretest"
	"github.com/AMA-Link/armadra-cloud/go/internal/identity"
	"github.com/AMA-Link/armadra-cloud/go/internal/jose"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/sqlite"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil/schema"
)

const (
	password = "plover-quartz-lantern-41"
	sourceID = "0123456789abcdef0123456789abcdef"
)

var (
	hashOnce sync.Once
	hashed   identity.PasswordRecord
)

func hashedPassword(t testing.TB) identity.PasswordRecord {
	t.Helper()
	hashOnce.Do(func() {
		var err error
		if hashed, err = identity.HashPassword(password); err != nil {
			t.Fatal(err)
		}
	})
	return hashed
}

// rig 是一套跑在 SQLite 上的真实 HTTP 服务：控制核服务 + 路由 + 处理器。
type rig struct {
	t       *testing.T
	srv     *httptest.Server
	h       *coretest.Harness
	api     *api.API
	openAPI *schema.OpenAPI
	stream  *core.MeStream
	// hits 记下被请求命中的过程，最后核对覆盖。
	mu   sync.Mutex
	hits map[string]bool
}

type rigOptions struct {
	mode      api.Mode
	name      string
	webApp    string
	challenge bool
	limit     func(core.LimitKind, string) core.LimitResult
}

var (
	openAPIOnce sync.Once
	openAPIDoc  *schema.OpenAPI
)

func newRig(t *testing.T, options rigOptions) *rig {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "relay.db"), sqlite.Options{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h := coretest.NewHarness(t, db, coretest.Options{Limit: options.limit})
	if options.challenge {
		gate, err := core.NewChallengeGate(core.ChallengeGateOptions{
			SiteKey: "test-site-key", Hostnames: []string{"relay.armadra.test"}, Now: h.Clock.Now,
			Verifier: acceptingVerifier{},
		})
		if err != nil {
			t.Fatal(err)
		}
		h.Context.Challenge = gate
	}
	stream := core.NewMeStream(h.Context)
	handlers, authenticator := api.PersonalHandlers(h.Context, api.PersonalOptions{
		Stream: stream,
		WebApp: func() string { return options.webApp },
		Name:   func() string { return options.name },
	})
	a, err := api.New(api.Options{Mode: options.mode, Handlers: handlers, Authenticator: authenticator})
	if err != nil {
		t.Fatal(err)
	}
	openAPIOnce.Do(func() {
		openAPIDoc = schema.Load(t, filepath.Join(testutil.ProtocolDir(t), "schema", "cloud-openapi.json"))
	})
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	return &rig{t: t, srv: srv, h: h, api: a, openAPI: openAPIDoc, stream: stream, hits: map[string]bool{}}
}

type acceptingVerifier struct{}

func (acceptingVerifier) Verify(context.Context, core.VerifyRequest) (core.Verdict, error) {
	return core.Verdict{OK: true}, nil
}

// addOwner 放账号 alice（acct:alice，与个人中转一致）。
func (r *rig) addOwner() {
	r.t.Helper()
	err := r.h.Backend.AddAccount(context.Background(), r.h.Context.TenantID, store.AccountRecord{
		AccountID: "acct:alice", Name: "alice", Password: hashedPassword(r.t),
	})
	if err != nil {
		r.t.Fatal(err)
	}
}

type response struct {
	status int
	header http.Header
	body   []byte
	json   map[string]any
}

// do 发一个请求；200 的答复按 OpenAPI 的输出 schema 校验。
func (r *rig) do(method, path string, body any, headers map[string]string) response {
	r.t.Helper()
	var reader io.Reader
	switch typed := body.(type) {
	case nil:
	case []byte:
		reader = bytes.NewReader(typed)
	case string:
		reader = strings.NewReader(typed)
	default:
		data, err := json.Marshal(body)
		if err != nil {
			r.t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, r.srv.URL+path, reader)
	if err != nil {
		r.t.Fatal(err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		r.t.Fatal(err)
	}
	out := response{status: res.StatusCode, header: res.Header, body: data}
	_ = json.Unmarshal(data, &out.json)

	lookup := method
	if lookup == http.MethodHead {
		lookup = http.MethodGet
	}
	if match := r.api.Router().Match(lookup, strings.SplitN(path, "?", 2)[0]); match != nil && match.Handler != nil && res.StatusCode == http.StatusOK && method != http.MethodHead {
		r.mu.Lock()
		r.hits[match.Route.Procedure] = true
		r.mu.Unlock()
		if err := r.openAPI.ValidateOutput(match.Route.Procedure, data); err != nil {
			r.t.Fatalf("%s %s 的答复不符合 %s 的输出 schema：%v\n%s", method, path, match.Route.Procedure, err, data)
		}
	}
	return out
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func (r *rig) expect(res response, status int, code string) {
	r.t.Helper()
	if res.status != status {
		r.t.Fatalf("状态码 %d，应为 %d：%s", res.status, status, res.body)
	}
	if code != "" && res.json["code"] != code {
		r.t.Fatalf("错误码 %v，应为 %s：%s", res.json["code"], code, res.body)
	}
}

// login 登录 alice，答访问令牌与刷新令牌。
func (r *rig) login() (access, refresh string, session map[string]any) {
	r.t.Helper()
	res := r.do("POST", "/v1/auth/login", map[string]any{
		"account": "alice", "password": password, "device": map[string]any{"platform": "desktop", "name": "test"},
	}, nil)
	r.expect(res, 200, "")
	session = res.json["session"].(map[string]any)
	return session["accessToken"].(string), session["refreshToken"].(string), session
}

// sourceJWS 用测试源的私钥签一个 Source 头（寿命 ttl 秒）。
func (r *rig) sourceJWS(jti string, ttlSeconds int64) string {
	r.t.Helper()
	_, source := testutil.Keys(r.t)
	issuer, err := r.h.Context.Signer.Issuer(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	nowSec := r.h.Clock.Ms() / 1000
	token, err := jose.Sign(source.Private, jose.Header{Kid: sourceID}, map[string]any{
		"iss": sourceID, "aud": issuer, "iat": nowSec, "exp": nowSec + ttlSeconds, "jti": jti,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return token
}

func sourcePublicX(t testing.TB) string {
	_, source := testutil.Keys(t)
	return base64.RawURLEncoding.EncodeToString(source.Public)
}

// registerSource 以 alice 的身份登记测试源。
func (r *rig) registerSource(access string) {
	r.t.Helper()
	token := r.do("POST", "/v1/sources/registration-tokens", map[string]any{}, bearer(access))
	r.expect(token, 200, "")
	res := r.do("POST", "/v1/sources/register", map[string]any{
		"registrationToken": token.json["registrationToken"], "sourceId": sourceID,
		"publicKey": map[string]any{"kty": "OKP", "crv": "Ed25519", "x": sourcePublicX(r.t)},
		"name":      "mac", "kind": "desktop", "coreVersion": "0.9.2", "capabilities": []string{"terminal"},
		"protocol": map[string]any{"major": gen.ProtocolMajor, "minor": gen.ProtocolMinor},
	}, nil)
	r.expect(res, 200, "")
}

// newServer 给任意 api.API 配一个只管 HTTP 的 rig（内部错误等不需要存储的用例用）。
func newServer(t *testing.T, a *api.API) *rig {
	t.Helper()
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	return &rig{t: t, srv: srv, api: a, hits: map[string]bool{}, openAPI: openAPIDoc}
}
