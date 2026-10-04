// bursty-init is PID 1 inside every bursty microVM. It does the minimum to get from
// kernel to a useful userspace fast: mount pseudo-filesystems, bring up loopback, report
// ready on the serial console, serve the guest HTTP service and run the user's command.
// It is injected into the rootfs by burstyd and selected with init=/bursty-init.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/theognis1002/bursty/internal/console"
	"github.com/theognis1002/bursty/internal/guest"
	"github.com/theognis1002/bursty/internal/netx"
)

func main() {
	// The kernel starts init with no PATH; children and the ip fallback need one.
	os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	// Subscribe before starting children so no SIGCHLD is missed.
	sigs := make(chan os.Signal, 16)
	signal.Notify(sigs, syscall.SIGCHLD, syscall.SIGTERM, syscall.SIGINT)
	pid1 := os.Getpid() == 1
	if pid1 {
		mountAll()
	}
	args := cmdline()
	if id := args["bursty.id"]; id != "" && pid1 {
		_ = syscall.Sethostname([]byte(id))
	}
	if pid1 {
		setupNetwork()
	}

	svc := guest.New(args["bursty.id"])
	svc.Uptime = uptime
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", guest.Port))
	if err != nil {
		fmt.Println("bursty-init: listen:", err)
	} else {
		go func() { _ = http.Serve(ln, svc) }()
	}
	// The agent must be listening before "ready": templates are snapshotted right after.
	if err := startAgent(svc); err != nil {
		fmt.Println("bursty-init: vsock agent:", err)
	}
	fmt.Printf("%s uptime=%.4f\n", console.ReadyMarker, uptime())

	if enc := args["bursty.cmd"]; enc != "" {
		if cmd, err := base64.RawURLEncoding.DecodeString(enc); err == nil {
			runCmd(string(cmd))
		}
	}
	if !pid1 {
		select {}
	}
	reap(sigs)
}

func mountAll() {
	mounts := []struct {
		src, target, fstype string
		flags               uintptr
		data                string
	}{
		{"proc", "/proc", "proc", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, ""},
		{"sysfs", "/sys", "sysfs", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, ""},
		{"devtmpfs", "/dev", "devtmpfs", syscall.MS_NOSUID, "mode=0755"},
		{"tmpfs", "/run", "tmpfs", syscall.MS_NOSUID | syscall.MS_NODEV, "mode=0755"},
		{"tmpfs", "/tmp", "tmpfs", syscall.MS_NOSUID | syscall.MS_NODEV, "mode=1777"},
	}
	for _, m := range mounts {
		_ = os.MkdirAll(m.target, 0o755)
		if err := syscall.Mount(m.src, m.target, m.fstype, m.flags, m.data); err != nil && err != syscall.EBUSY {
			fmt.Printf("bursty-init: mount %s: %v\n", m.target, err)
		}
	}
	_ = os.MkdirAll("/dev/pts", 0o755)
	_ = syscall.Mount("devpts", "/dev/pts", "devpts", syscall.MS_NOSUID|syscall.MS_NOEXEC, "")
}

// setupNetwork relies on the kernel's ip= autoconfig for eth0 and only brings up lo and
// DNS. If the kernel lacks IP autoconfig, it falls back to the rootfs's `ip` tool.
func setupNetwork() {
	ifUp("lo")
	if iface, err := net.InterfaceByName("eth0"); err == nil {
		if addrs, _ := iface.Addrs(); len(addrs) == 0 {
			for _, a := range [][]string{
				{"addr", "add", netx.GuestIP + "/30", "dev", "eth0"},
				{"link", "set", "eth0", "up"},
				{"route", "add", "default", "via", netx.GatewayIP},
			} {
				if out, err := exec.Command("ip", a...).CombinedOutput(); err != nil {
					fmt.Printf("bursty-init: ip %v: %v %s\n", a, err, out)
				}
			}
		}
	}
	// The root may be read-only: a symlinked /etc/resolv.conf (stock Ubuntu) points into
	// /run, our tmpfs; a regular file gets ours bind-mounted over it.
	resolv := []byte("nameserver 1.1.1.1\nnameserver 8.8.8.8\n")
	_ = os.MkdirAll("/run/systemd/resolve", 0o755)
	_ = os.WriteFile("/run/systemd/resolve/stub-resolv.conf", resolv, 0o644)
	_ = os.WriteFile("/run/resolv.conf", resolv, 0o644)
	if fi, err := os.Lstat("/etc/resolv.conf"); err == nil && fi.Mode().IsRegular() {
		if err := syscall.Mount("/run/resolv.conf", "/etc/resolv.conf", "", syscall.MS_BIND, ""); err != nil {
			fmt.Println("bursty-init: resolv.conf:", err)
		}
	}
}

