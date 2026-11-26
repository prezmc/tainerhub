package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/prezmc/tainerhub/internal/registry"
	"github.com/prezmc/tainerhub/internal/store"
	"github.com/prezmc/tainerhub/internal/wire"
)

// Who can do what with stacks:
//   - viewers see every stack's status and containers;
//   - operators run lifecycle actions (deploy, pull, start, stop, restart,
//     down) on definitions an admin has already saved;
//   - admins see and edit compose files and .env, and create or delete
//     stacks. A compose file can mount the host's filesystem or run
//     privileged containers, so writing one is root on that host.

type stackSummary struct {
	ID         string     `json:"id,omitempty"` // empty for stacks TainerHub doesn't manage
	Name       string     `json:"name"`
	HostID     string     `json:"hostId"`
	HostName   string     `json:"hostName"`
	Managed    bool       `json:"managed"`
	TainerHub  bool       `json:"tainerhub,omitempty"` // TainerHub's own project: can't be taken over
	Online     bool       `json:"online"`
	Status     string     `json:"status"` // running, partial, stopped, down, not deployed, unknown
	Containers int        `json:"containers"`
	Running    int        `json:"running"`
	Unhealthy  int        `json:"unhealthy"`
	UpdatedAt  *time.Time `json:"updatedAt,omitempty"`
	UpdatedBy  string     `json:"updatedBy,omitempty"`
	DeployedAt *time.Time `json:"deployedAt,omitempty"`
	Pending    bool       `json:"pending"`              // saved changes that haven't been deployed
	AutoUpdate bool       `json:"autoUpdate,omitempty"` // opted in to automatic updates
	Job        *jobRef    `json:"job,omitempty"`
}

func summarize(sum *stackSummary, cs []wire.Container) {
	for _, c := range cs {
		if c.Project != sum.Name {
			continue
		}
		sum.Containers++
		if c.State == "running" {
			sum.Running++
		}
		if strings.Contains(c.Status, "(unhealthy)") || c.State == "restarting" {
			sum.Unhealthy++
		}
	}
	switch {
	case !sum.Online:
		sum.Status = "unknown"
	case sum.Containers == 0 && sum.Managed && sum.DeployedAt == nil:
		sum.Status = "not deployed"
	case sum.Containers == 0:
		sum.Status = "down"
	case sum.Running == sum.Containers:
		sum.Status = "running"
	case sum.Running == 0:
		sum.Status = "stopped"
	default:
		sum.Status = "partial"
	}
}

// stackSummaries lists managed stacks plus compose projects found running on
// hosts that TainerHub doesn't manage (started with `docker compose` by hand).
func (s *Server) stackSummaries(ctx context.Context) ([]stackSummary, error) {
	hosts, err := s.aus().ListHosts(ctx)
	if err != nil {
		return nil, err
	}
	hostName := map[string]string{}
	for _, h := range hosts {
		hostName[h.ID] = h.Name
	}
	stacks, err := s.aus().ListStacks(ctx)
	if err != nil {
		return nil, err
	}
	live := map[string][]wire.Container{}
	own := map[string]string{} // host ID -> the agent's own compose project
	for _, a := range s.Reg.All() {
		st := a.Status()
		live[a.HostID] = st.Containers
		if st.Hello != nil {
			own[a.HostID] = st.Hello.OwnProject
		}
	}
	jobs := s.jobs.running()
	auto := map[string]bool{}
	if ts, err := s.aus().ListAutoUpdateTargets(ctx); err != nil {
		s.Log.Warn("list automatic update targets", "err", err)
	} else {
		for _, t := range ts {
			if t.Kind == "stack" {
				auto[t.StackID] = true
			}
		}
	}

	out := []stackSummary{}
	managed := map[string]bool{}
	for _, st := range stacks {
		managed[st.HostID+"/"+st.Name] = true
		cs, online := live[st.HostID]
		updated := st.UpdatedAt
		sum := stackSummary{
			ID: st.ID, Name: st.Name, HostID: st.HostID, HostName: hostName[st.HostID], Managed: true, Online: online,
			UpdatedAt: &updated, UpdatedBy: st.UpdatedBy, DeployedAt: st.DeployedAt,
			Pending: st.DeployedHash != stackHash(st), AutoUpdate: auto[st.ID],
		}
		if j, ok := jobs[st.ID]; ok {
			sum.Job = &j
		}
		summarize(&sum, cs)
		out = append(out, sum)
	}
	for hostID, cs := range live {
		seen := map[string]bool{}
		for _, c := range cs {
			if c.Project == "" || seen[c.Project] || managed[hostID+"/"+c.Project] {
				continue
			}
			seen[c.Project] = true
			sum := stackSummary{Name: c.Project, HostID: hostID, HostName: hostName[hostID], Online: true, TainerHub: c.Project == own[hostID]}
			summarize(&sum, cs)
			out = append(out, sum)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].HostName < out[j].HostName
	})
	return out, nil
}

