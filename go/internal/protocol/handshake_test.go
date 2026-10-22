package protocol_test

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil"
)

type handshakeFixture struct {
	SigningInput string          `json:"signingInput"`
	Hello        json.RawMessage `json:"hello"`
	Challenge    json.RawMessage `json:"challenge"`
	Auth         json.RawMessage `json:"auth"`
	Ready        json.RawMessage `json:"ready"`
	Reject       json.RawMessage `json:"reject"`
}

func loadHandshake(t *testing.T) handshakeFixture {
	var f handshakeFixture
	testutil.FixtureJSON(t, "tunnel/handshake.json", &f)
	return f
}

// 同一份 JSON 解开再编码，语义不变（字段名、字段数一一对应）。
func sameJSON(t *testing.T, name string, value any, original json.RawMessage) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var got, want any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(original, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s 解开再编码后不同：\n得到 %s\n应为 %s", name, encoded, original)
	}
}

// handshake.json：四步消息都过 schema，签名输入与签名逐项核对。
func TestHandshakeFixture(t *testing.T) {
	f := loadHandshake(t)
	_, source := testutil.Keys(t)

	hello, err := protocol.ParseHello(f.Hello)
	if err != nil {
		t.Fatalf("hello：%v", err)
	}
	challenge, err := protocol.ParseChallenge(f.Challenge)
	if err != nil {
		t.Fatalf("challenge：%v", err)
	}
	auth, err := protocol.ParseAuth(f.Auth)
	if err != nil {
		t.Fatalf("auth：%v", err)
	}
	ready, err := protocol.ParseReady(f.Ready)
	if err != nil {
		t.Fatalf("ready：%v", err)
	}
	reject, err := protocol.ParseReject(f.Reject)
	if err != nil {
		t.Fatalf("reject：%v", err)
	}
	sameJSON(t, "hello", hello, f.Hello)
	sameJSON(t, "challenge", challenge, f.Challenge)
	sameJSON(t, "auth", auth, f.Auth)
	sameJSON(t, "ready", ready, f.Ready)
	sameJSON(t, "reject", reject, f.Reject)

	input := protocol.SigningInput(hello.SourceID, hello.Nonce, challenge.Nonce2, challenge.RelayNode)
	if string(input) != f.SigningInput {
		t.Fatalf("签名输入不同：\n得到 %q\n应为 %q", input, f.SigningInput)
	}
	sig, err := base64.RawURLEncoding.DecodeString(auth.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		t.Fatalf("auth.sig 应是 64 字节的 base64url：%v", err)
	}
	if !ed25519.Verify(source.Public, input, sig) {
		t.Fatal("auth.sig 验不过")
	}
	if hello.SourceID != source.Kid {
		t.Errorf("hello.sourceId 应是源的 kid")
	}
	// Ed25519 签名是确定的：自己签出来的字节与夹具逐字节相同。
	if !reflect.DeepEqual(ed25519.Sign(source.Private, input), sig) {
		t.Error("重新签名的结果与夹具不同")
	}
	// 篡改任何一个输入，签名都验不过。
	for _, bad := range [][]byte{
		protocol.SigningInput(hello.SourceID, hello.Nonce, challenge.Nonce2, "relay-2"),
		protocol.SigningInput(hello.SourceID, challenge.Nonce2, hello.Nonce, challenge.RelayNode),
	} {
		if ed25519.Verify(source.Public, bad, sig) {
			t.Error("换了输入签名不该通过")
		}
	}
	if ready.Protocol.Major != gen.ProtocolMajor || ready.Protocol.Minor != gen.ProtocolMinor || ready.HeartbeatMs != protocol.HeartbeatMs {
		t.Errorf("ready 的协议版本、心跳与生成常量不一致：%+v", ready)
	}
	if ready.Limits.MaxStreams != protocol.MaxStreams || ready.Limits.StreamWindow != protocol.StreamWindow ||
		ready.Limits.TunnelWindow != protocol.TunnelWindow || ready.Limits.MaxFrameBytes != protocol.MaxFrameBytes {
		t.Errorf("ready 的限制与常量不一致：%+v", ready.Limits)
	}
}

