package orchestrator

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/theognis1002/bursty/internal/api"
	"github.com/theognis1002/bursty/internal/guest"
	"github.com/theognis1002/bursty/internal/vmm"
)

// A template is a snapshot of a freshly booted, idle VM of one shape (vCPUs, memory).
// Creating a VM from it is a restore plus a vsock message that gives the clone its own
// identity, clock, entropy and command: no kernel boot on the request path. This is
// how a create lands well under 200 ms; --cold still boots a kernel.
type template struct {
	Key       string    `json:"key"`
	VCPUs     int       `json:"vcpus"`
	MemoryMB  int       `json:"memory_mb"`
	Image     string    `json:"image,omitempty"`
	BuildMs   float64   `json:"build_ms"`
	CreatedAt time.Time `json:"created_at"`
	dir       string
}

type shape struct{ vcpus, mem int }

// maxTemplates bounds how many shapes get a template (each holds a memory image).
const maxTemplates = 8

// templateRetry is how long after a failed build a shape waits before the next attempt,
// so a broken template does not cost an extra VM boot on every create.
const templateRetry = time.Minute

func (m *Manager) templateKey(s shape) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%d|%d", m.driver.Name(), m.cfg.Rootfs, m.cfg.BootArgs, s.vcpus, s.mem)
	if st, err := os.Stat(m.cfg.Kernel); err == nil {
		fmt.Fprintf(h, "|%s|%d|%d", m.cfg.Kernel, st.Size(), st.ModTime().UnixNano())
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

func (m *Manager) templatesDir() string { return filepath.Join(m.cfg.DataDir, "templates") }

// loadTemplates picks up templates built by an earlier run with the same kernel, rootfs
// and boot args; stale ones are deleted.
func (m *Manager) loadTemplates() {
	dirs, _ := os.ReadDir(m.templatesDir())
	for _, d := range dirs {
		dir := filepath.Join(m.templatesDir(), d.Name())
		var t template
		if err := readJSON(filepath.Join(dir, "template.json"), &t); err != nil ||
			t.Key != d.Name() || m.templateKey(shape{t.VCPUs, t.MemoryMB}) != t.Key {
			os.RemoveAll(dir)
			continue
		}
		t.dir = dir
		m.templates[shape{t.VCPUs, t.MemoryMB}] = &t
	}
}

// templateFor returns the template for s, scheduling a build if there is none.
func (m *Manager) templateFor(s shape) *template {
	if !m.cfg.Templates {
		return nil
	}
	m.tmplMu.Lock()
	defer m.tmplMu.Unlock()
	if t := m.templates[s]; t != nil {
		return t
	}
	if !m.building[s] && len(m.templates)+len(m.building) < maxTemplates && m.poolCtx.Err() == nil &&
		time.Since(m.failedAt[s]) >= templateRetry {
		m.building[s] = true
		m.builds.Add(1)
		go m.buildTemplate(s)
	}
	return nil
}

func (m *Manager) buildTemplate(s shape) {
	defer m.builds.Done()
	t0 := time.Now()
	t, err := m.makeTemplate(m.poolCtx, s)
	m.tmplMu.Lock()
	delete(m.building, s)
	if err == nil {
		t.BuildMs = ms(time.Since(t0))
		m.templates[s] = t
		_ = writeJSON(filepath.Join(t.dir, "template.json"), t)
	} else {
		m.failedAt[s] = time.Now()
	}
	m.tmplMu.Unlock()
	if err != nil {
		if m.poolCtx.Err() == nil {
			slog.Error("template build failed; creates of this shape cold-boot until a retry succeeds", "vcpus", s.vcpus, "memory_mb", s.mem, "retry_in", templateRetry, "err", err)
		}
		return
	}
	slog.Info("template ready", "vcpus", s.vcpus, "memory_mb", s.mem, "build_ms", t.BuildMs)
	m.kickPool() // the prepared pool can fill now
}

func (m *Manager) makeTemplate(ctx context.Context, s shape) (*template, error) {
	key := m.templateKey(s)
	sb, _, err := m.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer m.destroy(sb)
	err = sb.machine.Boot(ctx, vmm.BootConfig{
		VCPUs:    s.vcpus,
		MemoryMB: s.mem,
		Kernel:   m.cfg.Kernel,
		BootArgs: m.bootArgs("template-"+key[:6], ""),
		ReadOnly: true,
	})
	if err != nil {
		return nil, fmt.Errorf("boot: %w", err)
	}
	select {
	case <-sb.machine.Ready():
	case <-sb.machine.Exited():
		return nil, fmt.Errorf("template VM exited during boot; console tail:\n%s", consoleTail(sb.dir))
	case <-time.After(m.cfg.ReadyTimeout):
		return nil, fmt.Errorf("template VM not ready after %s; console tail:\n%s", m.cfg.ReadyTimeout, consoleTail(sb.dir))
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := sb.machine.Pause(ctx); err != nil {
		return nil, err
	}
	final := filepath.Join(m.templatesDir(), key)
	tmp := final + ".building"
	os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return nil, err
	}
	if err := sb.machine.Snapshot(ctx, tmp); err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	image, _ := os.Readlink(filepath.Join(sb.dir, vmm.RootfsName))
	os.RemoveAll(final)
	if err := os.Rename(tmp, final); err != nil {
		return nil, err
	}
	return &template{Key: key, VCPUs: s.vcpus, MemoryMB: s.mem, Image: image, CreatedAt: time.Now().UTC(), dir: final}, nil
}

// createFromTemplate restores t into a warm sandbox and hands the guest its identity.
func (m *Manager) createFromTemplate(ctx context.Context, t0 time.Time, t *template, req api.CreateVMRequest) (api.VM, error) {
	sb, warm, err := m.acquire(ctx)
	if err != nil {
		return api.VM{}, err
	}
	tAcq := time.Now()
	e, err := m.register(sb, req.Name, req.Ports, func(vm *api.VM) {
		vm.VCPUs, vm.MemoryMB, vm.Cmd = req.VCPUs, req.MemoryMB, req.Cmd
	})
	if err != nil {
		m.destroy(sb)
		return api.VM{}, err
	}
	e.op.Lock()
	defer e.op.Unlock()
	entropy := make([]byte, 64)
	_, _ = rand.Read(entropy)
	var tRestored time.Time
	err = func() error {
		if err := m.snapshotRootfs(sb.dir, t.dir, false, t.Image); err != nil {
			return err
		}
		if err := sb.machine.Restore(ctx, t.dir); err != nil {
			return fmt.Errorf("restore template: %w", err)
		}
		tRestored = time.Now()
		return sb.machine.Init(ctx, guest.InitMsg{
			ID: sb.id, Cmd: req.Cmd, UnixNano: time.Now().UnixNano(), Entropy: entropy,
		})
	}()
	if err != nil {
		m.failLocked(e, err)
		return m.snapshotOf(e), err
	}
	tDone := time.Now()
	m.update(e, func(vm *api.VM) {
		vm.State = api.StateRunning
		vm.Timings = api.Timings{
			Mode:    api.ModeTemplate,
			Total:   ms(tDone.Sub(t0)),
			Acquire: ms(tAcq.Sub(t0)),
			Start:   ms(tRestored.Sub(tAcq)),
			Init:    ms(tDone.Sub(tRestored)),
			Warm:    warm,
		}
	})
	return m.started(e)
}

// Templates lists ready templates as "<vcpus>x<memory_mb>".
func (m *Manager) Templates() []string {
	m.tmplMu.Lock()
	defer m.tmplMu.Unlock()
	var out []string
	for s := range m.templates {
		out = append(out, fmt.Sprintf("%dx%d", s.vcpus, s.mem))
	}
	sort.Strings(out)
	return out
}

// consoleTail returns the end of a sandbox's console log, for errors about a VM whose
// directory is about to be removed.
func consoleTail(dir string) string {
	b, _ := os.ReadFile(filepath.Join(dir, "console.log"))
	if len(b) > 2048 {
		b = b[len(b)-2048:]
	}
	return string(b)
}

func (m *Manager) defaultShape() shape {
	return shape{m.cfg.DefaultVCPUs, m.cfg.DefaultMemoryMB}
}

// fillPrepared tops up the prepared pool: default-shape template clones that already
// have their identity, clock and entropy, paused. Called from the pool loop.
func (m *Manager) fillPrepared() {
	if m.cfg.PreparedPool <= 0 {
		return
	}
	for len(m.prepared) < m.cfg.PreparedPool && m.poolCtx.Err() == nil {
		t := m.templateFor(m.defaultShape())
		if t == nil {
			return // kicked again once the template is built
		}
		sb, err := m.prepare(m.poolCtx, t)
		if err != nil {
			if m.poolCtx.Err() == nil {
				slog.Error("prepared pool: prepare failed", "err", err)
			}
			return
		}
		select {
		case m.prepared <- sb:
		default:
			m.destroy(sb)
		}
	}
}

func (m *Manager) prepare(ctx context.Context, t *template) (*sandbox, error) {
	sb, err := m.spawn(ctx)
	if err != nil {
		return nil, err
	}
	entropy := make([]byte, 64)
	_, _ = rand.Read(entropy)
	err = func() error {
		if err := m.snapshotRootfs(sb.dir, t.dir, false, t.Image); err != nil {
			return err
		}
		if err := sb.machine.Restore(ctx, t.dir); err != nil {
			return fmt.Errorf("restore template: %w", err)
		}
		if err := sb.machine.Init(ctx, guest.InitMsg{
			ID: sb.id, UnixNano: time.Now().UnixNano(), Entropy: entropy,
		}); err != nil {
			return err
		}
		// Paused, the VM costs no CPU; the guest clock keeps up because the virtual
		// counter keeps running on the host.
		return sb.machine.Pause(ctx)
	}()
	if err != nil {
		m.destroy(sb)
		return nil, err
	}
	return sb, nil
}

func (m *Manager) takePrepared() *sandbox {
	for {
		select {
		case sb := <-m.prepared:
			m.kickPool()
			select {
			case <-sb.machine.Exited():
				m.destroy(sb)
				continue
			default:
				return sb
			}
		default:
			return nil
		}
	}
}

// createPrepared turns a prepared sandbox into the requested VM: resume, then hand the
// guest its command (if any).
func (m *Manager) createPrepared(ctx context.Context, t0 time.Time, sb *sandbox, req api.CreateVMRequest) (api.VM, error) {
	tAcq := time.Now()
	e, err := m.register(sb, req.Name, req.Ports, func(vm *api.VM) {
		vm.VCPUs, vm.MemoryMB, vm.Cmd = req.VCPUs, req.MemoryMB, req.Cmd
	})
	if err != nil {
		m.destroy(sb)
		return api.VM{}, err
	}
	e.op.Lock()
	defer e.op.Unlock()
	var tResumed time.Time
	err = func() error {
		if err := sb.machine.Resume(context.WithoutCancel(ctx)); err != nil {
			return fmt.Errorf("resume: %w", err)
		}
		tResumed = time.Now()
		if req.Cmd == "" {
			return nil
		}
		return sb.machine.Init(ctx, guest.InitMsg{Cmd: req.Cmd})
	}()
	if err != nil {
		m.failLocked(e, err)
		return m.snapshotOf(e), err
	}
	tDone := time.Now()
	m.update(e, func(vm *api.VM) {
		vm.State = api.StateRunning
		vm.Timings = api.Timings{
			Mode:     api.ModeTemplate,
			Total:    ms(tDone.Sub(t0)),
			Acquire:  ms(tAcq.Sub(t0)),
			Start:    ms(tResumed.Sub(tAcq)),
			Init:     ms(tDone.Sub(tResumed)),
			Warm:     true,
			Prepared: true,
		}
	})
	return m.started(e)
}