func (s *Server) listStacks(w http.ResponseWriter, r *http.Request) {
	out, err := s.stackSummaries(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, visible(userFrom(r), out, func(x stackSummary) string { return x.HostID }))
}

// loadStack fetches the stack named in the path and writes 404 if it's missing.
func (s *Server) loadStack(w http.ResponseWriter, r *http.Request) (store.Stack, bool) {
	id := r.PathValue("sid")
	if !validUUID(id) {
		jsonErr(w, http.StatusNotFound, "stack not found")
		return store.Stack{}, false
	}
	st, err := s.aus().GetStack(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			jsonErr(w, http.StatusNotFound, "stack not found")
		} else {
			s.fail(w, err)
		}
		return store.Stack{}, false
	}
	return st, true
}

func (s *Server) getStack(w http.ResponseWriter, r *http.Request) {
	st, ok := s.loadStack(w, r)
	if !ok {
		return
	}
	all, err := s.stackSummaries(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	var sum stackSummary
	for _, x := range all {
		if x.ID == st.ID {
			sum = x
		}
	}
	// Compose files, .env and the other files often hold passwords, like
	// container env values.
	canView := roleRank[userFrom(r).Role] >= roleRank["admin"]
	if !canView {
		hideStackFiles(&st)
	}
	moves, err := s.Store.StackMoves(r.Context(), st.ID, 5)
	if err != nil {
		s.fail(w, err)
		return
	}
	// A move from or to a host the caller can't see stays hidden, like that host.
	moves = visible(userFrom(r), nonNil(moves), func(m store.StackMove) string { return m.FromHost })
	moves = visible(userFrom(r), moves, func(m store.StackMove) string { return m.ToHost })
	// The newest rollback point, so the page knows whether Roll back can run.
	var point any
	if p, err := s.aus().LatestRollbackPoint(r.Context(), st.ID); err == nil {
		point = map[string]any{"id": p.ID, "createdAt": p.CreatedAt, "trigger": p.Trigger, "createdBy": p.CreatedBy, "services": len(p.Services)}
	} else if !errors.Is(err, store.ErrNotFound) {
		s.Log.Warn("latest rollback point", "stack", st.Name, "err", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"stack": st, "summary": sum, "filesVisible": canView, "moves": moves, "rollbackPoint": point})
}

type stackInput struct {
	HostID    string    `json:"hostId"`
	Name      string    `json:"name"`
	Compose   string    `json:"compose"`
	Env       string    `json:"env"`
	UpdatedAt time.Time `json:"updatedAt"` // edits: the version the editor loaded
	Deploy    bool      `json:"deploy"`    // deploy right after saving
	Adopt     bool      `json:"adopt"`     // take over a compose project already running on the host
	Ack       string    `json:"ack"`       // confirms the takeover check's findings (first deploy of a taken-over project)
	// Files are the stack's other files. Left out (null) on an update, the
	// saved ones stay as they are.
	Files *[]store.StackFile `json:"files"`
}

func (in *stackInput) validate() error {
	in.Name = strings.TrimSpace(in.Name)
	if !wire.ValidStackName(in.Name) {
		return errors.New("stack names use lowercase letters, digits, '-' and '_', start with a letter or digit, and are at most 63 characters")
	}
	if strings.TrimSpace(in.Compose) == "" {
		return errors.New("the compose file is empty")
	}
	if len(in.Compose) > wire.MaxComposeBytes {
		return errors.New("the compose file is too large (max 256 KB)")
	}
	if len(in.Env) > wire.MaxEnvBytes {
		return errors.New(".env is too large (max 64 KB)")
	}
	if in.Files != nil {
		return wire.ValidStackFiles(wireFiles(*in.Files))
	}
	return nil
}

// files is the input's files, or keep when it has none.
func (in *stackInput) files(keep []store.StackFile) []store.StackFile {
	if in.Files == nil {
		return keep
	}
	return *in.Files
}

func (s *Server) createStack(w http.ResponseWriter, r *http.Request) {
	var in stackInput
	if !readJSONLimit(w, r, &in, maxStackBody) {
		return
	}
	if err := in.validate(); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !validUUID(in.HostID) {
		jsonErr(w, http.StatusBadRequest, "choose a host")
		return
	}
	if !s.allowHosts(w, r, in.HostID) {
		return
	}
	h, err := s.Store.GetHost(r.Context(), in.HostID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			jsonErr(w, http.StatusBadRequest, "that host no longer exists")
		} else {
			s.fail(w, err)
		}
		return
	}
	// Deploying runs `compose up --remove-orphans` under this name, so a
	// stack named after a project already running on the host would take
	// over (or remove) its containers. That has to be a deliberate take-over.
	if s.Reg != nil {
		if a := s.Reg.Get(h.ID); a != nil {
			if hello := a.Status().Hello; hello != nil && hello.OwnProject != "" && strings.EqualFold(hello.OwnProject, in.Name) {
				jsonErr(w, http.StatusConflict, in.Name+" is TainerHub's own compose project on "+h.Name+"; choose another name")
				return
			}
		}
	}
	if !in.Adopt && s.Reg != nil {
		if a := s.Reg.Get(h.ID); a != nil {
			stacks, err := s.Store.ListStacks(r.Context())
			if err != nil {
				s.fail(w, err)
				return
			}
			if unmanagedProject(a.Status().Containers, stacks, h.ID, in.Name) {
				jsonErr(w, http.StatusConflict, "a compose project called "+in.Name+" is already running on "+h.Name+", started outside TainerHub; choose another name, or use Manage in TainerHub next to it on the Stacks page to take it over")
				return
			}
		}
	}
	if in.Deploy && s.isTakeover(h.ID, in.Name, false) && !s.requireTakeoverAck(w, r, h.ID, in.Name, in.Compose, in.Env, in.Ack) {
		return
	}
	u := userFrom(r)
	st, err := s.Store.CreateStack(r.Context(), store.Stack{HostID: h.ID, Name: in.Name, Compose: in.Compose, Env: in.Env, Files: in.files(nil), UpdatedBy: u.Username})
	if errors.Is(err, store.ErrConflict) {
		jsonErr(w, http.StatusConflict, "this host already has a stack called "+in.Name)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	details := map[string]any{"host": h.Name}
	if in.Adopt {
		details["adopted"] = "yes"
	}
	if len(st.Files) > 0 {
		details["files"] = filePaths(st.Files)
	}
	s.record(r, &u, "stack.created", h.Name+"/"+st.Name, details)
	res := map[string]any{"stack": st}
	if in.Deploy {
		if j, err := s.startStackJob(r, st, wire.StackDeploy, false, false); err != nil {
			res["deployError"] = err.Error()
		} else {
			res["jobId"] = j.ID
		}
	}
	writeJSON(w, http.StatusCreated, res)
}

// unmanagedProject reports whether compose project name has containers on
// the host (cs is its live list) but isn't one of TainerHub's stacks there.
func unmanagedProject(cs []wire.Container, stacks []store.Stack, hostID, name string) bool {
	if name == "" {
		return false
	}
	for _, st := range stacks {
		if st.HostID == hostID && st.Name == name {
			return false // a managed stack: CreateStack reports the duplicate
		}
	}
	for _, c := range cs {
		if c.Project == name {
			return true
		}
	}
	return false
}

func (s *Server) updateStack(w http.ResponseWriter, r *http.Request) {
	st, ok := s.loadStack(w, r)
	if !ok {
		return
	}
	var in stackInput
	if !readJSONLimit(w, r, &in, maxStackBody) {
		return
	}
	in.Name = st.Name // names are fixed: renaming would orphan the running project
	if err := in.validate(); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Deploy && s.isTakeover(st.HostID, st.Name, st.DeployedAt != nil) && !s.requireTakeoverAck(w, r, st.HostID, st.Name, in.Compose, in.Env, in.Ack) {
		return
	}
	u := userFrom(r)
	before := st.Files
	st, err := s.Store.UpdateStack(r.Context(), st.ID, in.Compose, in.Env, in.files(st.Files), u.Username, in.UpdatedAt)
	if errors.Is(err, store.ErrStale) {
		jsonErr(w, http.StatusConflict, "someone else saved this stack after you opened it; reload to see their changes")
		return
	}
	if err != nil {
		s.notFoundOr(w, err)
		return
	}
	var details map[string]any
	if changed := changedFiles(before, st.Files); len(changed) > 0 {
		details = map[string]any{"files": changed}
	}
	s.record(r, &u, "stack.updated", s.hostName(r.Context(), st.HostID)+"/"+st.Name, details)
	res := map[string]any{"stack": st}
	if in.Deploy {
		if j, err := s.startStackJob(r, st, wire.StackDeploy, false, false); err != nil {
			res["deployError"] = err.Error()
		} else {
			res["jobId"] = j.ID
		}
	}
	writeJSON(w, http.StatusOK, res)
}

// stackAction runs a lifecycle action. "pull" is a deploy that pulls newer
// images first.
func (s *Server) stackAction(w http.ResponseWriter, r *http.Request) {
	st, ok := s.loadStack(w, r)
	if !ok {
		return
	}
	action, pull := r.PathValue("action"), false
	switch action {
	case "pull":
		action, pull = wire.StackDeploy, true
	case wire.StackDeploy, wire.StackStart, wire.StackStop, wire.StackRestart, wire.StackDown:
	default:
		jsonErr(w, http.StatusNotFound, "unknown stack action")
		return
	}
	// The first deploy of a project taken over from outside TainerHub needs
	// the takeover check confirmed (body {"ack": ...}).
	if action == wire.StackDeploy && s.isTakeover(st.HostID, st.Name, st.DeployedAt != nil) {
		var in struct{ Ack string }
		if r.ContentLength != 0 {
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in)
		}
		if roleRank[userFrom(r).Role] < roleRank["admin"] {
			jsonErr(w, http.StatusForbidden, "this is the first deploy of a project taken over from outside TainerHub: an admin has to check and deploy it")
			return
		}
		if !s.requireTakeoverAck(w, r, st.HostID, st.Name, st.Compose, st.Env, in.Ack) {
			return
		}
	}
	s.respondJob(w, r, st, action, pull, false)
}

