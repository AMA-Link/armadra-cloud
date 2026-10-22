package edge

import (
	"bytes"
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/netpoll"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil/relayenv"
	"github.com/AMA-Link/armadra-cloud/go/internal/tunnel"
)

// 端到端通道管道的单元测试：假 netpoll.Conn 加真隧道（假传输），对应 e2e-pipe.ts 的行为。

type fakeConn struct {
	mu         sync.Mutex
	writes     [][]byte
	acks       []int
	paused     bool
	pauses     int
	resumes    int
	closed     bool
	afterFlush bool
	autoSent   bool
	h          netpoll.Handler
}

func (f *fakeConn) Write(b []byte, ack int) bool {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return false
	}
	f.writes = append(f.writes, bytes.Clone(b))
	f.acks = append(f.acks, ack)
	auto := f.autoSent
	f.mu.Unlock()
	if auto && ack > 0 {
		f.h.OnSent(f, ack)
	}
	return true
}

func (f *fakeConn) Pause()  { f.mu.Lock(); f.paused = true; f.pauses++; f.mu.Unlock() }
func (f *fakeConn) Resume() { f.mu.Lock(); f.paused = false; f.resumes++; f.mu.Unlock() }
func (f *fakeConn) Close() {
	f.mu.Lock()
	was := f.closed
	f.closed = true
	f.mu.Unlock()
	if !was {
		f.h.OnClose(f, nil)
	}
}
func (f *fakeConn) CloseAfterFlush(time.Duration) {
	f.mu.Lock()
	f.afterFlush = true
	f.mu.Unlock()
}
func (f *fakeConn) RemoteAddr() string { return "127.0.0.1:1" }

func (f *fakeConn) sent() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.writes...)
}

// closeFrames 取已写出的关闭帧（码、原因）。
func (f *fakeConn) closeFrames() []closeRec {
	var out []closeRec
	for _, w := range f.sent() {
		if len(w) >= 2 && w[0] == 0x80|opClose {
			p := w[2:]
			rec := closeRec{code: 1005}
			if len(p) >= 2 {
				rec = closeRec{code: int(binary.BigEndian.Uint16(p)), reason: string(p[2:])}
			}
			out = append(out, rec)
		}
	}
	return out
}

type closeRec struct {
	code   int
	reason string
}

// capture 记录隧道写出的帧。
type capture struct {
	mu     sync.Mutex
	frames []protocol.Frame
}

func (c *capture) Send(b []byte) error {
	f, err := protocol.Decode(b)
	if err != nil {
		panic(err)
	}
	c.mu.Lock()
	c.frames = append(c.frames, f)
	c.mu.Unlock()
	return nil
}
func (c *capture) Close(protocol.CloseCode, string) {}
func (c *capture) Terminate()                       {}

func (c *capture) wait(t testing.TB, pred func([]protocol.Frame) bool) []protocol.Frame {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		frames := append([]protocol.Frame(nil), c.frames...)
		c.mu.Unlock()
		if pred(frames) {
			return frames
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等帧超时：%+v", c.frames)
	return nil
}

func (c *capture) snapshot() []protocol.Frame {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]protocol.Frame(nil), c.frames...)
}

func (c *capture) has(typ protocol.FrameType, code protocol.RstCode) func([]protocol.Frame) bool {
	return func(fs []protocol.Frame) bool {
		for _, f := range fs {
			if f.Type == typ && (typ != protocol.FrameRst || f.Code == code) {
				return true
			}
		}
		return false
	}
}

type pipeRig struct {
	t    testing.TB
	e    *Edge
	ec   *edgeConn
	conn *fakeConn
	tun  *tunnel.Tunnel
	cap  *capture
	s    *tunnel.Stream
}

