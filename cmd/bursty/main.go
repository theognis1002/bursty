// bursty is the bursty CLI.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/theognis1002/bursty/internal/api"
	"github.com/theognis1002/bursty/internal/client"
)

const usage = `bursty - on-demand Firecracker microVMs

Usage: bursty [--api ADDR] <command> [args]

VMs:
  run   [--name N] [--cpus N] [--mem MB] [--cmd CMD] [--writable] [--cold] [-p HOST:GUEST]...
                              start a new microVM (waits until the guest is ready);
                              from a pre-booted template when one exists, else (or
                              with --cold / --writable) by booting the kernel
  ls                          list VMs
  get   <vm>                  show a VM as JSON
  logs  [-f] <vm>             print (or follow) the serial console
  curl  <vm> [path]           GET the guest's HTTP service (port 8080)
  pause <vm>... / resume <vm>... / stop <vm>... / rm <vm>...

Snapshots:
  snapshot <vm> [--name N] [--stop]   pause, snapshot memory+state, resume (or stop)
  snapshots                           list snapshots
  restore <snapshot> [--name N] [-p HOST:GUEST]...
                                      start a new VM from a snapshot
  rmsnap <snapshot>...

Other:
  info                        daemon, driver and pool status
  bench [-n N]                measure cold boot and snapshot restore latency

The API address defaults to $BURSTY_API or 127.0.0.1:7070.
`

