package protocol_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/schema"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil"
)

// 生成的校验规则与 JSON Schema 库对同一批输入的结论必须一致：
// 夹具里的真实载荷，加上对每个字段删掉、置空、改类型后的变体。
func TestGeneratedSchemasAgreeWithJSONSchemaLibrary(t *testing.T) {
	schemas := map[string]*schema.Schema{
		"assertion/access-claims.json":       gen.AccessClaimsSchema,
		"assertion/assertion-claims.json":    gen.AssertionClaimsSchema,
		"assertion/ed25519-public-jwk.json":  gen.Ed25519PublicJWKSchema,
		"assertion/jwk-set.json":             gen.JWKSetSchema,
		"assertion/relay-token-claims.json":  gen.RelayTokenClaimsSchema,
		"assertion/source-jws-claims.json":   gen.SourceJWSClaimsSchema,
		"assertion/tunnel-token-claims.json": gen.TunnelTokenClaimsSchema,
		"tunnel/auth.json":                   gen.AuthSchema,
		"tunnel/challenge.json":              gen.ChallengeSchema,
		"tunnel/goaway-payload.json":         gen.GoawayPayloadSchema,
		"tunnel/hello.json":                  gen.HelloSchema,
		"tunnel/open-payload.json":           gen.OpenPayloadSchema,
		"tunnel/ready.json":                  gen.ReadySchema,
		"tunnel/reject.json":                 gen.RejectSchema,
		"e2e/accept.json":                    gen.E2EAcceptSchema,
	}
	dir := filepath.Join(testutil.ProtocolDir(t), "schema")
	reference := map[string]*jsonschema.Schema{}
	for file := range schemas {
		data, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			t.Fatal(err)
		}
		inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		compiler := jsonschema.NewCompiler()
		if err := compiler.AddResource("https://armadra.test/"+file, inst); err != nil {
			t.Fatal(err)
		}
		if reference[file], err = compiler.Compile("https://armadra.test/" + file); err != nil {
			t.Fatal(err)
		}
	}

	var samples []map[string]any
	add := func(raw []byte) {
		var v map[string]any
		if json.Unmarshal(raw, &v) == nil {
			samples = append(samples, v)
		}
	}
	for _, name := range testutil.Fixtures(t, "assertion", ".jwt") {
		parts := strings.Split(strings.TrimSpace(string(testutil.Fixture(t, "assertion/"+name))), ".")
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Fatal(err)
		}
		add(payload)
	}
	var keys map[string]any
	testutil.FixtureJSON(t, "assertion/keys.json", &keys)
	for _, k := range []string{"issuer", "source"} {
		pub, _ := json.Marshal(keys[k].(map[string]any)["publicJwk"])
		add(pub)
		set, _ := json.Marshal(map[string]any{"keys": []any{keys[k].(map[string]any)["publicJwk"]}})
		add(set)
	}
	var hs map[string]any
	testutil.FixtureJSON(t, "tunnel/handshake.json", &hs)
	for _, k := range []string{"hello", "challenge", "auth", "ready", "reject"} {
		raw, _ := json.Marshal(hs[k])
		add(raw)
	}
	for _, name := range []string{"open-http.bin", "open-ws.bin", "open-e2e.bin", "goaway.bin"} {
		add(testutil.Fixture(t, "tunnel/"+name)[5:])
	}
	add([]byte(`{"type":"accept","deviceStatus":"trusted","limits":{"maxStreams":1,"streamWindow":1,"tunnelWindow":1,"maxFrameBytes":1}}`))
	if len(samples) < 25 {
		t.Fatalf("样本太少：%d", len(samples))
	}

	mutate := func(sample map[string]any) []map[string]any {
		out := []map[string]any{sample}
		for key := range sample {
			for _, repl := range []any{nil, "x", 1.5, -1.0, "", []any{}, map[string]any{}, true} {
				m := map[string]any{}
				for k, v := range sample {
					m[k] = v
				}
				m[key] = repl
				out = append(out, m)
			}
			m := map[string]any{}
			for k, v := range sample {
				if k != key {
					m[k] = v
				}
			}
			out = append(out, m)
		}
		return out
	}

	checked, valid := 0, 0
	for _, sample := range samples {
		for _, variant := range mutate(sample) {
			raw, _ := json.Marshal(variant)
			var generic any
			if err := json.Unmarshal(raw, &generic); err != nil {
				t.Fatal(err)
			}
			ref, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			for file, s := range schemas {
				checked++
				got := s.Validate(generic) == nil
				want := reference[file].Validate(ref) == nil
				if got {
					valid++
				}
				if got != want {
					t.Fatalf("%s 对 %s 的结论不同：自己的校验 %v，JSON Schema 库 %v", file, raw, got, want)
				}
			}
		}
	}
	if valid < 50 {
		t.Fatalf("通过的样本只有 %d 个，对比没有意义", valid)
	}
	t.Logf("对比 %d 次，其中 %d 次通过", checked, valid)
}
