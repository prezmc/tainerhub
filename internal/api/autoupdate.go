package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prezmc/tainerhub/internal/registry"
	"github.com/prezmc/tainerhub/internal/store"
	"github.com/prezmc/tainerhub/internal/wire"
)

// Automatic updates (opt-in). An admin turns them on per managed stack or
// per standalone container; in the maintenance window the hub checks those
// hosts for newer images (with the saved registry logins), and updates what
// has one: a stack with the same job as "Pull & redeploy", a container with
// the same recreate as "Update" (which puts the old container back if the
// new one doesn't start). Then it waits for the containers to be running
// (and healthy, if they have a health check) and records the outcome.
//
// Safety rules: nothing is updated unless opted in; TainerHub's own
// containers and project never are; one target at a time per host (so a
// host never has two updates going on), at most autoHostParallel hosts at
// once; targets with a job already running, offline hosts and stacks with
// saved-but-undeployed changes are skipped, not forced.

const (
	autoActor     = "automatic updates" // audit log actor
	autoActorName = "Automatic updates" // shown on jobs and in the activity feed

	autoTick         = 30 * time.Second
	autoHostParallel = 4
	autoHealthWait   = 2 * time.Minute
	autoHealthPoll   = 5 * time.Second
	// autoCheckWait is how long to wait for an image check that is already
	// running on a host (scheduled or from the UI) before running ours.
	autoCheckWait = 5 * time.Minute

	autoOK      = "ok"
	autoFailed  = "failed"
	autoSkipped = "skipped"
	// autoRolledBack: the update failed and the old images were put back
	// (a stack's automatic rollback, or a container's recreate).
	autoRolledBack = "rolled_back"
)

// autoUpdateStore is the part of the store automatic updates (and stack
// jobs) use; a fake in tests.
type autoUpdateStore interface {
	Setting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
	ListAutoUpdateTargets(ctx context.Context) ([]store.AutoUpdateTarget, error)
	EnableStackAutoUpdate(ctx context.Context, stackID, by string) error
	EnableContainerAutoUpdate(ctx context.Context, hostID, name, by string) error
	DisableStackAutoUpdate(ctx context.Context, stackID string) (bool, error)
	DisableContainerAutoUpdate(ctx context.Context, hostID, name string) (bool, error)
	RecordAutoUpdate(ctx context.Context, id string, at time.Time, result, errText string) error
	GetStack(ctx context.Context, id string) (store.Stack, error)
	GetHost(ctx context.Context, id string) (store.Host, error)
	MarkStackDeployed(ctx context.Context, id, hash string) error
	DeleteStack(ctx context.Context, id string) error
	SetStackAutoRollback(ctx context.Context, stackID string, on bool) (bool, error)
	// Rollback points (see rollback.go).
	AddRollbackPoint(ctx context.Context, p store.RollbackPoint) (store.RollbackPoint, error)
	LatestRollbackPoint(ctx context.Context, stackID string) (store.RollbackPoint, error)
	SetStackRolledBack(ctx context.Context, id string, to *time.Time) error
	// Lists the host-scope checks filter (see scope.go).
	ListHosts(ctx context.Context) ([]store.Host, error)
	ListStacks(ctx context.Context) ([]store.Stack, error)
	GetMove(ctx context.Context, id string) (store.StackMove, error)
	ContainerMoves(ctx context.Context, limit int) ([]store.StackMove, error)
	ListAudit(ctx context.Context, limit int, viewer store.User) ([]store.AuditEntry, error)
}

func (s *Server) aus() autoUpdateStore {
	if s.autoDB != nil {
		return s.autoDB
	}
	return s.Store
}

// autoUpdater is the scheduler's state.
type autoUpdater struct {
	mu       sync.Mutex
	settings *autoSettings // cached; nil: load from the store
	next     time.Time     // the next window's start; zero: not worked out yet
	run      *autoRun      // the running or last run since the hub started
	base     context.Context
	wg       sync.WaitGroup

	// Test hooks: a fake clock and sleep, and shorter health waits.
	now        func() time.Time
	sleep      func(ctx context.Context, d time.Duration) error
	healthWait time.Duration
	healthPoll time.Duration
}

