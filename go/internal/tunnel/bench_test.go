package tunnel_test

import (
	"encoding/binary"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/protocol"
	"github.com/AMA-Link/armadra-cloud/go/internal/tunnel"
)

// rateTransport 是限速的传输层：每帧按链路速率睡一会儿，并在探测流的 DATA 写出时记下时刻。
type rateTransport struct {
	nsPerByte float64
	probeID   atomic.Uint32
	probeAt   chan time.Time
	written   atomic.Int64
	// tun 非空时模拟对端：收到的 DATA 攒够 256 KiB 就回 WINDOW（流级与隧道级），大流量流才能一直写。
	tun    *tunnel.Tunnel
	unack  map[uint32]int
	linkUn int
}

func (r *rateTransport) ack(b []byte) {
	if r.tun == nil || protocol.FrameType(b[0]) != protocol.FrameData {
		return
	}
	id, n := binary.BigEndian.Uint32(b[1:5]), len(b)-protocol.FrameHeaderBytes
	if r.unack == nil {
		r.unack = map[uint32]int{}
	}
	r.unack[id] += n
	r.linkUn += n
	grant := func(id uint32, credit int) {
		f, _ := protocol.Encode(protocol.Frame{Type: protocol.FrameWindow, StreamID: id, Credit: uint32(credit)})
		r.tun.Receive(f)
	}
	if r.unack[id] >= 256<<10 {
		grant(id, r.unack[id])
		r.unack[id] = 0
	}
	if r.linkUn >= 256<<10 {
		grant(0, r.linkUn)
		r.linkUn = 0
	}
}

func (r *rateTransport) Send(b []byte) error {
	if r.nsPerByte > 0 {
		time.Sleep(time.Duration(float64(len(b)) * r.nsPerByte))
	}
	r.written.Add(int64(len(b)))
	r.ack(b)
	if protocol.FrameType(b[0]) == protocol.FrameData && binary.BigEndian.Uint32(b[1:5]) == r.probeID.Load() {
		select {
		case r.probeAt <- time.Now():
		default:
		}
	}
	return nil
}

func (r *rateTransport) Close(protocol.CloseCode, string) {}
func (r *rateTransport) Terminate()                       {}

// BenchmarkProbeLatencyUnderBulk 度量限速链路（200 MB/s）上一条交互流（每 5 毫秒一个 1 KiB 的小写入）
// 的出帧延迟：只有探测流、同一条隧道上有 N 条大流量流在持续满速写。输出 p50 / p99（毫秒）。
//
//	go test ./internal/tunnel -run '^$' -bench ProbeLatency -benchtime 200x
func BenchmarkProbeLatencyUnderBulk(b *testing.B) {
	for _, bulk := range []int{0, 4, 32} {
		b.Run(fmt.Sprintf("bulk=%d", bulk), func(b *testing.B) {
			cfg := tunnel.DefaultConfig()
			cfg.Heartbeat = time.Hour
			cfg.MaxStreams = 4096
			cfg.StreamWindow = 4 << 20
			cfg.TunnelWindow = 1 << 30
			tr := &rateTransport{nsPerByte: 5, probeAt: make(chan time.Time, 1)} // 200 MB/s
			tun := tunnel.New("t", "0123456789abcdef0123456789abcdef", "0.2.0", tr, cfg, tunnel.Hooks{})
			tr.tun = tun
			tun.Start()
			defer tun.Close(protocol.CloseNormal, "done")
			open := func() *tunnel.Stream {
				s, err := tun.OpenStream(openPayload(), &recSink{}, nil)
				if err != nil {
					b.Fatal(err)
				}
				return s
			}
			probe := open()
			tr.probeID.Store(probe.ID())
			stop := make(chan struct{})
			var wg sync.WaitGroup
			for i := 0; i < bulk; i++ {
				s := open()
				wg.Add(1)
				go func() { // 持续补积压，写端始终有活干
					defer wg.Done()
					buf := make([]byte, 1<<20)
					for {
						select {
						case <-stop:
							return
						default:
						}
						if !s.Parked() { // 积压没发完就不再补，免得内存无界增长
							s.Send(buf)
						}
						time.Sleep(200 * time.Microsecond)
					}
				}()
			}
			time.Sleep(100 * time.Millisecond)
			lat := make([]float64, 0, b.N)
			payload := make([]byte, 1024)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				time.Sleep(5 * time.Millisecond)
				start := time.Now()
				probe.Send(payload)
				select {
				case at := <-tr.probeAt:
					lat = append(lat, float64(at.Sub(start))/1e6)
				case <-time.After(10 * time.Second):
					b.Fatal("探测帧一直没写出")
				}
				probe.Consumed(0)
			}
			b.StopTimer()
			close(stop)
			wg.Wait()
			sort.Float64s(lat)
			b.ReportMetric(lat[len(lat)/2], "p50_ms")
			b.ReportMetric(lat[len(lat)*99/100], "p99_ms")
		})
	}
}

// BenchmarkBulkThroughput 度量调度器对大流量吞吐的影响：N 条大流量流共用一条不限速的隧道，
// 每秒写出多少 MB。与改动前对照（git stash 掉调度器）应无明显差别。
func BenchmarkBulkThroughput(b *testing.B) {
	for _, bulk := range []int{1, 8} {
		b.Run(fmt.Sprintf("bulk=%d", bulk), func(b *testing.B) {
			cfg := tunnel.DefaultConfig()
			cfg.Heartbeat = time.Hour
			cfg.StreamWindow = 4 << 20
			cfg.TunnelWindow = 1 << 30
			tr := &rateTransport{probeAt: make(chan time.Time, 1)}
			tun := tunnel.New("t", "0123456789abcdef0123456789abcdef", "0.2.0", tr, cfg, tunnel.Hooks{})
			tr.tun = tun
			tun.Start()
			defer tun.Close(protocol.CloseNormal, "done")
			streams := make([]*tunnel.Stream, bulk)
			for i := range streams {
				s, err := tun.OpenStream(openPayload(), &recSink{}, nil)
				if err != nil {
					b.Fatal(err)
				}
				streams[i] = s
			}
			buf := make([]byte, 1<<20)
			b.SetBytes(int64(len(buf)) * int64(bulk))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for _, s := range streams {
					s.Send(buf)
				}
				for tr.written.Load() < int64(i+1)*int64(len(buf))*int64(bulk) {
					time.Sleep(50 * time.Microsecond)
				}
			}
		})
	}
}
