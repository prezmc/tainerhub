package registry

import (
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

func f(v float64) *float64 { return &v }

func TestLoadAverages(t *testing.T) {
	r := New()
	now := time.Unix(1_700_000_000, 0)
	if r.loadAt("h", now) != nil {
		t.Fatal("load for a host that sent none")
	}
	r.recordLoad("h", &wire.HostStats{At: now.Unix() - 20, MemAvailable: 1000}, now) // first sample: no CPU yet
	r.recordLoad("h", &wire.HostStats{At: now.Unix() - 10, CPU: f(20), MemAvailable: 3000, DiskFree: 5, DiskTotal: 9}, now)
	r.recordLoad("h", &wire.HostStats{At: now.Unix() - 10, CPU: f(99), MemAvailable: 1}, now) // repeat of the same sample: ignored
	r.recordLoad("h", &wire.HostStats{At: now.Unix(), CPU: f(40), MemAvailable: 2000, Load1: 1.5, DiskFree: 4, DiskTotal: 9}, now)
	r.recordLoad("h", nil, now)

	l := r.loadAt("h", now)
	if l == nil || l.Samples != 3 {
		t.Fatalf("load = %+v, want 3 samples", l)
	}
	if l.CPU == nil || *l.CPU != 30 || *l.CPUNow != 40 {
		t.Errorf("cpu avg %v now %v, want 30 and 40", l.CPU, l.CPUNow)
	}
	if l.MemAvailable != 2000 || l.MemAvailableNow != 2000 {
		t.Errorf("mem avg %d now %d", l.MemAvailable, l.MemAvailableNow)
	}
	if l.DiskFree != 4 || l.Load1 != 1.5 {
		t.Errorf("latest figures %+v", l)
	}

	// Once the samples are older than the window, they no longer count.
	if l := r.loadAt("h", now.Add(LoadWindow+time.Minute)); l != nil {
		t.Errorf("stale load %+v", l)
	}
}

func TestLoadHistoryBounded(t *testing.T) {
	r := New()
	now := time.Unix(1_700_000_000, 0)
	for i := range 500 {
		at := now.Add(time.Duration(i-500) * time.Second) // one per second, faster than any agent sends
		r.recordLoad("h", &wire.HostStats{At: at.Unix(), CPU: f(float64(i % 100))}, now)
	}
	if n := len(r.load["h"].samples); n > maxLoadSamples {
		t.Errorf("kept %d samples, max %d", n, maxLoadSamples)
	}
}

func TestSnapshotRecordsLoad(t *testing.T) {
	r := New()
	a := r.Attach("h", "host", func() {})
	at := time.Now().Unix()
	a.HandleSnapshot(&wire.Snapshot{At: at, Host: &wire.HostStats{At: at, CPU: f(12), MemAvailable: 42}}, [32]byte{})
	l := r.Load("h")
	if l == nil || l.CPU == nil || *l.CPU != 12 || l.MemAvailable != 42 {
		t.Fatalf("load after snapshot = %+v", l)
	}
}
