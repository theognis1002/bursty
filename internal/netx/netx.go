// Package netx gives every VM a network "slot".
//
// On Linux each slot is a network namespace holding the VM's TAP device. The guest
// always sees the same addresses inside its namespace (GuestIP behind GatewayIP),
// which is what lets one snapshot be restored many times without re-addressing the
// guest. A veth pair connects the namespace to the host, and the namespace NATs the
// slot's unique host-routable IP to the guest.
package netx

import (
	"context"
	"fmt"
	"net"
	"strconv"
)

// Fixed in-namespace addressing, identical for every VM.
const (
	GuestIP   = "172.16.0.2"
	GatewayIP = "172.16.0.1"
	GuestMask = "255.255.255.252"
	TapName   = "tap0"
	GuestMAC  = "06:00:ac:10:00:02"
	// GatewayMAC is fixed so a restored guest's ARP cache stays valid in any namespace.
	GatewayMAC = "06:00:ac:10:00:01"
)

type Lease struct {
	Slot int
	// IP is the VM's unique, host-routable address.
	IP string
	// Netns is the namespace the VMM must run in; empty for drivers without one.
	Netns string
	// PortBase, when non-zero, means guest port P is reachable on 127.0.0.1:PortBase
	// (sim driver: a guest process has no IP of its own, so it serves one port).
	PortBase int
}

// Addr returns where a host process dials to reach guestPort inside the VM.
func (l Lease) Addr(guestPort int) string {
	if l.PortBase != 0 {
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(l.PortBase))
	}
	return net.JoinHostPort(l.IP, strconv.Itoa(guestPort))
}

type Provider interface {
	Name() string
	// Init prepares host-wide state (forwarding, NAT) and removes leftovers from a previous run.
	Init(ctx context.Context) error
	Acquire(ctx context.Context) (Lease, error)
	Release(l Lease)
	Close() error
}

// slotAllocator hands out the lowest free slot number.
type slotAllocator struct {
	used map[int]bool
	max  int
}

func newSlotAllocator(max int) *slotAllocator {
	return &slotAllocator{used: map[int]bool{}, max: max}
}

func (a *slotAllocator) take() (int, error) {
	for i := 0; i < a.max; i++ {
		if !a.used[i] {
			a.used[i] = true
			return i, nil
		}
	}
	return 0, fmt.Errorf("no free network slots (max %d)", a.max)
}

func (a *slotAllocator) free(i int) { delete(a.used, i) }