// autoRun is one pass over every opted-in target.
type autoRun struct {
	ID         string        `json:"id"`
	Trigger    string        `json:"trigger"` // "window" or "manual"
	StartedBy  string        `json:"startedBy,omitempty"`
	StartedAt  time.Time     `json:"startedAt"`
	Deadline   time.Time     `json:"deadline"` // targets not reached by then are left for the next window
	FinishedAt *time.Time    `json:"finishedAt"`
	Running    bool          `json:"running"`
	Error      string        `json:"error,omitempty"`
	Outcomes   []autoOutcome `json:"outcomes"`
}

type autoOutcome struct {
	TargetID string   `json:"targetId"`
	Kind     string   `json:"kind"`
	Name     string   `json:"name"`
	StackID  string   `json:"stackId,omitempty"`
	HostID   string   `json:"hostId"`
	Host     string   `json:"host"`
	Result   string   `json:"result"`           // ok, failed, skipped
	Reason   string   `json:"reason,omitempty"` // the error, or why it was skipped
	Images   []string `json:"images,omitempty"` // the image references that had a newer image
	OldImage string   `json:"oldImage,omitempty"`
	NewImage string   `json:"newImage,omitempty"`
	JobID    string   `json:"jobId,omitempty"`
	// RollbackJobID is the automatic rollback's job after a failed stack update.
	RollbackJobID string    `json:"rollbackJobId,omitempty"`
	At            time.Time `json:"at"`
}

func (s *Server) autoNow() time.Time {
	if s.autoUp.now != nil {
		return s.autoUp.now()
	}
	return time.Now()
}

