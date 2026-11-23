package registry

import (
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

// LoadWindow is how much recent host load the hub keeps (in memory only) to
// judge how busy a host is, so one spike doesn't decide where work goes.
const LoadWindow = 15 * time.Minute

// maxLoadSamples bounds each host's history: a sample every 10 s for the
// whole window, which is the most an agent sends.
const maxLoadSamples = int(LoadWindow / (10 * time.Second))

// HostLoad summarizes a host's recent load. Averages cover the samples in
// the last LoadWindow; the rest is the latest sample.
type HostLoad struct {
	// CPU is the average share of all cores busy, 0-100; nil until the agent
	// has sent two samples.
	CPU    *float64 `json:"cpu,omitempty"`
	CPUNow *float64 `json:"cpuNow,omitempty"`
	// MemAvailable is the average memory available for new work, in bytes.
	MemAvailable    int64     `json:"memAvailable"`
	MemAvailableNow int64     `json:"memAvailableNow"`
	Load1           float64   `json:"load1"`
	DiskFree        int64     `json:"diskFree,omitempty"` // stacks folder's filesystem
	DiskTotal       int64     `json:"diskTotal,omitempty"`
	Samples         int       `json:"samples"`
	Since           time.Time `json:"since"` // oldest sample in the averages
	At              time.Time `json:"at"`    // latest sample
}

type loadHistory struct {
	samples []wire.HostStats // oldest first
}

// recordLoad adds a host sample. Snapshots repeat the latest sample until
// the agent takes a new one, so repeats (same timestamp) are skipped.
func (r *Registry) recordLoad(hostID string, s *wire.HostStats, now time.Time) {
	if s == nil || s.At <= 0 {
		return
	}
	r.loadMu.Lock()
	defer r.loadMu.Unlock()
	if r.load == nil {
		r.load = map[string]*loadHistory{}
	}
	h := r.load[hostID]
	if h == nil {
		h = &loadHistory{}
		r.load[hostID] = h
	}
	if n := len(h.samples); n > 0 && h.samples[n-1].At >= s.At {
		return
	}
	h.samples = append(h.samples, *s)
	cutoff := now.Add(-LoadWindow).Unix()
	drop := 0
	for drop < len(h.samples) && h.samples[drop].At < cutoff {
		drop++
	}
	if over := len(h.samples) - drop - maxLoadSamples; over > 0 {
		drop += over
	}
	if drop > 0 {
		h.samples = append(h.samples[:0], h.samples[drop:]...)
	}
}

// Load returns a host's recent load, or nil when its agent has sent none in
// the last LoadWindow (an older agent, or one that has been offline).
func (r *Registry) Load(hostID string) *HostLoad {
	return r.loadAt(hostID, time.Now())
}

func (r *Registry) loadAt(hostID string, now time.Time) *HostLoad {
	r.loadMu.Lock()
	defer r.loadMu.Unlock()
	h := r.load[hostID]
	if h == nil {
		return nil
	}
	cutoff := now.Add(-LoadWindow).Unix()
	var (
		out         HostLoad
		cpuSum      float64
		cpuN        int
		memSum      int64
		memN        int64
		first, last *wire.HostStats
	)
	for i := range h.samples {
		s := &h.samples[i]
		if s.At < cutoff {
			continue
		}
		if first == nil {
			first = s
		}
		last = s
		out.Samples++
		if s.CPU != nil {
			cpuSum += *s.CPU
			cpuN++
		}
		if s.MemAvailable > 0 {
			memSum += s.MemAvailable
			memN++
		}
	}
	if last == nil {
		return nil
	}
	if cpuN > 0 {
		avg := round1(cpuSum / float64(cpuN))
		out.CPU = &avg
	}
	if last.CPU != nil {
		v := *last.CPU
		out.CPUNow = &v
	}
	if memN > 0 {
		out.MemAvailable = memSum / memN
	}
	out.MemAvailableNow = last.MemAvailable
	out.Load1 = last.Load1
	out.DiskFree, out.DiskTotal = last.DiskFree, last.DiskTotal
	out.Since, out.At = time.Unix(first.At, 0), time.Unix(last.At, 0)
	return &out
}

func round1(v float64) float64 {
	if v < 0 {
		return -round1(-v)
	}
	return float64(int64(v*10+0.5)) / 10
}
