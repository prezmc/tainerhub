package agent

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCgroupStats(t *testing.T) {
	root := t.TempDir()
	if newCgroupStats(root) != nil {
		t.Fatal("a folder without cgroup.controllers must not count as cgroup v2")
	}
	writeFiles(t, root, map[string]string{"cgroup.controllers": "cpu memory\n"})
	id := strings.Repeat("ab", 32)
	for _, dir := range []string{
		filepath.Join(root, "system.slice", "docker-"+id+".scope"), // systemd driver
		filepath.Join(root, "docker", id),                          // cgroupfs driver
	} {
		t.Run(filepath.Base(filepath.Dir(dir)), func(t *testing.T) {
			defer os.RemoveAll(dir)
			cg := newCgroupStats(root)
			cg.memTotal = 8 << 30
			stat := func(usec int) string { return "usage_usec " + itoa(usec) + "\nuser_usec 1\nsystem_usec 1\n" }
			writeFiles(t, dir, map[string]string{
				"cpu.stat":       stat(1_000_000),
				"memory.current": "300000000\n",
				"memory.stat":    "anon 1\ninactive_file 100000000\nactive_file 5\n",
				"memory.max":     "max\n",
			})
			// The first look sleeps and reads again; use 0.25 s of CPU in 0.5 s.
			cg.sleep = func(time.Duration) {
				time.Sleep(50 * time.Millisecond)
				writeFiles(t, dir, map[string]string{"cpu.stat": stat(1_025_000)})
			}
			u, ok := cg.Stats(id)
			if !ok {
				t.Fatal("expected cgroup stats")
			}
			if u.CPU < 10 || u.CPU > 55 { // 25 ms over ~50 ms of wall time, give or take scheduling
				t.Fatalf("first CPU = %.1f%%", u.CPU)
			}
			if u.Mem != 200000000 || u.MemLimit != 8<<30 {
				t.Fatalf("mem = %d / %d", u.Mem, u.MemLimit)
			}
			// Later samples use the previous reading: no sleep.
			cg.sleep = func(time.Duration) { t.Fatal("slept on a later sample") }
			writeFiles(t, dir, map[string]string{"memory.max": "536870912\n"})
			if u, _ = cg.Stats(id); u.MemLimit != 512<<20 {
				t.Fatalf("limit = %d", u.MemLimit)
			}
			// Gone: fall back to Docker.
			os.RemoveAll(dir)
			if _, ok := cg.Stats(id); ok {
				t.Fatal("stats for a removed container")
			}
			if _, ok := cg.Stats("short"); ok {
				t.Fatal("stats for a short ID")
			}
		})
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
