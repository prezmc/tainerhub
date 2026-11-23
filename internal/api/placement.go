package api

import (
	"net/http"
	"sort"
	"strings"

	"github.com/prezmc/tainerhub/internal/registry"
	"github.com/prezmc/tainerhub/internal/store"
	"github.com/prezmc/tainerhub/internal/wire"
)

// Placement: which host has the most room for a stack (or a new container).
// It uses each host's recent average load, not the latest sample, so a
// momentary spike doesn't steer work away from an otherwise quiet host.

const (
	// maxMemAfter: a host is not suggested if placing the work would leave
	// less than this share of its memory available.
	maxMemAfter = 90.0
	// minDiskFree: a host with less free disk than this isn't suggested.
	minDiskFree = 2 << 30
)

// footprint is what a stack uses now: CPU as a percentage of one core
// (summed over its containers, like `docker stats`) and memory in bytes.
type footprint struct {
	CPU float64 `json:"cpu"`
	Mem int64   `json:"mem"`
}

type placementHost struct {
	HostID   string `json:"hostId"`
	HostName string `json:"hostName"`
	// Eligible: the host can take the work (online, stacks set up, enough
	// memory and disk). Reason says why not, or notes missing load figures.
	Eligible bool   `json:"eligible"`
	Reason   string `json:"reason,omitempty"`
	Current  bool   `json:"current,omitempty"` // the stack is already here
	// CPU and Mem are the host's recent use (% of all cores, % of memory);
	// the After values add the work's footprint. Nil without load data.
	CPU      *float64 `json:"cpu,omitempty"`
	Mem      *float64 `json:"mem,omitempty"`
	CPUAfter *float64 `json:"cpuAfter,omitempty"`
	MemAfter *float64 `json:"memAfter,omitempty"`
	DiskFree int64    `json:"diskFree,omitempty"`
	// Score is the headroom left after placing the work, 0-100, on the
	// tighter of CPU and memory. Higher is better.
	Score     *float64 `json:"score,omitempty"`
	Suggested bool     `json:"suggested,omitempty"`
}

type placementResult struct {
	Footprint *footprint      `json:"footprint,omitempty"`
	Hosts     []placementHost `json:"hosts"`
}

// hostInfo is what rankHosts needs to know about one host.
type hostInfo struct {
	ID, Name string
	Online   bool
	Hello    *wire.Hello
	Load     *registry.HostLoad
}

// rankHosts scores every host for work with footprint fp (zero for new work
// of unknown size) and orders them best first. current is the host the work
// already runs on, if any: it is listed but never suggested.
func rankHosts(hosts []hostInfo, fp footprint, current string) []placementHost {
	out := make([]placementHost, 0, len(hosts))
	for _, h := range hosts {
		p := placementHost{HostID: h.ID, HostName: h.Name, Current: h.ID == current}
		switch {
		case !h.Online || h.Hello == nil:
			p.Reason = "offline"
		case h.Hello.Stacks == nil:
			p.Reason = "agent too old for stacks"
		case h.Hello.Stacks.Error != "":
			p.Reason = "stacks not set up"
		default:
			p.Eligible = true
		}
		if h.Online && h.Hello != nil && h.Load != nil {
			fill(&p, h.Hello, h.Load, fp)
		}
		if p.Eligible && p.Reason == "" && h.Load == nil && h.Online {
			p.Reason = "no load figures yet (update the agent if this lasts)"
		}
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Eligible != b.Eligible {
			return a.Eligible
		}
		if (a.Score != nil) != (b.Score != nil) {
			return a.Score != nil
		}
		if a.Score != nil && *a.Score != *b.Score {
			return *a.Score > *b.Score
		}
		return strings.ToLower(a.HostName) < strings.ToLower(b.HostName)
	})
	for i := range out {
		if out[i].Eligible && out[i].Score != nil && !out[i].Current {
			out[i].Suggested = true
			break
		}
	}
	return out
}

// fill adds a host's load figures and works out whether fp fits.
func fill(p *placementHost, hello *wire.Hello, l *registry.HostLoad, fp footprint) {
	p.DiskFree = l.DiskFree
	if p.Current {
		fp = footprint{} // already counted in the host's own figures
	}
	var tight float64 // the busier of CPU and memory after placing, in %
	known := false
	if l.CPU != nil && hello.NCPU > 0 {
		cpu := *l.CPU
		after := min(cpu+fp.CPU/float64(hello.NCPU), 100)
		p.CPU, p.CPUAfter = ptr(round1(cpu)), ptr(round1(after))
		tight, known = after, true
	}
	if hello.MemTotal > 0 && l.MemAvailable > 0 {
		total := float64(hello.MemTotal)
		used := total - float64(l.MemAvailable)
		mem := used / total * 100
		after := min((used+float64(fp.Mem))/total*100, 100)
		p.Mem, p.MemAfter = ptr(round1(max(mem, 0))), ptr(round1(max(after, 0)))
		tight, known = max(tight, after), true
		if p.Eligible && !p.Current && after > maxMemAfter {
			p.Eligible, p.Reason = false, "not enough free memory"
		}
	}
	if p.Eligible && !p.Current && l.DiskTotal > 0 && l.DiskFree < minDiskFree {
		p.Eligible, p.Reason = false, "less than 2 GB of disk free"
	}
	if known {
		p.Score = ptr(round1(100 - tight))
	}
}

func ptr[T any](v T) *T { return &v }

func round1(v float64) float64 {
	if v < 0 {
		return -round1(-v)
	}
	return float64(int64(v*10+0.5)) / 10
}

// stackFootprint adds up the CPU and memory of a stack's running containers.
func stackFootprint(cs []wire.Container, name string) footprint {
	var fp footprint
	for _, c := range cs {
		if c.Project == name && c.State == "running" {
			fp.CPU += c.CPU
			fp.Mem += c.Mem
		}
	}
	fp.CPU = round1(fp.CPU)
	return fp
}

// placement ranks hosts for new work, or with ?stack=<id> for moving that
// stack (its current footprint is added to each other host).
func (s *Server) placement(w http.ResponseWriter, r *http.Request) {
	hs, err := s.Store.ListHosts(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	var (
		fp      footprint
		current string
		res     placementResult
	)
	if id := r.URL.Query().Get("stack"); id != "" {
		if !validUUID(id) {
			jsonErr(w, http.StatusNotFound, "stack not found")
			return
		}
		st, err := s.Store.GetStack(r.Context(), id)
		if err != nil {
			s.notFoundOr(w, err)
			return
		}
		current = st.HostID
		if a := s.Reg.Get(st.HostID); a != nil {
			fp = stackFootprint(a.Status().Containers, st.Name)
		}
		res.Footprint = &fp
	}
	res.Hosts = rankHosts(s.hostInfos(hs), fp, current)
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) hostInfos(hs []store.Host) []hostInfo {
	out := make([]hostInfo, 0, len(hs))
	for _, h := range hs {
		hi := hostInfo{ID: h.ID, Name: h.Name}
		if a := s.Reg.Get(h.ID); a != nil {
			hi.Online = true
			hi.Hello = a.Status().Hello
			hi.Load = s.Reg.Load(h.ID)
		}
		out = append(out, hi)
	}
	return out
}
