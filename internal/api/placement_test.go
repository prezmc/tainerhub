package api

import (
	"testing"

	"github.com/prezmc/tainerhub/internal/registry"
	"github.com/prezmc/tainerhub/internal/wire"
)

const gib = int64(1) << 30

func host(id string, cpu float64, memFreeGiB int64, diskFreeGiB int64) hostInfo {
	return hostInfo{
		ID: id, Name: id, Online: true,
		Hello: &wire.Hello{NCPU: 4, MemTotal: 16 * gib, Stacks: &wire.StackSupport{Dir: "/opt/tainerhub/stacks"}},
		Load:  &registry.HostLoad{CPU: &cpu, MemAvailable: memFreeGiB * gib, DiskFree: diskFreeGiB * gib, DiskTotal: 100 * gib, Samples: 5},
	}
}

func byID(ps []placementHost) map[string]placementHost {
	m := map[string]placementHost{}
	for _, p := range ps {
		m[p.HostID] = p
	}
	return m
}

func TestRankHostsPrefersHeadroom(t *testing.T) {
	hosts := []hostInfo{
		host("busy", 80, 12, 50), // CPU-bound
		host("full", 10, 1, 50),  // little memory left
		host("quiet", 20, 10, 50),
		{ID: "off", Name: "off"},
	}
	got := rankHosts(hosts, footprint{}, "")
	order := []string{got[0].HostID, got[1].HostID, got[2].HostID, got[3].HostID}
	if order[0] != "quiet" || order[3] != "off" {
		t.Fatalf("order %v", order)
	}
	if !got[0].Suggested {
		t.Error("best host not suggested")
	}
	for _, p := range got[1:] {
		if p.Suggested {
			t.Errorf("%s also suggested", p.HostID)
		}
	}
	m := byID(got)
	// full: 15 of 16 GiB used = 93.75% > 90%: not eligible.
	if m["full"].Eligible || m["full"].Reason != "not enough free memory" {
		t.Errorf("full = %+v", m["full"])
	}
	if m["off"].Eligible || m["off"].Reason != "offline" {
		t.Errorf("off = %+v", m["off"])
	}
	// quiet: CPU 20%, memory 6/16 = 37.5%: score 62.5 on memory.
	if s := m["quiet"].Score; s == nil || *s != 62.5 {
		t.Errorf("quiet score %v", s)
	}
}

func TestRankHostsAddsFootprint(t *testing.T) {
	hosts := []hostInfo{host("a", 20, 10, 50), host("b", 20, 10, 50)}
	// 200% of one core on a 4-core host adds 50 points; 4 GiB adds 25 points of memory.
	got := byID(rankHosts(hosts, footprint{CPU: 200, Mem: 4 * gib}, "a"))
	a, b := got["a"], got["b"]
	if !a.Current || a.Suggested {
		t.Errorf("current host: %+v", a)
	}
	if *a.CPUAfter != 20 || *a.MemAfter != 37.5 {
		t.Errorf("current host's figures already include the stack: cpu %v mem %v", *a.CPUAfter, *a.MemAfter)
	}
	if *b.CPUAfter != 70 || *b.MemAfter != 62.5 || *b.Score != 30 {
		t.Errorf("b after: cpu %v mem %v score %v", *b.CPUAfter, *b.MemAfter, *b.Score)
	}
	if !b.Suggested {
		t.Error("the other host should be suggested")
	}
}

func TestRankHostsDiskAndMissingData(t *testing.T) {
	noLoad := host("old", 0, 0, 0)
	noLoad.Load = nil
	noStacks := host("nostacks", 5, 15, 50)
	noStacks.Hello.Stacks = &wire.StackSupport{Error: "stacks folder is not mounted"}
	got := rankHosts([]hostInfo{host("tiny-disk", 5, 15, 1), noLoad, noStacks}, footprint{}, "")
	m := byID(got)
	if m["tiny-disk"].Eligible {
		t.Error("host with 1 GiB free disk is eligible")
	}
	if !m["old"].Eligible || m["old"].Score != nil || m["old"].Suggested {
		t.Errorf("host without load figures: %+v", m["old"])
	}
	if got[0].HostID != "old" {
		t.Errorf("eligible host without figures should still come before ineligible ones, got %s first", got[0].HostID)
	}
	if m["nostacks"].Eligible || m["nostacks"].Reason != "stacks not set up" {
		t.Errorf("nostacks = %+v", m["nostacks"])
	}
}

func TestStackFootprint(t *testing.T) {
	cs := []wire.Container{
		{Project: "app", State: "running", CPU: 12.34, Mem: 100},
		{Project: "app", State: "exited", CPU: 50, Mem: 999},
		{Project: "other", State: "running", CPU: 5, Mem: 5},
		{Project: "app", State: "running", CPU: 1, Mem: 50},
	}
	if fp := stackFootprint(cs, "app"); fp.CPU != 13.3 || fp.Mem != 150 {
		t.Errorf("footprint %+v", fp)
	}
}