func (s *Server) autoSleep(ctx context.Context, d time.Duration) error {
	if s.autoUp.sleep != nil {
		return s.autoUp.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// loadAutoSettings returns the window settings (cached after the first read).
func (s *Server) loadAutoSettings(ctx context.Context) (autoSettings, error) {
	s.autoUp.mu.Lock()
	if c := s.autoUp.settings; c != nil {
		s.autoUp.mu.Unlock()
		return *c, nil
	}
	s.autoUp.mu.Unlock()
	raw, err := s.aus().Setting(ctx, settingAutoUpdates)
	if err != nil {
		return autoSettings{}, err
	}
	set := parseAutoSettings(raw)
	s.autoUp.mu.Lock()
	s.autoUp.settings = &set
	s.autoUp.mu.Unlock()
	return set, nil
}

// useAutoSettings caches newly saved settings and plans the next window by them.
func (s *Server) useAutoSettings(set autoSettings) {
	s.autoUp.mu.Lock()
	defer s.autoUp.mu.Unlock()
	s.autoUp.settings = &set
	s.autoUp.next = nextWindow(s.autoNow(), set)
}

// nextAutoWindow is when the next window starts (zero if unknown).
func (s *Server) nextAutoWindow(set autoSettings) time.Time {
	s.autoUp.mu.Lock()
	defer s.autoUp.mu.Unlock()
	if s.autoUp.next.IsZero() {
		return nextWindow(s.autoNow(), set)
	}
	return s.autoUp.next
}

// RunAutoUpdates starts a run at the start of every window until ctx ends.
func (s *Server) RunAutoUpdates(ctx context.Context) {
	s.autoUp.mu.Lock()
	s.autoUp.base = ctx
	s.autoUp.mu.Unlock()
	t := time.NewTicker(autoTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.autoUp.wg.Wait()
			return
		case <-t.C:
			s.autoTick(ctx)
		}
	}
}

// autoTick starts a run if a window has begun. A window that began more
// than its length ago (the hub was down or asleep) is skipped, not caught up.
func (s *Server) autoTick(ctx context.Context) {
	set, err := s.loadAutoSettings(ctx)
	if err != nil {
		s.Log.Warn("automatic updates: load settings", "err", err)
		return
	}
	now := s.autoNow()
	s.autoUp.mu.Lock()
	if s.autoUp.next.IsZero() {
		s.autoUp.next = nextWindow(now, set)
		s.autoUp.mu.Unlock()
		return
	}
	start := s.autoUp.next
	if now.Before(start) {
		s.autoUp.mu.Unlock()
		return
	}
	s.autoUp.next = nextWindow(now, set)
	s.autoUp.mu.Unlock()
	end := start.Add(set.maxDuration())
	switch {
	case !set.Enabled:
		s.Log.Info("automatic updates are paused: skipping this window")
	case !now.Before(end):
		s.Log.Warn("automatic updates: missed a window (was the hub stopped?)", "window", start)
	default:
		if _, err := s.startAutoRun("window", "", end); err != nil {
			s.Log.Warn("automatic updates: window skipped", "err", err)
		}
	}
}

var errAutoBusy = errors.New("automatic updates are already running")

// startAutoRun starts a run in the background unless one is running.
func (s *Server) startAutoRun(trigger, by string, deadline time.Time) (*autoRun, error) {
	s.autoUp.mu.Lock()
	defer s.autoUp.mu.Unlock()
	if cur := s.autoUp.run; cur != nil && cur.Running {
		return cur, errAutoBusy
	}
	ctx := s.autoUp.base
	if ctx == nil {
		ctx = context.Background()
	}
	run := &autoRun{ID: randID(), Trigger: trigger, StartedBy: by, StartedAt: s.autoNow(), Deadline: deadline, Running: true, Outcomes: []autoOutcome{}}
	s.autoUp.run = run
	s.autoUp.wg.Add(1)
	go func() {
		defer s.autoUp.wg.Done()
		s.executeAutoRun(ctx, run)
	}()
	return run, nil
}

// latestAutoRun is a copy of the running or last run (nil: none since the
// hub started).
func (s *Server) latestAutoRun() *autoRun {
	s.autoUp.mu.Lock()
	defer s.autoUp.mu.Unlock()
	if s.autoUp.run == nil {
		return nil
	}
	cp := *s.autoUp.run
	cp.Outcomes = append([]autoOutcome{}, s.autoUp.run.Outcomes...)
	return &cp
}

func (s *Server) executeAutoRun(ctx context.Context, run *autoRun) {
	defer func() {
		s.autoUp.mu.Lock()
		now := s.autoNow()
		run.Running, run.FinishedAt = false, &now
		s.autoUp.mu.Unlock()
	}()
	targets, err := s.aus().ListAutoUpdateTargets(ctx)
	if err != nil {
		s.autoUp.mu.Lock()
		run.Error = err.Error()
		s.autoUp.mu.Unlock()
		s.Log.Error("automatic updates: list targets", "err", err)
		return
	}
	byHost := map[string][]store.AutoUpdateTarget{}
	var hosts []string
	for _, t := range targets {
		if _, ok := byHost[t.HostID]; !ok {
			hosts = append(hosts, t.HostID)
		}
		byHost[t.HostID] = append(byHost[t.HostID], t)
	}
	sort.Strings(hosts)
	// Hosts in parallel (at most autoHostParallel), targets on a host one
	// after the other.
	sem := make(chan struct{}, autoHostParallel)
	var wg sync.WaitGroup
	for _, h := range hosts {
		wg.Add(1)
		go func(ts []store.AutoUpdateTarget) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s.autoHost(ctx, run, ts)
		}(byHost[h])
	}
	wg.Wait()
}

