package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/auth"
	"github.com/prezmc/tainerhub/internal/registry"
	"github.com/prezmc/tainerhub/internal/store"
	"github.com/prezmc/tainerhub/internal/wire"
)

// ---------------- window ----------------

func TestNextWindowDaysTimeAndZone(t *testing.T) {
	set := defaultAutoSettings()
	set.TimeZone = "UTC"
	tue := time.Date(2026, 11, 24, 2, 0, 0, 0, time.UTC) // a Tuesday
	if got, want := nextWindow(tue, set), time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("daily: %v, want %v", got, want)
	}
	// Exactly at the start: the next one (this one is already running).
	if got, want := nextWindow(time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC), set), time.Date(2026, 11, 25, 3, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("at the start: %v, want %v", got, want)
	}
	set.Days = []string{"mon"}
	if got, want := nextWindow(tue, set), time.Date(2026, 11, 30, 3, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("mondays: %v, want %v", got, want)
	}
	set.Days, set.Time, set.TimeZone = []string{"tue"}, "23:30", "America/New_York"
	// 02:00 UTC Tuesday is still Monday evening in New York.
	ny, _ := time.LoadLocation("America/New_York")
	if got, want := nextWindow(tue, set), time.Date(2026, 11, 24, 23, 30, 0, 0, ny); !got.Equal(want) {
		t.Fatalf("zone: %v, want %v", got, want)
	}
}

func TestNextWindowDST(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	set := defaultAutoSettings()
	set.TimeZone, set.Time = "America/New_York", "02:30"
	// 8 March 2026: clocks go from 02:00 to 03:00, so 02:30 doesn't exist.
	got := nextWindow(time.Date(2026, 3, 8, 0, 0, 0, 0, ny), set)
	if got.Day() != 8 || got.In(ny).Hour() != 3 || got.In(ny).Minute() != 30 {
		t.Fatalf("spring forward: %v, want 03:30 that day", got.In(ny))
	}
	if next := nextWindow(got, set); next.In(ny).Day() != 9 || next.In(ny).Hour() != 2 {
		t.Fatalf("after spring forward: %v", next.In(ny))
	}
	// 1 November 2026: 01:30 happens twice; the window runs once.
	set.Time = "01:30"
	first := nextWindow(time.Date(2026, 11, 1, 0, 0, 0, 0, ny), set)
	if first.In(ny).Day() != 1 || first.In(ny).Hour() != 1 {
		t.Fatalf("fall back: %v", first.In(ny))
	}
	if next := nextWindow(first, set); next.In(ny).Day() != 2 {
		t.Fatalf("ran twice on fall-back night: %v then %v", first, next.In(ny))
	}
	if next := nextWindow(first.Add(time.Hour), set); next.In(ny).Day() != 2 {
		t.Fatalf("the repeated hour started another window: %v", next.In(ny))
	}
}

