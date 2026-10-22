package protocol_test

import (
	"bytes"
	"math"
	"strings"
	"testing"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil"
)

func strp(s string) *string { return &s }

func sampleOpen() *gen.OpenPayload {
	return &gen.OpenPayload{
		Kind: "http", Method: "GET", Path: "/health?x=1",
		Headers:      [][2]string{{"host", "a"}},
		ClientOrigin: nil, RemoteIP: "198.51.100.1", RelayNode: "n1",
	}
}

func raw(typ byte, streamID uint32, payload ...byte) []byte {
	out := []byte{typ, byte(streamID >> 24), byte(streamID >> 16), byte(streamID >> 8), byte(streamID)}
	return append(out, payload...)
}

func wantCode(t *testing.T, err error, want protocol.FrameErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("应报 %s，实际成功", want)
	}
	if got := protocol.CodeOf(err); got != want {
		t.Fatalf("应报 %s，实际 %s（%v）", want, got, err)
	}
}

func TestRoundTrip(t *testing.T) {
	frames := []protocol.Frame{
		{Type: protocol.FrameOpen, StreamID: 7, Open: sampleOpen()},
		{Type: protocol.FrameData, StreamID: 7, Data: []byte{0, 1, 2, 255}},
		{Type: protocol.FrameData, StreamID: 7, Data: []byte{}},
		{Type: protocol.FrameEnd, StreamID: 7},
		{Type: protocol.FrameRst, StreamID: 7, Code: protocol.RstFlowControl},
		{Type: protocol.FrameWindow, StreamID: 7, Credit: 1},
		{Type: protocol.FrameWindow, StreamID: 0, Credit: math.MaxUint32},
		{Type: protocol.FramePing, Ts: 1_760_000_000_000.5},
		{Type: protocol.FramePong, Ts: 0},
		{Type: protocol.FrameGoaway, Goaway: &gen.GoawayPayload{Reason: "rebalance", GraceMs: 0}},
	}
	for _, f := range frames {
		encoded, err := protocol.Encode(f)
		if err != nil {
			t.Fatalf("%s：编码失败 %v", f.Type, err)
		}
		decoded, err := protocol.Decode(encoded)
		if err != nil {
			t.Fatalf("%s：解码失败 %v", f.Type, err)
		}
		again, err := protocol.Encode(decoded)
		if err != nil || !bytes.Equal(again, encoded) {
			t.Fatalf("%s：解码再编码的字节不同", f.Type)
		}
		if decoded.Type != f.Type || decoded.StreamID != f.StreamID || decoded.Credit != f.Credit ||
			decoded.Code != f.Code || decoded.Ts != f.Ts || !bytes.Equal(decoded.Data, f.Data) {
			t.Fatalf("%s：往返后字段不同：%+v", f.Type, decoded)
		}
	}
}

func TestFrameHeaderIsTypeAndBigEndianStreamID(t *testing.T) {
	got, err := protocol.Encode(protocol.Frame{Type: protocol.FrameEnd, StreamID: 0x01020304})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte{3, 1, 2, 3, 4}) {
		t.Fatalf("帧头是 %v", got)
	}
}

func TestOpenIgnoresUnknownFieldsAndKeyCase(t *testing.T) {
	json := `{"kind":"http","method":"GET","path":"/x","headers":[],"clientOrigin":null,` +
		`"remoteIp":"1.1.1.1","relayNode":"n","extra":1,"KIND":"ws"}`
	f, err := protocol.Decode(raw(1, 7, []byte(json)...))
	if err != nil {
		t.Fatal(err)
	}
	if f.Open.Kind != "http" {
		t.Fatalf("大小写不同的键覆盖了已校验的值：kind=%s", f.Open.Kind)
	}
}

// JSON.stringify 与 Go 的 encoding/json 在转义上不同；OPEN 载荷的字节要和 TypeScript 一致。
func TestOpenEscapingMatchesJavaScript(t *testing.T) {
	open := sampleOpen()
	open.Path = "/a<b>&c \u007f\x01\x1f\"\\\b\f\n\r\t"
	encoded, err := protocol.Encode(protocol.Frame{Type: protocol.FrameOpen, StreamID: 1, Open: open})
	if err != nil {
		t.Fatal(err)
	}
	want := `"path":"/a<b>&c` + " \u007f" + `\u0001\u001f\"\\\b\f\n\r\t"`
	if !strings.Contains(string(encoded), want) {
		t.Fatalf("转义不对：%s", encoded)
	}
	decoded, err := protocol.Decode(encoded)
	if err != nil || decoded.Open.Path != open.Path {
		t.Fatalf("往返失败：%v", err)
	}
}