// autoHost checks one host for newer images and updates its targets in turn.
func (s *Server) autoHost(ctx context.Context, run *autoRun, ts []store.AutoUpdateTarget) {
	hostID := ts[0].HostID
	a := s.Reg.Get(hostID)
	if a == nil {
		for _, t := range ts {
			s.autoRecord(ctx, run, t, autoOutcome{Result: autoSkipped, Reason: "host offline"})
		}
		return
	}
	// A check already running (scheduled or from the UI) is waited for, so
	// the two don't overlap; ours then sees the newest registry answers.
	for waited := time.Duration(0); s.Reg.ImageCheckRunning(hostID) && waited < autoCheckWait; waited += time.Second {
		if s.autoSleep(ctx, time.Second) != nil {
			return
		}
	}
	raw, err := a.Operate(ctx, wire.Operation{Kind: wire.OpImagesCheckUpdates})
	var upd wire.ImageUpdates
	if err == nil {
		err = json.Unmarshal(raw, &upd)
	}
	if err != nil {
		res, why := autoFailed, "the update check failed: "+err.Error()
		if errors.Is(err, registry.ErrOffline) {
			res, why = autoSkipped, "host offline"
		}
		for _, t := range ts {
			s.autoRecord(ctx, run, t, autoOutcome{Result: res, Reason: why})
		}
		return
	}
	for _, t := range ts {
		if !s.autoNow().Before(run.Deadline) {
			s.autoRecord(ctx, run, t, autoOutcome{Result: autoSkipped, Reason: "the window ended first; left for the next window"})
			continue
		}
		s.autoRecord(ctx, run, t, s.autoTarget(ctx, a, t, upd))
	}
}

// autoRecord saves a target's outcome: on the run, on the target's row,
// and (for updates tried) in the audit log and the activity feed.
func (s *Server) autoRecord(ctx context.Context, run *autoRun, t store.AutoUpdateTarget, o autoOutcome) {
	o.TargetID, o.Kind, o.Name, o.StackID, o.HostID, o.Host = t.ID, t.Kind, t.Name(), t.StackID, t.HostID, t.HostName
	o.At = s.autoNow()
	s.autoUp.mu.Lock()
	run.Outcomes = append(run.Outcomes, o)
	s.autoUp.mu.Unlock()
	if err := s.aus().RecordAutoUpdate(context.WithoutCancel(ctx), t.ID, o.At, o.Result, o.Reason); err != nil {
		s.Log.Warn("automatic updates: save the outcome", "target", o.Name, "err", err)
	}
	if o.Result == autoSkipped {
		return
	}
	action := "update.auto_applied"
	switch o.Result {
	case autoFailed:
		action = "update.auto_failed"
	case autoRolledBack:
		action = "update.auto_rolled_back"
	}
	detail := map[string]any{"kind": t.Kind, "host": t.HostName, "run": run.ID}
	if len(o.Images) > 0 {
		detail["images"] = o.Images
	}
	if o.OldImage != "" || o.NewImage != "" {
		detail["from"], detail["to"] = o.OldImage, o.NewImage
	}
	if o.Reason != "" {
		detail["error"] = o.Reason
	}
	e := store.AuditEntry{Actor: autoActor, Action: action, Target: t.HostName + "/" + o.Name, HostIDs: []string{t.HostID}}
	e.Detail, _ = json.Marshal(detail)
	s.writeAudit(ctx, e)
	s.Reg.AddActivity(registry.Activity{Kind: "user", Action: action, HostID: t.HostID, Host: t.HostName, Container: o.Name, Actor: autoActorName})
}

