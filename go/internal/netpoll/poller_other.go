//go:build !linux

package netpoll

import (
	"net"
	"sync/atomic"
)

const native = false

type goPoller struct {
	count atomic.Int64
	shut  atomic.Bool
}

func newPoller(int) (poller, error) { return &goPoller{}, nil }

func (p *goPoller) adopt(nc net.Conn, h Handler) (Conn, error) {
	if p.shut.Load() {
		return nil, ErrClosed
	}
	return newGConn(nc, h, &p.count), nil
}

func (p *goPoller) close()    { p.shut.Store(true) }
func (p *goPoller) size() int { return int(p.count.Load()) }
