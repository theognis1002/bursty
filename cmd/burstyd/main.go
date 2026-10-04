// burstyd is the bursty host daemon: it runs microVMs and serves the HTTP API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/theognis1002/bursty/internal/netx"
	"github.com/theognis1002/bursty/internal/orchestrator"
	"github.com/theognis1002/bursty/internal/server"
	"github.com/theognis1002/bursty/internal/vmm"
	"github.com/theognis1002/bursty/internal/vmm/firecracker"
	"github.com/theognis1002/bursty/internal/vmm/sim"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == sim.GuestArg {
		sim.GuestMain(os.Args[2:])
		return
	}
	if err := run(); err != nil {
		slog.Error("burstyd", "err", err)
		os.Exit(1)
	}
}

func run() error {
	images := "/var/lib/bursty/images"
	var (
		listen       = flag.String("listen", "127.0.0.1:7070", "API listen address")
		dataDir      = flag.String("data-dir", defaultDataDir(), "state directory (VMs, snapshots, prepared images)")
		driverName   = flag.String("driver", "auto", "VMM driver: auto, firecracker or sim")
		fcBin        = flag.String("firecracker", "firecracker", "firecracker binary")
		kernel       = flag.String("kernel", filepath.Join(images, "vmlinux"), "guest kernel (uncompressed vmlinux)")
		rootfs       = flag.String("rootfs", filepath.Join(images, "rootfs.ext4"), "base guest rootfs (ext4)")
		initBin      = flag.String("init", "", "bursty-init binary for the guest's architecture (default: bursty-init next to burstyd)")
		bootArgs     = flag.String("boot-args", defaultBootArgs(), "guest kernel command line (network and init args are appended)")
		pool         = flag.Int("pool", 3, "warm sandboxes kept ready (0 disables the pool)")
		templates    = flag.Bool("templates", true, "create VMs by restoring a pre-booted template (use --templates=false to always cold-boot)")
		prepared     = flag.Int("prepared", 2, "default-shape VMs kept restored from the template, initialized and paused (needs --templates)")
		readyTimeout = flag.Duration("ready-timeout", 10*time.Second, "max time for a guest to report ready")
		publishAddr  = flag.String("publish-addr", "127.0.0.1", "host address that published VM ports listen on")
		maxSlots     = flag.Int("max-vms", 1024, "max concurrent network slots (netns)")
		simDelay     = flag.Duration("sim-boot-delay", 0, "sim driver: simulated guest kernel boot time")
		simPorts     = flag.Int("sim-port-base", 20000, "sim driver: first loopback port for guest services")
		verbose      = flag.Bool("v", false, "debug logging")
	)
	flag.Parse()
	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		return err
	}
	*dataDir, _ = filepath.Abs(*dataDir)

	var (
		driver vmm.Driver
		nw     netx.Provider
		cfg    = orchestrator.Config{
			DataDir:      *dataDir,
			BootArgs:     *bootArgs + " init=" + orchestrator.InitPath,
			PoolSize:     *pool,
			Templates:    *templates,
			PreparedPool: *prepared,
			ReadyTimeout: *readyTimeout,
			PublishAddr:  *publishAddr,
			Version:      version,
		}
	)
	name := *driverName
	if name == "auto" {
		name = autoDriver(*fcBin)
	}
	switch name {
	case "firecracker":
		d, err := firecracker.New(*fcBin)
		if err != nil {
			return err
		}
		driver, nw = d, netx.NewNetns(*maxSlots)
		if cfg.Kernel, err = filepath.Abs(*kernel); err != nil {
			return err
		}
		if _, err := os.Stat(cfg.Kernel); err != nil {
			return fmt.Errorf("guest kernel: %w (run scripts/setup-linux.sh)", err)
		}
		if *initBin == "" {
			exe, _ := os.Executable()
			*initBin = filepath.Join(filepath.Dir(exe), "bursty-init")
		}
		slog.Info("preparing rootfs", "base", *rootfs, "init", *initBin)
		if cfg.Rootfs, err = orchestrator.PrepareRootfs(*rootfs, *initBin, *dataDir); err != nil {
			return err
		}
	case "sim":
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		driver, nw = &sim.Driver{Exe: exe, BootDelay: *simDelay}, netx.NewSim(*simPorts)
		slog.Warn("using the sim driver: VMs are host processes, not microVMs (no KVM here)")
	default:
		return fmt.Errorf("unknown driver %q", name)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := nw.Init(ctx); err != nil {
		return err
	}
	mgr, err := orchestrator.New(cfg, driver, nw)
	if err != nil {
		return err
	}
	defer mgr.Close()

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: server.New(mgr), ReadHeaderTimeout: 10 * time.Second}
	slog.Info("burstyd up", "listen", ln.Addr().String(), "driver", driver.Name(), "network", nw.Name(),
		"data", *dataDir, "pool", *pool)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		slog.Info("shutting down")
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	return nil
}

func autoDriver(fcBin string) string {
	if runtime.GOOS != "linux" {
		return "sim"
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return "sim"
	}
	if _, err := exec.LookPath(fcBin); err != nil {
		return "sim"
	}
	return "firecracker"
}

func defaultDataDir() string {
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		return "/var/lib/bursty"
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".bursty")
}

func defaultBootArgs() string {
	// quiet keeps the serial console (slow, emulated UART) out of the boot path.
	args := "console=ttyS0 reboot=k panic=1 pci=off quiet loglevel=1"
	if runtime.GOARCH == "amd64" {
		args += " i8042.noaux i8042.nomux i8042.dumbkbd i8042.nopnp"
	}
	return args
}