func newPipe(t testing.TB, cfg func(*tunnel.Config)) *pipeRig {
	t.Helper()
	env := relayenv.New(t, relayenv.Options{})
	c := tunnel.DefaultConfig()
	c.Heartbeat = time.Hour
	if cfg != nil {
		cfg(&c)
	}
	cp := &capture{}
	tun := tunnel.New("t1", env.SourceID, "0.2.0", cp, c, tunnel.Hooks{})
	tun.Start()
	t.Cleanup(func() { tun.Close(protocol.CloseNormal, "done") })
	e := &Edge{
		o:      Options{Log: obs.Nop(), WSPing: time.Second, WSIdle: 3 * time.Second},
		policy: NewPolicy(PolicyOptions{Control: env.Control, Metrics: obs.NewMetrics(func() (int, int) { return 0, 0 })}),
		wheel:  newWheel(),
	}
	ec := &edgeConn{e: e, sourceID: env.SourceID, subject: "acct", started: time.Now()}
	ec.lastSeen.Store(time.Now().UnixMilli())
	fc := &fakeConn{h: ec, autoSent: true}
	ec.bind(fc)
	e.wheel.add(ec)
	s, err := tun.OpenStream(&gen.OpenPayload{Kind: "e2e", Method: "GET", Path: gen.E2EPath, Headers: [][2]string{}, RemoteIP: "1.1.1.1", RelayNode: "n"}, ec, nil)
	if err != nil {
		t.Fatal(err)
	}
	ec.attach(s)
	return &pipeRig{t: t, e: e, ec: ec, conn: fc, tun: tun, cap: cp, s: s}
}