func TestBadFrames(t *testing.T) {
	okOpen := `{"kind":"http","method":"GET","path":"/","headers":[],"clientOrigin":null,"remoteIp":"x","relayNode":"n"}`
	cases := []struct {
		name  string
		bytes []byte
		want  protocol.FrameErrorCode
	}{
		{"空", nil, protocol.ErrBadFrame},
		{"帧头截断", []byte{3, 0, 0, 0}, protocol.ErrBadFrame},
		{"未知类型 0", raw(0, 1), protocol.ErrBadFrame},
		{"未知类型 9", raw(9, 1), protocol.ErrBadFrame},
		{"OPEN 用 streamId 0", raw(1, 0, []byte(okOpen)...), protocol.ErrBadFrame},
		{"DATA 用 streamId 0", raw(2, 0, 1), protocol.ErrBadFrame},
		{"END 用 streamId 0", raw(3, 0), protocol.ErrBadFrame},
		{"RST 用 streamId 0", raw(4, 0, 0, 1), protocol.ErrBadFrame},
		{"PING 带 streamId", raw(6, 1, make([]byte, 8)...), protocol.ErrBadFrame},
		{"PONG 带 streamId", raw(7, 1, make([]byte, 8)...), protocol.ErrBadFrame},
		{"GOAWAY 带 streamId", raw(8, 1, []byte(`{"reason":"shutdown","graceMs":1}`)...), protocol.ErrBadFrame},
		{"END 带载荷", raw(3, 1, 0), protocol.ErrBadFrame},
		{"RST 载荷长度不对", raw(4, 1, 0), protocol.ErrBadFrame},
		{"WINDOW 为 0", raw(5, 1, 0, 0, 0, 0), protocol.ErrBadFrame},
		{"WINDOW 载荷长度不对", raw(5, 1, 0, 0, 1), protocol.ErrBadFrame},
		{"PING 载荷长度不对", raw(6, 0, 1, 2, 3), protocol.ErrBadFrame},
		{"PING 的 ts 是 NaN", raw(6, 0, 0x7f, 0xf8, 0, 0, 0, 0, 0, 0), protocol.ErrBadFrame},
		{"PONG 的 ts 是无穷", raw(7, 0, 0x7f, 0xf0, 0, 0, 0, 0, 0, 0), protocol.ErrBadFrame},
		{"GOAWAY 不是 JSON", raw(8, 0, []byte("nope")...), protocol.ErrBadFrame},
		{"GOAWAY 的 reason 未知", raw(8, 0, []byte(`{"reason":"x","graceMs":1}`)...), protocol.ErrBadFrame},
		{"GOAWAY 的 graceMs 为负", raw(8, 0, []byte(`{"reason":"shutdown","graceMs":-1}`)...), protocol.ErrBadFrame},
		{"GOAWAY 的 graceMs 不是整数", raw(8, 0, []byte(`{"reason":"shutdown","graceMs":1.5}`)...), protocol.ErrBadFrame},
		{"DATA 超过 maxDataChunk", raw(2, 1, make([]byte, protocol.MaxDataChunk+1)...), protocol.ErrFrameTooLarge},
		{"整帧超过 maxFrameBytes", raw(2, 1, make([]byte, protocol.MaxFrameBytes)...), protocol.ErrFrameTooLarge},
		{"OPEN 不是 JSON", raw(1, 1, []byte("{")...), protocol.ErrBadOpen},
		{"OPEN 不是合法 UTF-8", raw(1, 1, 0xff, 0xfe), protocol.ErrBadOpen},
		{"OPEN 缺字段", raw(1, 1, []byte(`{"kind":"http"}`)...), protocol.ErrBadOpen},
		{"OPEN 的 kind 未知", raw(1, 1, []byte(strings.Replace(okOpen, `"http"`, `"tcp"`, 1))...), protocol.ErrBadOpen},
		{"OPEN 的 path 不以 / 开头", raw(1, 1, []byte(strings.Replace(okOpen, `"/"`, `"x"`, 1))...), protocol.ErrBadOpen},
		{"OPEN 的 method 为空", raw(1, 1, []byte(strings.Replace(okOpen, `"GET"`, `""`, 1))...), protocol.ErrBadOpen},
		{"OPEN 的 method 太长", raw(1, 1, []byte(strings.Replace(okOpen, `"GET"`, `"`+strings.Repeat("G", 33)+`"`, 1))...), protocol.ErrBadOpen},
		{"OPEN 的头名为空", raw(1, 1, []byte(strings.Replace(okOpen, `"headers":[]`, `"headers":[["","v"]]`, 1))...), protocol.ErrBadOpen},
		{"OPEN 的头不是二元组", raw(1, 1, []byte(strings.Replace(okOpen, `"headers":[]`, `"headers":[["a","b","c"]]`, 1))...), protocol.ErrBadOpen},
		{"OPEN 的 clientOrigin 缺失", raw(1, 1, []byte(strings.Replace(okOpen, `"clientOrigin":null,`, ``, 1))...), protocol.ErrBadOpen},
		{"OPEN 的 headers 超过 32 KiB", raw(1, 1, []byte(strings.Replace(okOpen, `"headers":[]`,
			`"headers":[["a","`+strings.Repeat("v", protocol.OpenHeadersMax)+`"]]`, 1))...), protocol.ErrBadOpen},
		{"e2e 的 OPEN 用了别的路径", raw(1, 1, []byte(strings.Replace(okOpen, `"http"`, `"e2e"`, 1))...), protocol.ErrBadOpen},
		{"e2e 的 OPEN 带了头", raw(1, 1, []byte(`{"kind":"e2e","method":"GET","path":"/_relay/e2e","headers":[["a","b"]],"clientOrigin":null,"remoteIp":"x","relayNode":"n"}`)...), protocol.ErrBadOpen},
		{"e2e 的 OPEN 用了别的方法", raw(1, 1, []byte(`{"kind":"e2e","method":"POST","path":"/_relay/e2e","headers":[],"clientOrigin":null,"remoteIp":"x","relayNode":"n"}`)...), protocol.ErrBadOpen},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := protocol.Decode(c.bytes)
			wantCode(t, err, c.want)
		})
	}
}

