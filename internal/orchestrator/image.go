package orchestrator

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// InitPath is where bursty-init lives inside every prepared rootfs (boot with init=InitPath).
const InitPath = "/bursty-init"

// PrepareRootfs returns a copy of base with bursty-init written in at InitPath, cached
// under dataDir/images by content hash. debugfs edits the ext4 image directly, so this
// needs neither root nor a loop mount.
func PrepareRootfs(base, initBin, dataDir string) (string, error) {
	if _, err := exec.LookPath("debugfs"); err != nil {
		return "", fmt.Errorf("preparing the rootfs needs debugfs (e2fsprogs): %w", err)
	}
	h := sha256.New()
	f, err := os.Open(initBin)
	if err != nil {
		return "", fmt.Errorf("bursty-init: %w", err)
	}
	_, err = io.Copy(h, f)
	f.Close()
	if err != nil {
		return "", err
	}
	st, err := os.Stat(base)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(h, "%s|%d|%d", base, st.Size(), st.ModTime().UnixNano())
	dir := filepath.Join(dataDir, "images")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	out := filepath.Join(dir, "rootfs-"+hex.EncodeToString(h.Sum(nil))[:12]+".ext4")
	if _, err := os.Stat(out); err == nil {
		return out, nil
	}
	tmp := out + ".tmp"
	if err := copyFile(base, tmp); err != nil {
		return "", err
	}
	script := fmt.Sprintf("rm %s\nwrite %s %s\nsif %s mode 0100755\n", InitPath, initBin, InitPath, InitPath)
	cmd := exec.Command("debugfs", "-w", "-f", "-", tmp)
	cmd.Stdin = strings.NewReader(script)
	if b, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("debugfs: %w: %s", err, b)
	}
	// debugfs exits 0 even when a request fails; verify the result.
	b, _ := exec.Command("debugfs", "-R", "stat "+InitPath, tmp).CombinedOutput()
	if !strings.Contains(string(b), "Type: regular") {
		os.Remove(tmp)
		return "", fmt.Errorf("debugfs did not install %s: %s", InitPath, b)
	}
	return out, os.Rename(tmp, out)
}
