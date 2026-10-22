package tunnel_test

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil"
	"github.com/AMA-Link/armadra-cloud/go/internal/tunnel"
)

// fakeTransport 记录隧道写出的帧。Send 是异步的（写协程），所以取帧前先等写端安静下来。
type fakeTransport struct {
	mu         sync.Mutex
	frames     []protocol.Frame
	raw        [][]byte
	closed     *closeRec
	terminated bool
	// gate 非空时，Send 先等它关闭（测试用来让写端停在第一帧上，攒齐几条流的队列）。
	gate chan struct{}
}

type closeRec struct {
	Code   protocol.CloseCode
	Reason string
}

func (f *fakeTransport) Send(b []byte) error {
	f.mu.Lock()
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	fr, err := protocol.Decode(b)
	if err != nil {
		panic("隧道写出了坏帧：" + err.Error())
	}
	f.mu.Lock()
	f.frames = append(f.frames, fr)
	f.raw = append(f.raw, bytes.Clone(b))
	f.mu.Unlock()
	return nil
}

func (f *fakeTransport) Close(code protocol.CloseCode, reason string) {
	f.mu.Lock()
	f.closed = &closeRec{code, reason}
	f.mu.Unlock()
}

func (f *fakeTransport) Terminate() {
	f.mu.Lock()
	f.terminated = true
	f.mu.Unlock()
}

func (f *fakeTransport) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.frames)
}

// settle 等到 20 毫秒内没有新帧。
func (f *fakeTransport) settle() {
	last := -1
	for i := 0; i < 200; i++ {
		n := f.count()
		if n == last {
			return
		}
		last = n
		time.Sleep(20 * time.Millisecond)
	}
}

// takeKeep 看到目前为止写出的全部帧，不取走。
func (f *fakeTransport) takeKeep() []protocol.Frame {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]protocol.Frame(nil), f.frames...)
}

// take 取走到目前为止写出的全部帧。
func (f *fakeTransport) take() []protocol.Frame {
	f.settle()
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.frames
	f.frames, f.raw = nil, nil
	return out
}

func (f *fakeTransport) takeRaw() [][]byte {
	f.settle()
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.raw
	f.frames, f.raw = nil, nil
	return out
}

func (f *fakeTransport) closedWith() *closeRec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeTransport) wasTerminated() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.terminated
}

// recSink 记录流收到的事件。
type recSink struct {
	mu          sync.Mutex
	data        []byte
	chunks      int
	ended       bool
	reset       *protocol.RstCode
	resumes     int
	autoConsume bool
}

func (r *recSink) Data(s *tunnel.Stream, p []byte) {
	r.mu.Lock()
	r.data = append(r.data, p...)
	r.chunks++
	auto := r.autoConsume
	r.mu.Unlock()
	if auto {
		s.Consumed(len(p))
	}
}

func (r *recSink) End(*tunnel.Stream) {
	r.mu.Lock()
	r.ended = true
	r.mu.Unlock()
}

func (r *recSink) Reset(_ *tunnel.Stream, code protocol.RstCode) {
	r.mu.Lock()
	r.reset = &code
	r.mu.Unlock()
}

func (r *recSink) Resume(*tunnel.Stream) {
	r.mu.Lock()
	r.resumes++
	r.mu.Unlock()
}

func (r *recSink) snapshot() (data []byte, ended bool, reset *protocol.RstCode, resumes int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return bytes.Clone(r.data), r.ended, r.reset, r.resumes
}

// rig 是一条假传输上的隧道。
type rig struct {
	t    *testing.T
	tun  *tunnel.Tunnel
	tr   *fakeTransport
	info []tunnel.CloseInfo
	mu   sync.Mutex
	pong int
	miss int
	peer []string
}

func testConfig(over func(*tunnel.Config)) tunnel.Config {
	cfg := tunnel.DefaultConfig()
	cfg.Heartbeat = time.Hour
	if over != nil {
		over(&cfg)
	}
	return cfg
}

func newRig(t *testing.T, over func(*tunnel.Config)) *rig {
	t.Helper()
	r := &rig{t: t, tr: &fakeTransport{}}
	r.tun = tunnel.New("t1", "0123456789abcdef0123456789abcdef", "0.2.0", r.tr, testConfig(over), tunnel.Hooks{
		OnClose:         func(i tunnel.CloseInfo) { r.mu.Lock(); r.info = append(r.info, i); r.mu.Unlock() },
		OnPong:          func() { r.mu.Lock(); r.pong++; r.mu.Unlock() },
		OnHeartbeatMiss: func() { r.mu.Lock(); r.miss++; r.mu.Unlock() },
		OnPeerGoaway:    func(reason string) { r.mu.Lock(); r.peer = append(r.peer, reason); r.mu.Unlock() },
	})
	r.tun.Start()
	t.Cleanup(func() { r.tun.Close(protocol.CloseNormal, "cleanup") })
	return r
}

func (r *rig) feed(f protocol.Frame) {
	r.t.Helper()
	b, err := protocol.Encode(f)
	if err != nil {
		r.t.Fatal(err)
	}
	r.tun.Receive(b)
}

func (r *rig) closes() []tunnel.CloseInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]tunnel.CloseInfo(nil), r.info...)
}

func openPayload() *gen.OpenPayload {
	return &gen.OpenPayload{
		Kind: "e2e", Method: "GET", Path: "/_relay/e2e", Headers: [][2]string{},
		RemoteIP: "203.0.113.7", RelayNode: "relay-1",
	}
}

func (r *rig) open(sink tunnel.Sink) *tunnel.Stream {
	r.t.Helper()
	s, err := r.tun.OpenStream(openPayload(), sink, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	return s
}

func dataFrames(frames []protocol.Frame) []protocol.Frame {
	var out []protocol.Frame
	for _, f := range frames {
		if f.Type == protocol.FrameData {
			out = append(out, f)
		}
	}
	return out
}

func sizes(frames []protocol.Frame) []int {
	var out []int
	for _, f := range dataFrames(frames) {
		out = append(out, len(f.Data))
	}
	return out
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}

// splitSequence 把「u32 BE 长度 + 帧」连起来的夹具拆开。
func splitSequence(t *testing.T, data []byte) [][]byte {
	t.Helper()
	var out [][]byte
	for len(data) > 0 {
		if len(data) < 4 {
			t.Fatal("长度前缀被截断")
		}
		n := int(data[0])<<24 | int(data[1])<<16 | int(data[2])<<8 | int(data[3])
		if len(data) < 4+n {
			t.Fatal("帧被截断")
		}
		out = append(out, data[4:4+n])
		data = data[4+n:]
	}
	return out
}

func fixtureBytes(t *testing.T, name string) []byte { return testutil.Fixture(t, name) }

func fixtureBytesTB(t testing.TB, name string) []byte { return testutil.Fixture(t, name) }