// 解码拒的，编码也不产出。
func TestEncodeRejectsWhatDecodeRejects(t *testing.T) {
	long := strings.Repeat("v", protocol.OpenHeadersMax)
	e2e := func(mutate func(*gen.OpenPayload)) *gen.OpenPayload {
		o := &gen.OpenPayload{Kind: "e2e", Method: "GET", Path: "/_relay/e2e", Headers: [][2]string{}, RemoteIP: "x", RelayNode: "n"}
		mutate(o)
		return o
	}
	open := func(mutate func(*gen.OpenPayload)) *gen.OpenPayload {
		o := sampleOpen()
		mutate(o)
		return o
	}
	cases := []struct {
		name  string
		frame protocol.Frame
		want  protocol.FrameErrorCode
	}{
		{"未知类型", protocol.Frame{Type: 0, StreamID: 1}, protocol.ErrBadFrame},
		{"OPEN 用 streamId 0", protocol.Frame{Type: protocol.FrameOpen, Open: sampleOpen()}, protocol.ErrBadFrame},
		{"DATA 用 streamId 0", protocol.Frame{Type: protocol.FrameData}, protocol.ErrBadFrame},
		{"PING 带 streamId", protocol.Frame{Type: protocol.FramePing, StreamID: 1}, protocol.ErrBadFrame},
		{"GOAWAY 带 streamId", protocol.Frame{Type: protocol.FrameGoaway, StreamID: 1, Goaway: &gen.GoawayPayload{Reason: "shutdown"}}, protocol.ErrBadFrame},
		{"WINDOW 为 0", protocol.Frame{Type: protocol.FrameWindow, StreamID: 1}, protocol.ErrBadFrame},
		{"ts 是 NaN", protocol.Frame{Type: protocol.FramePing, Ts: math.NaN()}, protocol.ErrBadFrame},
		{"ts 是无穷", protocol.Frame{Type: protocol.FramePong, Ts: math.Inf(-1)}, protocol.ErrBadFrame},
		{"GOAWAY 缺载荷", protocol.Frame{Type: protocol.FrameGoaway}, protocol.ErrBadFrame},
		{"GOAWAY 的 reason 未知", protocol.Frame{Type: protocol.FrameGoaway, Goaway: &gen.GoawayPayload{Reason: "x"}}, protocol.ErrBadFrame},
		{"GOAWAY 的 graceMs 为负", protocol.Frame{Type: protocol.FrameGoaway, Goaway: &gen.GoawayPayload{Reason: "shutdown", GraceMs: -1}}, protocol.ErrBadFrame},
		{"DATA 超过 maxDataChunk", protocol.Frame{Type: protocol.FrameData, StreamID: 1, Data: make([]byte, protocol.MaxDataChunk+1)}, protocol.ErrFrameTooLarge},
		{"OPEN 缺载荷", protocol.Frame{Type: protocol.FrameOpen, StreamID: 1}, protocol.ErrBadOpen},
		{"OPEN 的 kind 未知", protocol.Frame{Type: protocol.FrameOpen, StreamID: 1, Open: open(func(o *gen.OpenPayload) { o.Kind = "tcp" })}, protocol.ErrBadOpen},
		{"OPEN 的 path 不以 / 开头", protocol.Frame{Type: protocol.FrameOpen, StreamID: 1, Open: open(func(o *gen.OpenPayload) { o.Path = "x" })}, protocol.ErrBadOpen},
		{"OPEN 的 method 为空", protocol.Frame{Type: protocol.FrameOpen, StreamID: 1, Open: open(func(o *gen.OpenPayload) { o.Method = "" })}, protocol.ErrBadOpen},
		{"OPEN 的头名为空", protocol.Frame{Type: protocol.FrameOpen, StreamID: 1, Open: open(func(o *gen.OpenPayload) { o.Headers = [][2]string{{"", "v"}} })}, protocol.ErrBadOpen},
		{"OPEN 的 headers 超过 32 KiB", protocol.Frame{Type: protocol.FrameOpen, StreamID: 1, Open: open(func(o *gen.OpenPayload) { o.Headers = [][2]string{{"a", long}} })}, protocol.ErrBadOpen},
		{"OPEN 不是合法 UTF-8", protocol.Frame{Type: protocol.FrameOpen, StreamID: 1, Open: open(func(o *gen.OpenPayload) { o.Path = "/\xff" })}, protocol.ErrBadOpen},
		{"e2e 用了别的路径", protocol.Frame{Type: protocol.FrameOpen, StreamID: 1, Open: e2e(func(o *gen.OpenPayload) { o.Path = "/x" })}, protocol.ErrBadOpen},
		{"e2e 用了别的方法", protocol.Frame{Type: protocol.FrameOpen, StreamID: 1, Open: e2e(func(o *gen.OpenPayload) { o.Method = "POST" })}, protocol.ErrBadOpen},
		{"e2e 带了头", protocol.Frame{Type: protocol.FrameOpen, StreamID: 1, Open: e2e(func(o *gen.OpenPayload) { o.Headers = [][2]string{{"a", "b"}} })}, protocol.ErrBadOpen},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := protocol.Encode(c.frame)
			wantCode(t, err, c.want)
		})
	}
	// 恰好顶格的合法值要能编、能解。
	atLimit := open(func(o *gen.OpenPayload) { o.Headers = [][2]string{{"a", long[:protocol.OpenHeadersMax-1]}} })
	encoded, err := protocol.Encode(protocol.Frame{Type: protocol.FrameOpen, StreamID: 1, Open: atLimit})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := protocol.Decode(encoded); err != nil {
		t.Fatal(err)
	}
	data, err := protocol.Encode(protocol.Frame{Type: protocol.FrameData, StreamID: 1, Data: make([]byte, protocol.MaxDataChunk)})
	if err != nil || len(data) != protocol.MaxDataChunk+protocol.FrameHeaderBytes {
		t.Fatalf("64 KiB 的 DATA 应能编码：%v", err)
	}
}