func ifUp(name string) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return
	}
	defer syscall.Close(fd)
	var req struct {
		name  [syscall.IFNAMSIZ]byte
		flags uint16
		_     [22]byte
	}
	copy(req.name[:], name)
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.SIOCGIFFLAGS, uintptr(unsafe.Pointer(&req))); e != 0 {
		return
	}
	req.flags |= syscall.IFF_UP
	_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.SIOCSIFFLAGS, uintptr(unsafe.Pointer(&req)))
}

func cmdline() map[string]string {
	out := map[string]string{}
	b, _ := os.ReadFile("/proc/cmdline")
	for _, f := range strings.Fields(string(b)) {
		if k, v, ok := strings.Cut(f, "="); ok {
			out[k] = v
		}
	}
	return out
}

func uptime() float64 {
	b, _ := os.ReadFile("/proc/uptime")
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

// reap collects orphaned children forever; PID 1 must never exit.
func reap(sigs <-chan os.Signal) {
	for {
		for {
			var ws syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
			if pid <= 0 || err != nil {
				break
			}
		}
		if s := <-sigs; s == syscall.SIGTERM || s == syscall.SIGINT {
			syscall.Sync()
			_ = syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
		}
	}
}

func runCmd(cmd string) {
	if cmd == "" {
		return
	}
	c := exec.Command("/bin/sh", "-c", cmd)
	c.Stdout, c.Stderr = os.Stdout, os.Stdout
	c.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=/root", "TERM=linux"}
	if err := c.Start(); err != nil {
		fmt.Println("bursty-init: cmd:", err)
	}
	// PID 1's reaper collects it.
}

// startAgent listens on vsock for host messages. vsock is only reachable from the host
// (through Firecracker), never from the network, so it needs no authentication.
func startAgent(svc *guest.Service) error {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: guest.VsockPort}); err != nil {
		unix.Close(fd)
		return err
	}
	if err := unix.Listen(fd, 16); err != nil {
		unix.Close(fd)
		return err
	}
	go func() {
		for {
			nfd, _, err := unix.Accept4(fd, unix.SOCK_CLOEXEC)
			if err != nil {
				if err == unix.EINTR || err == unix.ECONNABORTED {
					continue
				}
				fmt.Println("bursty-init: vsock accept:", err)
				return
			}
			go handleAgent(os.NewFile(uintptr(nfd), "vsock"), svc)
		}
	}()
	return nil
}

func handleAgent(conn *os.File, svc *guest.Service) {
	defer conn.Close()
	var msg guest.InitMsg
	reply := guest.InitReply{OK: true}
	if err := json.NewDecoder(conn).Decode(&msg); err != nil {
		reply = guest.InitReply{Error: err.Error()}
	} else if err := applyInit(msg, svc); err != nil {
		reply = guest.InitReply{Error: err.Error()}
	}
	_ = json.NewEncoder(conn).Encode(reply)
}

// applyInit makes a template clone a distinct VM. Entropy goes in before anything that
// draws random numbers (the new boot ID).
func applyInit(msg guest.InitMsg, svc *guest.Service) error {
	if len(msg.Entropy) > 0 {
		if f, err := os.OpenFile("/dev/urandom", os.O_WRONLY, 0); err == nil {
			_, _ = f.Write(msg.Entropy)
			f.Close()
		}
	}
	if msg.UnixNano > 0 {
		tv := unix.NsecToTimeval(msg.UnixNano)
		if err := unix.Settimeofday(&tv); err != nil {
			return fmt.Errorf("settimeofday: %w", err)
		}
	}
	// An empty ID means "just run Cmd" (a prepared VM already has its identity).
	if msg.ID != "" {
		if err := unix.Sethostname([]byte(msg.ID)); err != nil {
			return fmt.Errorf("sethostname: %w", err)
		}
		svc.Reinit(msg.ID)
	}
	runCmd(msg.Cmd)
	return nil
}