// clientFrameBytes 编一个带掩码的客户端帧。
func clientFrameBytes(op byte, fin bool, payload []byte) []byte {
	b0 := op
	if fin {
		b0 |= 0x80
	}
	key := []byte{0x11, 0x22, 0x33, 0x44}
	var out []byte
	n := len(payload)
	switch {
	case n < 126:
		out = []byte{b0, 0x80 | byte(n)}
	case n <= 0xffff:
		out = []byte{b0, 0x80 | 126, byte(n >> 8), byte(n)}
	default:
		out = []byte{b0, 0x80 | 127, 0, 0, 0, 0, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
	}
	out = append(out, key...)
	for i, v := range payload {
		out = append(out, v^key[i&3])
	}
	return out
}

func (p *pipeRig) read(b []byte) { p.ec.OnRead(p.conn, b) }

func dataBytes(fs []protocol.Frame) []byte {
	var out []byte
	for _, f := range fs {
		if f.Type == protocol.FrameData {
			out = append(out, f.Data...)
		}
	}
	return out
}

func TestPipeUpstream(t *testing.T) {
	t.Run("二进制消息写进流；半帧跨读也能拼起来", func(t *testing.T) {
		p := newPipe(t, nil)
		frame := clientFrameBytes(opBinary, true, []byte("hello world"))
		p.read(frame[:5])
		p.read(frame[5:])
		fs := p.cap.wait(t, func(fs []protocol.Frame) bool { return string(dataBytes(fs)) == "hello world" })
		if len(fs) == 0 {
			t.Fatal("没有帧")
		}
		// 一次读里有多个帧。
		two := append(clientFrameBytes(opBinary, true, []byte("ab")), clientFrameBytes(opBinary, true, []byte("cd"))...)
		p.read(two)
		p.cap.wait(t, func(fs []protocol.Frame) bool { return string(dataBytes(fs)) == "hello worldabcd" })
	})

	t.Run("流还没接上时抢先发来的字节暂存，接上后按序补发", func(t *testing.T) {
		p := newPipe(t, nil)
		// 把连接退回「流还没接上」的状态。
		p.ec.mu.Lock()
		p.ec.s = nil
		p.ec.mu.Unlock()
		p.read(clientFrameBytes(opBinary, true, []byte("early-")))
		p.read(clientFrameBytes(opBinary, true, []byte("bird")))
		if got := dataBytes(p.cap.snapshot()); len(got) != 0 {
			t.Fatalf("流没接上不该发：%q", got)
		}
		p.ec.attach(p.s)
		p.read(clientFrameBytes(opBinary, true, []byte("!")))
		p.cap.wait(t, func(fs []protocol.Frame) bool { return string(dataBytes(fs)) == "early-bird!" })
	})

	t.Run("流没接上就抢发超过上限：以 1009 关闭", func(t *testing.T) {
		p := newPipe(t, nil)
		p.ec.mu.Lock()
		p.ec.s = nil
		p.ec.mu.Unlock()
		big := make([]byte, protocol.MaxFrameBytes-100)
		for i := 0; i < 5; i++ {
			p.read(clientFrameBytes(opBinary, true, big))
		}
		if cf := p.conn.closeFrames(); len(cf) != 1 || cf[0].code != closeTooBig {
			t.Fatalf("%+v", cf)
		}
	})

	t.Run("分片消息按字节透传，空消息忽略", func(t *testing.T) {
		p := newPipe(t, nil)
		p.read(clientFrameBytes(opBinary, false, []byte("12")))
		p.read(clientFrameBytes(opContinuation, false, []byte("34")))
		p.read(clientFrameBytes(opContinuation, true, []byte("56")))
		p.read(clientFrameBytes(opBinary, true, nil))
		p.cap.wait(t, func(fs []protocol.Frame) bool { return string(dataBytes(fs)) == "123456" })
	})

	t.Run("文本消息以 4400 关闭并重置流", func(t *testing.T) {
		p := newPipe(t, nil)
		p.read(clientFrameBytes(opText, true, []byte("plain")))
		if cf := p.conn.closeFrames(); len(cf) != 1 || cf[0].code != 4400 || cf[0].reason != "binary_only" {
			t.Fatalf("%+v", cf)
		}
		p.cap.wait(t, p.cap.has(protocol.FrameRst, protocol.RstClientGone))
	})

	t.Run("客户端正常关闭：回关闭帧、发 END；源回 END 后流摘掉", func(t *testing.T) {
		p := newPipe(t, nil)
		p.read(clientFrameBytes(opClose, true, closePayload(1000, "")))
		if cf := p.conn.closeFrames(); len(cf) != 1 || cf[0].code != 1000 {
			t.Fatalf("%+v", cf)
		}
		if !p.conn.afterFlush {
			t.Fatal("回完关闭帧应等对端断开")
		}
		p.cap.wait(t, p.cap.has(protocol.FrameEnd, 0))
		p.tun.Receive(mustEnc(protocol.Frame{Type: protocol.FrameEnd, StreamID: p.s.ID()}))
		if p.tun.StreamCount() != 0 {
			t.Fatal("两端都结束，流应摘掉")
		}
	})

	t.Run("客户端关闭后源迟迟不回 END：END 宽限到了重置流", func(t *testing.T) {
		old := endGrace
		endGrace = 50 * time.Millisecond
		defer func() { endGrace = old }()
		p := newPipe(t, nil)
		p.read(clientFrameBytes(opClose, true, closePayload(1000, "")))
		p.cap.wait(t, p.cap.has(protocol.FrameRst, protocol.RstClientGone))
	})

	t.Run("掉线（没有关闭帧）：重置流 RST clientGone", func(t *testing.T) {
		p := newPipe(t, nil)
		p.conn.Close()
		p.cap.wait(t, p.cap.has(protocol.FrameRst, protocol.RstClientGone))
		if p.tun.StreamCount() != 0 {
			t.Fatal("流应摘掉")
		}
	})

	t.Run("ping 回 pong，pong 只刷新活性", func(t *testing.T) {
		p := newPipe(t, nil)
		p.read(clientFrameBytes(opPing, true, []byte("x")))
		w := p.conn.sent()
		if len(w) != 1 || w[0][0] != 0x80|opPong || string(w[0][2:]) != "x" {
			t.Fatalf("%x", w)
		}
		before := p.ec.lastSeen.Load()
		time.Sleep(3 * time.Millisecond)
		p.read(clientFrameBytes(opPong, true, nil))
		if p.ec.lastSeen.Load() <= before {
			t.Fatal("pong 应刷新活性")
		}
	})
}

func TestPipeAttachAfterClose(t *testing.T) {
	t.Run("连接在流接上之前就没了：接上时重置这条流，不留在流表里", func(t *testing.T) {
		p := newPipe(t, nil)
		// 退回「101 之后、流还没接上」的状态，然后连接关闭（OnClose 看不到流，没东西可重置）。
		p.ec.mu.Lock()
		p.ec.s = nil
		p.ec.mu.Unlock()
		p.ec.OnClose(p.conn, nil)
		if p.tun.StreamCount() != 1 {
			t.Fatalf("OnClose 不该动流：%d", p.tun.StreamCount())
		}
		p.ec.attach(p.s)
		p.cap.wait(t, p.cap.has(protocol.FrameRst, protocol.RstClientGone))
		if p.tun.StreamCount() != 0 {
			t.Fatal("流应已摘掉")
		}
	})
}

func TestPipeDownstream(t *testing.T) {
	t.Run("源的 DATA 作为二进制消息发出，写进内核后才补信用", func(t *testing.T) {
		p := newPipe(t, func(c *tunnel.Config) { c.StreamWindow = 1000; c.TunnelWindow = 4000 })
		p.conn.autoSent = false
		p.tun.Receive(mustEnc(protocol.Frame{Type: protocol.FrameData, StreamID: p.s.ID(), Data: make([]byte, 600)}))
		w := p.conn.sent()
		if len(w) != 1 || w[0][0] != 0x80|opBinary || len(w[0]) != 4+600 || p.conn.acks[0] != 600 {
			t.Fatalf("%d %v", len(w), p.conn.acks)
		}
		time.Sleep(50 * time.Millisecond)
		p.cap.mu.Lock()
		for _, f := range p.cap.frames {
			if f.Type == protocol.FrameWindow {
				t.Fatal("还没写进内核，不该补信用")
			}
		}
		p.cap.mu.Unlock()
		p.ec.OnSent(p.conn, 600)
		fs := p.cap.wait(t, func(fs []protocol.Frame) bool {
			for _, f := range fs {
				if f.Type == protocol.FrameWindow && f.StreamID == p.s.ID() {
					return true
				}
			}
			return false
		})
		var credit uint32
		for _, f := range fs {
			if f.Type == protocol.FrameWindow && f.StreamID == p.s.ID() {
				credit = f.Credit
			}
		}
		if credit != 600 {
			t.Fatalf("应补 600，实际 %d", credit)
		}
	})

	t.Run("源 END → 1000 关闭；源 RST → 4404", func(t *testing.T) {
		a := newPipe(t, nil)
		a.tun.Receive(mustEnc(protocol.Frame{Type: protocol.FrameEnd, StreamID: a.s.ID()}))
		if cf := a.conn.closeFrames(); len(cf) != 1 || cf[0].code != 1000 {
			t.Fatalf("%+v", cf)
		}
		b := newPipe(t, nil)
		b.tun.Receive(mustEnc(protocol.Frame{Type: protocol.FrameRst, StreamID: b.s.ID(), Code: protocol.RstRefused}))
		if cf := b.conn.closeFrames(); len(cf) != 1 || cf[0].code != 4404 || cf[0].reason != "source_offline" {
			t.Fatalf("%+v", cf)
		}
	})

	t.Run("隧道关闭 → 4404", func(t *testing.T) {
		p := newPipe(t, nil)
		p.tun.Close(protocol.CloseNormal, "gone")
		if cf := p.conn.closeFrames(); len(cf) != 1 || cf[0].code != 4404 {
			t.Fatalf("%+v", cf)
		}
	})

	t.Run("关闭中收到的下行字节丢弃，但信用照还", func(t *testing.T) {
		p := newPipe(t, func(c *tunnel.Config) { c.StreamWindow = 1000 })
		p.ec.closeGracefully(1000, "")
		before := len(p.conn.sent())
		p.tun.Receive(mustEnc(protocol.Frame{Type: protocol.FrameData, StreamID: p.s.ID(), Data: make([]byte, 600)}))
		if len(p.conn.sent()) != before {
			t.Fatal("关闭中不该再写数据")
		}
		p.cap.wait(t, p.cap.has(protocol.FrameWindow, 0))
	})
}

func TestPipeBackpressure(t *testing.T) {
	t.Run("上行没有信用：停读客户端；WINDOW 之后恢复", func(t *testing.T) {
		p := newPipe(t, func(c *tunnel.Config) { c.StreamWindow = 100; c.TunnelWindow = 10000 })
		p.read(clientFrameBytes(opBinary, true, make([]byte, 250)))
		if p.conn.pauses != 1 || !p.conn.paused {
			t.Fatalf("应停读：pauses=%d paused=%v", p.conn.pauses, p.conn.paused)
		}
		p.tun.Receive(mustEnc(protocol.Frame{Type: protocol.FrameWindow, StreamID: p.s.ID(), Credit: 400}))
		p.conn.mu.Lock()
		defer p.conn.mu.Unlock()
		if p.conn.resumes == 0 || p.conn.paused {
			t.Fatalf("WINDOW 后应恢复读：resumes=%d paused=%v", p.conn.resumes, p.conn.paused)
		}
	})
}

func TestPipeProtocolErrors(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want int
	}{
		{"客户端帧没有掩码", []byte{0x82, 0x01, 'x'}, 1002},
		{"RSV 位置位", []byte{0xC2, 0x80, 0, 0, 0, 0}, 1002},
		{"未知操作码", []byte{0x83, 0x80, 0, 0, 0, 0}, 1002},
		{"控制帧分片", clientFrameBytes(opPing, false, nil), 1002},
		{"控制帧载荷超过 125", clientFrameBytes(opPing, true, make([]byte, 126)), 1002},
		{"没有可续的分片", clientFrameBytes(opContinuation, true, []byte("x")), 1002},
		{"关闭帧载荷只有一个字节", clientFrameBytes(opClose, true, []byte{3}), 1002},
		{"关闭码不合法", clientFrameBytes(opClose, true, closePayload(1005, "")), 1002},
		{"关闭原因不是 UTF-8", clientFrameBytes(opClose, true, append(closePayload(1000, ""), 0xff, 0xfe)), 1007},
		{"载荷超过一帧上限", []byte{0x82, 0x80 | 127, 0, 0, 0, 0, 0, 0x20, 0, 0, 1, 2, 3, 4}, 1009},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newPipe(t, nil)
			p.read(c.in)
			cf := p.conn.closeFrames()
			if len(cf) != 1 || cf[0].code != c.want {
				t.Fatalf("应以 %d 关闭：%+v", c.want, cf)
			}
			p.cap.wait(t, p.cap.has(protocol.FrameRst, protocol.RstClientGone))
		})
	}

	t.Run("分片没结束就开始新消息", func(t *testing.T) {
		p := newPipe(t, nil)
		p.read(clientFrameBytes(opBinary, false, []byte("a")))
		p.read(clientFrameBytes(opBinary, true, []byte("b")))
		if cf := p.conn.closeFrames(); len(cf) != 1 || cf[0].code != 1002 {
			t.Fatalf("%+v", cf)
		}
	})
}

