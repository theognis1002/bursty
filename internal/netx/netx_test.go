package netx

import (
	"context"
	"testing"
)

func TestSlotIPs(t *testing.T) {
	cases := []struct {
		slot, off int
		want      string
	}{
		{0, 1, "10.201.0.1"}, {0, 2, "10.201.0.2"},
		{1, 2, "10.201.0.6"}, {64, 1, "10.201.1.1"},
		{16383, 2, "10.201.255.254"},
	}
	for _, c := range cases {
		if got := slotIP(c.slot, c.off); got != c.want {
			t.Errorf("slotIP(%d,%d)=%s want %s", c.slot, c.off, got, c.want)
		}
	}
}

func TestSimReusesLowestSlot(t *testing.T) {
	s := NewSim(40000)
	ctx := context.Background()
	a, _ := s.Acquire(ctx)
	b, _ := s.Acquire(ctx)
	if a.Slot != 0 || b.Slot != 1 || b.Addr(8080) != "127.0.0.1:40001" {
		t.Fatalf("a=%+v b=%+v", a, b)
	}
	s.Release(a)
	if c, _ := s.Acquire(ctx); c.Slot != 0 {
		t.Fatalf("want slot 0 reused, got %d", c.Slot)
	}
}

func TestLeaseAddrRealIP(t *testing.T) {
	l := Lease{IP: "10.201.0.2"}
	if got := l.Addr(8080); got != "10.201.0.2:8080" {
		t.Fatal(got)
	}
}
