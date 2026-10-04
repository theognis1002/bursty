package netx

import (
	"context"
	"fmt"
	"sync"
)

// Sim is the network provider for the sim driver: no kernel networking, just a unique
// "IP" per slot for display and a loopback port where the guest process listens.
type Sim struct {
	mu       sync.Mutex
	slots    *slotAllocator
	portBase int
}

func NewSim(portBase int) *Sim {
	return &Sim{slots: newSlotAllocator(1000), portBase: portBase}
}

func (s *Sim) Name() string                   { return "sim (loopback ports)" }
func (s *Sim) Init(ctx context.Context) error { return nil }
func (s *Sim) Close() error                   { return nil }

func (s *Sim) Acquire(ctx context.Context) (Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	slot, err := s.slots.take()
	if err != nil {
		return Lease{}, err
	}
	return Lease{Slot: slot, IP: slotIP(slot, 2), PortBase: s.portBase + slot}, nil
}

func (s *Sim) Release(l Lease) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.slots.free(l.Slot)
}

// slotIP returns host (+1) or namespace (+2) address of slot's /30 inside 10.201.0.0/16.
func slotIP(slot, offset int) string {
	n := slot*4 + offset
	return fmt.Sprintf("10.201.%d.%d", n/256, n%256)
}
