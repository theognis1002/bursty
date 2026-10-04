// Package console tees a VM's serial output into a log file and watches for the
// readiness line the guest init prints once userspace is up.
package console

import (
	"bufio"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
)

// ReadyMarker is printed by bursty-init (and the sim guest) as "BURSTY_READY uptime=<seconds>".
const ReadyMarker = "BURSTY_READY"

type Console struct {
	ready     chan struct{}
	done      chan struct{}
	once      sync.Once
	mu        sync.Mutex
	uptimeSec float64
	armed     bool
}

// Start copies r into the file at path until r hits EOF. Only lines seen after Arm count
// as ready, so a warm process's startup chatter never satisfies a later boot.
func Start(r io.Reader, path string) (*Console, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	c := &Console{ready: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(c.done)
		defer f.Close()
		if rc, ok := r.(io.Closer); ok {
			defer rc.Close()
		}
		br := bufio.NewReaderSize(r, 64<<10)
		for {
			line, err := br.ReadString('\n')
			if len(line) > 0 {
				_, _ = f.WriteString(line)
				c.scan(line)
			}
			if err != nil {
				return
			}
		}
	}()
	return c, nil
}

func (c *Console) Arm() {
	c.mu.Lock()
	c.armed = true
	c.mu.Unlock()
}

func (c *Console) scan(line string) {
	i := strings.Index(line, ReadyMarker)
	if i < 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.armed {
		return
	}
	for _, f := range strings.Fields(line[i+len(ReadyMarker):]) {
		if v, ok := strings.CutPrefix(f, "uptime="); ok {
			c.uptimeSec, _ = strconv.ParseFloat(v, 64)
		}
	}
	c.once.Do(func() { close(c.ready) })
}

// Ready closes when the guest reports ready.
func (c *Console) Ready() <-chan struct{} { return c.ready }

// Done closes when the output stream ends (the VMM exited).
func (c *Console) Done() <-chan struct{} { return c.done }

// GuestUptime is the guest kernel uptime (seconds) it reported in the ready line.
func (c *Console) GuestUptime() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.uptimeSec
}
