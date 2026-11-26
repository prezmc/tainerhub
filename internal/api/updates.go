package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/prezmc/tainerhub/internal/version"
	"github.com/prezmc/tainerhub/internal/wire"
)

// Updates: every green CI run on main publishes images tagged :latest and
// :0.1.0-<short commit>, so the newest successful run is the latest version.
// The hub reads it from GitHub (no account needed) and compares it with the
// version each part reports. Updating is done by each server's agent (see
// agent selfUpdate), because the hub has no Docker access of its own.

var (
	updateRepo   = envOr("TAINERHUB_UPDATE_REPO", "prezmc/tainerhub")
	githubAPI    = "https://api.github.com" // tests point this elsewhere
	githubClient = &http.Client{Timeout: 10 * time.Second}
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type latestRelease struct {
	Version string    `json:"version"` // e.g. 0.1.0-1a2b3c4
	Commit  string    `json:"commit"`
	Title   string    `json:"title"` // the commit message's first line
	At      time.Time `json:"at"`
}

// fetchTimeout bounds one GitHub lookup. It runs on its own context, not the
// request's, so a browser closing the page can't cancel the shared fetch.
const fetchTimeout = 15 * time.Second

// flightCache caches one value fetched from GitHub. At most one fetch runs
// at a time and the lock is never held during it: while a fetch is running,
// callers get the previous value straight away, or (on the first fetch) wait
// for it or for their own request to end, whichever comes first.
type flightCache[T any] struct {
	mu       sync.Mutex
	at       time.Time // when val/err were fetched; zero: never
	val      T         // the last value fetched successfully
	err      error     // the last fetch's error, if it failed
	inflight chan struct{}
}

// get returns the cached value, refreshing it after ttl (or after retry
// when the last fetch failed).
func (c *flightCache[T]) get(ctx context.Context, ttl, retry time.Duration, fetch func(context.Context) (T, error)) (T, error) {
	c.mu.Lock()
	age := ttl
	if c.err != nil {
		age = retry
	}
	if !c.at.IsZero() && time.Since(c.at) < age {
		v, err := c.val, c.err
		c.mu.Unlock()
		return v, err
	}
	if c.inflight == nil {
		done := make(chan struct{})
		c.inflight = done
		go c.refresh(done, fetch)
	}
	done := c.inflight
	if !c.at.IsZero() { // stale but usable: don't make the page wait
		v, err := c.val, c.err
		c.mu.Unlock()
		return v, err
	}
	c.mu.Unlock()
	select {
	case <-done:
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.val, c.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// fresh fetches now and waits for the answer, unless the last fetch is
// younger than minAge (so repeated clicks can't run into GitHub's rate
// limit). A fetch already running is joined rather than started again.
func (c *flightCache[T]) fresh(ctx context.Context, minAge time.Duration, fetch func(context.Context) (T, error)) (T, error) {
	c.mu.Lock()
	if !c.at.IsZero() && time.Since(c.at) < minAge {
		v, err := c.val, c.err
		c.mu.Unlock()
		return v, err
	}
	if c.inflight == nil {
		done := make(chan struct{})
		c.inflight = done
		go c.refresh(done, fetch)
	}
	done := c.inflight
	c.mu.Unlock()
	select {
	case <-done:
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.val, c.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// checkedAt is when the last fetch finished (zero: never).
func (c *flightCache[T]) checkedAt() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *flightCache[T]) refresh(done chan struct{}, fetch func(context.Context) (T, error)) {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	v, err := fetch(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at, c.err = time.Now(), err
	if err == nil {
		c.val = v
	}
	c.inflight = nil
	close(done)
}

type latestCache struct{ flightCache[*latestRelease] }

func (c *latestCache) get(ctx context.Context) (*latestRelease, error) {
	return c.flightCache.get(ctx, 15*time.Minute, 2*time.Minute, fetchLatest)
}

// check asks GitHub now (at most every 30 seconds).
func (c *latestCache) check(ctx context.Context) (*latestRelease, error) {
	return c.flightCache.fresh(ctx, 30*time.Second, fetchLatest)
}

func fetchLatest(ctx context.Context) (*latestRelease, error) {
	u := fmt.Sprintf("%s/repos/%s/actions/workflows/ci.yml/runs?branch=main&event=push&status=success&per_page=1", githubAPI, updateRepo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "TainerHub")
	resp, err := githubClient.Do(req)
	if err != nil {
		return nil, errors.New("couldn't reach GitHub to check for updates")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub answered %s while checking for updates", resp.Status)
	}
	var raw struct {
		Runs []struct {
			HeadSHA      string    `json:"head_sha"`
			DisplayTitle string    `json:"display_title"`
			UpdatedAt    time.Time `json:"updated_at"`
		} `json:"workflow_runs"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&raw); err != nil {
		return nil, err
	}
	if len(raw.Runs) == 0 || len(raw.Runs[0].HeadSHA) < 7 {
		return nil, errors.New("no published version found")
	}
	r := raw.Runs[0]
	return &latestRelease{Version: "0.1.0-" + r.HeadSHA[:7], Commit: r.HeadSHA, Title: strings.SplitN(r.DisplayTitle, "\n", 2)[0], At: r.UpdatedAt}, nil
}

// isCurrent reports whether a running version (0.1.0-<short sha>) is the
// latest commit. Unknown versions (e.g. "dev") are never current.
func isCurrent(running string, latest *latestRelease) bool {
	if latest == nil {
		return true // can't tell: don't nag
	}
	i := strings.LastIndexByte(running, '-')
	if i < 0 || len(running)-i-1 < 7 {
		return false
	}
	return strings.HasPrefix(latest.Commit, running[i+1:])
}

type hostUpdate struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Online  bool   `json:"online"`
	Version string `json:"version"` // agent version
	Docker  string `json:"docker"`  // Docker Engine version
	// DockerBehind: Docker has released a newer engine. Whether this host's
	// package repository offers it is DockerStatus.
	DockerBehind bool `json:"dockerBehind"`
	// DockerStatus: what an upgrade can install now (see dockerUpdateStatus).
	DockerStatus    string `json:"dockerStatus,omitempty"`
	DockerCandidate string `json:"dockerCandidate,omitempty"` // what the repository offers, e.g. 29.10.1
	DockerManager   string `json:"dockerManager,omitempty"`   // apt, dnf, snap…, once checked
	Current         bool   `json:"current"`
	HubHost         bool   `json:"hubHost"` // this server also runs the hub: updating it updates the hub
	CanTry          bool   `json:"canUpdate"`
	// CgroupStats: the agent reads stats from cgroup files. False offers
	// "Lighter stats"; nil means an agent too old to say.
	CgroupStats *bool `json:"cgroupStats,omitempty"`
	// StatsFromDocker: an admin chose Docker as this host's stats source.
	StatsFromDocker bool   `json:"statsFromDocker"`
	Note            string `json:"note,omitempty"`
}

func (s *Server) updates(w http.ResponseWriter, r *http.Request) {
	latest, err := s.latest.get(r.Context())
	s.writeUpdates(w, r, latest, err)
}

// checkUpdates answers POST /api/updates/check: it asks GitHub for the
// newest TainerHub and Docker Engine releases now, instead of waiting for
// the cached answers to expire, then returns the same as GET /api/updates.
func (s *Server) checkUpdates(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), fetchTimeout+5*time.Second)
	defer cancel()
	latest, err := s.latest.check(ctx)
	_, _ = s.dockerLatest.fresh(ctx, time.Minute, fetchDockerLatest)
	s.writeUpdates(w, r, latest, err)
}

func (s *Server) writeUpdates(w http.ResponseWriter, r *http.Request, latest *latestRelease, err error) {
	out := map[string]any{"hubVersion": version.Version, "latest": latest, "hubCurrent": isCurrent(version.Version, latest)}
	if at := s.latest.checkedAt(); !at.IsZero() {
		out["checkedAt"] = at
	}
	if err != nil {
		out["error"] = err.Error()
	}
	hosts, herr := s.Store.ListHosts(r.Context())
	if herr != nil {
		s.fail(w, herr)
		return
	}
	local := s.localHostID()
	dockerLatest := s.dockerLatest.get(r.Context())
	out["dockerLatest"] = dockerLatest
	list := []hostUpdate{}
	for _, h := range hosts {
		u := hostUpdate{ID: h.ID, Name: h.Name, HubHost: h.ID == local}
		if a := s.Reg.Get(h.ID); a != nil {
			u.Online = true
			if hello := a.Status().Hello; hello != nil {
				u.Version = hello.AgentVersion
				u.Docker = hello.DockerVersion
				u.CgroupStats = hello.CgroupStats
				u.StatsFromDocker = s.Reg.StatsFromDocker(h.ID)
				u.DockerBehind = dockerLatest != "" && u.Docker != "" && versionLess(u.Docker, dockerLatest)
				plan, checked := s.dockerPlans.lookup(h.ID, u.Docker, dockerLatest, time.Now())
				var pp *wire.DockerPlan
				if checked {
					pp = &plan
					u.DockerManager = plan.Manager
				}
				u.DockerStatus, u.DockerCandidate = dockerUpdateStatus(u.Docker, dockerLatest, pp)
			}
		}
		u.Current = isCurrent(u.Version, latest) && (!u.HubHost || isCurrent(version.Version, latest))
		u.CanTry = u.Online
		if !u.Online {
			u.Note = "offline"
		}
		list = append(list, u)
	}
	out["hosts"] = list
	out["hubManaged"] = local != "" // the hub can update itself through its own server's agent
	writeJSON(w, http.StatusOK, out)
}

// localHostID is the host whose agent runs next to the hub (set up by the
// hub's local-agent bootstrap), or "" if the hub was installed without one.
func (s *Server) localHostID() string {
	if s.DataDir == "" {
		return ""
	}
	b, err := os.ReadFile(s.DataDir + "/local_host_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// updateHost asks a host's agent to update its compose project (on the hub's
// server, that's the hub itself).
func (s *Server) updateHost(w http.ResponseWriter, r *http.Request) {
	a := s.Reg.Get(r.PathValue("id"))
	if a == nil {
		jsonErr(w, http.StatusServiceUnavailable, "host is offline")
		return
	}
	data, err := a.Operate(r.Context(), wire.Operation{Kind: wire.OpSelfUpdate})
	u := userFrom(r)
	detail := map[string]any{"host": a.Name}
	var res wire.SelfUpdateResult
	if err == nil {
		_ = json.Unmarshal(data, &res)
		detail["project"], detail["dir"] = res.Project, res.Dir
	} else {
		detail["error"] = err.Error()
		if strings.Contains(err.Error(), "unknown operation") {
			err = errors.New("this agent is too old to update itself; update it once on the server with: docker compose pull && docker compose up -d")
			detail["error"] = err.Error()
		}
	}
	s.record(r, &u, "host.update", a.Name, detail)
	if err != nil {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"result": res, "hubHost": a.HostID == s.localHostID()})
}

// lighterStats asks a host's agent to add the read-only cgroup mount to its
// own compose service (in compose.override.yml) and recreate itself, so it
// reads container stats from the kernel instead of asking Docker.
func (s *Server) lighterStats(w http.ResponseWriter, r *http.Request) {
	a := s.Reg.Get(r.PathValue("id"))
	if a == nil {
		jsonErr(w, http.StatusServiceUnavailable, "host is offline")
		return
	}
	if h := a.Status().Hello; h == nil || h.CgroupStats == nil {
		jsonErr(w, http.StatusConflict, "update this host's agent first")
		return
	}
	data, err := a.Operate(r.Context(), wire.Operation{Kind: wire.OpSelfUpdate, Arg: wire.SelfUpdateAddCgroupMount})
	u := userFrom(r)
	detail := map[string]any{"host": a.Name}
	var res wire.SelfUpdateResult
	if err == nil {
		_ = json.Unmarshal(data, &res)
		detail["file"] = res.Dir + "/compose.override.yml"
	} else {
		detail["error"] = err.Error()
	}
	s.record(r, &u, "host.lighter_stats", a.Name, detail)
	if err != nil {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"result": res})
}

const settingStatsFromDocker = "stats_from_docker" // comma-separated host IDs

// LoadStatsSettings restores which hosts were switched to stats from Docker.
func (s *Server) LoadStatsSettings(ctx context.Context) error {
	raw, err := s.Store.Setting(ctx, settingStatsFromDocker)
	if err != nil {
		return err
	}
	for _, id := range strings.Split(raw, ",") {
		if validUUID(id) {
			s.Reg.SetStatsFromDocker(id, true)
		}
	}
	return nil
}

// statsSource switches a host between reading stats from cgroup files
// (lighter) and asking Docker, in case the lighter numbers look wrong. The
// agent switches live; the choice is kept across restarts.
func (s *Server) statsSource(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validUUID(id) {
		jsonErr(w, http.StatusNotFound, "not found")
		return
	}
	var in struct{ Source string }
	if !readJSON(w, r, &in) {
		return
	}
	if in.Source != "docker" && in.Source != "cgroup" {
		jsonErr(w, http.StatusBadRequest, `source must be "docker" or "cgroup"`)
		return
	}
	h, err := s.Store.GetHost(r.Context(), id)
	if err != nil {
		s.notFoundOr(w, err)
		return
	}
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	prev := s.Reg.StatsFromDocker(id)
	s.Reg.SetStatsFromDocker(id, in.Source == "docker")
	if err := s.Store.SetSetting(r.Context(), settingStatsFromDocker, strings.Join(s.Reg.StatsFromDockerHosts(), ",")); err != nil {
		s.Reg.SetStatsFromDocker(id, prev)
		s.fail(w, err)
		return
	}
	u := userFrom(r)
	s.record(r, &u, "host.stats_source", h.Name, map[string]string{"source": in.Source})
	writeJSON(w, http.StatusOK, map[string]any{"source": in.Source})
}

// ---------------- Docker Engine ----------------

// dockerLatestCache holds the newest Docker Engine release (moby/moby tags
// look like "docker-v29.10.0"), for the "newer Docker available" badge. What
// a host can actually install depends on its package repository: the
// upgrade dialog asks the host (OpDockerPlan) before anything happens.
type dockerLatestCache struct{ flightCache[string] }

// get returns the newest release, or "" if it isn't known (yet).
func (c *dockerLatestCache) get(ctx context.Context) string {
	v, _ := c.flightCache.get(ctx, 6*time.Hour, 10*time.Minute, fetchDockerLatest)
	return v
}

func fetchDockerLatest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPI+"/repos/moby/moby/releases?per_page=20", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "TainerHub")
	resp, err := githubClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub answered %s", resp.Status)
	}
	var rels []struct {
		Tag        string `json:"tag_name"`
		Prerelease bool   `json:"prerelease"`
		Draft      bool   `json:"draft"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&rels); err != nil {
		return "", err
	}
	for _, r := range rels {
		v := strings.TrimPrefix(r.Tag, "docker-")
		if r.Prerelease || r.Draft || !strings.HasPrefix(v, "v") || strings.Contains(v, "-") {
			continue
		}
		return strings.TrimPrefix(v, "v"), nil
	}
	return "", errors.New("no Docker Engine release found")
}

// versionLess compares dotted versions like 29.9.2 and 29.10.0.
func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			fmt.Sscanf(pa[i], "%d", &x)
		}
		if i < len(pb) {
			fmt.Sscanf(pb[i], "%d", &y)
		}
		if x != y {
			return x < y
		}
	}
	return false
}

// Docker Engine update states for a host, from dockerUpdateStatus.
const (
	dockerCurrent    = "current"     // nothing newer, upstream or in the repository
	dockerAvailable  = "available"   // the host's repository offers a newer version
	dockerNotInRepo  = "not-in-repo" // Docker released a newer one; the repository doesn't have it yet
	dockerUnverified = "released"    // Docker released a newer one; the repository wasn't asked (or can't tell)
)

var pkgVersionRe = regexp.MustCompile(`\d+\.\d+\.\d+`)

// pkgVersion turns a package version like 5:29.10.0-1~debian.12~bookworm
// into 29.10.0 ("" if there's none).
func pkgVersion(v string) string { return pkgVersionRe.FindString(v) }

// dockerUpdateStatus says whether a host running Docker installed can
// update, and to what. latest is Docker's newest release; plan is what the
// host's package manager said when last asked (nil if not asked, or the
// answer is stale). Only the repository decides "available": Docker's own
// release often reaches apt and dnf a while later.
func dockerUpdateStatus(installed, latest string, plan *wire.DockerPlan) (status, candidate string) {
	if installed == "" {
		return "", ""
	}
	behind := latest != "" && versionLess(installed, latest)
	if plan == nil {
		if behind {
			return dockerUnverified, ""
		}
		return dockerCurrent, ""
	}
	cand := pkgVersion(plan.Candidate)
	if plan.Available && cand != "" && versionLess(installed, cand) {
		return dockerAvailable, cand
	}
	switch {
	case !behind:
		return dockerCurrent, cand
	case plan.Manager == "apt" || plan.Manager == "dnf":
		return dockerNotInRepo, cand
	default: // snap, distribution packages…: the hub can't see their repository
		return dockerUnverified, ""
	}
}

// dockerPlanCache remembers each host's last Docker plan, so the Updates
// page can say what the host's repository offers without asking it (asking
// runs a privileged helper and refreshes the package lists). An answer
// counts only while the host's Docker version and Docker's newest release
// are what they were then, and for dockerPlanTTL.
type dockerPlanCache struct {
	mu sync.Mutex
	m  map[string]cachedDockerPlan
}

type cachedDockerPlan struct {
	plan           wire.DockerPlan
	docker, latest string
	at             time.Time
}

const dockerPlanTTL = 12 * time.Hour

func (c *dockerPlanCache) put(hostID, docker, latest string, p wire.DockerPlan, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]cachedDockerPlan{}
	}
	c.m[hostID] = cachedDockerPlan{plan: p, docker: docker, latest: latest, at: now}
}

