package netpoll

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const gReadBuf = 32 << 10

// gconn 是每连接两个协程的实现：非 Linux 平台与非 TCP 连接用。
type gconn struct {
	nc net.Conn
	h  Handler

	mu      sync.Mutex
	wake    *sync.Cond // 写协程等队列
	rwake   *sync.Cond // 读协程等恢复
	out     [][]byte
	acks    []int
	paused  bool
	closing bool
	closed  bool
	timer   *time.Timer
	once    sync.Once
	err     error
	count   *atomic.Int64
}

func newGConn(nc net.Conn, h Handler, count *atomic.Int64) *gconn {
	c := &gconn{nc: nc, h: h, count: count}
	c.wake = sync.NewCond(&c.mu)
	c.rwake = sync.NewCond(&c.mu)
	count.Add(1)
	go c.readLoop()
	go c.writeLoop()
	return c
}

func (c *gconn) RemoteAddr() string { return c.nc.RemoteAddr().String() }

func (c *gconn) readLoop() {
	buf := make([]byte, gReadBuf)
	for {
		c.mu.Lock()
		for c.paused && !c.closed {
			c.rwake.Wait()
		}
		closed := c.closed
		c.mu.Unlock()
		if closed {
			return
		}
		n, err := c.nc.Read(buf)
		if n > 0 {
			c.h.OnRead(c, buf[:n])
		}
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			c.shutdown(err)
			return
		}
	}
}

func (c *gconn) writeLoop() {
	for {
		c.mu.Lock()
		for len(c.out) == 0 && !c.closed {
			if c.closing {
				c.mu.Unlock()
				c.shutdown(nil)
				return
			}
			c.wake.Wait()
		}
		if c.closed {
			c.mu.Unlock()
			return
		}
		frame, ack := c.out[0], c.acks[0]
		c.mu.Unlock()
		_, err := c.nc.Write(frame)
		if err != nil {
			c.shutdown(err)
			return
		}
		c.mu.Lock()
		if !c.closed && len(c.out) > 0 {
			c.out[0] = nil
			c.out, c.acks = c.out[1:], c.acks[1:]
		}
		c.mu.Unlock()
		c.h.OnSent(c, ack)
	}
}

func (c *gconn) Write(b []byte, ack int) bool {
	c.mu.Lock()
	if c.closed || c.closing {
		c.mu.Unlock()
		return false
	}
	c.out = append(c.out, b)
	c.acks = append(c.acks, ack)
	c.wake.Signal()
	c.mu.Unlock()
	return true
}

func (c *gconn) Pause() {
	c.mu.Lock()
	c.paused = true
	c.mu.Unlock()
}

func (c *gconn) Resume() {
	c.mu.Lock()
	c.paused = false
	c.rwake.Signal()
	c.mu.Unlock()
}

func (c *gconn) Close() { c.shutdown(nil) }

func (c *gconn) CloseAfterFlush(timeout time.Duration) {
	c.mu.Lock()
	if c.closed || c.closing {
		c.mu.Unlock()
		return
	}
	c.closing = true
	empty := len(c.out) == 0
	if !empty {
		c.timer = time.AfterFunc(timeout, func() { c.shutdown(nil) })
	}
	c.wake.Signal()
	c.mu.Unlock()
	if empty {
		c.shutdown(nil)
	}
}

// shutdown 关连接：丢掉没写出的帧并经 OnSent 报告，再通知 OnClose（恰好一次）。
func (c *gconn) shutdown(err error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.closed = true
		dropped := 0
		for _, a := range c.acks {
			dropped += a
		}
		c.out, c.acks = nil, nil
		if c.timer != nil {
			c.timer.Stop()
		}
		c.err = err
		c.wake.Broadcast()
		c.rwake.Broadcast()
		c.mu.Unlock()
		_ = c.nc.Close()
		c.count.Add(-1)
		if dropped > 0 {
			c.h.OnSent(c, dropped)
		}
		c.h.OnClose(c, err)
	})
}