// autoTarget decides whether a target has something newer and, if so,
// updates it and waits for it to come up.
func (s *Server) autoTarget(ctx context.Context, a *registry.Agent, t store.AutoUpdateTarget, upd wire.ImageUpdates) autoOutcome {
	st := a.Status()
	if t.Kind == "container" {
		// A container being moved to another host runs as a job too.
		if _, moving := s.jobs.running()["container:"+t.HostID+"/"+t.ContainerName]; moving {
			return autoOutcome{Result: autoSkipped, Reason: "a job is running for this container"}
		}
		c, refs, skip := planContainer(t.ContainerName, st, upd)
		if skip != "" {
			return autoOutcome{Result: autoSkipped, Reason: skip}
		}
		o := autoOutcome{Images: refs}
		raw, err := a.Operate(ctx, wire.Operation{Kind: wire.OpContainerRecreate, Target: c.ID})
		var res wire.RecreateResult
		_ = json.Unmarshal(raw, &res)
		o.OldImage, o.NewImage = res.OldImage, res.NewImage
		if err != nil {
			o.Result, o.Reason = autoFailed, err.Error()
			if strings.Contains(o.Reason, "the old container was put back") {
				o.Result = autoRolledBack // recreate rolls back on its own
			}
			return o
		}
		if res.Status == "current" {
			o.Result, o.Reason = autoSkipped, "already up to date (the pull found no newer image)"
			return o
		}
		o.Result = autoOK
		if c.State != "running" {
			return o // a stopped container is recreated stopped: nothing to wait for
		}
		oldID := c.ID
		err = s.waitHealthy(ctx, a, func(x wire.Container) bool { return x.Name == c.Name && x.ID != oldID })
		if err != nil {
			o.Result, o.Reason = autoFailed, err.Error()
		}
		return o
	}

	stack, err := s.aus().GetStack(ctx, t.StackID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return autoOutcome{Result: autoSkipped, Reason: "the stack no longer exists"}
		}
		return autoOutcome{Result: autoFailed, Reason: err.Error()}
	}
	if stack.HostID != a.HostID {
		return autoOutcome{Result: autoSkipped, Reason: "the stack moved to another host; left for the next window"}
	}
	_, busy := s.jobs.running()[stack.ID]
	refs, skip := planStack(stack, st, upd, busy)
	if skip != "" {
		return autoOutcome{Result: autoSkipped, Reason: skip}
	}
	o := autoOutcome{Images: refs}
	j, err := s.startStackJobAs(stack, wire.StackDeploy, true, false, jobActor{Name: autoActorName, Audit: autoActor, Auto: true})
	var be *busyError
	switch {
	case errors.As(err, &be):
		o.Result, o.Reason = autoSkipped, "a job is running for this stack"
		return o
	case errors.Is(err, registry.ErrOffline):
		o.Result, o.Reason = autoSkipped, "host offline"
		return o
	case err != nil:
		o.Result, o.Reason = autoFailed, err.Error()
		return o
	}
	o.JobID = j.ID
	jobErr, err := j.wait(ctx)
	if err == nil && jobErr != "" {
		err = errors.New(jobErr)
	}
	if err == nil {
		err = s.waitHealthy(ctx, a, func(x wire.Container) bool { return x.Project == stack.Name })
	}
	o.Result = autoOK
	if err != nil {
		o.Result, o.Reason = autoFailed, err.Error()
		if t.AutoRollback {
			s.autoRollback(ctx, a, stack, j.point, &o)
		}
	}
	return o
}

// autoRollback puts a stack whose automatic update failed back on the
// images it ran before (the rollback point the update's job recorded),
// and says how that went in o.
func (s *Server) autoRollback(ctx context.Context, a *registry.Agent, stack store.Stack, point *store.RollbackPoint, o *autoOutcome) {
	if point == nil {
		o.Reason += "; there was no rollback point to go back to"
		return
	}
	if ctx.Err() != nil {
		// The hub is stopping: the rollback must still happen.
		ctx = context.WithoutCancel(ctx)
	}
	j, err := s.launchStackJob(stack, wire.StackRollback, stackJobOpts{rollback: point}, jobActor{Name: autoActorName, Audit: autoActor, Auto: true})
	if err == nil {
		o.RollbackJobID = j.ID
		var jobErr string
		jobErr, err = j.wait(ctx)
		if err == nil && jobErr != "" {
			err = errors.New(jobErr)
		}
	}
	if err != nil {
		o.Reason += "; rollback failed: " + err.Error()
		return
	}
	o.Result, o.Reason = autoRolledBack, "the update failed ("+o.Reason+"), so the previous images were put back"
	if err := s.waitHealthy(ctx, a, func(x wire.Container) bool { return x.Project == stack.Name }); err != nil {
		o.Reason += "; after the rollback: " + err.Error()
	}
}

