// Package api holds the wire types shared by the burstyd daemon and the bursty CLI.
package api

import "time"

// VM states.
const (
	StateBooting = "booting"
	StateRunning = "running"
	StatePaused  = "paused"
	StateStopped = "stopped"
	StateFailed  = "failed"
)

// Port publishes a host TCP port to a guest TCP port through the daemon's proxy.
type Port struct {
	Host  int `json:"host"`
	Guest int `json:"guest"`
}

// How a VM came up.
const (
	ModeTemplate = "template" // restored from a pre-booted template + identity via vsock
	ModeCold     = "cold"     // kernel boot
	ModeRestore  = "restore"  // restored from a user snapshot
)

// Timings records where the time went while a VM came up. All values are milliseconds.
type Timings struct {
	Mode string `json:"mode"`
	// Total is request received -> guest ready (cold boot) or -> resumed (restore).
	Total float64 `json:"total_ms"`
	// Acquire is time spent taking a warm sandbox (or spawning one if the pool was empty).
	Acquire float64 `json:"acquire_ms"`
	// Start is configure + InstanceStart (cold boot) or snapshot load (template, restore).
	Start float64 `json:"start_ms"`
	// Init is the guest agent applying the new identity (template only).
	Init float64 `json:"init_ms,omitempty"`
	// Guest is the guest kernel's own uptime when its init reported ready (cold boot only).
	Guest float64 `json:"guest_uptime_ms,omitempty"`
	// Warm is true when the VM came from the warm pool.
	Warm bool `json:"warm"`
	// Prepared is true when the VM was restored from its template and given its identity
	// ahead of time, then paused in the pool; the request only resumed it.
	Prepared bool `json:"prepared,omitempty"`
}

type VM struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	State     string    `json:"state"`
	Driver    string    `json:"driver"`
	VCPUs     int       `json:"vcpus"`
	MemoryMB  int       `json:"memory_mb"`
	Cmd       string    `json:"cmd,omitempty"`
	Writable  bool      `json:"writable"`
	IP        string    `json:"ip"`
	GuestIP   string    `json:"guest_ip"`
	MAC       string    `json:"mac"`
	Netns     string    `json:"netns,omitempty"`
	Endpoint  string    `json:"endpoint"`
	Ports     []Port    `json:"ports,omitempty"`
	PID       int       `json:"pid"`
	Snapshot  string    `json:"from_snapshot,omitempty"`
	Error     string    `json:"error,omitempty"`
	Timings   Timings   `json:"timings"`
	CreatedAt time.Time `json:"created_at"`
}

type CreateVMRequest struct {
	Name     string `json:"name,omitempty"`
	VCPUs    int    `json:"vcpus,omitempty"`
	MemoryMB int    `json:"memory_mb,omitempty"`
	// Cmd is an optional shell command the guest init runs after boot.
	Cmd string `json:"cmd,omitempty"`
	// Writable gives the VM a private copy of the rootfs instead of the shared read-only one.
	Writable bool   `json:"writable,omitempty"`
	Ports    []Port `json:"ports,omitempty"`
	// Cold boots a kernel even when a template for this shape exists.
	Cold bool `json:"cold,omitempty"`
}

type Snapshot struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	SourceVM string `json:"source_vm"`
	Driver   string `json:"driver"`
	VCPUs    int    `json:"vcpus"`
	MemoryMB int    `json:"memory_mb"`
	Writable bool   `json:"writable"`
	Cmd      string `json:"cmd,omitempty"`
	// Image is the shared rootfs a read-only VM ran on; restores must use the same one.
	Image     string    `json:"image,omitempty"`
	SizeBytes int64     `json:"size_bytes"`
	TookMs    float64   `json:"took_ms"`
	CreatedAt time.Time `json:"created_at"`
}

type CreateSnapshotRequest struct {
	Name string `json:"name,omitempty"`
	// Stop stops the source VM after the snapshot instead of resuming it.
	Stop bool `json:"stop,omitempty"`
}

type RestoreRequest struct {
	Name  string `json:"name,omitempty"`
	Ports []Port `json:"ports,omitempty"`
}

type Info struct {
	Version    string `json:"version"`
	Driver     string `json:"driver"`
	Network    string `json:"network"`
	Kernel     string `json:"kernel,omitempty"`
	Rootfs     string `json:"rootfs,omitempty"`
	DataDir    string `json:"data_dir"`
	PoolTarget int    `json:"pool_target"`
	PoolReady  int    `json:"pool_ready"`
	// PreparedTarget/PreparedReady count paused, fully initialized template clones.
	PreparedTarget int `json:"prepared_target"`
	PreparedReady  int `json:"prepared_ready"`
	// Templates lists shapes ("<vcpus>x<memory_mb>") that create from a template.
	Templates []string `json:"templates"`
	VMs       int      `json:"vms"`
	Snapshots int      `json:"snapshots"`
	HostOS    string   `json:"host_os"`
	HostArch  string   `json:"host_arch"`
}

type Error struct {
	Error string `json:"error"`
}