func TestOpenWithNullableOrigin(t *testing.T) {
	open := sampleOpen()
	open.ClientOrigin = strp("https://app.armadra.test")
	encoded, err := protocol.Encode(protocol.Frame{Type: protocol.FrameOpen, StreamID: 1, Open: open})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := protocol.Decode(encoded)
	if err != nil || decoded.Open.ClientOrigin == nil || *decoded.Open.ClientOrigin != *open.ClientOrigin {
		t.Fatalf("clientOrigin 往返失败：%v", err)
	}
}

func TestDecodeCopiesData(t *testing.T) {
	b := raw(2, 1, 9, 9, 9)
	f, err := protocol.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	b[5] = 0
	if f.Data[0] != 9 {
		t.Fatal("Data 应是拷贝")
	}
}

func TestConstantsMatchGenerated(t *testing.T) {
	if protocol.MaxFrameBytes != 1<<20 || protocol.MaxDataChunk != 64<<10 || protocol.StreamWindow != 256<<10 ||
		protocol.TunnelWindow != 8<<20 || protocol.MaxStreams != 256 {
		t.Fatal("限制与协议不一致")
	}
	if gen.ProtocolMajor != 1 {
		t.Fatalf("协议 major 应为 1，实际 %d", gen.ProtocolMajor)
	}
	names := map[protocol.FrameType]string{1: "open", 2: "data", 3: "end", 4: "rst", 5: "window", 6: "ping", 7: "pong", 8: "goaway"}
	for typ, name := range names {
		if typ.String() != name {
			t.Errorf("帧类型 %d 应叫 %s", typ, name)
		}
	}
}

