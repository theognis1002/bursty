// Package orchestrator owns VM and snapshot lifecycle on one host: it keeps a warm pool
// of spawned sandboxes, boots or restores VMs into them, and tracks state on disk.
package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/theognis1002/bursty/internal/api"
	"github.com/theognis1002/bursty/internal/guest"
	"github.com/theognis1002/bursty/internal/netx"
	"github.com/theognis1002/bursty/internal/vmm"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
	ErrInvalid  = errors.New("invalid request")
)

var nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)

type Config struct {
	DataDir string
	// Kernel and Rootfs are absolute paths (rootfs already has bursty-init injected).
	Kernel   string
	Rootfs   string
	BootArgs string
	PoolSize int
	// PreparedPool is how many default-shape VMs to keep restored from the template,
	// initialized and paused, so a create only has to resume one.
	PreparedPool int
	// ReadyTimeout bounds how long a cold boot may take to report ready.
	ReadyTimeout time.Duration
	// PublishAddr is the host address published ports listen on.
	PublishAddr     string
	DefaultVCPUs    int
	DefaultMemoryMB int
	Version         string
	// Templates enables template-based creates (see templates.go). The default shape's
	// template is built at startup; other shapes on first use.
	Templates bool
}

type Manager struct {
	cfg    Config
	driver vmm.Driver
	net    netx.Provider

	mu     sync.Mutex
	closed bool
	vms    map[string]*entry
	snaps  map[string]*api.Snapshot

	tmplMu    sync.Mutex
	templates map[shape]*template
	building  map[shape]bool
	failedAt  map[shape]time.Time // last failed build, for retry backoff
	builds    sync.WaitGroup

	pool     chan *sandbox
	prepared chan *sandbox
	refill   chan struct{}
	poolCtx  context.Context
	poolStop context.CancelFunc
	poolDone chan struct{}
}

// sandbox is a spawned-but-unconfigured VMM with its network slot and directory.
type sandbox struct {
	id      string
	dir     string
	lease   netx.Lease
	machine vmm.Machine
	release sync.Once
}

type entry struct {
	op    sync.Mutex // serializes lifecycle operations on one VM
	vm    api.VM
	sb    *sandbox
	proxy *proxy
}

