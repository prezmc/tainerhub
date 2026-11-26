package api

import (
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

func TestDockerUpdateStatus(t *testing.T) {
	aptCurrent := &wire.DockerPlan{Manager: "apt", Installed: "5:29.10.0-1~debian.12~bookworm", Candidate: "5:29.10.0-1~debian.12~bookworm", Supported: true}
	aptNewer := &wire.DockerPlan{Manager: "apt", Installed: "5:29.10.0-1~debian.12~bookworm", Candidate: "5:29.10.1-1~debian.12~bookworm", Supported: true, Available: true}
	dnfCurrent := &wire.DockerPlan{Manager: "dnf", Installed: "29.10.0-1.fc41", Candidate: "29.10.0-1.fc41", Supported: true}
	snap := &wire.DockerPlan{Manager: "snap"}
	// A repository can carry a packaging fix (same engine version): not an update.
	aptRepack := &wire.DockerPlan{Manager: "apt", Installed: "5:29.10.0-1~ubuntu", Candidate: "5:29.10.0-2~ubuntu", Supported: true, Available: true}
	for _, c := range []struct {
		name, installed, latest string
		plan                    *wire.DockerPlan
		status, candidate       string
	}{
		{"released, repository not asked", "29.10.0", "29.10.1", nil, dockerUnverified, ""},
		{"released, not in apt yet", "29.10.0", "29.10.1", aptCurrent, dockerNotInRepo, "29.10.0"},
		{"released, not in dnf yet", "29.10.0", "29.10.1", dnfCurrent, dockerNotInRepo, "29.10.0"},
		{"apt has it", "29.10.0", "29.10.1", aptNewer, dockerAvailable, "29.10.1"},
		{"apt has it before the release cache does", "29.10.0", "29.10.0", aptNewer, dockerAvailable, "29.10.1"},
		{"up to date", "29.10.1", "29.10.1", nil, dockerCurrent, ""},
		{"up to date, checked", "29.10.0", "29.10.0", aptCurrent, dockerCurrent, "29.10.0"},
		{"release unknown", "29.10.0", "", nil, dockerCurrent, ""},
		{"snap: can't see its repository", "29.10.0", "29.10.1", snap, dockerUnverified, ""},
		{"repackaged same version", "29.10.0", "29.10.1", aptRepack, dockerNotInRepo, "29.10.0"},
		{"no Docker version", "", "29.10.1", nil, "", ""},
	} {
		st, cand := dockerUpdateStatus(c.installed, c.latest, c.plan)
		if st != c.status || cand != c.candidate {
			t.Errorf("%s: got %q %q, want %q %q", c.name, st, cand, c.status, c.candidate)
		}
	}
}

func TestDockerPlanCache(t *testing.T) {
	var c dockerPlanCache
	now := time.Now()
	if _, ok := c.lookup("h1", "29.10.0", "29.10.1", now); ok {
		t.Fatal("empty cache answered")
	}
	p := wire.DockerPlan{Manager: "apt", Candidate: "5:29.10.0-1"}
	c.put("h1", "29.10.0", "29.10.1", p, now)
	if got, ok := c.lookup("h1", "29.10.0", "29.10.1", now.Add(time.Hour)); !ok || got.Candidate != p.Candidate {
		t.Fatalf("lookup = %+v, %v", got, ok)
	}
	for _, miss := range []struct {
		name, host, docker, latest string
		at                         time.Time
	}{
		{"other host", "h2", "29.10.0", "29.10.1", now},
		{"Docker upgraded since", "h1", "29.10.1", "29.10.1", now},
		{"new release since", "h1", "29.10.0", "29.10.2", now},
		{"too old", "h1", "29.10.0", "29.10.1", now.Add(dockerPlanTTL + time.Minute)},
	} {
		if _, ok := c.lookup(miss.host, miss.docker, miss.latest, miss.at); ok {
			t.Errorf("%s: stale plan used", miss.name)
		}
	}
}

func TestPkgVersion(t *testing.T) {
	for in, want := range map[string]string{"5:29.10.0-1~debian.12~bookworm": "29.10.0", "29.10.1-1.fc41": "29.10.1", "": "", "(none)": ""} {
		if got := pkgVersion(in); got != want {
			t.Errorf("pkgVersion(%q) = %q, want %q", in, got, want)
		}
	}
}
