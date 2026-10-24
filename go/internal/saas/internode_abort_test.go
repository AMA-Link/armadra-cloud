package saas

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/tunnel"
)

// gatedTransport 记下本地隧道写给源的帧；gate 关闭之前 Send 卡住（写协程停在第一帧上，后面的帧都留在出队列）。
type gatedTransport struct {
	gate   chan struct{}
	mu     sync.Mutex
	frames []protocol.Frame
}

func (g *gatedTransport) Send(b []byte) error {
	<-g.gate
	f, err := protocol.Decode(b)
	if err != nil {
		panic(err)
	}
	g.mu.Lock()
	g.frames = append(g.frames, f)
	g.mu.Unlock()
	return nil
}
func (*gatedTransport) Close(protocol.CloseCode, string) {}
func (*gatedTransport) Terminate()                       {}

// stream 是流 id 写给源的载荷（按序拼起来）与是否写出了 END、RST。
func (g *gatedTransport) stream(id uint32) (data []byte, end, rst bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, f := range g.frames {
		if f.StreamID != id {
			continue
		}
		switch f.Type {
		case protocol.FrameData:
			data = append(data, f.Data...)
		case protocol.FrameEnd:
			end = true
		case protocol.FrameRst:
			rst = true
		}
	}
	return data, end, rst
}

// #136：转来的流两端都已 END（对端 END 已转成本地隧道流的 End、源也发了 END），而本地隧道的写端还没把
// 上行尾巴与 END 写给源时，对端 RST 或链路断开再中止本地隧道上的流：源仍要收到全部字节与 END，
// 不能两样都收不到、流在源那边一直挂着。用链路信用为 0 让源发来的下行字节挂在持有节点上，转来的流留在流表里。
func TestForwardedAbortAfterBothEndsKeepsTail(t *testing.T) {
	for _, how := range []string{"peer-rst", "link-drop"} {
		t.Run(how, func(t *testing.T) {
			tr := &gatedTransport{gate: make(chan struct{})}
			tun := tunnel.New("t1", "0123456789abcdef0123456789abcdef", "0.2.0", tr, tunnel.DefaultConfig(), tunnel.Hooks{})
			tun.Start()
			t.Cleanup(func() { tun.Close(protocol.CloseNormal, "cleanup") })
			c := &internodeConn{streams: map[uint32]*fwdStream{}, wake: make(chan struct{}, 1)}
			fs := &fwdStream{c: c, id: 7, credit: protocol.StreamWindow, recvLeft: protocol.StreamWindow}
			s, err := tun.OpenStream(&gen.OpenPayload{Kind: "e2e", Method: "GET", Path: gen.E2EPath, Headers: [][2]string{}}, fs, nil)
			if err != nil {
				t.Fatal(err)
			}
			fs.s = s
			c.streams[fs.id] = fs

			tail := bytes.Repeat([]byte("tail"), 5000)
			if !s.Send(tail) { // 对端转来的上行字节（data 交给本地隧道）
				t.Fatal("信用够时 Send 应全部排进出队列")
			}
			if !c.receive(encode(protocol.Frame{Type: protocol.FrameEnd, StreamID: fs.id})) {
				t.Fatal("对端 END")
			}
			// 源发来下行字节（链路信用为 0，挂在持有节点上）与 END。
			tun.Receive(encode(protocol.Frame{Type: protocol.FrameData, StreamID: s.ID(), Data: []byte("down")}))
			tun.Receive(encode(protocol.Frame{Type: protocol.FrameEnd, StreamID: s.ID()}))
			if n := tun.StreamCount(); n != 0 {
				t.Fatalf("本地隧道上两端都已 END，流应已摘出流表，实际 %d", n)
			}
			c.mu.Lock()
			inTable := c.streams[fs.id] == fs
			c.mu.Unlock()
			if !inTable {
				t.Fatal("下行字节还挂着，转来的流应还在流表里")
			}
			if how == "peer-rst" {
				c.receive(encode(protocol.Frame{Type: protocol.FrameRst, StreamID: fs.id, Code: protocol.RstClientGone}))
			} else {
				c.resetAll()
			}
			close(tr.gate)
			deadline := time.Now().Add(3 * time.Second)
			for {
				if _, end, rst := tr.stream(s.ID()); end || rst {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("等待超时：源收不到 END 也收不到 RST")
				}
				time.Sleep(5 * time.Millisecond)
			}
			time.Sleep(20 * time.Millisecond)
			data, end, rst := tr.stream(s.ID())
			if rst || !end || !bytes.Equal(data, tail) {
				t.Fatalf("源应收到全部 %d 字节与 END、无 RST：收到 %d 字节，END=%v，RST=%v", len(tail), len(data), end, rst)
			}
		})
	}
}