// ownContainer: TainerHub's own containers (the hub, its database, agents)
// are updated from Hosts → Updates, never automatically, and can't be
// moved. They're found by name (as in the install files), by image (a
// renamed hub or agent), or by being in the compose project the host's
// agent runs in. The web UI's own.ts makes the same check.
func ownContainer(c wire.Container, ownProject string) bool {
	img := ownImageName(c.Image)
	return strings.HasPrefix(c.Name, "tainerhub") || img == "tainerhub" || img == "tainerhub-agent" ||
		(ownProject != "" && strings.EqualFold(c.Project, ownProject))
}

// ownImageName is an image reference's repository name, without registry,
// owner, tag or digest: "ghcr.io/prezmc/tainerhub-agent:1.2" → "tainerhub-agent".
func ownImageName(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	slash := strings.LastIndex(ref, "/")
	if colon := strings.LastIndex(ref, ":"); colon > slash {
		ref = ref[:colon]
	}
	return ref[slash+1:]
}

func ownProjectOf(st registry.Status) string {
	if st.Hello != nil {
		return st.Hello.OwnProject
	}
	return ""
}

// containerNewer says whether container c runs an older image than the
// registry's: the fresh check's answer for that container (it may be behind
// even when the tag on disk is current), else the answer for its image.
func containerNewer(c wire.Container, upd wire.ImageUpdates) bool {
	if v, ok := upd.Containers[c.ID]; ok {
		return v
	}
	return newer(c.Image, upd, c.UpdateAvailable)
}

// newer says whether ref has a newer image: the fresh check's answer, or
// (when the check couldn't tell, e.g. the registry didn't answer) the
// host's last known answer.
func newer(ref string, upd wire.ImageUpdates, known bool) bool {
	if v, ok := upd.Images[ref]; ok {
		return v
	}
	return known
}

// updateableRef mirrors the agent: images pinned by digest or ID never change.
func updateableRef(ref string) bool {
	return ref != "" && !strings.Contains(ref, "@") && !strings.HasPrefix(ref, "sha256:")
}

// planContainer finds a standalone container by name and says whether it
// has a newer image (refs) or why it's skipped.
func planContainer(name string, st registry.Status, upd wire.ImageUpdates) (*wire.Container, []string, string) {
	own := ownProjectOf(st)
	var c *wire.Container
	for i := range st.Containers {
		if st.Containers[i].Name == name {
			c = &st.Containers[i]
		}
	}
	switch {
	case strings.HasPrefix(name, "tainerhub") || (c != nil && ownContainer(*c, own)):
		return nil, nil, "TainerHub's own containers are updated from Hosts → Updates"
	case c == nil:
		return nil, nil, "no container with this name on the host"
	case c.Project != "":
		return nil, nil, "it now belongs to the compose project " + c.Project + ", which updates as a stack"
	case !updateableRef(c.Image):
		return nil, nil, "it uses a pinned image (by digest or ID)"
	case !containerNewer(*c, upd):
		return nil, nil, "already up to date"
	}
	if users := wire.NetworkUsers(*c, st.Containers); len(users) > 0 {
		return nil, nil, strings.Join(users, ", ") + " " + plural(len(users), "uses", "use") + " its network; put them in one compose stack to update them together"
	}
	return c, []string{c.Image}, ""
}