// deleteStack runs `compose down` on the host, removes the stack's files and
// then forgets it. ?volumes=1 also deletes its named volumes. ?forget=1 only
// removes it from TainerHub and leaves the host untouched (for hosts that are
// gone or offline).
func (s *Server) deleteStack(w http.ResponseWriter, r *http.Request) {
	st, ok := s.loadStack(w, r)
	if !ok {
		return
	}
	if r.URL.Query().Get("forget") == "1" {
		if err := s.Store.DeleteStack(r.Context(), st.ID); err != nil {
			s.notFoundOr(w, err)
			return
		}
		u := userFrom(r)
		s.record(r, &u, "stack.forgotten", s.hostName(r.Context(), st.HostID)+"/"+st.Name, nil)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.respondJob(w, r, st, wire.StackRemove, false, r.URL.Query().Get("volumes") == "1")
}

func (s *Server) respondJob(w http.ResponseWriter, r *http.Request, st store.Stack, action string, pull, volumes bool) {
	j, err := s.startStackJob(r, st, action, pull, volumes)
	var busy *busyError
	switch {
	case errors.As(err, &busy):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "jobId": busy.jobID})
	case errors.Is(err, registry.ErrOffline):
		jsonErr(w, http.StatusServiceUnavailable, "host is offline")
	case err != nil:
		jsonErr(w, http.StatusBadGateway, err.Error())
	default:
		writeJSON(w, http.StatusAccepted, map[string]string{"jobId": j.ID})
	}
}

