//go:build !linux

package netpoll

import (
	"net"
	"sync/atomic"
)

const native = false

type goPoller struct{ count atomic.Int64 }

func newPoller(int) (poller, error) { return &goPoller{}, nil }

func (p *goPoller) adopt(nc net.Conn, h Handler) (Conn, error) {
	return newGConn(nc, h, &p.count), nil
}

func (p *goPoller) close()    {}
func (p *goPoller) size() int { return int(p.count.Load()) }
