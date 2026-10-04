package server_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/theognis1002/bursty/internal/api"
	"github.com/theognis1002/bursty/internal/client"
	"github.com/theognis1002/bursty/internal/netx"
	"github.com/theognis1002/bursty/internal/orchestrator"
	"github.com/theognis1002/bursty/internal/server"
	"github.com/theognis1002/bursty/internal/vmm/sim"
)

// The test binary doubles as the sim guest, like burstyd does.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == sim.GuestArg {
		sim.GuestMain(os.Args[2:])
		return
	}
	os.Exit(m.Run())
}

func newDaemon(t *testing.T, pool int) (*client.Client, *orchestrator.Manager) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := orchestrator.New(orchestrator.Config{
		DataDir:      t.TempDir(),
		PoolSize:     pool,
		ReadyTimeout: 5 * time.Second,
		Templates:    true,
		PreparedPool: 2,
	}, &sim.Driver{Exe: exe}, netx.NewSim(31000+os.Getpid()%1000*20))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(mgr))
	t.Cleanup(func() {
		srv.Close()
		mgr.Close()
	})
	return client.New(srv.URL), mgr
}

type guestReply struct {
	VMID     string `json:"vm_id"`
	BootID   string `json:"boot_id"`
	Requests int64  `json:"requests"`
}

