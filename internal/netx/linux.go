package netx

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
)

const (
	nsPrefix   = "bs"
	vethPrefix = "bsv"
	hostCIDR   = "10.201.0.0/16"
)

// Netns implements per-VM network namespaces on Linux. Needs root, iproute2 and iptables.
type Netns struct {
	mu    sync.Mutex
	slots *slotAllocator
	// free keeps released, still-configured slots for reuse, so a later Acquire is cheap.
	free []Lease
}

// MaxSlots is how many /30 blocks fit in hostCIDR.
const MaxSlots = 16384

func NewNetns(maxSlots int) *Netns {
	return &Netns{slots: newSlotAllocator(min(maxSlots, MaxSlots))}
}

func (n *Netns) Name() string { return "netns (veth + tap per VM, " + hostCIDR + ")" }

func (n *Netns) Init(ctx context.Context) error {
	for _, bin := range []string{"ip", "iptables"} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("netns networking needs %q: %w", bin, err)
		}
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("netns networking needs root")
	}
	// Leftovers from a crashed daemon.
	out, _ := exec.CommandContext(ctx, "ip", "netns", "list").Output()
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && strings.HasPrefix(f[0], nsPrefix) && isDigits(f[0][len(nsPrefix):]) {
			name := f[0]
			_ = run(ctx, "ip", "netns", "del", name)
		}
	}
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644); err != nil {
		return fmt.Errorf("enable ip_forward: %w", err)
	}
	// Egress NAT for VMs and forwarding between veths; -C first so restarts stay idempotent.
	rules := [][]string{
		{"-t", "nat", "POSTROUTING", "-s", hostCIDR, "!", "-d", hostCIDR, "-j", "MASQUERADE"},
		{"-t", "filter", "FORWARD", "-i", vethPrefix + "+", "-j", "ACCEPT"},
		{"-t", "filter", "FORWARD", "-o", vethPrefix + "+", "-j", "ACCEPT"},
		// Guests reach the internet and each other, not services on the host.
		{"-t", "filter", "INPUT", "-i", vethPrefix + "+", "-m", "conntrack", "!", "--ctstate", "ESTABLISHED,RELATED", "-j", "DROP"},
	}
	for _, r := range rules {
		table, chain, spec := r[1], r[2], r[3:]
		check := append([]string{"-t", table, "-C", chain}, spec...)
		if run(ctx, "iptables", check...) == nil {
			continue
		}
		insert := append([]string{"-t", table, "-I", chain}, spec...)
		if err := run(ctx, "iptables", insert...); err != nil {
			return err
		}
	}
	return nil
}

func (n *Netns) Acquire(ctx context.Context) (Lease, error) {
	n.mu.Lock()
	if len(n.free) > 0 {
		l := n.free[len(n.free)-1]
		n.free = n.free[:len(n.free)-1]
		n.mu.Unlock()
		return l, nil
	}
	slot, err := n.slots.take()
	n.mu.Unlock()
	if err != nil {
		return Lease{}, err
	}
	l := Lease{Slot: slot, IP: slotIP(slot, 2), Netns: fmt.Sprintf("%s%d", nsPrefix, slot)}
	if err := setupSlot(ctx, l); err != nil {
		_ = run(context.Background(), "ip", "netns", "del", l.Netns)
		n.mu.Lock()
		n.slots.free(slot)
		n.mu.Unlock()
		return Lease{}, err
	}
	return l, nil
}

// Release keeps the namespace configured for reuse. The TAP device is single-owner, so a
// slot is only handed out again after its Firecracker process has exited.
func (n *Netns) Release(l Lease) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.free = append(n.free, l)
}

func (n *Netns) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	for slot := range n.slots.used {
		_ = run(context.Background(), "ip", "netns", "del", fmt.Sprintf("%s%d", nsPrefix, slot))
	}
	n.slots = newSlotAllocator(n.slots.max)
	n.free = nil
	return nil
}

func setupSlot(ctx context.Context, l Lease) error {
	ns := l.Netns
	veth := fmt.Sprintf("%s%d", vethPrefix, l.Slot)
	hostIP, nsIP := slotIP(l.Slot, 1), slotIP(l.Slot, 2)
	// A namespace deleted moments ago (daemon restart) may still be tearing down its veth.
	_ = run(ctx, "ip", "link", "del", veth)
	if err := run(ctx, "ip", "netns", "add", ns); err != nil {
		return err
	}
	// Host-side commands, then namespace-side commands, each as a single `ip -batch`.
	host := strings.Join([]string{
		fmt.Sprintf("link add %s type veth peer name veth0 netns %s", veth, ns),
		fmt.Sprintf("addr add %s/30 dev %s", hostIP, veth),
		fmt.Sprintf("link set %s up", veth),
	}, "\n")
	inNS := strings.Join([]string{
		"link set lo up",
		fmt.Sprintf("addr add %s/30 dev veth0", nsIP),
		"link set veth0 up",
		fmt.Sprintf("route add default via %s", hostIP),
		fmt.Sprintf("tuntap add %s mode tap", TapName),
		fmt.Sprintf("link set %s address %s", TapName, GatewayMAC),
		fmt.Sprintf("addr add %s/30 dev %s", GatewayIP, TapName),
		fmt.Sprintf("link set %s up", TapName),
	}, "\n")
	if err := runStdin(ctx, host, "ip", "-batch", "-"); err != nil {
		return err
	}
	if err := runStdin(ctx, inNS, "ip", "-n", ns, "-batch", "-"); err != nil {
		return err
	}
	rules := "*nat\n" +
		fmt.Sprintf("-A PREROUTING -i veth0 -d %s -j DNAT --to-destination %s\n", nsIP, GuestIP) +
		fmt.Sprintf("-A POSTROUTING -o veth0 -s %s -j SNAT --to-source %s\n", GuestIP, nsIP) +
		"COMMIT\n"
	if err := runStdin(ctx, rules, "ip", "netns", "exec", ns, "iptables-restore"); err != nil {
		return err
	}
	return run(ctx, "ip", "netns", "exec", ns, "sysctl", "-qw", "net.ipv4.ip_forward=1")
}

func run(ctx context.Context, name string, args ...string) error {
	return runStdin(ctx, "", name, args...)
}

func runStdin(ctx context.Context, stdin, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		slog.Debug("command failed", "cmd", name, "args", args, "err", err, "stderr", msg)
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
	}
	return nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
