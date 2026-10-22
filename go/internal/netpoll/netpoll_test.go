package netpoll_test

import (
	"bytes"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/netpoll"
)

// hide 把 *net.TCPConn 包起来，让 Poller 走每连接协程的实现。
type hide struct{ net.Conn }

type recorder struct {
	mu     sync.Mutex
	read   []byte
	sent   int
	closed chan error
	echo   bool
	onRead func(c netpoll.Conn, p []byte)
}

func newRecorder() *recorder { return &recorder{closed: make(chan error, 1)} }

func (r *recorder) OnRead(c netpoll.Conn, p []byte) {
	r.mu.Lock()
	r.read = append(r.read, p...)
	echo, hook := r.echo, r.onRead
	r.mu.Unlock()
	if echo {
		c.Write(bytes.Clone(p), len(p))
	}
	if hook != nil {
		hook(c, p)
	}
}

func (r *recorder) OnSent(_ netpoll.Conn, ack int) {
	r.mu.Lock()
	r.sent += ack
	r.mu.Unlock()
}

func (r *recorder) OnClose(_ netpoll.Conn, err error) { r.closed <- err }

func (r *recorder) got() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return bytes.Clone(r.read)
}

func (r *recorder) acked() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sent
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}

// pair 建一对 TCP 连接：server 端交给 Poller，client 端交给测试。
func pair(t *testing.T, wrap bool, h netpoll.Handler) (netpoll.Conn, net.Conn, *netpoll.Poller) {
	t.Helper()
	p, err := netpoll.New(2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() { c, err := ln.Accept(); ch <- res{c, err} }()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	var nc net.Conn = r.c
	if wrap {
		nc = hide{r.c}
	}
	c, err := p.Adopt(nc, h)
	if err != nil {
		t.Fatal(err)
	}
	return c, client, p
}

func forBoth(t *testing.T, fn func(t *testing.T, wrap bool)) {
	t.Run("事件循环", func(t *testing.T) { fn(t, false) })
	t.Run("每连接协程", func(t *testing.T) { fn(t, true) })
}

func TestEcho(t *testing.T) {
	forBoth(t, func(t *testing.T, wrap bool) {
		h := newRecorder()
		h.echo = true
		_, client, p := pair(t, wrap, h)
		if p.Size() != 1 {
			t.Fatalf("连接数 %d", p.Size())
		}
		msg := bytes.Repeat([]byte("abc"), 1000)
		if _, err := client.Write(msg); err != nil {
			t.Fatal(err)
		}
		back := make([]byte, len(msg))
		if _, err := io.ReadFull(client, back); err != nil || !bytes.Equal(back, msg) {
			t.Fatalf("回显不一致：%v", err)
		}
		eventually(t, "OnSent 报告全部字节", func() bool { return h.acked() == len(msg) })
	})
}

func TestPauseResume(t *testing.T) {
	forBoth(t, func(t *testing.T, wrap bool) {
		h := newRecorder()
		c, client, _ := pair(t, wrap, h)
		c.Pause()
		time.Sleep(20 * time.Millisecond) // 让 Pause 生效
		// 每连接协程的读协程可能已阻塞在 Read 里，暂停后最多再读到一次；之后必须停住。
		_, _ = client.Write([]byte("a"))
		time.Sleep(100 * time.Millisecond)
		_, _ = client.Write([]byte("b"))
		time.Sleep(100 * time.Millisecond)
		if bytes.Contains(h.got(), []byte("b")) {
			t.Fatal("暂停期间不该读")
		}
		c.Resume()
		eventually(t, "恢复后读到", func() bool { return string(h.got()) == "ab" })
	})
}

func TestBackpressureWrite(t *testing.T) {
	forBoth(t, func(t *testing.T, wrap bool) {
		h := newRecorder()
		c, client, _ := pair(t, wrap, h)
		// 对端不读：写满内核缓冲后帧排队，OnSent 只报告真正写出的部分。
		frame := bytes.Repeat([]byte{7}, 256<<10)
		total := 0
		for i := 0; i < 64; i++ {
			if !c.Write(frame, len(frame)) {
				t.Fatal("连接未关，应接受")
			}
			total += len(frame)
		}
		time.Sleep(100 * time.Millisecond)
		if h.acked() >= total {
			t.Fatalf("对端不读，不该全部写出：%d/%d", h.acked(), total)
		}
		// 对端开始读，排队的帧陆续写出。
		go func() { _, _ = io.Copy(io.Discard, client) }()
		eventually(t, "全部写出", func() bool { return h.acked() == total })
	})
}

func TestCloseAfterFlush(t *testing.T) {
	forBoth(t, func(t *testing.T, wrap bool) {
		h := newRecorder()
		c, client, _ := pair(t, wrap, h)
		c.Write([]byte("bye"), 3)
		c.CloseAfterFlush(5 * time.Second)
		if c.Write([]byte("late"), 4) {
			t.Fatal("关闭中不该再接受写")
		}
		all, err := io.ReadAll(client)
		if err != nil || string(all) != "bye" {
			t.Fatalf("应先收到 bye 再被关闭：%q %v", all, err)
		}
		select {
		case err := <-h.closed:
			if err != nil {
				t.Fatalf("本地主动关闭 err 应为空：%v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("没收到 OnClose")
		}
	})
}

func TestCloseAfterFlushTimeout(t *testing.T) {
	forBoth(t, func(t *testing.T, wrap bool) {
		h := newRecorder()
		c, _, _ := pair(t, wrap, h)
		frame := bytes.Repeat([]byte{1}, 256<<10)
		total := 0
		for i := 0; i < 64; i++ {
			c.Write(frame, len(frame))
			total += len(frame)
		}
		c.CloseAfterFlush(100 * time.Millisecond)
		select {
		case <-h.closed:
		case <-time.After(5 * time.Second):
			t.Fatal("写不完应在超时后强关")
		}
		eventually(t, "丢弃的帧也报告 ack", func() bool { return h.acked() == total })
	})
}

func TestPeerClose(t *testing.T) {
	forBoth(t, func(t *testing.T, wrap bool) {
		h := newRecorder()
		_, client, p := pair(t, wrap, h)
		_ = client.Close()
		select {
		case err := <-h.closed:
			if err == nil {
				t.Fatal("对端关闭 err 不应为空")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("没收到 OnClose")
		}
		eventually(t, "连接数归零", func() bool { return p.Size() == 0 })
	})
}

func TestPeerClosePaused(t *testing.T) {
	forBoth(t, func(t *testing.T, wrap bool) {
		h := newRecorder()
		c, client, _ := pair(t, wrap, h)
		c.Pause()
		time.Sleep(20 * time.Millisecond)
		_ = client.Close()
		if wrap || !netpoll.Native() {
			c.Resume() // 协程实现的读协程停着，恢复后才发现对端关了
		}
		select {
		case <-h.closed:
		case <-time.After(5 * time.Second):
			t.Fatal("暂停中对端关闭也要通知")
		}
	})
}

func TestCloseOnce(t *testing.T) {
	forBoth(t, func(t *testing.T, wrap bool) {
		h := newRecorder()
		c, _, _ := pair(t, wrap, h)
		c.Write([]byte("x"), 1)
		c.Close()
		c.Close()
		<-h.closed
		select {
		case <-h.closed:
			t.Fatal("OnClose 只能一次")
		case <-time.After(50 * time.Millisecond):
		}
	})
}

func TestManyConns(t *testing.T) {
	p, err := netpoll.New(2)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	const n = 200
	hs := make([]*recorder, n)
	go func() {
		for i := 0; i < n; i++ {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			h := newRecorder()
			h.echo = true
			hs[i] = h
			if _, err := p.Adopt(c, h); err != nil {
				return
			}
		}
	}()
	clients := make([]net.Conn, n)
	for i := range clients {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		clients[i] = c
	}
	var wg sync.WaitGroup
	for i, c := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			msg := []byte{byte(i), 1, 2, 3}
			_, _ = c.Write(msg)
			back := make([]byte, 4)
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := io.ReadFull(c, back); err != nil || !bytes.Equal(back, msg) {
				t.Errorf("连接 %d 回显不对：%v", i, err)
			}
		}()
	}
	wg.Wait()
	if p.Size() != n {
		t.Errorf("连接数 %d，应为 %d", p.Size(), n)
	}
}