func New(cfg Config, d vmm.Driver, n netx.Provider) (*Manager, error) {
	if cfg.DefaultVCPUs == 0 {
		cfg.DefaultVCPUs = 1
	}
	if cfg.DefaultMemoryMB == 0 {
		cfg.DefaultMemoryMB = 128
	}
	if cfg.ReadyTimeout == 0 {
		cfg.ReadyTimeout = 10 * time.Second
	}
	if cfg.PublishAddr == "" {
		cfg.PublishAddr = "127.0.0.1"
	}
	for _, sub := range []string{"vms", "snapshots"} {
		if err := os.MkdirAll(filepath.Join(cfg.DataDir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		cfg:       cfg,
		driver:    d,
		net:       n,
		vms:       map[string]*entry{},
		snaps:     map[string]*api.Snapshot{},
		templates: map[shape]*template{},
		building:  map[shape]bool{},
		failedAt:  map[shape]time.Time{},
		pool:      make(chan *sandbox, max(cfg.PoolSize, 1)),
		prepared:  make(chan *sandbox, max(cfg.PreparedPool, 1)),
		refill:    make(chan struct{}, 1),
		poolCtx:   ctx,
		poolStop:  cancel,
		poolDone:  make(chan struct{}),
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(m.templatesDir(), 0o755); err != nil {
		return nil, err
	}
	m.loadTemplates()
	go m.poolLoop()
	m.kickPool()
	m.templateFor(m.defaultShape())
	return m, nil
}

func (m *Manager) Info() api.Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	return api.Info{
		Version:        m.cfg.Version,
		Driver:         m.driver.Name(),
		Network:        m.net.Name(),
		Kernel:         m.cfg.Kernel,
		Rootfs:         m.cfg.Rootfs,
		DataDir:        m.cfg.DataDir,
		PoolTarget:     m.cfg.PoolSize,
		PoolReady:      len(m.pool),
		PreparedTarget: m.cfg.PreparedPool,
		PreparedReady:  len(m.prepared),
		Templates:      m.Templates(),
		VMs:            len(m.vms),
		Snapshots:      len(m.snaps),
		HostOS:         runtime.GOOS,
		HostArch:       runtime.GOARCH,
	}
}

// ---- warm pool ----

func (m *Manager) kickPool() {
	select {
	case m.refill <- struct{}{}:
	default:
	}
}

func (m *Manager) poolLoop() {
	defer close(m.poolDone)
	for {
		select {
		case <-m.poolCtx.Done():
			return
		case <-m.refill:
		}
		for len(m.pool) < m.cfg.PoolSize {
			sb, err := m.spawn(m.poolCtx)
			if err != nil {
				if m.poolCtx.Err() == nil {
					slog.Error("warm pool: spawn failed", "err", err)
				}
				break
			}
			select {
			case m.pool <- sb:
			default:
				m.destroy(sb)
			}
		}
		m.fillPrepared()
	}
}

func (m *Manager) spawn(ctx context.Context) (*sandbox, error) {
	id := newID()
	dir := filepath.Join(m.cfg.DataDir, "vms", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if m.cfg.Rootfs != "" {
		if err := os.Symlink(m.cfg.Rootfs, filepath.Join(dir, vmm.RootfsName)); err != nil {
			os.RemoveAll(dir)
			return nil, err
		}
	}
	lease, err := m.net.Acquire(ctx)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	mc, err := m.driver.Spawn(ctx, vmm.SpawnSpec{ID: id, Dir: dir, Lease: lease})
	if err != nil {
		// Drivers reap their process before failing Spawn, so the slot is free again.
		m.net.Release(lease)
		os.RemoveAll(dir)
		return nil, err
	}
	sb := &sandbox{id: id, dir: dir, lease: lease, machine: mc}
	// The network slot (TAP) is only reusable once the VMM is gone.
	go func() {
		<-mc.Exited()
		sb.release.Do(func() { m.net.Release(lease) })
	}()
	return sb, nil
}

// acquire takes a warm sandbox, or spawns one inline if the pool is empty.
func (m *Manager) acquire(ctx context.Context) (*sandbox, bool, error) {
	defer m.kickPool()
	select {
	case sb := <-m.pool:
		select {
		case <-sb.machine.Exited():
			m.destroy(sb) // died while idle; fall through to a fresh one
		default:
			return sb, true, nil
		}
	default:
	}
	sb, err := m.spawn(ctx)
	return sb, false, err
}

func (m *Manager) destroy(sb *sandbox) {
	_ = sb.machine.Kill()
	os.RemoveAll(sb.dir)
}

// Close kills every VM and pooled sandbox. VM records stay on disk as stopped.
func (m *Manager) Close() {
	m.poolStop()
	<-m.poolDone
	m.builds.Wait()
	for _, ch := range []chan *sandbox{m.pool, m.prepared} {
	drain:
		for {
			select {
			case sb := <-ch:
				m.destroy(sb)
			default:
				break drain
			}
		}
	}
	m.mu.Lock()
	m.closed = true
	entries := make([]*entry, 0, len(m.vms))
	for _, e := range m.vms {
		entries = append(entries, e)
	}
	m.mu.Unlock()
	for _, e := range entries {
		e.op.Lock()
		if s := m.snapshotOf(e).State; s != api.StateStopped && s != api.StateFailed {
			m.stopLocked(e, api.StateStopped)
		}
		e.op.Unlock()
	}
	_ = m.net.Close()
}

// ---- VMs ----

func (m *Manager) CreateVM(ctx context.Context, req api.CreateVMRequest) (api.VM, error) {
	t0 := time.Now()
	if req.VCPUs == 0 {
		req.VCPUs = m.cfg.DefaultVCPUs
	}
	if req.MemoryMB == 0 {
		req.MemoryMB = m.cfg.DefaultMemoryMB
	}
	if req.VCPUs < 1 || req.VCPUs > 32 || req.MemoryMB < 64 || req.MemoryMB > 32768 {
		return api.VM{}, fmt.Errorf("%w: vcpus must be 1-32 and memory_mb 64-32768", ErrInvalid)
	}
	// Reject bad names and ports before taking a warm or prepared sandbox.
	if err := m.checkNew(req.Name, req.Ports); err != nil {
		return api.VM{}, err
	}
	// Writable VMs boot cold: a template's memory belongs to a read-only root mount.
	if !req.Writable && !req.Cold {
		s := shape{req.VCPUs, req.MemoryMB}
		if s == m.defaultShape() {
			if sb := m.takePrepared(); sb != nil {
				return m.createPrepared(ctx, t0, sb, req)
			}
		}
		if t := m.templateFor(s); t != nil {
			return m.createFromTemplate(ctx, t0, t, req)
		}
	}
	sb, warm, err := m.acquire(ctx)
	if err != nil {
		return api.VM{}, err
	}
	tAcq := time.Now()
	e, err := m.register(sb, req.Name, req.Ports, func(vm *api.VM) {
		vm.VCPUs, vm.MemoryMB, vm.Cmd, vm.Writable = req.VCPUs, req.MemoryMB, req.Cmd, req.Writable
	})
	if err != nil {
		m.destroy(sb)
		return api.VM{}, err
	}
	e.op.Lock()
	defer e.op.Unlock()

	fail := func(err error) (api.VM, error) {
		m.failLocked(e, err)
		return m.snapshotOf(e), err
	}
	if req.Writable {
		if err := m.privateRootfs(sb.dir, m.cfg.Rootfs); err != nil {
			return fail(err)
		}
	}
	err = sb.machine.Boot(ctx, vmm.BootConfig{
		VCPUs:    req.VCPUs,
		MemoryMB: req.MemoryMB,
		Kernel:   m.cfg.Kernel,
		BootArgs: m.bootArgs(sb.id, req.Cmd),
		ReadOnly: !req.Writable,
		Cmd:      req.Cmd,
	})
	if err != nil {
		return fail(fmt.Errorf("boot: %w", err))
	}
	tStart := time.Now()
	select {
	case <-sb.machine.Ready():
	case <-sb.machine.Exited():
		return fail(errors.New("VM exited during boot (see logs)"))
	case <-time.After(m.cfg.ReadyTimeout):
		return fail(fmt.Errorf("guest not ready after %s (see logs)", m.cfg.ReadyTimeout))
	case <-ctx.Done():
		return fail(ctx.Err())
	}
	tReady := time.Now()
	m.update(e, func(vm *api.VM) {
		vm.State = api.StateRunning
		vm.Timings = api.Timings{
			Mode:    api.ModeCold,
			Total:   ms(tReady.Sub(t0)),
			Acquire: ms(tAcq.Sub(t0)),
			Start:   ms(tStart.Sub(tAcq)),
			Guest:   sb.machine.GuestUptime() * 1000,
			Warm:    warm,
		}
	})
	return m.started(e)
}

func (m *Manager) bootArgs(id, cmd string) string {
	args := m.cfg.BootArgs +
		fmt.Sprintf(" ip=%s::%s:%s::eth0:off bursty.id=%s", netx.GuestIP, netx.GatewayIP, netx.GuestMask, id)
	if cmd != "" {
		args += " bursty.cmd=" + base64.RawURLEncoding.EncodeToString([]byte(cmd))
	}
	return args
}

// checkNew validates a new VM's name and published ports. register re-checks the name
// under the lock; this early check only keeps bad requests from using up a sandbox.
func (m *Manager) checkNew(name string, ports []api.Port) error {
	if name != "" && !nameRE.MatchString(name) {
		return fmt.Errorf("%w: name %q must match %s", ErrInvalid, name, nameRE)
	}
	seen := map[int]bool{}
	for _, p := range ports {
		if p.Host < 1 || p.Host > 65535 || p.Guest < 1 || p.Guest > 65535 {
			return fmt.Errorf("%w: port %d:%d: host and guest ports must be 1-65535", ErrInvalid, p.Host, p.Guest)
		}
		if seen[p.Host] {
			return fmt.Errorf("%w: host port %d published twice", ErrInvalid, p.Host)
		}
		seen[p.Host] = true
	}
	if name == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.vms {
		if e.vm.Name == name {
			return fmt.Errorf("%w: a VM named %q exists", ErrConflict, name)
		}
	}
	return nil
}

// register records a new VM in booting state for sandbox sb.
func (m *Manager) register(sb *sandbox, name string, ports []api.Port, set func(*api.VM)) (*entry, error) {
	if name == "" {
		name = "vm-" + sb.id
	}
	if !nameRE.MatchString(name) {
		return nil, fmt.Errorf("%w: name %q must match %s", ErrInvalid, name, nameRE)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, fmt.Errorf("%w: daemon is shutting down", ErrConflict)
	}
	for _, e := range m.vms {
		if e.vm.Name == name {
			return nil, fmt.Errorf("%w: a VM named %q exists", ErrConflict, name)
		}
	}
	e := &entry{sb: sb, vm: api.VM{
		ID:        sb.id,
		Name:      name,
		State:     api.StateBooting,
		Driver:    m.driver.Name(),
		IP:        sb.lease.IP,
		GuestIP:   netx.GuestIP,
		MAC:       netx.GuestMAC,
		Netns:     sb.lease.Netns,
		Endpoint:  "http://" + sb.lease.Addr(guest.Port),
		Ports:     ports,
		PID:       sb.machine.PID(),
		CreatedAt: time.Now().UTC(),
	}}
	set(&e.vm)
	m.vms[e.vm.ID] = e
	return e, nil
}

// started finishes a successful boot/restore: publish ports, watch for exit, persist.
func (m *Manager) started(e *entry) (api.VM, error) {
	if len(e.vm.Ports) > 0 {
		p, err := startProxy(m.cfg.PublishAddr, e.vm.Ports, e.sb.lease)
		if err != nil {
			m.failLocked(e, err)
			return m.snapshotOf(e), err
		}
		e.proxy = p
	}
	sb := e.sb
	// Notice VMMs that die on their own (guest reboot, crash). Deliberate stops change the
	// state first, under e.op, so this only acts on unexpected exits.
	go func() {
		<-sb.machine.Exited()
		e.op.Lock()
		defer e.op.Unlock()
		m.mu.Lock()
		unexpected := m.vms[e.vm.ID] == e && (e.vm.State == api.StateRunning || e.vm.State == api.StatePaused)
		m.mu.Unlock()
		if unexpected {
			m.update(e, func(vm *api.VM) { vm.Error = "VMM exited unexpectedly" })
			m.stopLocked(e, api.StateStopped)
		}
	}()
	m.persist(e)
	return m.snapshotOf(e), nil
}

// failLocked records err on the VM and stops it as failed; caller holds e.op.
func (m *Manager) failLocked(e *entry, err error) {
	m.update(e, func(vm *api.VM) { vm.Error = err.Error() })
	m.stopLocked(e, api.StateFailed)
}

// stopLocked kills the VM's VMM and persists the record; caller holds e.op.
func (m *Manager) stopLocked(e *entry, state string) {
	if e.proxy != nil {
		e.proxy.Close()
		e.proxy = nil
	}
	m.update(e, func(vm *api.VM) {
		vm.State = state
		vm.PID = 0
	})
	if e.sb != nil {
		_ = e.sb.machine.Kill()
	}
	m.persist(e)
}

func (m *Manager) StopVM(ctx context.Context, id string) (api.VM, error) {
	e, err := m.lookup(id)
	if err != nil {
		return api.VM{}, err
	}
	e.op.Lock()
	defer e.op.Unlock()
	m.stopLocked(e, api.StateStopped)
	return m.snapshotOf(e), nil
}

func (m *Manager) DeleteVM(ctx context.Context, id string) error {
	e, err := m.lookup(id)
	if err != nil {
		return err
	}
	e.op.Lock()
	defer e.op.Unlock()
	m.stopLocked(e, api.StateStopped)
	m.mu.Lock()
	delete(m.vms, e.vm.ID)
	m.mu.Unlock()
	return os.RemoveAll(filepath.Join(m.cfg.DataDir, "vms", e.vm.ID))
}

func (m *Manager) PauseVM(ctx context.Context, id string) (api.VM, error) {
	return m.transition(ctx, id, api.StateRunning, api.StatePaused, vmm.Machine.Pause)
}

func (m *Manager) ResumeVM(ctx context.Context, id string) (api.VM, error) {
	return m.transition(ctx, id, api.StatePaused, api.StateRunning, vmm.Machine.Resume)
}

func (m *Manager) transition(ctx context.Context, id, from, to string, op func(vmm.Machine, context.Context) error) (api.VM, error) {
	e, err := m.lookup(id)
	if err != nil {
		return api.VM{}, err
	}
	e.op.Lock()
	defer e.op.Unlock()
	if cur := m.snapshotOf(e).State; cur != from {
		return api.VM{}, fmt.Errorf("%w: VM is %s, want %s", ErrConflict, cur, from)
	}
	if err := op(e.sb.machine, context.WithoutCancel(ctx)); err != nil {
		return api.VM{}, err
	}
	m.update(e, func(vm *api.VM) { vm.State = to })
	m.persist(e)
	return m.snapshotOf(e), nil
}

func (m *Manager) GetVM(id string) (api.VM, error) {
	e, err := m.lookup(id)
	if err != nil {
		return api.VM{}, err
	}
	return m.snapshotOf(e), nil
}

func (m *Manager) ListVMs() []api.VM {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]api.VM, 0, len(m.vms))
	for _, e := range m.vms {
		out = append(out, e.vm)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// ConsolePath returns the VM's serial console log.
func (m *Manager) ConsolePath(id string) (string, error) {
	e, err := m.lookup(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(m.cfg.DataDir, "vms", m.snapshotOf(e).ID, "console.log"), nil
}

// lookup finds a VM by ID or name.
func (m *Manager) lookup(ref string) (*entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.vms[ref]; ok {
		return e, nil
	}
	for _, e := range m.vms {
		if e.vm.Name == ref {
			return e, nil
		}
	}
	return nil, fmt.Errorf("%w: VM %q", ErrNotFound, ref)
}

func (m *Manager) update(e *entry, f func(*api.VM)) {
	m.mu.Lock()
	f(&e.vm)
	m.mu.Unlock()
}

func (m *Manager) snapshotOf(e *entry) api.VM {
	m.mu.Lock()
	defer m.mu.Unlock()
	return e.vm
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func newID() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
