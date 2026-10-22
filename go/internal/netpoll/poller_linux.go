//go:build linux

package netpoll

import (
	"net"
	"runtime"
	"sync/atomic"
)

const native = true

type linuxPoller struct {
	loops []*loop
	next  atomic.Uint32
	count atomic.Int64
	shut  atomic.Bool
}

func newPoller(n int) (poller, error) {
	if n < 1 {
		n = runtime.GOMAXPROCS(0)
	}
	p := &linuxPoller{}
	for i := 0; i < n; i++ {
		l, err := newLoop()
		if err != nil {
			p.close()
			return nil, err
		}
		p.loops = append(p.loops, l)
		go l.run()
	}
	return p, nil
}

func (p *linuxPoller) adopt(nc net.Conn, h Handler) (Conn, error) {
	if p.shut.Load() {
		return nil, ErrClosed
	}
	tcp, ok := nc.(*net.TCPConn)
	if !ok {
		return newGConn(nc, h, &p.count), nil
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return newGConn(nc, h, &p.count), nil
	}
	fd := -1
	if err := raw.Control(func(f uintptr) { fd = int(f) }); err != nil || fd < 0 {
		return newGConn(nc, h, &p.count), nil
	}
	l := p.loops[int(p.next.Add(1))%len(p.loops)]
	c := &econn{fd: fd, nc: nc, l: l, h: h, count: &p.count}
	if err := l.add(c); err != nil {
		if p.shut.Load() {
			return nil, ErrClosed
		}
		// epoll 不可用：退回每连接协程。
		return newGConn(nc, h, &p.count), nil
	}
	p.count.Add(1)
	return c, nil
}

func (p *linuxPoller) close() {
	p.shut.Store(true)
	for _, l := range p.loops {
		l.stop()
	}
}

func (p *linuxPoller) size() int { return int(p.count.Load()) }
