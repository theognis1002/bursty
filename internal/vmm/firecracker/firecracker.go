// Package firecracker drives real Firecracker microVMs through Firecracker's REST API
// on a per-VM unix socket. Needs Linux with /dev/kvm.
package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/theognis1002/bursty/internal/console"
	"github.com/theognis1002/bursty/internal/guest"
	"github.com/theognis1002/bursty/internal/netx"
	"github.com/theognis1002/bursty/internal/vmm"
)

type Driver struct {
	Bin string
}

func New(bin string) (*Driver, error) {
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("firecracker binary: %w", err)
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return nil, fmt.Errorf("firecracker needs /dev/kvm: %w", err)
	}
	return &Driver{Bin: path}, nil
}

func (d *Driver) Name() string { return "firecracker" }

type machine struct {
	dir     string
	cmd     *exec.Cmd
	sock    string
	client  *http.Client
	console *console.Console
	exited  chan struct{}
	killMu  sync.Mutex
}

func (d *Driver) Spawn(ctx context.Context, spec vmm.SpawnSpec) (vmm.Machine, error) {
	sock := filepath.Join(spec.Dir, "fc.sock")
	_ = os.Remove(sock)
	args := []string{d.Bin, "--api-sock", sock, "--id", spec.ID}
	if spec.Lease.Netns != "" {
		args = append([]string{"ip", "netns", "exec", spec.Lease.Netns}, args...)
	}
	// Not CommandContext: the process outlives the request that spawned it.
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = spec.Dir
	cmd.SysProcAttr = sysProcAttr()
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return nil, fmt.Errorf("start firecracker: %w", err)
	}
	pw.Close()
	con, err := console.Start(pr, filepath.Join(spec.Dir, "console.log"))
	if err != nil {
		pr.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	m := &machine{
		dir:     spec.Dir,
		cmd:     cmd,
		sock:    sock,
		console: con,
		exited:  make(chan struct{}),
		// Snapshotting a large VM to slow disk is the longest call; nothing should exceed this.
		client: &http.Client{Timeout: 2 * time.Minute, Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		}},
	}
	go func() {
		_ = cmd.Wait()
		close(m.exited)
	}()
	if err := m.waitSocket(ctx); err != nil {
		_ = m.Kill()
		return nil, err
	}
	return m, nil
}

func (m *machine) waitSocket(ctx context.Context) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.Dial("unix", m.sock)
		if err == nil {
			c.Close()
			return nil
		}
		select {
		case <-m.exited:
			return fmt.Errorf("firecracker exited during startup (see console.log)")
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("firecracker API socket not ready: %w", err)
		}
		time.Sleep(500 * time.Microsecond)
	}
}

func (m *machine) PID() int                { return m.cmd.Process.Pid }
func (m *machine) Ready() <-chan struct{}  { return m.console.Ready() }
func (m *machine) GuestUptime() float64    { return m.console.GuestUptime() }
func (m *machine) Exited() <-chan struct{} { return m.exited }

func (m *machine) Kill() error {
	m.killMu.Lock()
	defer m.killMu.Unlock()
	select {
	case <-m.exited:
		return nil
	default:
	}
	_ = m.cmd.Process.Kill()
	select {
	case <-m.exited:
	case <-time.After(5 * time.Second):
		return errors.New("firecracker did not exit after SIGKILL")
	}
	return nil
}

