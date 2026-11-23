package agent

import (
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

// hostSampler measures the whole host's CPU, memory and disk, so the hub can
// tell which hosts have room for more work. /proc/stat, /proc/meminfo and
// /proc/loadavg describe the host even inside the agent's container, so it
// needs no extra mounts.
type hostSampler struct {
	proc     string // usually /proc
	diskPath string // a folder on the disk to report (the stacks folder), or ""

	mu   sync.Mutex
	prev cpuTimes
}

type cpuTimes struct{ busy, total uint64 }

func newHostSampler(diskPath string) *hostSampler {
	return &hostSampler{proc: "/proc", diskPath: diskPath}
}

// Sample returns the host's current figures, or nil when /proc can't be read
// (e.g. not Linux). CPU is the share of all cores busy since the previous
// sample, so the first sample after start has none.
func (h *hostSampler) Sample() *wire.HostStats {
	if h == nil {
		return nil
	}
	mem, okMem := readMemAvailable(h.proc + "/meminfo")
	cur, okCPU := readCPUTimes(h.proc + "/stat")
	if !okMem && !okCPU {
		return nil
	}
	st := &wire.HostStats{At: time.Now().Unix()}
	if okMem {
		st.MemAvailable = mem
	}
	if okCPU {
		h.mu.Lock()
		prev := h.prev
		h.prev = cur
		h.mu.Unlock()
		if prev.total > 0 && cur.total > prev.total && cur.busy >= prev.busy {
			pct := float64(cur.busy-prev.busy) / float64(cur.total-prev.total) * 100
			v := math.Round(min(pct, 100)*10) / 10
			st.CPU = &v
		}
	}
	st.Load1 = readLoad1(h.proc + "/loadavg")
	if h.diskPath != "" {
		st.DiskFree, st.DiskTotal = diskSpace(h.diskPath)
	}
	return st
}

// readCPUTimes parses the first ("cpu") line of /proc/stat. Busy time is
// everything except idle and iowait. guest and guest_nice are already counted
// in user and nice, so they are left out of the total.
func readCPUTimes(path string) (cpuTimes, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return cpuTimes{}, false
	}
	line, _, _ := strings.Cut(string(b), "\n")
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return cpuTimes{}, false
	}
	var t cpuTimes
	for i, s := range f[1:] {
		if i >= 8 { // user nice system idle iowait irq softirq steal
			break
		}
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return cpuTimes{}, false
		}
		t.total += n
		if i != 3 && i != 4 {
			t.busy += n
		}
	}
	return t, true
}

// readMemAvailable returns MemAvailable from /proc/meminfo in bytes: the
// kernel's estimate of memory available for new work without swapping.
func readMemAvailable(path string) (int64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "MemAvailable:"); ok {
			f := strings.Fields(rest)
			if len(f) > 0 {
				if kb, err := strconv.ParseInt(f[0], 10, 64); err == nil {
					return kb * 1024, true
				}
			}
		}
	}
	return 0, false
}

func readLoad1(path string) float64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}