func hit(t *testing.T, endpoint string) guestReply {
	t.Helper()
	resp, err := http.Get(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var g guestReply
	if err := json.NewDecoder(resp.Body).Decode(&g); err != nil {
		t.Fatal(err)
	}
	return g
}

func TestLifecycle(t *testing.T) {
	c, _ := newDaemon(t, 2)
	ctx := context.Background()

	vm, err := c.CreateVM(ctx, api.CreateVMRequest{Name: "web", Cmd: "echo booted-cmd"})
	if err != nil {
		t.Fatal(err)
	}
	if vm.State != api.StateRunning || vm.Timings.Total <= 0 {
		t.Fatalf("unexpected VM: %+v", vm)
	}
	if g := hit(t, vm.Endpoint); g.VMID != vm.ID || g.Requests != 1 {
		t.Fatalf("guest reply %+v", g)
	}
	if _, err := c.CreateVM(ctx, api.CreateVMRequest{Name: "web"}); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatalf("duplicate name: got %v", err)
	}
	if _, err := c.CreateVM(ctx, api.CreateVMRequest{MemoryMB: 1}); err == nil {
		t.Fatal("expected invalid memory to fail")
	}

	if vm, err = c.VMAction(ctx, "web", "pause"); err != nil || vm.State != api.StatePaused {
		t.Fatalf("pause: %v %s", err, vm.State)
	}
	if _, err := c.VMAction(ctx, "web", "pause"); err == nil {
		t.Fatal("pausing a paused VM should conflict")
	}
	if vm, err = c.VMAction(ctx, "web", "resume"); err != nil || vm.State != api.StateRunning {
		t.Fatalf("resume: %v %s", err, vm.State)
	}

	// Snapshot, then restore twice: both clones carry the source's memory (boot ID and
	// counter) yet live at their own addresses.
	hit(t, vm.Endpoint) // requests=2
	snap, err := c.CreateSnapshot(ctx, "web", api.CreateSnapshotRequest{Name: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	src := hit(t, vm.Endpoint) // source keeps running: requests=3
	for _, name := range []string{"clone-a", "clone-b"} {
		clone, err := c.Restore(ctx, "s1", api.RestoreRequest{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		if clone.Snapshot != snap.ID || clone.Endpoint == vm.Endpoint {
			t.Fatalf("clone %+v", clone)
		}
		g := hit(t, clone.Endpoint)
		if g.BootID != src.BootID || g.Requests != 3 {
			t.Fatalf("clone state %+v, want boot %s requests 3", g, src.BootID)
		}
	}

	var logs strings.Builder
	if err := c.Logs(ctx, "web", false, &logs); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "booted-cmd") {
		t.Fatalf("console missing command output: %q", logs.String())
	}

	if _, err := c.CreateSnapshot(ctx, "web", api.CreateSnapshotRequest{Name: "s2", Stop: true}); err != nil {
		t.Fatal(err)
	}
	if vm, _ = c.GetVM(ctx, "web"); vm.State != api.StateStopped {
		t.Fatalf("snapshot --stop left VM %s", vm.State)
	}
	vms, _ := c.ListVMs(ctx)
	if len(vms) != 3 {
		t.Fatalf("want 3 VMs, got %d", len(vms))
	}
	for _, v := range vms {
		if err := c.DeleteVM(ctx, v.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.DeleteSnapshot(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetVM(ctx, "web"); err == nil {
		t.Fatal("deleted VM still found")
	}
}

func TestPublishedPort(t *testing.T) {
	c, _ := newDaemon(t, 1)
	port := freePort(t)
	vm, err := c.CreateVM(context.Background(), api.CreateVMRequest{Ports: []api.Port{{Host: port, Guest: 8080}}})
	if err != nil {
		t.Fatal(err)
	}
	if g := hit(t, "http://127.0.0.1:"+strconv.Itoa(port)); g.VMID != vm.ID {
		t.Fatalf("proxied reply %+v", g)
	}
}

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestColdSpawnWithoutPool(t *testing.T) {
	c, _ := newDaemon(t, 0)
	vm, err := c.CreateVM(context.Background(), api.CreateVMRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if vm.Timings.Warm {
		t.Fatal("pool is disabled; VM must not be warm")
	}
}

func TestUnexpectedExitMarksStopped(t *testing.T) {
	c, _ := newDaemon(t, 1)
	ctx := context.Background()
	vm, err := c.CreateVM(ctx, api.CreateVMRequest{Name: "doomed"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := os.FindProcess(vm.PID)
	if err != nil {
		t.Fatal(err)
	}
	_ = p.Kill()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if vm, _ = c.GetVM(ctx, "doomed"); vm.State == api.StateStopped {
			if !strings.Contains(vm.Error, "unexpectedly") {
				t.Fatalf("error %q", vm.Error)
			}
			if err := c.DeleteVM(ctx, "doomed"); err != nil {
				t.Fatal(err)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("VM still %s after its VMM died", vm.State)
}

func TestCreateFromTemplate(t *testing.T) {
	c, _ := newDaemon(t, 2)
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for {
		info, err := c.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(info.Templates) == 1 && info.Templates[0] == "1x128" && info.PreparedReady == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no template after 10s: %+v", info)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var boots []string
	for _, name := range []string{"t1", "t2"} {
		vm, err := c.CreateVM(ctx, api.CreateVMRequest{Name: name, Cmd: "echo hi-from-" + name})
		if err != nil {
			t.Fatal(err)
		}
		if vm.Timings.Mode != api.ModeTemplate || !vm.Timings.Prepared {
			t.Fatalf("timings %+v, want a prepared template VM", vm.Timings)
		}
		g := hit(t, vm.Endpoint)
		// The clone must be a new VM, not the template: own ID, fresh boot ID and counter.
		if g.VMID != vm.ID || g.Requests != 1 {
			t.Fatalf("clone identity %+v for %s", g, vm.ID)
		}
		boots = append(boots, g.BootID)
		var logs strings.Builder
		for i := 0; i < 50 && !strings.Contains(logs.String(), "hi-from-"+name); i++ {
			logs.Reset()
			_ = c.Logs(ctx, name, false, &logs)
			time.Sleep(20 * time.Millisecond)
		}
		if !strings.Contains(logs.String(), "hi-from-"+name) {
			t.Fatalf("cmd did not run: %q", logs.String())
		}
	}
	if boots[0] == boots[1] {
		t.Fatal("two template clones share a boot ID")
	}
	vm, err := c.CreateVM(ctx, api.CreateVMRequest{Cold: true})
	if err != nil {
		t.Fatal(err)
	}
	if vm.Timings.Mode != api.ModeCold {
		t.Fatalf("--cold gave mode %q", vm.Timings.Mode)
	}
	vm, err = c.CreateVM(ctx, api.CreateVMRequest{Writable: true})
	if err != nil || vm.Timings.Mode != api.ModeCold {
		t.Fatalf("writable: %v mode %q", err, vm.Timings.Mode)
	}
}