func (m *machine) Boot(ctx context.Context, cfg vmm.BootConfig) error {
	steps := []struct {
		method, path string
		body         any
	}{
		{"PUT", "/machine-config", map[string]any{"vcpu_count": cfg.VCPUs, "mem_size_mib": cfg.MemoryMB}},
		{"PUT", "/boot-source", map[string]any{"kernel_image_path": cfg.Kernel, "boot_args": cfg.BootArgs}},
		{"PUT", "/drives/rootfs", map[string]any{
			"drive_id": "rootfs", "path_on_host": vmm.RootfsName,
			"is_root_device": true, "is_read_only": cfg.ReadOnly,
		}},
		{"PUT", "/network-interfaces/eth0", map[string]any{
			"iface_id": "eth0", "host_dev_name": netx.TapName, "guest_mac": netx.GuestMAC,
		}},
		// Relative uds_path: like the rootfs, it resolves in whatever dir a snapshot of
		// this VM is later restored into.
		{"PUT", "/vsock", map[string]any{"guest_cid": 3, "uds_path": vmm.VsockName}},
	}
	for _, s := range steps {
		if err := m.call(ctx, s.method, s.path, s.body); err != nil {
			return err
		}
	}
	m.console.Arm()
	return m.call(ctx, "PUT", "/actions", map[string]any{"action_type": "InstanceStart"})
}

func (m *machine) Restore(ctx context.Context, snapDir string) error {
	// A restored guest is already past boot; it never prints the marker again.
	return m.call(ctx, "PUT", "/snapshot/load", map[string]any{
		"snapshot_path": filepath.Join(snapDir, "vmstate"),
		"mem_backend":   map[string]any{"backend_type": "File", "backend_path": filepath.Join(snapDir, "mem")},
		"resume_vm":     true,
	})
}

// Init speaks Firecracker's vsock handshake ("CONNECT <port>" / "OK <hostport>") on the
// VM's unix socket, then exchanges one JSON message with the guest agent.
func (m *machine) Init(ctx context.Context, msg guest.InitMsg) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var conn net.Conn
	var err error
	t0, attempts := time.Now(), 0
	// The guest re-arms its vsock listener right after a restore; retry briefly.
	for {
		attempts++
		conn, err = m.vsockConnect(ctx)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("guest agent: %w", err)
		case <-m.exited:
			return errors.New("VM exited before its guest agent answered")
		case <-time.After(2 * time.Millisecond):
		}
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if err := json.NewEncoder(conn).Encode(msg); err != nil {
		return err
	}
	tConn := time.Now()
	var reply guest.InitReply
	if err := json.NewDecoder(conn).Decode(&reply); err != nil {
		return fmt.Errorf("guest agent reply: %w", err)
	}
	slog.Debug("guest init", "attempts", attempts, "connect", tConn.Sub(t0), "apply", time.Since(tConn), "last_err", err)
	if !reply.OK {
		return fmt.Errorf("guest agent: %s", reply.Error)
	}
	return nil
}

func (m *machine) vsockConnect(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", filepath.Join(m.dir, vmm.VsockName))
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", guest.VsockPort); err != nil {
		conn.Close()
		return nil, err
	}
	// Read the "OK <port>\n" ack byte by byte so no guest payload is buffered away.
	var line []byte
	b := make([]byte, 1)
	for len(line) < 64 {
		if _, err := conn.Read(b); err != nil {
			conn.Close()
			return nil, err
		}
		if b[0] == '\n' {
			break
		}
		line = append(line, b[0])
	}
	if !bytes.HasPrefix(line, []byte("OK ")) {
		conn.Close()
		return nil, fmt.Errorf("vsock handshake: %q", line)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func (m *machine) Pause(ctx context.Context) error {
	return m.call(ctx, "PATCH", "/vm", map[string]any{"state": "Paused"})
}

func (m *machine) Resume(ctx context.Context) error {
	return m.call(ctx, "PATCH", "/vm", map[string]any{"state": "Resumed"})
}

func (m *machine) Snapshot(ctx context.Context, dir string) error {
	return m.call(ctx, "PUT", "/snapshot/create", map[string]any{
		"snapshot_type": "Full",
		"snapshot_path": filepath.Join(dir, "vmstate"),
		"mem_file_path": filepath.Join(dir, "mem"),
	})
}

func (m *machine) call(ctx context.Context, method, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://fc"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		var fe struct {
			FaultMessage string `json:"fault_message"`
		}
		if json.Unmarshal(msg, &fe) == nil && fe.FaultMessage != "" {
			msg = []byte(fe.FaultMessage)
		}
		return fmt.Errorf("firecracker %s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(msg))
	}
	return nil
}