func main() {
	api := os.Getenv("BURSTY_API")
	if api == "" {
		api = "127.0.0.1:7070"
	}
	args := os.Args[1:]
	if len(args) >= 2 && (args[0] == "--api" || args[0] == "-api") {
		api, args = args[1], args[2:]
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(usage)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	c := &cli{c: client.New(api), ctx: ctx, out: os.Stdout}
	if err := c.run(args[0], args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type cli struct {
	c   *client.Client
	ctx context.Context
	out io.Writer
}

func (c *cli) run(cmd string, args []string) error {
	switch cmd {
	case "info":
		info, err := c.c.Info(c.ctx)
		if err != nil {
			return err
		}
		return c.json(info)
	case "run", "create":
		return c.create(args)
	case "ls", "ps", "list":
		return c.ls()
	case "get", "inspect":
		if len(args) != 1 {
			return errors.New("usage: bursty get <vm>")
		}
		vm, err := c.c.GetVM(c.ctx, args[0])
		if err != nil {
			return err
		}
		return c.json(vm)
	case "logs":
		fs := flag.NewFlagSet("logs", flag.ContinueOnError)
		follow := fs.Bool("f", false, "follow")
		pos, err := parse(fs, args)
		if err != nil || len(pos) != 1 {
			return errors.New("usage: bursty logs [-f] <vm>")
		}
		return c.c.Logs(c.ctx, pos[0], *follow, c.out)
	case "curl":
		return c.curl(args)
	case "pause", "resume", "stop":
		return c.each(args, func(id string) error {
			vm, err := c.c.VMAction(c.ctx, id, cmd)
			if err == nil {
				fmt.Fprintf(c.out, "%s %s\n", vm.Name, vm.State)
			}
			return err
		})
	case "rm", "delete":
		return c.each(args, func(id string) error {
			err := c.c.DeleteVM(c.ctx, id)
			if err == nil {
				fmt.Fprintf(c.out, "%s deleted\n", id)
			}
			return err
		})
	case "snapshot":
		return c.snapshot(args)
	case "snapshots":
		return c.snapshots()
	case "restore":
		return c.restore(args)
	case "rmsnap":
		return c.each(args, func(id string) error {
			err := c.c.DeleteSnapshot(c.ctx, id)
			if err == nil {
				fmt.Fprintf(c.out, "%s deleted\n", id)
			}
			return err
		})
	case "bench":
		return c.bench(args)
	}
	return fmt.Errorf("unknown command %q (see bursty help)", cmd)
}

func (c *cli) create(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var req api.CreateVMRequest
	fs.StringVar(&req.Name, "name", "", "VM name")
	fs.IntVar(&req.VCPUs, "cpus", 0, "vCPUs (default 1)")
	fs.IntVar(&req.MemoryMB, "mem", 0, "memory in MiB (default 128)")
	fs.StringVar(&req.Cmd, "cmd", "", "shell command the guest runs after boot")
	fs.BoolVar(&req.Writable, "writable", false, "private writable rootfs copy (default: shared read-only root)")
	fs.BoolVar(&req.Cold, "cold", false, "boot the kernel even if a template exists")
	ports := portFlag{}
	fs.Var(&ports, "p", "publish HOST:GUEST port (repeatable)")
	asJSON := fs.Bool("json", false, "print JSON")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	req.Ports = ports
	vm, err := c.c.CreateVM(c.ctx, req)
	if err != nil {
		return err
	}
	if *asJSON {
		return c.json(vm)
	}
	verb := "booted"
	if vm.Timings.Mode == api.ModeTemplate {
		verb = "started"
	}
	c.printUp(vm, verb)
	return nil
}

func (c *cli) printUp(vm api.VM, verb string) {
	t := vm.Timings
	warm := "cold sandbox"
	if t.Warm {
		warm = "warm pool"
	}
	fmt.Fprintf(c.out, "%s %s (%s) in %.1fms  [%s: acquire %.1fms (%s), start %.1fms", vm.Name, verb, vm.ID, t.Total, t.Mode, t.Acquire, warm, t.Start)
	if t.Init > 0 {
		fmt.Fprintf(c.out, ", guest init %.1fms", t.Init)
	}
	if t.Guest > 0 {
		fmt.Fprintf(c.out, ", guest kernel→ready %.1fms", t.Guest)
	}
	fmt.Fprintf(c.out, "]\n  ip %s  endpoint %s  driver %s\n", vm.IP, vm.Endpoint, vm.Driver)
	for _, p := range vm.Ports {
		fmt.Fprintf(c.out, "  published %d -> %d\n", p.Host, p.Guest)
	}
}

func (c *cli) ls() error {
	vms, err := c.c.ListVMs(c.ctx)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(c.out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATE\tIP\tCPU\tMEM\tBOOT\tFROM\tAGE")
	for _, vm := range vms {
		boot := "-"
		if vm.Timings.Total > 0 {
			boot = fmt.Sprintf("%.1fms", vm.Timings.Total)
		}
		from := vm.Snapshot
		if from == "" {
			from = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\t%s\n", vm.ID, vm.Name, vm.State, vm.IP, vm.VCPUs, vm.MemoryMB, boot, from, age(vm.CreatedAt))
	}
	return tw.Flush()
}

func (c *cli) curl(args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return errors.New("usage: bursty curl <vm> [path]")
	}
	vm, err := c.c.GetVM(c.ctx, args[0])
	if err != nil {
		return err
	}
	path := "/"
	if len(args) == 2 {
		path = "/" + strings.TrimPrefix(args[1], "/")
	}
	hc := &http.Client{Timeout: 5 * time.Second}
	resp, err := hc.Get(vm.Endpoint + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(c.out, resp.Body)
	return err
}

func (c *cli) snapshot(args []string) error {
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	var req api.CreateSnapshotRequest
	fs.StringVar(&req.Name, "name", "", "snapshot name")
	fs.BoolVar(&req.Stop, "stop", false, "stop the VM after the snapshot")
	pos, err := parse(fs, args)
	if err != nil || len(pos) != 1 {
		return errors.New("usage: bursty snapshot <vm> [--name N] [--stop]")
	}
	s, err := c.c.CreateSnapshot(c.ctx, pos[0], req)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "snapshot %s (%s) of %s in %.1fms, %s\n", s.Name, s.ID, s.SourceVM, s.TookMs, size(s.SizeBytes))
	return nil
}

func (c *cli) snapshots() error {
	snaps, err := c.c.ListSnapshots(c.ctx)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(c.out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSOURCE\tCPU\tMEM\tSIZE\tTOOK\tAGE")
	for _, s := range snaps {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\t%.1fms\t%s\n", s.ID, s.Name, s.SourceVM, s.VCPUs, s.MemoryMB, size(s.SizeBytes), s.TookMs, age(s.CreatedAt))
	}
	return tw.Flush()
}

func (c *cli) restore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	var req api.RestoreRequest
	fs.StringVar(&req.Name, "name", "", "new VM name")
	ports := portFlag{}
	fs.Var(&ports, "p", "publish HOST:GUEST port (repeatable)")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parse(fs, args)
	if err != nil || len(pos) != 1 {
		return errors.New("usage: bursty restore <snapshot> [--name N] [-p HOST:GUEST]")
	}
	req.Ports = ports
	vm, err := c.c.Restore(c.ctx, pos[0], req)
	if err != nil {
		return err
	}
	if *asJSON {
		return c.json(vm)
	}
	c.printUp(vm, "restored")
	return nil
}

// bench boots N VMs one after another, then restores a snapshot N times, and reports
// latency percentiles. Every VM and the snapshot are deleted afterwards.
func (c *cli) bench(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	n := fs.Int("n", 10, "iterations per phase")
	gap := fs.Duration("gap", 300*time.Millisecond, "pause between iterations (lets the warm pool refill)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if *n < 1 {
		return errors.New("bench: -n must be at least 1")
	}
	info, err := c.c.Info(c.ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "driver=%s pool=%d host=%s/%s n=%d\n", info.Driver, info.PoolTarget, info.HostOS, info.HostArch, *n)
	var created []string
	defer func() {
		ctx := context.Background()
		for _, id := range created {
			_ = c.c.DeleteVM(ctx, id)
		}
	}()
	var tmpl, boot, guestBoot, restore []float64
	warmTmpl, warmBoots, warmRestores := 0, 0, 0
	if len(info.Templates) == 0 {
		fmt.Fprintln(c.out, "no template ready yet (burstyd builds one at startup); skipping the template phase")
	}
	for i := 0; i < *n && len(info.Templates) > 0; i++ {
		vm, err := c.c.CreateVM(c.ctx, api.CreateVMRequest{Name: fmt.Sprintf("bench-tmpl-%d-%d", time.Now().Unix(), i)})
		if err != nil {
			return err
		}
		created = append(created, vm.ID)
		if vm.Timings.Mode != api.ModeTemplate {
			return fmt.Errorf("expected a template create, got %q", vm.Timings.Mode)
		}
		tmpl = append(tmpl, vm.Timings.Total)
		if vm.Timings.Prepared {
			warmTmpl++
		}
		_ = c.c.DeleteVM(c.ctx, vm.ID)
		created = created[:len(created)-1]
		time.Sleep(*gap)
	}
	for i := 0; i < *n; i++ {
		vm, err := c.c.CreateVM(c.ctx, api.CreateVMRequest{Name: fmt.Sprintf("bench-boot-%d-%d", time.Now().Unix(), i), Cold: true})
		if err != nil {
			return err
		}
		created = append(created, vm.ID)
		boot = append(boot, vm.Timings.Total)
		if vm.Timings.Guest > 0 {
			guestBoot = append(guestBoot, vm.Timings.Guest)
		}
		if vm.Timings.Warm {
			warmBoots++
		}
		time.Sleep(*gap)
	}
	snap, err := c.c.CreateSnapshot(c.ctx, created[0], api.CreateSnapshotRequest{Name: fmt.Sprintf("bench-%d", time.Now().UnixNano())})
	if err != nil {
		return err
	}
	defer func() { _ = c.c.DeleteSnapshot(context.Background(), snap.ID) }()
	for _, id := range created {
		_ = c.c.DeleteVM(c.ctx, id)
	}
	created = created[:0]
	time.Sleep(*gap)
	for i := 0; i < *n; i++ {
		vm, err := c.c.Restore(c.ctx, snap.ID, api.RestoreRequest{Name: fmt.Sprintf("bench-restore-%d-%d", time.Now().Unix(), i)})
		if err != nil {
			return err
		}
		created = append(created, vm.ID)
		restore = append(restore, vm.Timings.Total)
		if vm.Timings.Warm {
			warmRestores++
		}
		time.Sleep(*gap)
	}
	fmt.Fprintf(c.out, "snapshot create: %.1fms (%s)\n", snap.TookMs, size(snap.SizeBytes))
	tw := tabwriter.NewWriter(c.out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PHASE\tN\tWARM\tP50\tP95\tMIN\tMAX\t<200ms")
	row := func(name string, xs []float64, warm int) {
		if len(xs) == 0 {
			return
		}
		s := append([]float64(nil), xs...)
		sort.Float64s(s)
		under := 0
		for _, x := range s {
			if x < 200 {
				under++
			}
		}
		w := "-"
		if warm >= 0 {
			w = strconv.Itoa(warm)
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%.1fms\t%.1fms\t%.1fms\t%.1fms\t%d/%d\n", name, len(s), w,
			pct(s, 50), pct(s, 95), s[0], s[len(s)-1], under, len(s))
	}
	row("create from template (WARM=prepared)", tmpl, warmTmpl)
	row("cold boot (API→guest ready)", boot, warmBoots)
	row("  guest kernel→init ready", guestBoot, -1)
	row("snapshot restore", restore, warmRestores)
	return tw.Flush()
}

func pct(sorted []float64, p int) float64 {
	i := (len(sorted)*p+99)/100 - 1
	return sorted[max(0, min(i, len(sorted)-1))]
}

func (c *cli) each(ids []string, f func(string) error) error {
	if len(ids) == 0 {
		return errors.New("need at least one VM or snapshot")
	}
	var errs []error
	for _, id := range ids {
		if err := f(id); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

func (c *cli) json(v any) error {
	enc := json.NewEncoder(c.out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// parse lets flags and positional args interleave (bursty snapshot web --stop).
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

type portFlag []api.Port

func (p *portFlag) String() string { return fmt.Sprint(*p) }

func (p *portFlag) Set(s string) error {
	h, g, ok := strings.Cut(s, ":")
	if !ok {
		g = h
	}
	hp, err1 := strconv.Atoi(h)
	gp, err2 := strconv.Atoi(g)
	if err1 != nil || err2 != nil || hp < 1 || hp > 65535 || gp < 1 || gp > 65535 {
		return fmt.Errorf("bad port %q, want HOST:GUEST", s)
	}
	*p = append(*p, api.Port{Host: hp, Guest: gp})
	return nil
}

func age(t time.Time) string {
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return d.String()
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}