func TestAutoSettingsValidate(t *testing.T) {
	s := autoSettings{Enabled: true, Days: []string{"Sunday", "mon", "MON"}, Time: " 04:15 ", TimeZone: "Europe/Berlin", MaxMinutes: 60}
	if err := s.validate(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(s.Days, ",") != "mon,sun" || s.Time != "04:15" {
		t.Fatalf("normalized: %+v", s)
	}
	for name, bad := range map[string]autoSettings{
		"no days":  {Days: nil, Time: "03:00", MaxMinutes: 60},
		"bad day":  {Days: []string{"someday"}, Time: "03:00", MaxMinutes: 60},
		"bad time": {Days: []string{"mon"}, Time: "3:00", MaxMinutes: 60},
		"25:00":    {Days: []string{"mon"}, Time: "25:00", MaxMinutes: 60},
		"zone":     {Days: []string{"mon"}, Time: "03:00", TimeZone: "Mars/Olympus", MaxMinutes: 60},
		"short":    {Days: []string{"mon"}, Time: "03:00", MaxMinutes: 5},
		"long":     {Days: []string{"mon"}, Time: "03:00", MaxMinutes: 13 * 60},
	} {
		if err := bad.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if got := parseAutoSettings(`{"days":[]}`); got.Time != "03:00" || len(got.Days) != 7 || got.MaxMinutes != 120 || !got.Enabled {
		t.Fatalf("broken settings should fall back to the defaults: %+v", got)
	}
}

// ---------------- selection ----------------

func TestPlanContainer(t *testing.T) {
	st := registry.Status{Hello: &wire.Hello{OwnProject: "tainerhub"}, Containers: []wire.Container{
		{ID: "1", Name: "web", Image: "nginx:1"},
		{ID: "2", Name: "db", Image: "postgres:16"},
		{ID: "3", Name: "pinned", Image: "redis@sha256:abc"},
		{ID: "4", Name: "app-1", Image: "app:1", Project: "app"},
		{ID: "5", Name: "agent", Image: "x:1", Project: "tainerhub"},
		{ID: "6", Name: "cached", Image: "cache:1", UpdateAvailable: true},
	}}
	upd := wire.ImageUpdates{Images: map[string]bool{"nginx:1": true, "postgres:16": false, "app:1": true}}
	for _, tc := range []struct {
		name, skip string
	}{
		{"web", ""},
		{"db", "already up to date"},
		{"pinned", "pinned"},
		{"app-1", "compose project app"},
		{"agent", "TainerHub's own"},
		{"tainerhub-agent", "TainerHub's own"},
		{"gone", "no container"},
		{"cached", ""}, // the check couldn't tell this time: the host's last answer counts
	} {
		c, refs, skip := planContainer(tc.name, st, upd)
		if tc.skip == "" && (skip != "" || c == nil || len(refs) != 1) {
			t.Errorf("%s: skipped (%q), want an update", tc.name, skip)
		}
		if tc.skip != "" && !strings.Contains(skip, tc.skip) {
			t.Errorf("%s: skip %q, want %q", tc.name, skip, tc.skip)
		}
	}
}

func deployedStack(name, compose string) store.Stack {
	now := time.Now()
	st := store.Stack{ID: "5d7c8f0e-0000-4000-8000-00000000000" + name[:1], HostID: autoHostA, Name: name, Compose: compose, Files: []store.StackFile{}, DeployedAt: &now}
	st.DeployedHash = stackHash(st)
	return st
}

func TestPlanStack(t *testing.T) {
	compose := "services:\n  app:\n    image: app:1\n  cache:\n    image: \"redis:7\" # pinned major\n  env:\n    image: ${IMG}\n"
	st := registry.Status{Hello: &wire.Hello{OwnProject: "tainerhub"}, Containers: []wire.Container{
		{Name: "app-app-1", Image: "app:1", Project: "app"},
		{Name: "other", Image: "redis:7"}, // not the stack's, but its image is in the compose file
	}}
	upd := wire.ImageUpdates{Images: map[string]bool{"app:1": false, "redis:7": true}}
	app := deployedStack("app", compose)
	if refs, skip := planStack(app, st, upd, false); skip != "" || strings.Join(refs, ",") != "redis:7" {
		t.Fatalf("compose images: %v %q", refs, skip)
	}
	if _, skip := planStack(app, st, upd, true); !strings.Contains(skip, "job is running") {
		t.Fatalf("job running: %q", skip)
	}
	pending := app
	pending.Compose += "  more:\n    image: x:1\n"
	if _, skip := planStack(pending, st, upd, false); !strings.Contains(skip, "saved changes") {
		t.Fatalf("pending: %q", skip)
	}
	never := app
	never.DeployedAt = nil
	if _, skip := planStack(never, st, upd, false); !strings.Contains(skip, "never deployed") {
		t.Fatalf("never deployed: %q", skip)
	}
	none := wire.ImageUpdates{Images: map[string]bool{"app:1": false, "redis:7": false}}
	if _, skip := planStack(app, st, none, false); skip != "already up to date" {
		t.Fatalf("no update: %q", skip)
	}
	// A stack that builds and has no pulled image with an update: can't tell.
	build := deployedStack("bld", "services:\n  api:\n    build: .\n  db:\n    image: postgres:16\n")
	if _, skip := planStack(build, st, wire.ImageUpdates{Images: map[string]bool{"postgres:16": false}}, false); !strings.HasPrefix(skip, "builds") {
		t.Fatalf("builds: %q", skip)
	}
	// ... but a pulled image with an update is applied (build --pull also refreshes base images).
	if refs, skip := planStack(build, st, wire.ImageUpdates{Images: map[string]bool{"postgres:16": true}}, false); skip != "" || len(refs) != 1 {
		t.Fatalf("builds with a pulled update: %v %q", refs, skip)
	}
	// The tag on disk is current (another stack pulled it), but this
	// stack's container still runs the old image: it has an update.
	shared := registry.Status{Containers: []wire.Container{{ID: "c9", Name: "app-app-1", Image: "app:1", Project: "app"}}}
	behind := wire.ImageUpdates{Images: map[string]bool{"app:1": false, "redis:7": false}, Containers: map[string]bool{"c9": true}}
	if refs, skip := planStack(app, shared, behind, false); skip != "" || strings.Join(refs, ",") != "app:1" {
		t.Fatalf("container behind a current tag: %v %q", refs, skip)
	}
	current := wire.ImageUpdates{Images: map[string]bool{"app:1": true, "redis:7": false}, Containers: map[string]bool{"c9": false}}
	if _, skip := planStack(app, shared, current, false); skip != "already up to date" {
		t.Fatalf("container already on the registry's image: %q", skip)
	}
	own := deployedStack("tainerhub", "services:\n  hub:\n    image: tainerhub:latest\n")
	if _, skip := planStack(own, st, upd, false); !strings.Contains(skip, "TainerHub's own") {
		t.Fatalf("own project: %q", skip)
	}
	sneaky := deployedStack("ops", "services:\n  x:\n    image: x:1\n")
	st2 := registry.Status{Containers: []wire.Container{{Name: "tainerhub-agent", Image: "x:1", Project: "ops"}}}
	if _, skip := planStack(sneaky, st2, upd, false); !strings.Contains(skip, "TainerHub's own") {
		t.Fatalf("own containers in a stack: %q", skip)
	}
}

// ---------------- fakes ----------------

const (
	autoHostA = "0a000000-0000-4000-8000-00000000000a"
	autoHostB = "0b000000-0000-4000-8000-00000000000b"
	stackX    = "5d7c8f0e-0000-4000-8000-0000000000aa"
)

type fakeAutoStore struct {
	mu       sync.Mutex
	settings map[string]string
	targets  []store.AutoUpdateTarget
	stacks   map[string]store.Stack
	hosts    map[string]store.Host
	deployed map[string]string
	points   map[string][]store.RollbackPoint // newest last
	n        int
	moves    []store.StackMove // see scope_test.go
	auditFor *store.User       // the viewer ListAudit was last asked for
}

func newFakeAutoStore() *fakeAutoStore {
	return &fakeAutoStore{settings: map[string]string{}, stacks: map[string]store.Stack{}, hosts: map[string]store.Host{}, deployed: map[string]string{}, points: map[string][]store.RollbackPoint{}}
}

// AddRollbackPoint keeps the newest store.RollbackKeep, like the SQL does.
func (f *fakeAutoStore) AddRollbackPoint(_ context.Context, p store.RollbackPoint) (store.RollbackPoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	p.ID = fmt.Sprintf("70000000-0000-4000-8000-%012d", f.n)
	p.CreatedAt = time.Now()
	ps := append(f.points[p.StackID], p)
	if len(ps) > store.RollbackKeep {
		ps = ps[len(ps)-store.RollbackKeep:]
	}
	f.points[p.StackID] = ps
	return p, nil
}

func (f *fakeAutoStore) LatestRollbackPoint(_ context.Context, id string) (store.RollbackPoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ps := f.points[id]
	if len(ps) == 0 {
		return store.RollbackPoint{}, store.ErrNotFound
	}
	return ps[len(ps)-1], nil
}

func (f *fakeAutoStore) SetStackRolledBack(_ context.Context, id string, to *time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.stacks[id]
	st.RolledBackTo = to
	f.stacks[id] = st
	return nil
}

func (f *fakeAutoStore) SetStackAutoRollback(_ context.Context, id string, on bool) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, t := range f.targets {
		if t.Kind == "stack" && t.StackID == id {
			f.targets[i].AutoRollback = on
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeAutoStore) Setting(_ context.Context, k string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.settings[k], nil
}

func (f *fakeAutoStore) SetSetting(_ context.Context, k, v string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settings[k] = v
	return nil
}

func (f *fakeAutoStore) ListAutoUpdateTargets(context.Context) ([]store.AutoUpdateTarget, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []store.AutoUpdateTarget{}
	for _, t := range f.targets {
		if t.Kind == "stack" {
			st := f.stacks[t.StackID]
			t.StackName, t.HostID = st.Name, st.HostID
		}
		t.HostName = f.hosts[t.HostID].Name
		out = append(out, t)
	}
	return out, nil
}

func (f *fakeAutoStore) add(t store.AutoUpdateTarget) {
	f.n++
	t.ID = fmt.Sprintf("7a000000-0000-4000-8000-%012d", f.n)
	f.targets = append(f.targets, t)
}

func (f *fakeAutoStore) EnableStackAutoUpdate(_ context.Context, id, by string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.targets {
		if t.Kind == "stack" && t.StackID == id {
			return nil
		}
	}
	f.add(store.AutoUpdateTarget{Kind: "stack", StackID: id, EnabledBy: by, AutoRollback: true})
	return nil
}

func (f *fakeAutoStore) EnableContainerAutoUpdate(_ context.Context, host, name, by string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.targets {
		if t.Kind == "container" && t.HostID == host && t.ContainerName == name {
			return nil
		}
	}
	f.add(store.AutoUpdateTarget{Kind: "container", HostID: host, ContainerName: name, EnabledBy: by})
	return nil
}

func (f *fakeAutoStore) remove(match func(store.AutoUpdateTarget) bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, t := range f.targets {
		if match(t) {
			f.targets = append(f.targets[:i], f.targets[i+1:]...)
			return true
		}
	}
	return false
}

func (f *fakeAutoStore) DisableStackAutoUpdate(_ context.Context, id string) (bool, error) {
	return f.remove(func(t store.AutoUpdateTarget) bool { return t.Kind == "stack" && t.StackID == id }), nil
}

func (f *fakeAutoStore) DisableContainerAutoUpdate(_ context.Context, host, name string) (bool, error) {
	return f.remove(func(t store.AutoUpdateTarget) bool {
		return t.Kind == "container" && t.HostID == host && t.ContainerName == name
	}), nil
}

func (f *fakeAutoStore) RecordAutoUpdate(_ context.Context, id string, at time.Time, result, errText string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.targets {
		if f.targets[i].ID == id {
			f.targets[i].LastRunAt, f.targets[i].LastResult, f.targets[i].LastError = &at, result, errText
		}
	}
	return nil
}

func (f *fakeAutoStore) GetStack(_ context.Context, id string) (store.Stack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.stacks[id]
	if !ok {
		return st, store.ErrNotFound
	}
	return st, nil
}

func (f *fakeAutoStore) GetHost(_ context.Context, id string) (store.Host, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, ok := f.hosts[id]
	if !ok {
		return h, store.ErrNotFound
	}
	return h, nil
}

func (f *fakeAutoStore) MarkStackDeployed(_ context.Context, id, hash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deployed[id] = hash
	return nil
}

func (f *fakeAutoStore) DeleteStack(context.Context, string) error { return nil }

func (f *fakeAutoStore) target(name string) store.AutoUpdateTarget {
	ts, _ := f.ListAutoUpdateTargets(context.Background())
	for _, t := range ts {
		if t.Name() == name {
			return t
		}
	}
	return store.AutoUpdateTarget{}
}

type autoClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *autoClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *autoClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func (c *autoClock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// fakeHost is a connected agent that answers update checks, recreates and
// stack deploys, changing its container list like Docker would.
type fakeHost struct {
	rig        *autoRig
	a          *registry.Agent
	mu         sync.Mutex
	containers []wire.Container
	updates    map[string]bool
	// What a recreate or deploy leaves behind: state and status of the new
	// containers (default running), or an error.
	newState, newStatus string
	failWith            string
	failRollback        string        // a rollback's error (rollbacks ignore failWith)
	hold                time.Duration // how long an update takes (real time)
	ops                 []string
	rollbacks           []wire.StackCommand
}

// fakeImageID is a container's image ID in the fake: changes with its ID.
func fakeImageID(containerID string) string {
	return fmt.Sprintf("sha256:%064x", sha256.Sum256([]byte(containerID)))
}

// stackImages answers OpStackImages from the fake's containers.
func (h *fakeHost) stackImages(project string) wire.StackImages {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := wire.StackImages{Services: map[string]wire.StackImage{}}
	for _, c := range h.containers {
		if c.Project != project {
			continue
		}
		svc := c.Labels["com.docker.compose.service"]
		if svc == "" {
			svc = strings.TrimPrefix(strings.TrimSuffix(c.Name, "-1"), project+"-")
		}
		out.Services[svc] = wire.StackImage{Ref: c.Image, ImageID: fakeImageID(c.ID)}
	}
	return out
}

func (h *fakeHost) snapshot() {
	h.mu.Lock()
	cs := append([]wire.Container(nil), h.containers...)
	h.mu.Unlock()
	h.a.Handle(wire.AgentMessage{Snapshot: &wire.Snapshot{At: time.Now().Unix(), Containers: cs}}, nil)
}

func (h *fakeHost) reply(id string, data any, errText string) {
	b, _ := json.Marshal(data)
	h.a.Handle(wire.AgentMessage{Result: &wire.CommandResult{CommandID: id, OK: errText == "", Error: errText, Data: b}}, nil)
}

// busy runs one update's work and tracks how many updates run at once, on
// this host and overall. The update stops counting as running before busy
// returns, so callers must tell the hub it finished only after busy returns:
// once the hub hears that, it may start the next update, and if that one
// were counted before this one stopped, the counts would show two at once
// (a gap a slow CI machine opens up).
func (h *fakeHost) busy(name string, work func()) {
	r := h.rig
	r.mu.Lock()
	r.active[h.a.HostID]++
	r.total++
	r.maxHost = max(r.maxHost, r.active[h.a.HostID])
	r.maxTotal = max(r.maxTotal, r.total)
	r.mu.Unlock()
	h.mu.Lock()
	h.ops = append(h.ops, name)
	h.mu.Unlock()
	time.Sleep(h.hold)
	work()
	r.mu.Lock()
	r.active[h.a.HostID]--
	r.total--
	r.mu.Unlock()
}

func (h *fakeHost) replace(match func(wire.Container) bool) {
	h.mu.Lock()
	for i, c := range h.containers {
		if match(c) {
			h.containers[i].ID = "new-" + c.Name
			h.containers[i].State, h.containers[i].Status = h.newState, h.newStatus
			if h.newState == "" {
				h.containers[i].State, h.containers[i].Status = "running", "Up 1 second"
			}
		}
	}
	h.mu.Unlock()
	h.snapshot()
}

func (h *fakeHost) serve() {
	for {
		select {
		case <-h.a.Done():
			return
		case m := <-h.a.Out():
			cmd := m.Command
			if cmd == nil {
				continue
			}
			switch {
			case cmd.Op != nil && cmd.Op.Kind == wire.OpImagesCheckUpdates:
				h.mu.Lock()
				res := wire.ImageUpdates{Images: h.updates}
				h.mu.Unlock()
				go h.reply(cmd.ID, res, "")
			case cmd.Op != nil && cmd.Op.Kind == wire.OpStackImages:
				h.mu.Lock()
				h.ops = append(h.ops, "images "+cmd.Op.Target+" "+cmd.Op.Arg)
				h.mu.Unlock()
				go h.reply(cmd.ID, h.stackImages(cmd.Op.Target), "")
			case cmd.Stack != nil && cmd.Stack.Action == wire.StackRollback:
				go func(cmd wire.Command) {
					var fail string
					h.busy("rollback "+cmd.Stack.Name, func() {
						h.mu.Lock()
						h.rollbacks = append(h.rollbacks, *cmd.Stack)
						fail = h.failRollback
						if fail == "" {
							// The old images come back up fine.
							for i, c := range h.containers {
								if c.Project == cmd.Stack.Name {
									h.containers[i].ID = "back-" + c.Name
									h.containers[i].State, h.containers[i].Status = "running", "Up 1 second"
								}
							}
						}
						h.mu.Unlock()
						h.snapshot()
					})
					h.a.Handle(wire.AgentMessage{Log: &wire.LogChunk{CommandID: cmd.ID, EOF: true, Error: fail}}, nil)
				}(*cmd)
			case cmd.Op != nil && cmd.Op.Kind == wire.OpContainerRecreate:
				go func(cmd wire.Command) {
					fail := h.failWith
					h.busy("recreate "+cmd.Op.Target, func() {
						if fail == "" {
							h.replace(func(c wire.Container) bool { return c.ID == cmd.Op.Target })
						}
					})
					if fail != "" {
						h.reply(cmd.ID, nil, fail)
						return
					}
					h.reply(cmd.ID, wire.RecreateResult{Status: "updated", OldImage: "sha256:old", NewImage: "sha256:new"}, "")
				}(*cmd)
			case cmd.Op != nil: // anything else: noted and answered OK
				h.mu.Lock()
				h.ops = append(h.ops, "op "+cmd.Op.Kind+" "+cmd.Op.Arg)
				h.mu.Unlock()
				go h.reply(cmd.ID, nil, "")
			case cmd.Stack != nil:
				go func(cmd wire.Command) {
					if !cmd.Stack.Pull || cmd.Stack.Action != wire.StackDeploy {
						h.rig.t.Errorf("stack command %+v, want a deploy with pull", cmd.Stack)
					}
					fail := h.failWith
					h.busy("stack "+cmd.Stack.Name, func() {
						if fail == "" {
							h.replace(func(c wire.Container) bool { return c.Project == cmd.Stack.Name })
						}
					})
					h.a.Handle(wire.AgentMessage{Log: &wire.LogChunk{CommandID: cmd.ID, EOF: true, Error: fail}}, nil)
				}(*cmd)
			}
		}
	}
}

type autoRig struct {
	t     *testing.T
	s     *Server
	db    *fakeAutoStore
	clk   *autoClock
	h     http.Handler
	user  store.User
	token *auth.TokenInfo
	fresh bool

	mu       sync.Mutex
	audit    []store.AuditEntry
	active   map[string]int
	total    int
	maxHost  int
	maxTotal int
}

func newAutoRig(t *testing.T) *autoRig {
	t.Helper()
	x := &autoRig{t: t, db: newFakeAutoStore(), clk: &autoClock{t: time.Date(2026, 11, 24, 2, 0, 0, 0, time.UTC)},
		user: store.User{ID: "u1", Username: "ann", Role: "admin"}, fresh: true, active: map[string]int{}}
	x.s = &Server{
		Auth:   &auth.Service{},
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Reg:    registry.New(),
		autoDB: x.db,
		audited: func(e store.AuditEntry) {
			x.mu.Lock()
			x.audit = append(x.audit, e)
			x.mu.Unlock()
		},
		authn: func(*http.Request) (store.User, *auth.TokenInfo, error) { return x.user, x.token, nil },
		freshCheck: func(*http.Request) error {
			if x.fresh {
				return nil
			}
			return auth.ErrReauth
		},
	}
	x.s.autoUp.now = x.clk.Now
	// Waiting moves the fake clock on (and lets the fake agents run).
	x.s.autoUp.sleep = func(ctx context.Context, d time.Duration) error {
		x.clk.Add(d)
		time.Sleep(100 * time.Microsecond)
		return ctx.Err()
	}
	x.h = x.s.Handler()
	x.db.settings[settingAutoUpdates] = `{"enabled":true,"days":["mon","tue","wed","thu","fri","sat","sun"],"time":"03:00","timeZone":"UTC","maxMinutes":120}`
	return x
}

func (x *autoRig) host(id, name string, cs ...wire.Container) *fakeHost {
	x.db.hosts[id] = store.Host{ID: id, Name: name}
	h := &fakeHost{rig: x, a: x.s.Reg.Attach(id, name, func() {}), containers: cs, updates: map[string]bool{}}
	h.a.Handle(wire.AgentMessage{Hello: &wire.Hello{OwnProject: "tainerhub", Stacks: &wire.StackSupport{Dir: "/stacks", Rollback: true}}}, nil)
	h.snapshot()
	go h.serve()
	x.t.Cleanup(func() { x.s.Reg.Detach(h.a) })
	return h
}

func (x *autoRig) optIn(host, name string) {
	_ = x.db.EnableContainerAutoUpdate(context.Background(), host, name, "ann")
}

// runNow runs every target and waits for the run to finish.
func (x *autoRig) runNow() *autoRun {
	x.t.Helper()
	if _, err := x.s.startAutoRun("manual", "ann", x.clk.Now().Add(2*time.Hour)); err != nil {
		x.t.Fatal(err)
	}
	x.s.autoUp.wg.Wait()
	return x.s.latestAutoRun()
}

func outcome(t *testing.T, run *autoRun, name string) autoOutcome {
	t.Helper()
	for _, o := range run.Outcomes {
		if o.Name == name {
			return o
		}
	}
	t.Fatalf("no outcome for %s in %+v", name, run.Outcomes)
	return autoOutcome{}
}

func (x *autoRig) do(method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("X-TainerHub", "1")
	w := httptest.NewRecorder()
	x.h.ServeHTTP(w, r)
	return w
}

// ---------------- runs ----------------

func TestAutoRunUpdatesWhatHasNewerImages(t *testing.T) {
	x := newAutoRig(t)
	stack := deployedStack("app", "services:\n  web:\n    image: app:1\n")
	stack.ID = stackX
	x.db.stacks[stack.ID] = stack
	h := x.host(autoHostA, "alpha",
		wire.Container{ID: "c1", Name: "web", Image: "nginx:1", State: "running", Status: "Up 2 days"},
		wire.Container{ID: "c2", Name: "db", Image: "postgres:16", State: "running", Status: "Up 2 days"},
		wire.Container{ID: "c3", Name: "app-web-1", Image: "app:1", Project: "app", State: "running", Status: "Up 2 days"},
	)
	h.updates = map[string]bool{"nginx:1": true, "postgres:16": false, "app:1": true}
	x.optIn(autoHostA, "web")
	x.optIn(autoHostA, "db")
	_ = x.db.EnableStackAutoUpdate(context.Background(), stack.ID, "ann")
	_ = x.db.EnableContainerAutoUpdate(context.Background(), autoHostB, "far", "ann")
	x.db.hosts[autoHostB] = store.Host{ID: autoHostB, Name: "beta"} // never connects

	run := x.runNow()
	if run.Running || run.FinishedAt == nil || len(run.Outcomes) != 4 {
		t.Fatalf("run: %+v", run)
	}
	if o := outcome(t, run, "web"); o.Result != autoOK || o.OldImage != "sha256:old" || o.NewImage != "sha256:new" {
		t.Errorf("web: %+v", o)
	}
	if o := outcome(t, run, "db"); o.Result != autoSkipped || o.Reason != "already up to date" {
		t.Errorf("db: %+v", o)
	}
	if o := outcome(t, run, "app"); o.Result != autoOK || o.JobID == "" || strings.Join(o.Images, ",") != "app:1" {
		t.Errorf("app: %+v", o)
	}
	if o := outcome(t, run, "far"); o.Result != autoSkipped || o.Reason != "host offline" {
		t.Errorf("far: %+v", o)
	}
	if got := x.db.target("web"); got.LastResult != autoOK || got.LastRunAt == nil {
		t.Errorf("web's row: %+v", got)
	}
	if got := x.db.target("db"); got.LastResult != autoSkipped || got.LastError != "already up to date" {
		t.Errorf("db's row: %+v", got)
	}
	if x.db.deployed[stack.ID] != stackHash(stack) {
		t.Error("the stack's deploy wasn't recorded")
	}
	// The stack job ran as "Automatic updates".
	if j := x.s.jobs.get(outcome(t, run, "app").JobID); j == nil || j.Actor != autoActorName || j.Action != "pull" {
		t.Errorf("job: %+v", j)
	}

	x.mu.Lock()
	var actions []string
	for _, e := range x.audit {
		if e.Actor == autoActor {
			actions = append(actions, e.Action+" "+e.Target)
			if e.UserID != nil {
				t.Errorf("%s has a user", e.Action)
			}
		}
	}
	x.mu.Unlock()
	joined := strings.Join(actions, "; ")
	for _, want := range []string{"update.auto_applied alpha/web", "update.auto_applied alpha/app", "stack.pull alpha/app"} {
		if !strings.Contains(joined, want) {
			t.Errorf("audit lacks %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "alpha/db") || strings.Contains(joined, "far") {
		t.Errorf("skipped targets are not audited: %s", joined)
	}
	feed := 0
	for _, a := range x.s.Reg.Activity(50) {
		if a.Actor == autoActorName {
			feed++
			if a.Action != "update.auto_applied" {
				t.Errorf("activity %+v", a)
			}
		}
	}
	if feed != 2 {
		t.Errorf("%d activity entries, want 2 (one per update)", feed)
	}
}

func TestAutoRunFailuresAndSkips(t *testing.T) {
	x := newAutoRig(t)
	stack := deployedStack("app", "services:\n  web:\n    image: app:1\n")
	stack.ID = stackX
	x.db.stacks[stack.ID] = stack
	h := x.host(autoHostA, "alpha",
		wire.Container{ID: "c1", Name: "web", Image: "nginx:1", State: "running"},
		wire.Container{ID: "c3", Name: "app-web-1", Image: "app:1", Project: "app", State: "running"},
	)
	h.updates = map[string]bool{"nginx:1": true, "app:1": true}
	x.optIn(autoHostA, "web")
	_ = x.db.EnableStackAutoUpdate(context.Background(), stack.ID, "ann")

	// A job already running for the stack: skipped, not queued behind it.
	busy, _ := x.s.jobs.start(&job{StackID: stack.ID, Action: "deploy"})
	h.failWith = "pull access denied"
	run := x.runNow()
	if o := outcome(t, run, "web"); o.Result != autoFailed || o.Reason != "pull access denied" {
		t.Errorf("web: %+v", o)
	}
	if o := outcome(t, run, "app"); o.Result != autoSkipped || !strings.Contains(o.Reason, "job is running") {
		t.Errorf("app: %+v", o)
	}
	busy.finish("")

	// The stack's deploy fails: failed, with the job's error (automatic
	// rollback is off here; TestAutoRollback covers it).
	_, _ = x.db.SetStackAutoRollback(context.Background(), stack.ID, false)
	h.failWith = "compose up failed"
	run = x.runNow()
	if o := outcome(t, run, "app"); o.Result != autoFailed || o.Reason != "compose up failed" {
		t.Errorf("app: %+v", o)
	}
	x.mu.Lock()
	failed := 0
	for _, e := range x.audit {
		if e.Action == "update.auto_failed" {
			failed++
			var d map[string]any
			_ = json.Unmarshal(e.Detail, &d)
			if d["error"] == "" || d["error"] == nil {
				t.Errorf("failure without its error: %s", e.Detail)
			}
		}
	}
	x.mu.Unlock()
	if failed != 3 { // web twice, app once
		t.Errorf("%d update.auto_failed entries, want 3", failed)
	}

	// The container is being moved to another host: skipped.
	h.failWith = ""
	moving, _ := x.s.jobs.start(&job{StackID: "container:" + autoHostA + "/web", Action: "move"})
	defer moving.finish("")
	if o := outcome(t, x.runNow(), "web"); o.Result != autoSkipped || !strings.Contains(o.Reason, "job is running") {
		t.Errorf("web while moving: %+v", o)
	}
}

func TestAutoRunWindowEnds(t *testing.T) {
	x := newAutoRig(t)
	h := x.host(autoHostA, "alpha",
		wire.Container{ID: "c1", Name: "one", Image: "a:1", State: "running"},
		wire.Container{ID: "c2", Name: "two", Image: "b:1", State: "running"},
	)
	h.updates = map[string]bool{"a:1": true, "b:1": true}
	x.optIn(autoHostA, "one")
	x.optIn(autoHostA, "two")
	// The first update's health wait (one 5 s poll) runs past a 3 s window.
	if _, err := x.s.startAutoRun("window", "", x.clk.Now().Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	x.s.autoUp.wg.Wait()
	run := x.s.latestAutoRun()
	if o := outcome(t, run, "one"); o.Result != autoOK {
		t.Errorf("one: %+v", o)
	}
	if o := outcome(t, run, "two"); o.Result != autoSkipped || !strings.Contains(o.Reason, "next window") {
		t.Errorf("two: %+v", o)
	}
}

func TestAutoRunOneAtATimePerHostFourHosts(t *testing.T) {
	x := newAutoRig(t)
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("0c000000-0000-4000-8000-00000000000%d", i)
		h := x.host(id, fmt.Sprintf("h%d", i),
			wire.Container{ID: "a", Name: "one", Image: "a:1", State: "running"},
			wire.Container{ID: "b", Name: "two", Image: "a:1", State: "running"},
		)
		h.updates = map[string]bool{"a:1": true}
		h.hold = 20 * time.Millisecond
		x.optIn(id, "one")
		x.optIn(id, "two")
	}
	run := x.runNow()
	if len(run.Outcomes) != 12 {
		t.Fatalf("%d outcomes", len(run.Outcomes))
	}
	for _, o := range run.Outcomes {
		if o.Result != autoOK {
			t.Errorf("%s on %s: %+v", o.Name, o.Host, o)
		}
	}
	if x.maxHost != 1 {
		t.Errorf("up to %d updates at once on one host, want 1", x.maxHost)
	}
	if x.maxTotal > autoHostParallel || x.maxTotal < 2 {
		t.Errorf("up to %d hosts at once, want 2..%d", x.maxTotal, autoHostParallel)
	}
}

func TestAutoHealthWait(t *testing.T) {
	for _, tc := range []struct {
		name, state, status string
		later               string // the status a few polls later; "" keeps it
		result, reason      string
	}{
		{"healthy", "running", "Up 3 seconds (healthy)", "", autoOK, ""},
		{"starts healthy", "running", "Up 1 second (health: starting)", "Up 20 seconds (healthy)", autoOK, ""},
		{"crashes", "exited", "Exited (1) 2 seconds ago", "", autoFailed, "web is exited"},
		{"unhealthy", "running", "Up 1 minute (unhealthy)", "", autoFailed, "web is unhealthy"},
		{"never ready", "running", "Up 1 second (health: starting)", "", autoFailed, "still starting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newAutoRig(t)
			h := x.host(autoHostA, "alpha", wire.Container{ID: "c1", Name: "web", Image: "nginx:1", State: "running"})
			h.updates = map[string]bool{"nginx:1": true}
			h.newState, h.newStatus = tc.state, tc.status
			x.optIn(autoHostA, "web")
			if tc.later != "" {
				polls := 0
				inner := x.s.autoUp.sleep
				x.s.autoUp.sleep = func(ctx context.Context, d time.Duration) error {
					if d == autoHealthPoll {
						if polls++; polls == 3 {
							h.mu.Lock()
							h.containers[0].Status = tc.later
							h.mu.Unlock()
							h.snapshot()
						}
					}
					return inner(ctx, d)
				}
			}
			run := x.runNow()
			o := outcome(t, run, "web")
			if o.Result != tc.result || !strings.Contains(o.Reason, tc.reason) {
				t.Fatalf("%+v, want %s %q", o, tc.result, tc.reason)
			}
			if tc.result == autoFailed && !strings.Contains(o.Reason, "2m0s") {
				t.Errorf("the reason should say how long it waited: %q", o.Reason)
			}
		})
	}
}

func TestHealthProblem(t *testing.T) {
	if p := healthProblem([]wire.Container{{Name: "web", State: "running", Status: "Up 1 minute"}, {Name: "migrate", State: "exited", Status: "Exited (0) 5 seconds ago"}}); p != "" {
		t.Fatalf("a one-off service that finished fine counts as up: %q", p)
	}
	if p := healthProblem([]wire.Container{{Name: "migrate", State: "exited", Status: "Exited (1) 5 seconds ago"}}); p != "migrate is exited" {
		t.Fatalf("failed one-off: %q", p)
	}
	if p := healthProblem(nil); p != "no containers found" {
		t.Fatalf("none: %q", p)
	}
}

func TestAutoUpdateStoppedContainerNotWaitedFor(t *testing.T) {
	x := newAutoRig(t)
	h := x.host(autoHostA, "alpha", wire.Container{ID: "c1", Name: "web", Image: "nginx:1", State: "exited"})
	h.updates = map[string]bool{"nginx:1": true}
	h.newState, h.newStatus = "created", "Created"
	x.optIn(autoHostA, "web")
	if o := outcome(t, x.runNow(), "web"); o.Result != autoOK {
		t.Fatalf("%+v", o)
	}
}

func TestAutoHealthWaitHostGoesOffline(t *testing.T) {
	x := newAutoRig(t)
	h := x.host(autoHostA, "alpha", wire.Container{ID: "c1", Name: "web", Image: "nginx:1", State: "running"})
	h.updates = map[string]bool{"nginx:1": true}
	h.newState, h.newStatus = "running", "Up 1 second (health: starting)"
	x.optIn(autoHostA, "web")
	inner := x.s.autoUp.sleep
	x.s.autoUp.sleep = func(ctx context.Context, d time.Duration) error {
		if d == autoHealthPoll {
			x.s.Reg.Detach(h.a)
		}
		return inner(ctx, d)
	}
	if o := outcome(t, x.runNow(), "web"); o.Result != autoFailed || !strings.Contains(o.Reason, "offline") {
		t.Fatalf("%+v", o)
	}
}

// A check already running on the host (scheduled, or from the UI) is
// waited for rather than overlapped.
func TestAutoRunWaitsForRunningCheck(t *testing.T) {
	x := newAutoRig(t)
	reg := x.s.Reg
	a := reg.Attach(autoHostA, "alpha", func() {})
	x.db.hosts[autoHostA] = store.Host{ID: autoHostA, Name: "alpha"}
	a.Handle(wire.AgentMessage{Snapshot: &wire.Snapshot{Containers: []wire.Container{{ID: "c1", Name: "web", Image: "nginx:1", State: "running"}}}}, nil)
	x.optIn(autoHostA, "web")
	release := make(chan struct{})
	var mu sync.Mutex
	inflight, maxInflight := 0, 0
	go func() {
		for {
			select {
			case <-a.Done():
				return
			case m := <-a.Out():
				if m.Command == nil || m.Command.Op == nil || m.Command.Op.Kind != wire.OpImagesCheckUpdates {
					continue
				}
				mu.Lock()
				inflight++
				maxInflight = max(maxInflight, inflight)
				first := maxInflight == 1 && inflight == 1
				mu.Unlock()
				go func(id string, first bool) {
					if first {
						<-release
					}
					mu.Lock()
					inflight--
					mu.Unlock()
					b, _ := json.Marshal(wire.ImageUpdates{Images: map[string]bool{"nginx:1": false}})
					a.Handle(wire.AgentMessage{Result: &wire.CommandResult{CommandID: id, OK: true, Data: b}}, nil)
				}(m.Command.ID, first)
			}
		}
	}()
	defer reg.Detach(a)
	other := make(chan error, 1)
	go func() {
		_, err := a.Operate(context.Background(), wire.Operation{Kind: wire.OpImagesCheckUpdates})
		other <- err
	}()
	for !reg.ImageCheckRunning(autoHostA) {
		time.Sleep(time.Millisecond)
	}
	if _, err := x.s.startAutoRun("manual", "ann", x.clk.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	x.s.autoUp.wg.Wait()
	if err := <-other; err != nil {
		t.Fatal(err)
	}
	if maxInflight != 1 {
		t.Fatalf("%d checks at once on the host, want 1", maxInflight)
	}
	if o := outcome(t, x.s.latestAutoRun(), "web"); o.Result != autoSkipped || o.Reason != "already up to date" {
		t.Fatalf("%+v", o)
	}
}

func TestAutoTickRunsAtWindowStart(t *testing.T) {
	x := newAutoRig(t)
	ctx := context.Background()
	x.clk.Set(time.Date(2026, 11, 24, 2, 59, 0, 0, time.UTC))
	x.s.autoTick(ctx) // plans 03:00
	x.s.autoTick(ctx)
	if x.s.latestAutoRun() != nil {
		t.Fatal("ran before the window")
	}
	x.clk.Set(time.Date(2026, 11, 24, 3, 0, 10, 0, time.UTC))
	x.s.autoTick(ctx)
	x.s.autoUp.wg.Wait()
	run := x.s.latestAutoRun()
	if run == nil || run.Trigger != "window" || !run.Deadline.Equal(time.Date(2026, 11, 24, 5, 0, 0, 0, time.UTC)) {
		t.Fatalf("run: %+v", run)
	}
	x.s.autoTick(ctx)
	x.s.autoUp.wg.Wait()
	if x.s.latestAutoRun().ID != run.ID {
		t.Fatal("ran twice in one window")
	}
	if next := x.s.nextAutoWindow(defaultAutoSettings()); !next.Equal(time.Date(2026, 11, 25, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("next: %v", next)
	}

	// Paused: the window passes without a run.
	set := parseAutoSettings(x.db.settings[settingAutoUpdates])
	set.Enabled = false
	x.s.useAutoSettings(set)
	x.clk.Set(time.Date(2026, 11, 25, 3, 0, 5, 0, time.UTC))
	x.s.autoTick(ctx)
	x.s.autoUp.wg.Wait()
	if x.s.latestAutoRun().ID != run.ID {
		t.Fatal("ran while paused")
	}

	// A window long gone (the hub was stopped through it) isn't caught up.
	set.Enabled = true
	x.s.useAutoSettings(set)
	x.clk.Set(time.Date(2026, 11, 26, 6, 0, 0, 0, time.UTC))
	x.s.autoTick(ctx)
	x.s.autoUp.wg.Wait()
	if x.s.latestAutoRun().ID != run.ID {
		t.Fatal("caught up a missed window")
	}
}

// ---------------- API ----------------

func TestAutoUpdateAPIRolesAndFreshSignIn(t *testing.T) {
	x := newAutoRig(t)
	x.host(autoHostA, "alpha", wire.Container{ID: "c1", Name: "web", Image: "nginx:1", State: "running"})
	app := deployedStack("app", "services: {}\n")
	app.ID = stackX
	x.db.stacks[app.ID] = app
	put := `{"enabled":true}`
	x.user.Role = "operator"
	for _, c := range [][2]string{{"GET", "/api/auto-updates"}, {"PUT", "/api/hosts/" + autoHostA + "/containers/web/auto-update"}, {"POST", "/api/auto-updates/run"}, {"GET", "/api/auto-updates/runs/latest"}} {
		if w := x.do(c[0], c[1], put); w.Code != http.StatusForbidden {
			t.Errorf("operator %s %s: %d", c[0], c[1], w.Code)
		}
	}
	x.user.Role = "admin"
	x.fresh = false
	for _, c := range [][3]string{
		{"PUT", "/api/hosts/" + autoHostA + "/containers/web/auto-update", put},
		{"PUT", "/api/stacks/" + stackX + "/auto-update", put},
		{"PUT", "/api/auto-updates/settings", `{"enabled":true,"days":["mon"],"time":"03:00","maxMinutes":60}`},
		{"POST", "/api/auto-updates/run", `{}`},
	} {
		w := x.do(c[0], c[1], c[2])
		var body struct{ Reauth bool }
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if w.Code != http.StatusForbidden || !body.Reauth {
			t.Errorf("stale sign-in %s %s: %d %s", c[0], c[1], w.Code, w.Body)
		}
	}
	x.fresh = true
	x.token = &auth.TokenInfo{ID: "t1", Name: "ci"}
	if w := x.do("PUT", "/api/hosts/"+autoHostA+"/containers/web/auto-update", put); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "API token") {
		t.Errorf("token: %d %s", w.Code, w.Body)
	}
	if w := x.do("GET", "/api/auto-updates", ""); w.Code != http.StatusOK {
		t.Errorf("tokens can read: %d", w.Code)
	}
	if len(x.db.targets) != 0 {
		t.Fatalf("refused requests changed something: %+v", x.db.targets)
	}
}

func TestContainerAutoUpdateOptIn(t *testing.T) {
	x := newAutoRig(t)
	x.host(autoHostA, "alpha",
		wire.Container{ID: "c1", Name: "web", Image: "nginx:1", State: "running"},
		wire.Container{ID: "c2", Name: "app-web-1", Image: "app:1", Project: "app", State: "running"},
		wire.Container{ID: "c3", Name: "hub", Image: "tainerhub:latest", Project: "tainerhub", State: "running"},
		wire.Container{ID: "c4", Name: "pinned", Image: "redis@sha256:abc", State: "running"},
	)
	x.db.hosts[autoHostB] = store.Host{ID: autoHostB, Name: "beta"}
	path := func(host, name string) string { return "/api/hosts/" + host + "/containers/" + name + "/auto-update" }
	for _, c := range []struct {
		host, name, body string
		code             int
		msg              string
	}{
		{autoHostA, "app-web-1", `{"enabled":true}`, http.StatusConflict, "turn on automatic updates for the stack"},
		{autoHostA, "hub", `{"enabled":true}`, http.StatusConflict, "TainerHub's own"},
		{autoHostA, "tainerhub-agent", `{"enabled":true}`, http.StatusConflict, "TainerHub's own"},
		{autoHostA, "pinned", `{"enabled":true}`, http.StatusBadRequest, "pinned"},
		{autoHostA, "nope", `{"enabled":true}`, http.StatusNotFound, "no container"},
		{autoHostB, "web", `{"enabled":true}`, http.StatusServiceUnavailable, "offline"},
		{autoHostA, "web", `{}`, http.StatusBadRequest, "enabled"},
		{autoHostA, "bad%20name", `{"enabled":true}`, http.StatusBadRequest, "invalid"},
		{"0d000000-0000-4000-8000-00000000000d", "web", `{"enabled":true}`, http.StatusNotFound, ""},
	} {
		w := x.do("PUT", path(c.host, c.name), c.body)
		if w.Code != c.code || !strings.Contains(w.Body.String(), c.msg) {
			t.Errorf("%s: %d %s, want %d %q", c.name, w.Code, w.Body, c.code, c.msg)
		}
	}
	if len(x.db.targets) != 0 {
		t.Fatalf("refused opt-ins saved: %+v", x.db.targets)
	}
	if w := x.do("PUT", path(autoHostA, "web"), `{"enabled":true}`); w.Code != http.StatusOK {
		t.Fatalf("opt in: %d %s", w.Code, w.Body)
	}
	_ = x.do("PUT", path(autoHostA, "web"), `{"enabled":true}`) // again: no duplicate
	if len(x.db.targets) != 1 || x.db.targets[0].ContainerName != "web" || x.db.targets[0].EnabledBy != "ann" {
		t.Fatalf("targets: %+v", x.db.targets)
	}
	// Opting out works with the host offline.
	if w := x.do("PUT", path(autoHostB, "web"), `{"enabled":false}`); w.Code != http.StatusOK {
		t.Fatalf("opt out offline: %d %s", w.Code, w.Body)
	}
	if w := x.do("PUT", path(autoHostA, "web"), `{"enabled":false}`); w.Code != http.StatusOK || len(x.db.targets) != 0 {
		t.Fatalf("opt out: %d %+v", w.Code, x.db.targets)
	}
	x.mu.Lock()
	var got []string
	for _, e := range x.audit {
		got = append(got, e.Action+" "+e.Target)
	}
	x.mu.Unlock()
	if want := "update.auto_enabled alpha/web,update.auto_enabled alpha/web,update.auto_disabled beta/web,update.auto_disabled alpha/web"; strings.Join(got, ",") != want {
		t.Fatalf("audit: %v", got)
	}
}

func TestStackAutoUpdateOptIn(t *testing.T) {
	x := newAutoRig(t)
	x.host(autoHostA, "alpha", wire.Container{ID: "c1", Name: "web", Image: "nginx:1", Project: "app", State: "running"})
	app := deployedStack("app", "services:\n  web:\n    image: nginx:1\n")
	app.ID = stackX
	x.db.stacks[app.ID] = app
	own := deployedStack("tainerhub", "services: {}\n")
	own.ID = "5d7c8f0e-0000-4000-8000-0000000000bb"
	x.db.stacks[own.ID] = own
	if w := x.do("PUT", "/api/stacks/"+own.ID+"/auto-update", `{"enabled":true}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "TainerHub's own") {
		t.Fatalf("own: %d %s", w.Code, w.Body)
	}
	if w := x.do("PUT", "/api/stacks/5d7c8f0e-0000-4000-8000-0000000000cc/auto-update", `{"enabled":true}`); w.Code != http.StatusNotFound {
		t.Fatalf("missing: %d", w.Code)
	}
	if w := x.do("PUT", "/api/stacks/"+app.ID+"/auto-update", `{"enabled":true}`); w.Code != http.StatusOK {
		t.Fatalf("opt in: %d %s", w.Code, w.Body)
	}
	w := x.do("GET", "/api/auto-updates", "")
	var list struct {
		Targets []store.AutoUpdateTarget
		Window  autoSettingsView
	}
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Targets) != 1 || list.Targets[0].StackName != "app" || list.Targets[0].HostName != "alpha" || list.Window.Next == nil {
		t.Fatalf("list: %s", w.Body)
	}
}

func TestAutoSettingsAPI(t *testing.T) {
	x := newAutoRig(t)
	if w := x.do("PUT", "/api/auto-updates/settings", `{"enabled":true,"days":[],"time":"03:00","maxMinutes":60}`); w.Code != http.StatusBadRequest {
		t.Fatalf("no days: %d", w.Code)
	}
	w := x.do("PUT", "/api/auto-updates/settings", `{"enabled":false,"days":["sat","sun"],"time":"04:30","timeZone":"Europe/Berlin","maxMinutes":90}`)
	if w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	var v autoSettingsView
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	// From Tuesday 24 Nov, 02:00 UTC: Saturday 28 Nov, 04:30 in Berlin.
	if v.Next == nil || !v.Next.Equal(time.Date(2026, 11, 28, 3, 30, 0, 0, time.UTC)) || v.Zone != "Europe/Berlin" || !strings.HasPrefix(v.NextText, "Sat 28 Nov 04:30") {
		t.Fatalf("view: %+v", v)
	}
	if saved := parseAutoSettings(x.db.settings[settingAutoUpdates]); saved.Enabled || saved.MaxMinutes != 90 || saved.Time != "04:30" {
		t.Fatalf("saved: %+v", saved)
	}
	if w := x.do("GET", "/api/auto-updates/settings", ""); !strings.Contains(w.Body.String(), `"04:30"`) {
		t.Fatalf("get: %s", w.Body)
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(x.audit) != 1 || x.audit[0].Action != "update.auto_settings" {
		t.Fatalf("audit: %+v", x.audit)
	}
}

func TestRunNowAPI(t *testing.T) {
	x := newAutoRig(t)
	h := x.host(autoHostA, "alpha", wire.Container{ID: "c1", Name: "web", Image: "nginx:1", State: "running"})
	h.updates = map[string]bool{"nginx:1": true}
	h.hold = 50 * time.Millisecond
	x.optIn(autoHostA, "web")
	// Paused windows don't stop "Run now".
	set := parseAutoSettings(x.db.settings[settingAutoUpdates])
	set.Enabled = false
	x.s.useAutoSettings(set)

	w := x.do("POST", "/api/auto-updates/run", `{}`)
	var res struct{ RunID string }
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if w.Code != http.StatusAccepted || res.RunID == "" {
		t.Fatalf("run: %d %s", w.Code, w.Body)
	}
	if w := x.do("POST", "/api/auto-updates/run", `{}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), res.RunID) {
		t.Fatalf("second run: %d %s", w.Code, w.Body)
	}
	x.s.autoUp.wg.Wait()
	w = x.do("GET", "/api/auto-updates/runs/latest", "")
	var latest struct{ Run autoRun }
	_ = json.Unmarshal(w.Body.Bytes(), &latest)
	if latest.Run.ID != res.RunID || latest.Run.Running || latest.Run.Trigger != "manual" || latest.Run.StartedBy != "ann" ||
		len(latest.Run.Outcomes) != 1 || latest.Run.Outcomes[0].Result != autoOK {
		t.Fatalf("latest: %s", w.Body)
	}
}

func TestComposeImagesAndBuilds(t *testing.T) {
	c := "services:\n  a:\n    image: nginx:1.27 # web\n  b:\n    image: 'ghcr.io/x/y:2'\n  c:\n    image: ${REG}/z:1\n  d:\n    build:\n      context: .\n"
	if got := strings.Join(composeImages(c), ","); got != "nginx:1.27,ghcr.io/x/y:2" {
		t.Fatalf("images: %s", got)
	}
	if !composeBuilds(c) || composeBuilds("services:\n  a:\n    image: x\n") {
		t.Fatal("builds")
	}
}
