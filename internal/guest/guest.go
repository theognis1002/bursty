// Package guest is the small HTTP service every VM runs (bursty-init inside a real
// microVM, or the sim guest process). Its in-memory counter is a cheap way to prove a
// snapshot really captured guest memory: a restored VM keeps counting from where the
// source left off, with the same boot ID.
package guest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"sync"
	"time"
)

// Port is the guest port the service listens on.
const Port = 8080

type State struct {
	VMID     string `json:"vm_id"`
	BootID   string `json:"boot_id"`
	Requests int64  `json:"requests"`
}

type Service struct {
	mu    sync.Mutex
	state State
	boot  time.Time
	// OnChange, if set, is called with the new state after every request (sim persistence).
	OnChange func(State)
	// Uptime overrides the uptime source (real guests read /proc/uptime).
	Uptime func() float64
}

func New(vmID string) *Service {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return &Service{state: State{VMID: vmID, BootID: hex.EncodeToString(b)}, boot: time.Now()}
}

// VsockPort is where the guest agent listens for host messages over vsock.
const VsockPort = 52

// InitMsg turns a VM restored from a template into a new VM: new identity, correct
// clock, fresh entropy, and the user's command.
type InitMsg struct {
	ID string `json:"id"`
	// Cmd is a shell command to start; empty for none.
	Cmd string `json:"cmd,omitempty"`
	// UnixNano is the host wall clock when the message was sent.
	UnixNano int64 `json:"unix_nano"`
	// Entropy is mixed into the guest RNG so clones of one snapshot diverge.
	Entropy []byte `json:"entropy"`
}

type InitReply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Reinit gives the service a new identity (a new VM cloned from a template).
func (s *Service) Reinit(vmID string) {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	s.mu.Lock()
	s.state = State{VMID: vmID, BootID: hex.EncodeToString(b)}
	s.boot = time.Now()
	s.mu.Unlock()
	if s.OnChange != nil {
		s.OnChange(s.State())
	}
}

// FromState resumes a service from saved state (sim restore).
func FromState(s State) *Service {
	return &Service{state: s, boot: time.Now()}
}

func (s *Service) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.state.Requests++
	st, boot := s.state, s.boot
	s.mu.Unlock()
	if s.OnChange != nil {
		s.OnChange(st)
	}
	uptime := time.Since(boot).Seconds()
	if s.Uptime != nil {
		uptime = s.Uptime()
	}
	host, _ := os.Hostname()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"hello":    "from a bursty VM",
		"vm_id":    st.VMID,
		"boot_id":  st.BootID,
		"requests": st.Requests,
		"uptime_s": uptime,
		"hostname": host,
	})
}
