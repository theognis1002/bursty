package console

import (
	"io"
	"path/filepath"
	"testing"
	"time"
)

func TestReadyOnlyAfterArm(t *testing.T) {
	r, w := io.Pipe()
	c, err := Start(r, filepath.Join(t.TempDir(), "console.log"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(w, ReadyMarker+" uptime=9.9\n")
	select {
	case <-c.Ready():
		t.Fatal("ready before Arm")
	case <-time.After(50 * time.Millisecond):
	}
	c.Arm()
	_, _ = io.WriteString(w, "[    0.1] kernel noise\n"+ReadyMarker+" uptime=0.1234\n")
	select {
	case <-c.Ready():
	case <-time.After(time.Second):
		t.Fatal("not ready after marker")
	}
	if got := c.GuestUptime(); got != 0.1234 {
		t.Fatalf("uptime %v", got)
	}
	w.Close()
	<-c.Done()
}
