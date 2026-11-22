package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
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

type latestCache struct {
	mu  sync.Mutex
	at  time.Time
	val *latestRelease
	err error
}

func (c *latestCache) get(ctx context.Context) (*latestRelease, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && time.Since(c.at) < 15*time.Minute {
		return c.val, c.err
	}
	c.val, c.err = fetchLatest(ctx)
	c.at = time.Now()
	if c.err != nil {
		c.at = time.Now().Add(-13 * time.Minute) // retry a failure after ~2 minutes
	}
	return c.val, c.err
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
	// package repository offers it yet is checked when upgrading.
	DockerBehind bool   `json:"dockerBehind"`
	Current      bool   `json:"current"`
	HubHost      bool   `json:"hubHost"` // this server also runs the hub: updating it updates the hub
	CanTry       bool   `json:"canUpdate"`
	Note         string `json:"note,omitempty"`
}

func (s *Server) updates(w http.ResponseWriter, r *http.Request) {
	latest, err := s.latest.get(r.Context())
	out := map[string]any{"hubVersion": version.Version, "latest": latest, "hubCurrent": isCurrent(version.Version, latest)}
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
				u.DockerBehind = dockerLatest != "" && u.Docker != "" && versionLess(u.Docker, dockerLatest)
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

// ---------------- Docker Engine ----------------

// dockerLatestCache holds the newest Docker Engine release (moby/moby tags
// look like "docker-v29.10.0"), for the "newer Docker available" badge. What
// a host can actually install depends on its package repository: the
// upgrade dialog asks the host (OpDockerPlan) before anything happens.
type dockerLatestCache struct {
	mu  sync.Mutex
	at  time.Time
	val string
}

func (c *dockerLatestCache) get(ctx context.Context) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && time.Since(c.at) < 6*time.Hour {
		return c.val
	}
	c.at = time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPI+"/repos/moby/moby/releases?per_page=20", nil)
	if err != nil {
		return c.val
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "TainerHub")
	resp, err := githubClient.Do(req)
	if err != nil {
		c.at = time.Now().Add(-5*time.Hour - 50*time.Minute) // retry in ~10 min
		return c.val
	}
	defer resp.Body.Close()
	var rels []struct {
		Tag        string `json:"tag_name"`
		Prerelease bool   `json:"prerelease"`
		Draft      bool   `json:"draft"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&rels) != nil {
		return c.val
	}
	for _, r := range rels {
		v := strings.TrimPrefix(r.Tag, "docker-")
		if r.Prerelease || r.Draft || !strings.HasPrefix(v, "v") || strings.Contains(v, "-") {
			continue
		}
		c.val = strings.TrimPrefix(v, "v")
		break
	}
	return c.val
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

func (s *Server) dockerPlan(w http.ResponseWriter, r *http.Request) {
	s.runOp(w, r, wire.Operation{Kind: wire.OpDockerPlan}, "docker")
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