type busyError struct{ jobID, action string }

func (e *busyError) Error() string {
	return "a " + e.action + " is already running for this stack"
}

// jobActor is who started a stack job: a signed-in user, or automatic
// updates (which has no user and no IP).
type jobActor struct {
	Name   string  // shown on the job and in the activity feed
	Audit  string  // the audit log's actor
	UserID *string // nil for automatic updates
	IP     string
	Auto   bool // automatic updates write their own activity entry
}

func (s *Server) requestActor(r *http.Request) jobActor {
	u := userFrom(r)
	id := u.ID
	return jobActor{Name: u.Username, Audit: u.Username, UserID: &id, IP: s.Auth.ClientIP(r)}
}

// startStackJob sends a stack command to the host's agent and follows it in
// the background: output goes to the job, and the outcome to the audit log.
func (s *Server) startStackJob(r *http.Request, st store.Stack, action string, pull, volumes bool) (*job, error) {
	if s.Reg.Get(st.HostID) == nil {
		return nil, registry.ErrOffline
	}
	return s.startStackJobAs(st, action, pull, volumes, s.requestActor(r))
}

// startStackJobAs is startStackJob for a given actor (automatic updates
// start jobs without a request).
func (s *Server) startStackJobAs(st store.Stack, action string, pull, volumes bool, actor jobActor) (*job, error) {
	return s.launchStackJob(st, action, stackJobOpts{pull: pull, volumes: volumes}, actor)
}

