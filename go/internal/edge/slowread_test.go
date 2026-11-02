package edge

import (
	"net"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/netpoll"
	"github.com/AMA-Link/armadra-cloud/go/internal/obs"
	"github.com/AMA-Link/armadra-cloud/go/internal/testutil/relayenv"
)

// tcpPair 建一对 TCP 连接；small 为真时把缓冲压到很小：对端不读时写很快就排队。
// 读得动的一方不要压：Linux 上极小的接收缓冲会让对端写入长时间停滞。
func tcpPair(t *testing.T, small bool) (server, client net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ch := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); ch <- c }()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server = <-ch
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	if small {
		_ = server.(*net.TCPConn).SetWriteBuffer(4096)
		_ = client.(*net.TCPConn).SetReadBuffer(4096)
	}
	return server, client
}

// 慢读客户端（#191）：已认证的客户端不读 socket、每个 ping 轮发满限额的 PING，PONG 在写队列里
// 越攒越多。写队列必须有上限：超限就断开，记 warn 与指标。
func TestSlowReaderPingFloodIsDropped(t *testing.T) {
	env := relayenv.New(t, relayenv.Options{})
	metrics := obs.NewMetrics(func() (int, int) { return 0, 0 })
	e := &Edge{
		o:      Options{Log: obs.Nop(), Metrics: metrics, WSPing: time.Second, WSIdle: time.Hour},
		policy: NewPolicy(PolicyOptions{Control: env.Control, Metrics: metrics}),
		wheel:  newWheel(),
	}
	poller, err := netpoll.New(1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(poller.Close)
	server, _ := tcpPair(t, true)
	ec := &edgeConn{e: e, sourceID: env.SourceID, subject: "acct", started: time.Now()}
	ec.lastSeen.Store(time.Now().UnixMilli())
	e.wheel.add(ec)
	c, err := poller.Adopt(server, ec)
	if err != nil {
		t.Fatal(err)
	}
	ec.bind(c)

	ping := clientFrameBytes(opPing, true, make([]byte, 125)) // 最大的 PING，PONG 回同样的载荷
	burst := make([]byte, 0, maxPingsPerRound*len(ping))
	for i := 0; i < maxPingsPerRound; i++ {
		burst = append(burst, ping...)
	}
	// 每轮满限额；上限是 5000 轮（约 80 MiB 的 PONG），没有写队列上限就不会断。
	rounds := 0
	for ; rounds < 5000 && !ec.isFinished(); rounds++ {
		ec.OnRead(c, burst)
		ec.pings.Store(0) // 新的 ping 轮（tick 做的事）
	}
	deadline := time.Now().Add(5 * time.Second)
	for !ec.isFinished() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !ec.isFinished() {
		t.Fatalf("慢读客户端发了 %d 轮 PING 还没被断开：写队列无上限", rounds)
	}
	if got := metrics.Snapshot().EdgeDropped[dropWriteQueueOverflow]; got != 1 {
		t.Fatalf("指标 write_queue_overflow = %d，应为 1", got)
	}
	if tracked, wheel := e.ConnCounts(); tracked != 0 || wheel != 0 {
		t.Fatalf("断开后登记没摘干净：%d %d", tracked, wheel)
	}
}

// 读得动的客户端不受影响：队列写完额度就还回来，PING 一轮接一轮也不会被断开。
func TestFastReaderPingFloodNotDropped(t *testing.T) {
	env := relayenv.New(t, relayenv.Options{})
	metrics := obs.NewMetrics(func() (int, int) { return 0, 0 })
	e := &Edge{
		o:      Options{Log: obs.Nop(), Metrics: metrics, WSPing: time.Second, WSIdle: time.Hour},
		policy: NewPolicy(PolicyOptions{Control: env.Control, Metrics: metrics}),
		wheel:  newWheel(),
	}
	poller, err := netpoll.New(1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(poller.Close)
	server, client := tcpPair(t, false)
	ec := &edgeConn{e: e, sourceID: env.SourceID, subject: "acct", started: time.Now()}
	ec.lastSeen.Store(time.Now().UnixMilli())
	e.wheel.add(ec)
	c, err := poller.Adopt(server, ec)
	if err != nil {
		t.Fatal(err)
	}
	ec.bind(c)
	go func() {
		buf := make([]byte, 64<<10)
		for {
			if _, err := client.Read(buf); err != nil {
				return
			}
		}
	}()
	ping := clientFrameBytes(opPing, true, make([]byte, 125))
	for round := 0; round < 150; round++ {
		for i := 0; i < 8; i++ {
			ec.OnRead(c, ping)
		}
		ec.pings.Store(0)
		time.Sleep(2 * time.Millisecond) // 让读协程把对端缓冲读走
	}
	if ec.isFinished() {
		t.Fatal("读得动的客户端被断开了")
	}
	if got := metrics.Snapshot().EdgeDropped[dropWriteQueueOverflow]; got != 0 {
		t.Fatalf("指标 write_queue_overflow = %d，应为 0", got)
	}
}