func replace(t *testing.T, raw json.RawMessage, from, to string) []byte {
	t.Helper()
	if !strings.Contains(string(raw), from) {
		t.Fatalf("找不到 %q", from)
	}
	return []byte(strings.Replace(string(raw), from, to, 1))
}

func TestHandshakeRejectsBadMessages(t *testing.T) {
	f := loadHandshake(t)
	cases := []struct {
		name  string
		parse func() error
	}{
		{"hello 的 nonce 太短", func() error {
			_, err := protocol.ParseHello(replace(t, f.Hello, `"nonce": "oaGhoaGhoaGhoaGhoaGhoQ"`, `"nonce": "oaGh"`))
			return err
		}},
		{"hello 的 sourceId 不是 32 位小写 hex", func() error {
			_, err := protocol.ParseHello(replace(t, f.Hello, `"sourceId": "0123456789abcdef0123456789abcdef"`, `"sourceId": "0123456789ABCDEF0123456789abcdef"`))
			return err
		}},
		{"hello 缺 tunnelToken", func() error {
			_, err := protocol.ParseHello(replace(t, f.Hello, `"tunnelToken"`, `"tunnelTokenX"`))
			return err
		}},
		{"hello 的 type 不对", func() error {
			_, err := protocol.ParseHello(replace(t, f.Hello, `"hello"`, `"auth"`))
			return err
		}},
		{"challenge 的 relayNode 为空", func() error {
			_, err := protocol.ParseChallenge(replace(t, f.Challenge, `"relay-1"`, `""`))
			return err
		}},
		{"auth 的 sig 长度不对", func() error {
			_, err := protocol.ParseAuth([]byte(`{"type":"auth","sig":"AAAA"}`))
			return err
		}},
		{"ready 的 maxStreams 为 0", func() error {
			_, err := protocol.ParseReady(replace(t, f.Ready, `"maxStreams": 256`, `"maxStreams": 0`))
			return err
		}},
		{"ready 的 heartbeatMs 为 0", func() error {
			_, err := protocol.ParseReady(replace(t, f.Ready, `"heartbeatMs": 20000`, `"heartbeatMs": 0`))
			return err
		}},
		{"reject 的 code 未登记", func() error {
			_, err := protocol.ParseReject(replace(t, f.Reject, `tunnel_token_expired`, `nope`))
			return err
		}},
		{"不是 JSON", func() error { _, err := protocol.ParseHello([]byte("hello")); return err }},
		{"不是合法 UTF-8", func() error { _, err := protocol.ParseAuth([]byte("{\"type\":\"auth\",\"sig\":\"\xff\"}")); return err }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.parse(); err == nil {
				t.Fatal("应当被拒")
			}
		})
	}
}

func TestHandshakeIgnoresUnknownFields(t *testing.T) {
	f := loadHandshake(t)
	extended := replace(t, f.Challenge, `"type": "challenge"`, `"type": "challenge", "futureField": {"a": 1}`)
	challenge, err := protocol.ParseChallenge(extended)
	if err != nil {
		t.Fatalf("更高 minor 加的字段应被忽略：%v", err)
	}
	if challenge.RelayNode != "relay-1" {
		t.Errorf("解出来的 relayNode 是 %q", challenge.RelayNode)
	}
}

func TestParseE2EAccept(t *testing.T) {
	accept, err := protocol.ParseE2EAccept([]byte(`{"type":"accept","deviceStatus":"pending","pendingId":"p1",` +
		`"limits":{"maxStreams":8,"streamWindow":65536,"tunnelWindow":1048576,"maxFrameBytes":65528}}`))
	if err != nil {
		t.Fatal(err)
	}
	if accept.DeviceStatus != "pending" || accept.PendingID == nil || *accept.PendingID != "p1" || accept.Limits.MaxStreams != 8 {
		t.Errorf("%+v", accept)
	}
	if _, err := protocol.ParseE2EAccept([]byte(`{"type":"accept","deviceStatus":"maybe","limits":{"maxStreams":8,"streamWindow":1,"tunnelWindow":1,"maxFrameBytes":1}}`)); err == nil {
		t.Error("未知的 deviceStatus 应被拒")
	}
}
