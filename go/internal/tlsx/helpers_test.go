package tlsx

import (
	"net"
	"strconv"
	"testing"
)

func itoa(n int) string { return strconv.Itoa(n) }

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
