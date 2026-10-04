package orchestrator

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/theognis1002/bursty/internal/api"
)

// persist writes the VM record to vms/<id>/vm.json.
func (m *Manager) persist(e *entry) {
	vm := m.snapshotOf(e)
	if err := writeJSON(filepath.Join(m.cfg.DataDir, "vms", vm.ID, "vm.json"), vm); err != nil {
		slog.Warn("persist VM", "id", vm.ID, "err", err)
	}
}

// load restores records from disk. VMMs die with the daemon, so live VMs come back as
// stopped; their snapshots (if any) are how to bring them back.
func (m *Manager) load() error {
	vmDirs, err := os.ReadDir(filepath.Join(m.cfg.DataDir, "vms"))
	if err != nil {
		return err
	}
	for _, d := range vmDirs {
		dir := filepath.Join(m.cfg.DataDir, "vms", d.Name())
		var vm api.VM
		if err := readJSON(filepath.Join(dir, "vm.json"), &vm); err != nil {
			os.RemoveAll(dir) // a pooled sandbox that never became a VM
			continue
		}
		if vm.State != api.StateStopped && vm.State != api.StateFailed {
			vm.State = api.StateStopped
			vm.Error = "daemon restarted"
		}
		vm.PID = 0
		m.vms[vm.ID] = &entry{vm: vm}
	}
	snapDirs, err := os.ReadDir(filepath.Join(m.cfg.DataDir, "snapshots"))
	if err != nil {
		return err
	}
	for _, d := range snapDirs {
		var s api.Snapshot
		if err := readJSON(filepath.Join(m.cfg.DataDir, "snapshots", d.Name(), "snapshot.json"), &s); err != nil {
			continue
		}
		m.snaps[s.ID] = &s
	}
	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// copyFile copies a (possibly large, sparse) disk image, using reflinks where the
// filesystem supports them.
func copyFile(src, dst string) error {
	if runtime.GOOS == "linux" {
		if err := exec.Command("cp", "--reflink=auto", "--sparse=always", src, dst).Run(); err == nil {
			return nil
		}
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
