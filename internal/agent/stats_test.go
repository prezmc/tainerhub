package agent

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// After the first sample, Stats asks for one-shot (no one-second wait in the
// Engine) and works out CPU from its own previous sample.
func TestStatsOneShotUsesPreviousSample(t *testing.T) {
	dir, err := os.MkdirTemp("", "st") // short: macOS limits socket paths to 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/{id}/stats", func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		oneShot := r.URL.Query().Get("one-shot") == "true"
		if (n == 1) == oneShot {
			t.Errorf("call %d: one-shot=%v", n, oneShot)
		}
		// Each call: the container used 0.5s of CPU while the system used 4s (4 CPUs, 1s).
		cpu, sys := uint64(n)*500e6, uint64(n)*4e9
		pre := `"precpu_stats":{"cpu_usage":{"total_usage":0},"system_cpu_usage":0}`
		if !oneShot {
			pre = fmt.Sprintf(`"precpu_stats":{"cpu_usage":{"total_usage":%d},"system_cpu_usage":%d}`, cpu-500e6, sys-4e9)
		}
		fmt.Fprintf(w, `{"cpu_stats":{"cpu_usage":{"total_usage":%d},"system_cpu_usage":%d,"online_cpus":4},%s,"memory_stats":{"usage":100,"limit":1000}}`, cpu, sys, pre)
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })

	d := NewSocketDocker(sock)
	for i := 1; i <= 3; i++ {
		u, err := d.Stats(context.Background(), "abc123")
		if err != nil {
			t.Fatal(err)
		}
		if u.CPU < 49.9 || u.CPU > 50.1 {
			t.Errorf("sample %d: CPU = %.2f%%, want 50%%", i, u.CPU)
		}
	}
}
