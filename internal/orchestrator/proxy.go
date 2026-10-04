package orchestrator

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/theognis1002/bursty/internal/api"
	"github.com/theognis1002/bursty/internal/netx"
)

// proxy forwards published host ports to the VM (a toy version of an edge proxy).
type proxy struct {
	lns   []net.Listener
	wg    sync.WaitGroup
	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

func startProxy(addr string, ports []api.Port, lease netx.Lease) (*proxy, error) {
	p := &proxy{conns: map[net.Conn]struct{}{}}
	for _, port := range ports {
		ln, err := net.Listen("tcp", net.JoinHostPort(addr, strconv.Itoa(port.Host)))
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("publish port %d: %w", port.Host, err)
		}
		p.lns = append(p.lns, ln)
		target := lease.Addr(port.Guest)
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			for {
				c, err := ln.Accept()
				if err != nil {
					if !errors.Is(err, net.ErrClosed) {
						slog.Warn("proxy accept", "err", err)
					}
					return
				}
				go p.forward(c, target)
			}
		}()
	}
	return p, nil
}

func (p *proxy) forward(c net.Conn, target string) {
	if !p.track(c) {
		return
	}
	defer p.untrack(c)
	up, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		slog.Warn("proxy dial", "target", target, "err", err)
		return
	}
	if !p.track(up) {
		return
	}
	defer p.untrack(up)
	// Pass half-closes through, so a client that shuts down its write side still gets
	// the whole response.
	var wg sync.WaitGroup
	pipe := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}
	wg.Add(2)
	go pipe(up, c)
	go pipe(c, up)
	wg.Wait()
}

// track registers a live connection; false (and closed) if the proxy is shutting down.
func (p *proxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conns == nil {
		c.Close()
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

func (p *proxy) untrack(c net.Conn) {
	c.Close()
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
}

// Close stops listening and cuts every forwarded connection.
func (p *proxy) Close() {
	for _, ln := range p.lns {
		_ = ln.Close()
	}
	p.wg.Wait()
	p.mu.Lock()
	for c := range p.conns {
		c.Close()
	}
	p.conns = nil
	p.mu.Unlock()
}