// stackJobOpts are a stack job's options.
type stackJobOpts struct {
	pull, volumes bool
	// rollback is the point a StackRollback goes back to.
	rollback *store.RollbackPoint
}

// launchStackJob starts any stack job. Deploys and rollbacks first record
// a rollback point (what the services run now; see rollback.go).
func (s *Server) launchStackJob(st store.Stack, action string, opts stackJobOpts, actor jobActor) (*job, error) {
	pull, volumes := opts.pull, opts.volumes
	if action == wire.StackRollback && (opts.rollback == nil || len(opts.rollback.Services) == 0) {
		return nil, errNoRollbackPoint
	}
	a := s.Reg.Get(st.HostID)
	if a == nil {
		return nil, registry.ErrOffline
	}
	if hello := a.Status().Hello; hello == nil || hello.Stacks == nil {
		return nil, errors.New("this host's agent is too old for stacks; update it to the latest tainerhub-agent image")
	} else if hello.Stacks.Error != "" {
		return nil, errors.New("stacks aren't set up on this host: " + hello.Stacks.Error)
	} else if action == wire.StackDeploy && len(st.Files) > 0 && !hello.Stacks.Files {
		return nil, errors.New("this host's agent is too old for stacks with extra files (such as a Dockerfile); update it to the latest tainerhub-agent image")
	} else if action == wire.StackRollback && !hello.Stacks.Rollback {
		return nil, errors.New("this host's agent can't roll back stacks; update it to the latest tainerhub-agent image")
	}
	j, ok := s.jobs.start(&job{StackID: st.ID, Stack: st.Name, HostID: st.HostID, Host: a.Name, Action: actionLabel(action, pull), Actor: actor.Name})
	if !ok {
		return nil, &busyError{jobID: j.ID, action: j.Action}
	}
	cmd := wire.StackCommand{Action: action, Name: st.Name, Pull: pull, Volumes: volumes}
	hash := ""
	if action == wire.StackDeploy {
		cmd.Compose, cmd.Env, cmd.Files = st.Compose, st.Env, wireFiles(st.Files)
		hash = stackHash(st)
	}
	if action == wire.StackRollback {
		cmd.Rollback = rollbackServices(*opts.rollback)
	}
	if action == wire.StackDeploy || action == wire.StackRollback {
		j.point = s.recordRollbackPoint(a, st, rollbackTrigger(action, pull, actor), actor, j)
	}
	sendCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	ch, err := a.Stack(sendCtx, cmd)
	cancel()
	if err != nil {
		j.finish(err.Error())
		return nil, err
	}
	db := s.aus()
	go func() {
		final, gotEnd := "", false
		timeout := time.NewTimer(35 * time.Minute) // the agent gives up after 30
		defer timeout.Stop()
	loop:
		for {
			select {
			case c, open := <-ch:
				if !open {
					break loop
				}
				if len(c.Data) > 0 {
					j.append(c.Data)
				}
				if c.EOF {
					final, gotEnd = c.Error, true
				}
			case <-timeout.C:
				final, gotEnd = "no result from the agent after 35 minutes", true
				break loop
			}
		}
		if !gotEnd {
			final = "lost track of the operation"
		}
		if final == registry.ErrOffline.Error() {
			final = "the host disconnected; the operation may still be finishing there"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if action == wire.StackRollback && !strings.Contains(final, wire.RollbackUnchanged) {
			// Even a failed rollback may have pinned the stack on the host
			// (its override file stays until the next deploy).
			at := opts.rollback.CreatedAt
			if err := db.SetStackRolledBack(ctx, st.ID, &at); err != nil {
				s.Log.Warn("mark stack rolled back", "err", err)
			}
		}
		if final == "" {
			switch action {
			case wire.StackDeploy:
				if err := db.MarkStackDeployed(ctx, st.ID, hash); err != nil {
					s.Log.Warn("mark stack deployed", "err", err)
				}
				// The agent removed the rollback override: newest images again.
				if err := db.SetStackRolledBack(ctx, st.ID, nil); err != nil {
					s.Log.Warn("clear stack rollback", "err", err)
				}
			case wire.StackRemove:
				if err := db.DeleteStack(ctx, st.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
					s.Log.Warn("delete stack", "err", err)
				}
			}
			if !actor.Auto {
				s.Reg.AddActivity(registry.Activity{Kind: "user", Action: "stack." + j.Action, HostID: st.HostID, Host: a.Name, Container: st.Name, Actor: actor.Name})
			}
		}
		detail := map[string]any{"host": a.Name}
		if pull {
			detail["pull"] = true
		}
		if volumes {
			detail["volumes"] = true
		}
		if opts.rollback != nil {
			detail["to"] = opts.rollback.CreatedAt
			detail["images"] = pointImages(*opts.rollback)
		}
		if final != "" {
			detail["error"] = final
		}
		e := store.AuditEntry{Actor: actor.Audit, UserID: actor.UserID, Action: auditStackAction(j.Action), Target: a.Name + "/" + st.Name, IP: actor.IP, HostIDs: []string{st.HostID}}
		e.Detail, _ = json.Marshal(detail)
		s.writeAudit(ctx, e)
		j.finish(final)
	}()
	return j, nil
}

func actionLabel(action string, pull bool) string {
	if action == wire.StackDeploy && pull {
		return "pull"
	}
	return action
}

func (s *Server) hostName(ctx context.Context, id string) string {
	if h, err := s.Store.GetHost(ctx, id); err == nil {
		return h.Name
	}
	return id
}

func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func readJSONLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(v); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid JSON body (or larger than the limit)")
		return false
	}
	return true
}
