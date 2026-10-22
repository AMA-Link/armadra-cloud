package core_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/core/coretest"
	"github.com/AMA-Link/armadra-cloud/go/internal/identity"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/store/sqlite"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil"
)

const (
	password = "plover-quartz-lantern-41"
	sourceID = "0123456789abcdef0123456789abcdef"
)

var (
	hashOnce sync.Once
	hashed   identity.PasswordRecord
)

type env struct {
	t   *testing.T
	ctx context.Context
	h   *coretest.Harness
}

func newEnv(t *testing.T, options coretest.Options) *env {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "relay.db"), sqlite.Options{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &env{t: t, ctx: context.Background(), h: coretest.NewHarness(t, db, options)}
}

func (e *env) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) addAccount(name, id string) {
	e.t.Helper()
	hashOnce.Do(func() {
		var err error
		if hashed, err = identity.HashPassword(password); err != nil {
			e.t.Fatal(err)
		}
	})
	e.must(e.h.Backend.AddAccount(e.ctx, e.h.Context.TenantID, store.AccountRecord{AccountID: id, Name: name, Password: hashed}))
}

func (e *env) owner() core.Caller {
	e.t.Helper()
	e.addAccount("alice", "acct:alice")
	session, err := e.login("alice", password)
	e.must(err)
	caller, err := core.NewAuthenticator(e.h.Context).Authenticate(e.ctx, "Bearer "+session.AccessToken)
	e.must(err)
	return caller
}

func (e *env) login(name, secret string) (core.Session, error) {
	return core.NewAccounts(e.h.Context).Login(e.ctx, core.LoginInput{
		Account: name, Password: secret, Device: core.DeviceInput{Platform: "desktop", Name: "t"},
	}, "198.51.100.1")
}

func (e *env) register(owner core.Caller, id string) *core.Sources {
	e.t.Helper()
	_, source := testutil.Keys(e.t)
	sources := core.NewSources(e.h.Context)
	token, err := sources.RegistrationToken(e.ctx, owner)
	e.must(err)
	_, err = sources.Register(e.ctx, core.RegisterInput{
		RegistrationToken: token.RegistrationToken, SourceID: id,
		PublicKey: gen.Ed25519PublicJWK{Kty: "OKP", Crv: "Ed25519", X: base64.RawURLEncoding.EncodeToString(source.Public)},
		Name:      "mac", Kind: "desktop", CoreVersion: "0.9.2", Capabilities: []string{}, ProtocolMajor: 1,
	}, "ip")
	e.must(err)
	return sources
}

func code(err error) string {
	if err == nil {
		return "ok"
	}
	if c, ok := core.CodeOf(err); ok {
		return string(c)
	}
	return err.Error()
}

func claimsOf(t *testing.T, token string) map[string]any {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[1])
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func eq[T comparable](t *testing.T, got, want T, what string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s：得到 %v，应为 %v", what, got, want)
	}
}

func asError(err error, target **core.Error) bool {
	for err != nil {
		if e, ok := err.(*core.Error); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
