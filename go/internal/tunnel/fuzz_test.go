package tunnel_test

import (
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/tunnel"
)

// FuzzReceive：任意字节当作一条二进制消息（或切成几条）交给隧道，不得 panic、死锁，
// 也不得让信用记账出现负数（负数会让后续合法帧被误判为流控违规）。
func FuzzReceive(f *testing.F) {
	for _, name := range []string{
		"tunnel/data-64k.bin", "tunnel/end.bin", "tunnel/goaway.bin", "tunnel/open-http.bin", "tunnel/ping.bin",
		"tunnel/pong.bin", "tunnel/rst-3.bin", "tunnel/window-stream.bin", "tunnel/window-tunnel.bin",
	} {
		f.Add(fixtureBytesTB(f, name), uint8(0))
	}
	f.Add([]byte{byte(protocol.FrameData), 0, 0, 0, 1, 1, 2, 3}, uint8(2))
	f.Add([]byte{byte(protocol.FrameWindow), 0, 0, 0, 0, 0xff, 0xff, 0xff, 0xff}, uint8(0))
	f.Fuzz(func(t *testing.T, data []byte, cut uint8) {
		tr := &fakeTransport{}
		cfg := testConfig(func(c *tunnel.Config) {
			c.StreamWindow = 100000
			c.TunnelWindow = 400000
			c.GoawayGrace = 20 * time.Millisecond
		})
		tun := tunnel.New("t", "0123456789abcdef0123456789abcdef", "0", tr, cfg, tunnel.Hooks{})
		tun.Start()
		defer tun.Close(protocol.CloseNormal, "done")
		sinks := []*recSink{{autoConsume: true}, {}}
		for _, s := range sinks {
			if _, err := tun.OpenStream(openPayload(), s, nil); err != nil {
				t.Fatal(err)
			}
		}
		if cut == 0 {
			tun.Receive(data)
		} else {
			step := int(cut)%32 + 1
			for off := 0; off < len(data); off += step {
				tun.Receive(data[off:min(off+step, len(data))])
			}
		}
		tun.StreamCount()
		tun.State()
	})
}
