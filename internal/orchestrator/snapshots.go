package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/theognis1002/bursty/internal/api"
	"github.com/theognis1002/bursty/internal/vmm"
)

// CreateSnapshot pauses the VM, writes a full snapshot (vmstate + guest memory, plus the
// disk if the VM has a private one), then resumes the VM (or stops it if req.Stop).
func (m *Manager) CreateSnapshot(ctx context.Context, vmRef string, req api.CreateSnapshotRequest) (api.Snapshot, error) {
	e, err := m.lookup(vmRef)
	if err != nil {
		return api.Snapshot{}, err
	}
	e.op.Lock()
	defer e.op.Unlock()
	vm := m.snapshotOf(e)
	if vm.State != api.StateRunning && vm.State != api.StatePaused {
		return api.Snapshot{}, fmt.Errorf("%w: VM is %s; only running or paused VMs can be snapshotted", ErrConflict, vm.State)
	}
	id := newID()
	if req.Name == "" {
		req.Name = vm.Name + "-" + id
	}
	if !nameRE.MatchString(req.Name) {
		return api.Snapshot{}, fmt.Errorf("%w: snapshot name %q must match %s", ErrInvalid, req.Name, nameRE)
	}
	m.mu.Lock()
	for _, s := range m.snaps {
		if s.Name == req.Name {
			m.mu.Unlock()
			return api.Snapshot{}, fmt.Errorf("%w: a snapshot named %q exists", ErrConflict, req.Name)
		}
	}
	m.mu.Unlock()

	t0 := time.Now()
	mc := e.sb.machine
	// Once the VM is paused, finish (and resume) even if the client goes away.
	ctx = context.WithoutCancel(ctx)
	if vm.State == api.StateRunning {
		if err := mc.Pause(ctx); err != nil {
			return api.Snapshot{}, fmt.Errorf("pause: %w", err)
		}
	}
	dir := filepath.Join(m.cfg.DataDir, "snapshots", id)
	err = func() error {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := mc.Snapshot(ctx, dir); err != nil {
			return err
		}
		if vm.Writable {
			return copyFile(filepath.Join(e.sb.dir, vmm.RootfsName), filepath.Join(dir, vmm.RootfsName))
		}
		return nil
	}()
	// Leave the VM as we found it (or stopped, if asked), even when the snapshot failed.
	switch {
	case req.Stop && err == nil:
		m.stopLocked(e, api.StateStopped)
	case vm.State == api.StateRunning:
		if rerr := mc.Resume(ctx); rerr != nil && err == nil {
			err = fmt.Errorf("resume after snapshot: %w", rerr)
		}
	}
	if err != nil {
		os.RemoveAll(dir)
		return api.Snapshot{}, fmt.Errorf("snapshot: %w", err)
	}
	image := ""
	if !vm.Writable {
		image, _ = os.Readlink(filepath.Join(e.sb.dir, vmm.RootfsName))
	}
	snap := &api.Snapshot{
		ID:        id,
		Name:      req.Name,
		SourceVM:  vm.ID,
		Driver:    m.driver.Name(),
		VCPUs:     vm.VCPUs,
		MemoryMB:  vm.MemoryMB,
		Writable:  vm.Writable,
		Cmd:       vm.Cmd,
		Image:     image,
		SizeBytes: diskUsage(dir),
		TookMs:    ms(time.Since(t0)),
		CreatedAt: time.Now().UTC(),
	}
	if err := writeJSON(filepath.Join(dir, "snapshot.json"), snap); err != nil {
		os.RemoveAll(dir)
		return api.Snapshot{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.snaps {
		if s.Name == snap.Name {
			os.RemoveAll(dir)
			return api.Snapshot{}, fmt.Errorf("%w: a snapshot named %q exists", ErrConflict, snap.Name)
		}
	}
	m.snaps[id] = snap
	return *snap, nil
}

// RestoreSnapshot starts a new VM from a snapshot. Every VM sees identical in-namespace
// addressing, so one snapshot can be restored any number of times concurrently.
func (m *Manager) RestoreSnapshot(ctx context.Context, ref string, req api.RestoreRequest) (api.VM, error) {
	t0 := time.Now()
	snap, err := m.lookupSnap(ref)
	if err != nil {
		return api.VM{}, err
	}
	if snap.Driver != m.driver.Name() {
		return api.VM{}, fmt.Errorf("%w: snapshot was taken with driver %q, daemon runs %q", ErrConflict, snap.Driver, m.driver.Name())
	}
	if err := m.checkNew(req.Name, req.Ports); err != nil {
		return api.VM{}, err
	}
	sb, warm, err := m.acquire(ctx)
	if err != nil {
		return api.VM{}, err
	}
	tAcq := time.Now()
	e, err := m.register(sb, req.Name, req.Ports, func(vm *api.VM) {
		vm.VCPUs, vm.MemoryMB, vm.Cmd, vm.Writable = snap.VCPUs, snap.MemoryMB, snap.Cmd, snap.Writable
		vm.Snapshot = snap.ID
	})
	if err != nil {
		m.destroy(sb)
		return api.VM{}, err
	}
	e.op.Lock()
	defer e.op.Unlock()
	snapDir := filepath.Join(m.cfg.DataDir, "snapshots", snap.ID)
	err = func() error {
		if err := m.snapshotRootfs(sb.dir, snapDir, snap.Writable, snap.Image); err != nil {
			return err
		}
		return sb.machine.Restore(ctx, snapDir)
	}()
	if err != nil {
		err = fmt.Errorf("restore: %w", err)
		m.failLocked(e, err)
		return m.snapshotOf(e), err
	}
	tDone := time.Now()
	m.update(e, func(vm *api.VM) {
		vm.State = api.StateRunning
		vm.Timings = api.Timings{
			Mode:    api.ModeRestore,
			Total:   ms(tDone.Sub(t0)),
			Acquire: ms(tAcq.Sub(t0)),
			Start:   ms(tDone.Sub(tAcq)),
			Warm:    warm,
		}
	})
	return m.started(e)
}

func (m *Manager) ListSnapshots() []api.Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]api.Snapshot, 0, len(m.snaps))
	for _, s := range m.snaps {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func (m *Manager) GetSnapshot(ref string) (api.Snapshot, error) {
	s, err := m.lookupSnap(ref)
	if err != nil {
		return api.Snapshot{}, err
	}
	return *s, nil
}

func (m *Manager) DeleteSnapshot(ref string) error {
	s, err := m.lookupSnap(ref)
	if err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.snaps, s.ID)
	m.mu.Unlock()
	// Restored Firecracker VMs mmap the memory file; unlinking it is safe for them.
	return os.RemoveAll(filepath.Join(m.cfg.DataDir, "snapshots", s.ID))
}

