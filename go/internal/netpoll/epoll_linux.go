//go:build linux

package netpoll

import (
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	evIn   = uint32(syscall.EPOLLIN | syscall.EPOLLRDHUP)
	evOut  = uint32(syscall.EPOLLOUT)
	evDead = uint32(syscall.EPOLLHUP | syscall.EPOLLERR)

	// batch 是一批最多处理的事件数，处理完让出 P：循环唤醒的隧道写协程才能及时跑（单核时尤其要紧）。
	batch = 64
	// scratchSize 是循环共用的读暂存区。
	scratchSize = 128 << 10
)

// loop 是一个 epoll 循环。
type loop struct {
	epfd    int
	file    *os.File // 持有 epoll fd；关闭只经它，免得 finalizer 重复关一个已被复用的 fd 号
	mu      sync.RWMutex
	conns   map[int32]*econn
	scratch []byte
	stopped atomic.Bool
}

func newLoop() (*loop, error) {
	epfd, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		return nil, err
	}
	// epoll fd 设成非阻塞，交给 Go 的 netpoller 等「可读」（嵌套 epoll）：等待时不占线程与 P。
	// 直接阻塞在 epoll_wait 上，单核时调度器要等 sysmon 才能把 P 让出去。
	if err := syscall.SetNonblock(epfd, true); err != nil {
		_ = syscall.Close(epfd)
		return nil, err
	}
	return &loop{epfd: epfd, file: os.NewFile(uintptr(epfd), "epoll"), conns: map[int32]*econn{}, scratch: make([]byte, scratchSize)}, nil
}

func (l *loop) stop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopped.Swap(true) {
		return
	}
	_ = l.file.Close()
}

// ctl 是带停止保护的 epoll_ctl：stop 之后 epfd 号可能被别的文件复用，不能再对它下手。
func (l *loop) ctl(op int, fd int, ev *syscall.EpollEvent) error {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.stopped.Load() {
		return syscall.EBADF
	}
	return syscall.EpollCtl(l.epfd, op, fd, ev)
}

func (l *loop) add(c *econn) error {
	l.mu.Lock()
	l.conns[int32(c.fd)] = c
	l.mu.Unlock()
	c.mu.Lock()
	c.armed = evIn
	err := l.ctl(syscall.EPOLL_CTL_ADD, c.fd, &syscall.EpollEvent{Events: evIn, Fd: int32(c.fd)})
	c.mu.Unlock()
	if err != nil {
		l.mu.Lock()
		delete(l.conns, int32(c.fd))
		l.mu.Unlock()
	}
	return err
}

func (l *loop) remove(c *econn) {
	_ = l.ctl(syscall.EPOLL_CTL_DEL, c.fd, nil)
	l.mu.Lock()
	if l.conns[int32(c.fd)] == c {
		delete(l.conns, int32(c.fd))
	}
	l.mu.Unlock()
}

func (l *loop) run() {
	rc, err := l.file.SyscallConn()
	if err != nil {
		return
	}
	events := make([]syscall.EpollEvent, batch)
	for !l.stopped.Load() {
		err := rc.Read(func(uintptr) bool {
			n, err := syscall.EpollWait(l.epfd, events, 0)
			if err != nil || n <= 0 {
				return err == syscall.EINTR // 没有事件：交给 netpoller 等
			}
			l.dispatch(events[:n])
			return true
		})
		if err != nil {
			return
		}
		runtime.Gosched()
	}
}

func (l *loop) dispatch(events []syscall.EpollEvent) {
	for _, ev := range events {
		l.mu.RLock()
		c := l.conns[ev.Fd]
		l.mu.RUnlock()
		if c == nil {
			continue
		}
		if ev.Events&evOut != 0 {
			c.flush()
		}
		if ev.Events&(evIn|evDead) != 0 {
			c.onReadable(l.scratch, ev.Events)
		}
	}
}

// econn 是 epoll 上的一条连接。
type econn struct {
	fd    int
	nc    net.Conn // 持有它，fd 才不会被回收
	l     *loop
	h     Handler
	count *atomic.Int64

	mu      sync.Mutex
	closed  bool
	closing bool
	paused  bool
	armed   uint32
	out     [][]byte
	ack     []int
	outOff  int
	timer   *time.Timer
}

func (c *econn) RemoteAddr() string { return c.nc.RemoteAddr().String() }