func (c *dockerPlanCache) lookup(hostID, docker, latest string, now time.Time) (wire.DockerPlan, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[hostID]
	if !ok || e.docker != docker || e.latest != latest || now.Sub(e.at) > dockerPlanTTL {
		return wire.DockerPlan{}, false
	}
	return e.plan, true
}

// dockerPlan asks the host how Docker is installed and what its repository
// offers, and remembers the answer for the Updates page.
func (s *Server) dockerPlan(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a := s.Reg.Get(id)
	if a == nil {
		jsonErr(w, http.StatusServiceUnavailable, "host is offline")
		return
	}
	var docker string
	if h := a.Status().Hello; h != nil {
		docker = h.DockerVersion
	}
	data, err := a.Operate(r.Context(), wire.Operation{Kind: wire.OpDockerPlan})
	if err != nil {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	var p wire.DockerPlan
	if json.Unmarshal(data, &p) == nil && docker != "" {
		s.dockerPlans.put(id, docker, s.dockerLatest.get(r.Context()), p, time.Now())
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// dockerUpgrade upgrades Docker Engine on a host. It restarts every
// container there, so the request must name the host exactly ("confirm"),
// on top of the confirmation the UI asks for.
func (s *Server) dockerUpgrade(w http.ResponseWriter, r *http.Request) {
	var in struct{ Confirm string }
	if !readJSON(w, r, &in) {
		return
	}
	a := s.Reg.Get(r.PathValue("id"))
	if a == nil {
		jsonErr(w, http.StatusServiceUnavailable, "host is offline")
		return
	}
	if strings.TrimSpace(in.Confirm) != a.Name {
		jsonErr(w, http.StatusBadRequest, "confirm by giving the host's exact name: upgrading Docker restarts every container on it")
		return
	}
	var before string
	if h := a.Status().Hello; h != nil {
		before = h.DockerVersion
	}
	data, err := a.Operate(r.Context(), wire.Operation{Kind: wire.OpDockerUpgrade, Arg: "restart-all"})
	var p wire.DockerPlan
	_ = json.Unmarshal(data, &p)
	u := userFrom(r)
	detail := map[string]string{"host": a.Name, "from": before, "package_from": p.Installed, "package_to": p.Candidate, "manager": p.Manager}
	if err != nil {
		detail["error"] = err.Error()
	}
	s.record(r, &u, "host.docker_upgrade", a.Name, detail)
	if err != nil {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, p)
}