func (m *Manager) lookupSnap(ref string) (*api.Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.snaps[ref]; ok {
		return s, nil
	}
	for _, s := range m.snaps {
		if s.Name == ref {
			return s, nil
		}
	}
	return nil, fmt.Errorf("%w: snapshot %q", ErrNotFound, ref)
}

// snapshotRootfs points the sandbox at the disk the snapshot's memory expects: its own
// copy for writable VMs, otherwise the exact shared image it ran on (which may predate
// the daemon's current one, e.g. after bursty-init changed).
func (m *Manager) snapshotRootfs(dir, snapDir string, writable bool, image string) error {
	if writable {
		return m.privateRootfs(dir, filepath.Join(snapDir, vmm.RootfsName))
	}
	if image == "" {
		return nil
	}
	if _, err := os.Stat(image); err != nil {
		return fmt.Errorf("snapshot's rootfs image is gone: %w", err)
	}
	link := filepath.Join(dir, vmm.RootfsName)
	if cur, _ := os.Readlink(link); cur == image {
		return nil
	}
	if err := os.Remove(link); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.Symlink(image, link)
}

// privateRootfs replaces the VM dir's rootfs symlink with a private copy of src.
func (m *Manager) privateRootfs(dir, src string) error {
	if src == "" {
		return nil // sim driver: no disk image
	}
	dst := filepath.Join(dir, vmm.RootfsName)
	if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return copyFile(src, dst)
}

func diskUsage(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}