// 夹具：每个单帧 .bin 解码再编码逐字节相同。
func TestGoldenFrames(t *testing.T) {
	names := testutil.Fixtures(t, "tunnel", ".bin")
	n := 0
	for _, name := range names {
		if strings.Contains(name, "sequence") {
			continue
		}
		n++
		t.Run(name, func(t *testing.T) {
			golden := testutil.Fixture(t, "tunnel/"+name)
			f, err := protocol.Decode(golden)
			if err != nil {
				t.Fatalf("解码失败：%v", err)
			}
			again, err := protocol.Encode(f)
			if err != nil {
				t.Fatalf("重新编码失败：%v", err)
			}
			if !bytes.Equal(again, golden) {
				t.Fatalf("重编码的字节不同：\n得到 %q\n应为 %q", again, golden)
			}
		})
	}
	if n != 11 {
		t.Fatalf("单帧夹具应有 11 个，实际 %d", n)
	}
}

func TestGoldenFieldValues(t *testing.T) {
	check := func(name string, want protocol.Frame) {
		f, err := protocol.Decode(testutil.Fixture(t, "tunnel/"+name))
		if err != nil {
			t.Fatalf("%s：%v", name, err)
		}
		if f.Type != want.Type || f.StreamID != want.StreamID || f.Code != want.Code ||
			f.Credit != want.Credit || f.Ts != want.Ts {
			t.Errorf("%s：得到 %+v", name, f)
		}
	}
	check("end.bin", protocol.Frame{Type: protocol.FrameEnd, StreamID: 1})
	check("rst-3.bin", protocol.Frame{Type: protocol.FrameRst, StreamID: 1, Code: protocol.RstSourceGone})
	check("window-stream.bin", protocol.Frame{Type: protocol.FrameWindow, StreamID: 1, Credit: 131072})
	check("window-tunnel.bin", protocol.Frame{Type: protocol.FrameWindow, Credit: 4194304})
	check("ping.bin", protocol.Frame{Type: protocol.FramePing, Ts: 1_760_000_000_000})
	check("pong.bin", protocol.Frame{Type: protocol.FramePong, Ts: 1_760_000_000_000})
	g, err := protocol.Decode(testutil.Fixture(t, "tunnel/goaway.bin"))
	if err != nil || g.Goaway.Reason != "shutdown" || g.Goaway.GraceMs != protocol.GoawayGraceMs {
		t.Errorf("goaway.bin：%+v %v", g, err)
	}
	e, err := protocol.Decode(testutil.Fixture(t, "tunnel/open-e2e.bin"))
	if err != nil || e.StreamID != 5 || e.Open.Kind != "e2e" || e.Open.Path != gen.E2EPath ||
		e.Open.ClientOrigin == nil || *e.Open.ClientOrigin != "https://app.armadra.test" {
		t.Errorf("open-e2e.bin：%+v %v", e, err)
	}
	d, err := protocol.Decode(testutil.Fixture(t, "tunnel/data-64k.bin"))
	if err != nil || len(d.Data) != protocol.MaxDataChunk {
		t.Errorf("data-64k.bin：%v", err)
	}
}

// sequence-http-roundtrip.bin：每帧前面加 u32 BE 的长度。
func TestGoldenSequence(t *testing.T) {
	data := testutil.Fixture(t, "tunnel/sequence-http-roundtrip.bin")
	var types []protocol.FrameType
	for len(data) > 0 {
		if len(data) < 4 {
			t.Fatal("长度前缀被截断")
		}
		n := int(data[0])<<24 | int(data[1])<<16 | int(data[2])<<8 | int(data[3])
		if len(data) < 4+n {
			t.Fatal("帧被截断")
		}
		golden := data[4 : 4+n]
		data = data[4+n:]
		f, err := protocol.Decode(golden)
		if err != nil {
			t.Fatal(err)
		}
		again, err := protocol.Encode(f)
		if err != nil || !bytes.Equal(again, golden) {
			t.Fatalf("第 %d 帧重编码不同", len(types)+1)
		}
		types = append(types, f.Type)
	}
	want := []protocol.FrameType{protocol.FrameOpen, protocol.FrameData, protocol.FrameData, protocol.FrameEnd, protocol.FrameData, protocol.FrameEnd}
	if len(types) != len(want) {
		t.Fatalf("帧序列 %v", types)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("第 %d 帧应为 %s，实际 %s", i+1, want[i], types[i])
		}
	}
}
