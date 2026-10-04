package orchestrator

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/theognis1002/bursty/internal/api"
	"github.com/theognis1002/bursty/internal/netx"
)

// A client that half-closes after its request must still get the full response, and
// Close must cut connections that are still open.
func TestProxyHalfCloseAndClose(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() {
		for {
			c, err := backend.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				req, _ := io.ReadAll(c) // until the client's half-close
				_, _ = c.Write(append([]byte("echo:"), req...))
			}()
		}
	}()
	front, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := front.Addr().(*net.TCPAddr).Port
	front.Close()
	lease := netx.Lease{PortBase: backend.Addr().(*net.TCPAddr).Port}
	p, err := startProxy("127.0.0.1", []api.Port{{Host: port, Guest: 8080}}, lease)
	if err != nil {
		t.Fatal(err)
	}

	c, err := net.Dial("tcp", front.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Write([]byte("hi"))
	_ = c.(*net.TCPConn).CloseWrite()
	got, _ := io.ReadAll(c)
	c.Close()
	if string(got) != "echo:hi" {
		t.Fatalf("got %q", got)
	}

	idle, err := net.Dial("tcp", front.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = idle.Write([]byte("x")) // make sure the proxy has accepted it
	p.Close()
	// EOF or a reset both mean the proxy cut the connection; only a timeout is a failure.
	_ = idle.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = io.ReadAll(idle)
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("Close left a forwarded connection open")
	}
	idle.Close()
}
