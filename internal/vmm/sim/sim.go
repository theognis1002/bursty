// Package sim is a VMM stand-in for hosts without KVM (e.g. macOS). Each "VM" is a real
// child process that serves the guest HTTP service on a loopback port, so the whole
// control plane (API, CLI, pool, proxy, snapshot bookkeeping) runs unchanged. Pause is
// SIGSTOP; a snapshot is the guest's persisted state.
package sim

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/theognis1002/bursty/internal/console"
	"github.com/theognis1002/bursty/internal/guest"
	"github.com/theognis1002/bursty/internal/vmm"
)

// GuestArg is the hidden subcommand of the daemon binary that runs a sim guest.
const GuestArg = "sim-guest"

type Driver struct {
	// Exe is the binary to re-exec as the guest (the daemon itself).
	Exe string
	// BootDelay simulates guest kernel boot time.
	BootDelay time.Duration
}

func (d *Driver) Name() string { return "sim" }

type machine struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	replies chan string
	console *console.Console
	exited  chan struct{}
	dir     string
	mu      sync.Mutex
}

func (d *Driver) Spawn(ctx context.Context, spec vmm.SpawnSpec) (vmm.Machine, error) {
	cmd := exec.Command(d.Exe, GuestArg,
		"--id", spec.ID,
		"--dir", spec.Dir,
		"--port", strconv.Itoa(spec.Lease.PortBase),
		"--boot-delay", d.BootDelay.String())
	cmd.Dir = spec.Dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	// fd 3 in the guest: replies to init messages (stdout is the console).
	rr, rw, err := os.Pipe()
	if err != nil {
		pr.Close()
		pw.Close()
		return nil, err
	}
	cmd.ExtraFiles = []*os.File{rw}
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		rr.Close()
		rw.Close()
		return nil, err
	}
	pw.Close()
	rw.Close()
	con, err := console.Start(pr, filepath.Join(spec.Dir, "console.log"))
	if err != nil {
		pr.Close()
		rr.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	m := &machine{cmd: cmd, stdin: stdin, replies: make(chan string, 4), console: con, exited: make(chan struct{}), dir: spec.Dir}
	go func() {
		defer rr.Close()
		sc := bufio.NewScanner(rr)
		for sc.Scan() {
			m.replies <- sc.Text()
		}
	}()
	go func() {
		_ = cmd.Wait()
		close(m.exited)
	}()
	return m, nil
}

func (m *machine) PID() int                { return m.cmd.Process.Pid }
func (m *machine) Ready() <-chan struct{}  { return m.console.Ready() }
func (m *machine) GuestUptime() float64    { return m.console.GuestUptime() }
func (m *machine) Exited() <-chan struct{} { return m.exited }

func (m *machine) send(line string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, err := io.WriteString(m.stdin, line+"\n")
	return err
}

func (m *machine) Boot(ctx context.Context, cfg vmm.BootConfig) error {
	m.console.Arm()
	return m.send("boot " + base64.RawURLEncoding.EncodeToString([]byte(cfg.Cmd)))
}