// planStack says which of a stack's images have a newer image (refs), or
// why it's skipped. The images are those its containers run plus those its
// compose file names. Services that build their image can't be checked
// (a built image has no registry digest), so a stack whose only possible
// updates would be in built images is skipped as "builds".
func planStack(stack store.Stack, st registry.Status, upd wire.ImageUpdates, jobRunning bool) ([]string, string) {
	own := ownProjectOf(st)
	if own != "" && strings.EqualFold(own, stack.Name) {
		return nil, "TainerHub's own project is updated from Hosts → Updates"
	}
	var cs []wire.Container
	for _, c := range st.Containers {
		if c.Project == stack.Name {
			if ownContainer(c, own) {
				return nil, "TainerHub's own project is updated from Hosts → Updates"
			}
			cs = append(cs, c)
		}
	}
	switch {
	case jobRunning:
		return nil, "a job is running for this stack"
	case stack.DeployedAt == nil:
		return nil, "never deployed by TainerHub: deploy it once yourself"
	case stack.DeployedHash != stackHash(stack):
		// Pull & redeploy would also deploy these unreviewed edits.
		return nil, "it has saved changes that haven't been deployed: deploy them yourself first"
	case stack.RolledBackTo != nil:
		// Someone (or an earlier failed update) went back to older images
		// on purpose; Pull & redeploy would undo that.
		return nil, "it runs a rollback: use Return to latest on its page first"
	}
	// An image counts when any of the stack's containers runs an older one
	// than the registry's; images only the compose file names (services not
	// running) go by the image's own answer.
	seen := map[string]bool{}
	var refs []string
	for _, c := range cs {
		if updateableRef(c.Image) && !seen[c.Image] && containerNewer(c, upd) {
			seen[c.Image] = true
			refs = append(refs, c.Image)
		}
	}
	running := map[string]bool{}
	for _, c := range cs {
		running[c.Image] = true
	}
	for _, ref := range composeImages(stack.Compose) {
		if updateableRef(ref) && !seen[ref] && !running[ref] && newer(ref, upd, false) {
			seen[ref] = true
			refs = append(refs, ref)
		}
	}
	sort.Strings(refs)
	if len(refs) == 0 {
		if composeBuilds(stack.Compose) {
			return nil, "builds: it builds its images, and whether their base images changed can't be checked"
		}
		return nil, "already up to date"
	}
	return refs, ""
}

var (
	composeImageLine = regexp.MustCompile(`(?m)^[ \t]+image:[ \t]*["']?([^"'\s#]+)`)
	composeBuildLine = regexp.MustCompile(`(?m)^[ \t]+build:`)
)

// composeImages lists the image: references in a compose file, skipping
// ones with variables (their value depends on .env and the host). Best
// effort, without a YAML parser: the hub has none, and the containers'
// own images (from the host) are the main source anyway.
func composeImages(compose string) []string {
	var out []string
	for _, m := range composeImageLine.FindAllStringSubmatch(compose, -1) {
		if !strings.Contains(m[1], "$") {
			out = append(out, m[1])
		}
	}
	return out
}

func composeBuilds(compose string) bool { return composeBuildLine.MatchString(compose) }

// waitHealthy waits for the containers match picks to be running (and
// healthy, when they have a health check), up to autoHealthWait.
func (s *Server) waitHealthy(ctx context.Context, a *registry.Agent, match func(wire.Container) bool) error {
	limit, poll := s.autoUp.healthWait, s.autoUp.healthPoll
	if limit == 0 {
		limit = autoHealthWait
	}
	if poll == 0 {
		poll = autoHealthPoll
	}
	end := s.autoNow().Add(limit)
	for {
		// Wait first: the host's container list lags the change a little.
		if err := s.autoSleep(ctx, poll); err != nil {
			return err
		}
		select {
		case <-a.Done():
			return errors.New("the host went offline while waiting for the containers to start")
		default:
		}
		var cs []wire.Container
		for _, c := range a.Status().Containers {
			if match(c) {
				cs = append(cs, c)
			}
		}
		problem := healthProblem(cs)
		if problem == "" {
			return nil
		}
		if !s.autoNow().Before(end) {
			return fmt.Errorf("not running and healthy %s after the update: %s", limit.Round(time.Second), problem)
		}
	}
}

// healthProblem says what isn't up yet ("" when everything is).
func healthProblem(cs []wire.Container) string {
	if len(cs) == 0 {
		return "no containers found"
	}
	var bad []string
	for _, c := range cs {
		switch {
		case c.State == "exited" && strings.HasPrefix(c.Status, "Exited (0)"):
			// A one-off service (migrations, an init step) that finished fine.
		case c.State != "running":
			bad = append(bad, c.Name+" is "+c.State)
		case strings.Contains(c.Status, "(health: starting)"):
			bad = append(bad, c.Name+" is still starting (health check)")
		case strings.Contains(c.Status, "(unhealthy)"):
			bad = append(bad, c.Name+" is unhealthy")
		}
	}
	sort.Strings(bad)
	return strings.Join(bad, ", ")
}