func TestPipeLiveness(t *testing.T) {
	t.Run("ping 轮：到点发 ping，空闲超过 idle 就断开", func(t *testing.T) {
		p := newPipe(t, nil)
		now := time.Now().UnixMilli()
		p.ec.tick(now, time.Minute)
		w := p.conn.sent()
		if len(w) != 1 || w[0][0] != 0x80|opPing {
			t.Fatalf("%x", w)
		}
		p.ec.lastSeen.Store(now - 61_000)
		p.ec.tick(now, time.Minute)
		p.conn.mu.Lock()
		closed := p.conn.closed
		p.conn.mu.Unlock()
		if !closed {
			t.Fatal("空闲超时应断开")
		}
		p.cap.wait(t, p.cap.has(protocol.FrameRst, protocol.RstClientGone))
	})
}

func mustEnc(f protocol.Frame) []byte {
	b, err := protocol.Encode(f)
	if err != nil {
		panic(err)
	}
	return b
}

// FuzzEdgeRead：任意字节、任意切分喂给连接的读路径，不得 panic，也不得死锁。
func FuzzEdgeRead(f *testing.F) {
	f.Add(clientFrameBytes(opBinary, true, []byte("hello")), uint8(3))
	f.Add(append(clientFrameBytes(opText, true, []byte("x")), clientFrameBytes(opClose, true, closePayload(1000, "bye"))...), uint8(1))
	f.Add([]byte{0x82, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 1, 2, 3, 4}, uint8(2))
	f.Add(clientFrameBytes(opPing, true, []byte("p")), uint8(7))
	f.Fuzz(func(t *testing.T, data []byte, cut uint8) {
		p := newPipe(t, nil)
		step := int(cut)%17 + 1
		for off := 0; off < len(data); off += step {
			p.read(data[off:min(off+step, len(data))])
		}
		// 不管喂了什么，连接状态都得自洽：关闭帧最多一个。
		if cf := p.conn.closeFrames(); len(cf) > 2 {
			t.Fatalf("关闭帧过多：%+v", cf)
		}
	})
}

// FuzzClientFrame：帧解析不得 panic，用掉的字节与载荷范围自洽。
func FuzzClientFrame(f *testing.F) {
	f.Add(clientFrameBytes(opBinary, true, []byte("hello")))
	f.Add(clientFrameBytes(opClose, true, closePayload(1000, "")))
	f.Add([]byte{0x82, 0xfe, 0x00})
	f.Add([]byte{0x82, 0xff, 0x80, 0, 0, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		buf := bytes.Clone(data)
		fr, used, err := parseClientFrame(buf, protocol.MaxFrameBytes)
		if err != nil {
			if err.code != closeProtocolError && err.code != closeTooBig {
				t.Fatalf("意外的关闭码 %d", err.code)
			}
			return
		}
		if used < 0 || used > len(buf) {
			t.Fatalf("used=%d len=%d", used, len(buf))
		}
		if used == 0 {
			return
		}
		if len(fr.Payload) > used {
			t.Fatalf("载荷 %d 超过帧 %d", len(fr.Payload), used)
		}
		if fr.Op == opClose {
			_, _ = parseClose(fr.Payload)
		}
	})
}