func (m *machine) Restore(ctx context.Context, snapDir string) error {
	m.console.Arm()
	if err := m.send("restore " + filepath.Join(snapDir, "vmstate")); err != nil {
		return err
	}
	// Mirror Firecracker: snapshot/load with resume_vm returns once the VM is running.
	select {
	case <-m.console.Ready():
		return nil
	case <-m.exited:
		return errors.New("sim guest exited during restore (see console.log)")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *machine) Init(ctx context.Context, msg guest.InitMsg) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if err := m.send("init " + base64.RawURLEncoding.EncodeToString(b)); err != nil {
		return err
	}
	select {
	case r := <-m.replies:
		var reply guest.InitReply
		if err := json.Unmarshal([]byte(r), &reply); err != nil {
			return err
		}
		if !reply.OK {
			return errors.New("guest agent: " + reply.Error)
		}
		return nil
	case <-m.exited:
		return errors.New("sim guest exited during init")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *machine) Pause(ctx context.Context) error {
	return m.cmd.Process.Signal(syscall.SIGSTOP)
}

func (m *machine) Resume(ctx context.Context) error {
	return m.cmd.Process.Signal(syscall.SIGCONT)
}

func (m *machine) Snapshot(ctx context.Context, dir string) error {
	b, err := os.ReadFile(filepath.Join(m.dir, "guest-state.json"))
	if err != nil {
		return fmt.Errorf("read guest state: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, "vmstate"), b, 0o644)
}

func (m *machine) Kill() error {
	select {
	case <-m.exited:
		return nil
	default:
	}
	// The guest leads its own process group (Setpgid); kill the group so processes its
	// --cmd started die with the "VM" instead of leaking onto the host.
	pgid := -m.cmd.Process.Pid
	_ = syscall.Kill(pgid, syscall.SIGCONT)
	_ = syscall.Kill(pgid, syscall.SIGKILL)
	<-m.exited
	return nil
}

// GuestMain runs the sim guest. It waits on stdin for "boot <b64cmd>" or
// "restore <statefile>", then serves the guest HTTP service.
func GuestMain(args []string) {
	var id, dir, port, delay string
	for i := 0; i+1 < len(args); i += 2 {
		switch args[i] {
		case "--id":
			id = args[i+1]
		case "--dir":
			dir = args[i+1]
		case "--port":
			port = args[i+1]
		case "--boot-delay":
			delay = args[i+1]
		}
	}
	start := time.Now()
	stdin := bufio.NewReader(os.Stdin)
	line, err := stdin.ReadString('\n')
	if err != nil {
		os.Exit(1)
	}
	verb, arg, _ := strings.Cut(strings.TrimSpace(line), " ")
	statePath := filepath.Join(dir, "guest-state.json")
	var svc *guest.Service
	switch verb {
	case "boot":
		start = time.Now() // "power on"
		if d, err := time.ParseDuration(delay); err == nil {
			time.Sleep(d)
		}
		svc = guest.New(id)
		cmd, _ := base64.RawURLEncoding.DecodeString(arg)
		runCmd(string(cmd))
	case "restore":
		b, err := os.ReadFile(arg)
		var st guest.State
		if err == nil {
			err = json.Unmarshal(b, &st)
		}
		if err != nil {
			fmt.Println("restore failed:", err)
			os.Exit(1)
		}
		svc = guest.FromState(st)
	default:
		os.Exit(1)
	}
	// Requests run concurrently: serialize writes and always save the current state, so
	// an older state never lands last and two writers never share the tmp file.
	var saveMu sync.Mutex
	save := func(guest.State) {
		saveMu.Lock()
		defer saveMu.Unlock()
		b, _ := json.Marshal(svc.State())
		tmp := statePath + ".tmp"
		if os.WriteFile(tmp, b, 0o644) == nil {
			_ = os.Rename(tmp, statePath)
		}
	}
	svc.OnChange = save
	save(svc.State())
	// Later stdin lines are agent messages; EOF means the daemon is gone, and so is this "VM".
	replies := os.NewFile(3, "replies")
	go func() {
		for {
			line, err := stdin.ReadString('\n')
			if err != nil {
				os.Exit(0)
			}
			verb, arg, _ := strings.Cut(strings.TrimSpace(line), " ")
			if verb != "init" {
				continue
			}
			reply := guest.InitReply{OK: true}
			var msg guest.InitMsg
			b, err := base64.RawURLEncoding.DecodeString(arg)
			if err == nil {
				err = json.Unmarshal(b, &msg)
			}
			if err != nil {
				reply = guest.InitReply{Error: err.Error()}
			} else {
				if msg.ID != "" {
					svc.Reinit(msg.ID)
				}
				runCmd(msg.Cmd)
			}
			out, _ := json.Marshal(reply)
			_, _ = replies.Write(append(out, '\n'))
		}
	}()
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		fmt.Println("listen:", err)
		os.Exit(1)
	}
	fmt.Printf("%s uptime=%.4f\n", console.ReadyMarker, time.Since(start).Seconds())
	_ = http.Serve(ln, svc)
}

func runCmd(cmd string) {
	if cmd == "" {
		return
	}
	c := exec.Command("/bin/sh", "-c", cmd)
	c.Stdout, c.Stderr = os.Stdout, os.Stdout
	if c.Start() == nil {
		go func() { _ = c.Wait() }()
	}
}
