package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func writeProc(t *testing.T, dir, stat string) {
	t.Helper()
	files := map[string]string{
		"stat":    stat,
		"meminfo": "MemTotal:       16000000 kB\nMemFree:          500000 kB\nMemAvailable:    4000000 kB\n",
		"loadavg": "1.25 0.80 0.50 2/345 6789\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHostSampler(t *testing.T) {
	dir := t.TempDir()
	// user nice system idle iowait irq softirq steal guest guest_nice
	writeProc(t, dir, "cpu  100 0 100 700 100 0 0 0 50 0\ncpu0 1 2 3 4\n")
	h := &hostSampler{proc: dir, diskPath: dir}

	first := h.Sample()
	if first == nil {
		t.Fatal("no sample")
	}
	if first.CPU != nil {
		t.Errorf("first sample has CPU %v; want none until there is a previous reading", *first.CPU)
	}
	if first.MemAvailable != 4000000*1024 {
		t.Errorf("MemAvailable = %d", first.MemAvailable)
	}
	if first.Load1 != 1.25 {
		t.Errorf("Load1 = %v", first.Load1)
	}
	if first.DiskTotal <= 0 || first.DiskFree <= 0 || first.DiskFree > first.DiskTotal {
		t.Errorf("disk free %d of %d", first.DiskFree, first.DiskTotal)
	}

	// 300 more busy (user+system+irq), 100 more idle, 100 more iowait: 60% busy.
	// The guest column grows too but is already part of user, so it must not count twice.
	writeProc(t, dir, "cpu  250 0 200 800 200 50 0 0 999 0\n")
	second := h.Sample()
	if second.CPU == nil || *second.CPU != 60 {
		t.Fatalf("CPU = %v, want 60", second.CPU)
	}
}

func TestHostSamplerNoProc(t *testing.T) {
	h := &hostSampler{proc: filepath.Join(t.TempDir(), "missing")}
	if s := h.Sample(); s != nil {
		t.Errorf("got %+v without /proc", s)
	}
	var none *hostSampler
	if none.Sample() != nil {
		t.Error("nil sampler returned a sample")
	}
}

func TestReadCPUTimesRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	for _, body := range []string{"", "intr 1 2 3\n", "cpu  1 2 x 4 5\n"} {
		p := filepath.Join(dir, "stat")
		_ = os.WriteFile(p, []byte(body), 0o600)
		if _, ok := readCPUTimes(p); ok {
			t.Errorf("accepted %q", body)
		}
	}
}