func (c *econn) rearmLocked() {
	if c.closed {
		return
	}
	var want uint32
	if c.paused {
		// 停读时仍关注对端关闭，免得死连接一直占着名额。
		want |= uint32(syscall.EPOLLRDHUP)
	} else {
		want |= evIn
	}
	if len(c.out) > 0 {
		want |= evOut
	}
	if want == c.armed {
		return
	}
	c.armed = want
	_ = c.l.ctl(syscall.EPOLL_CTL_MOD, c.fd, &syscall.EpollEvent{Events: want, Fd: int32(c.fd)})
}

// Write 实现 Conn：队列空时先直接写，写不完的排队并关注 EPOLLOUT。
func (c *econn) Write(b []byte, ack int) bool {
	c.mu.Lock()
	if c.closed || c.closing {
		c.mu.Unlock()
		return false
	}
	sent, broken := c.queueLocked(b, ack)
	c.mu.Unlock()
	if broken {
		// 写坏了：帧已被接受，经 OnSent 报告 ack 再关连接。
		if ack > 0 {
			c.h.OnSent(c, ack)
		}
		c.closeNow(io.ErrClosedPipe)
		return true
	}
	if sent && ack > 0 {
		c.h.OnSent(c, ack)
	}
	return true
}

// queueLocked 答是否已经全部写进内核、写是否坏了。
func (c *econn) queueLocked(b []byte, ack int) (sent, broken bool) {
	if len(c.out) == 0 {
		n, err := syscall.Write(c.fd, b)
		if n == len(b) {
			return true, false
		}
		if err != nil && err != syscall.EAGAIN && err != syscall.EINTR {
			return false, true
		}
		c.outOff = max(n, 0)
	}
	c.out = append(c.out, b)
	c.ack = append(c.ack, ack)
	c.rearmLocked()
	return false, false
}

func (c *econn) flush() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	acked, broken := 0, false
	for len(c.out) > 0 {
		f := c.out[0]
		n, err := syscall.Write(c.fd, f[c.outOff:])
		if n > 0 {
			c.outOff += n
		}
		if c.outOff == len(f) {
			acked += c.ack[0]
			c.out[0] = nil
			c.out, c.ack, c.outOff = c.out[1:], c.ack[1:], 0
			continue
		}
		if err != nil && err != syscall.EAGAIN && err != syscall.EINTR {
			broken = true
		}
		break
	}
	if len(c.out) == 0 {
		c.out, c.ack = nil, nil
	}
	done := c.closing && len(c.out) == 0
	c.rearmLocked()
	c.mu.Unlock()
	if acked > 0 {
		c.h.OnSent(c, acked)
	}
	if broken {
		c.closeNow(io.ErrClosedPipe)
	} else if done {
		c.closeNow(nil)
	}
}

func (c *econn) Pause() {
	c.mu.Lock()
	c.paused = true
	c.rearmLocked()
	c.mu.Unlock()
}

func (c *econn) Resume() {
	c.mu.Lock()
	c.paused = false
	c.rearmLocked()
	c.mu.Unlock()
}

func (c *econn) Close() { c.closeNow(nil) }

func (c *econn) CloseAfterFlush(timeout time.Duration) {
	c.mu.Lock()
	if c.closed || c.closing {
		c.mu.Unlock()
		return
	}
	c.closing = true
	empty := len(c.out) == 0
	if !empty {
		c.timer = time.AfterFunc(timeout, func() { c.closeNow(nil) })
	}
	c.mu.Unlock()
	if empty {
		c.closeNow(nil)
	}
}

func (c *econn) onReadable(buf []byte, evs uint32) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	if c.paused {
		c.mu.Unlock()
		if evs&(evDead|uint32(syscall.EPOLLRDHUP)) != 0 {
			c.closeNow(io.ErrUnexpectedEOF)
		}
		return
	}
	n, err := syscall.Read(c.fd, buf)
	c.mu.Unlock()
	if n > 0 {
		c.h.OnRead(c, buf[:n])
		return
	}
	if err == syscall.EAGAIN || err == syscall.EINTR {
		return
	}
	if err == nil {
		err = io.ErrUnexpectedEOF // 对端关闭
	}
	c.closeNow(err)
}

func (c *econn) closeNow(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	dropped := 0
	for _, a := range c.ack {
		dropped += a
	}
	c.out, c.ack = nil, nil
	if c.timer != nil {
		c.timer.Stop()
	}
	c.l.remove(c)
	_ = c.nc.Close()
	c.mu.Unlock()
	c.count.Add(-1)
	if dropped > 0 {
		c.h.OnSent(c, dropped)
	}
	c.h.OnClose(c, err)
}
