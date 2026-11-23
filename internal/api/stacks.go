package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	Online     bool       `json:"online"`
	Status     string     `json:"status"` // running, partial, stopped, down, not deployed, unknown
	Containers int        `json:"containers"`
	Running    int        `json:"running"`
	Unhealthy  int        `json:"unhealthy"`
	UpdatedAt  *time.Time `json:"updatedAt,omitempty"`
	UpdatedBy  string     `json:"updatedBy,omitempty"`
	DeployedAt *time.Time `json:"deployedAt,omitempty"`
	Pending    bool       `json:"pending"` // saved changes that haven't been deployed
	Job        *jobRef    `json:"job,omitempty"`
}

func stackHash(compose, env string) string {
	h := sha256.Sum256([]byte(compose + "\x00" + env))
	return hex.EncodeToString(h[:])
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
	hosts, err := s.Store.ListHosts(ctx)
	if err != nil {
		return nil, err
	}
	hostName := map[string]string{}
	for _, h := range hosts {
		hostName[h.ID] = h.Name
	}
	stacks, err := s.Store.ListStacks(ctx)
	if err != nil {
		return nil, err
	}
	live := map[string][]wire.Container{}
	for _, a := range s.Reg.All() {
		live[a.HostID] = a.Status().Containers
	}
	jobs := s.jobs.running()

	out := []stackSummary{}
	managed := map[string]bool{}
	for _, st := range stacks {
		managed[st.HostID+"/"+st.Name] = true
		cs, online := live[st.HostID]
		updated := st.UpdatedAt
		sum := stackSummary{
			ID: st.ID, Name: st.Name, HostID: st.HostID, HostName: hostName[st.HostID], Managed: true, Online: online,
			UpdatedAt: &updated, UpdatedBy: st.UpdatedBy, DeployedAt: st.DeployedAt,
			Pending: st.DeployedHash != stackHash(st.Compose, st.Env),
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
			sum := stackSummary{Name: c.Project, HostID: hostID, HostName: hostName[hostID], Online: true}
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
	writeJSON(w, http.StatusOK, out)
}

// loadStack fetches the stack named in the path and writes 404 if it's missing.
func (s *Server) loadStack(w http.ResponseWriter, r *http.Request) (store.Stack, bool) {
	id := r.PathValue("sid")
	if !validUUID(id) {
		jsonErr(w, http.StatusNotFound, "stack not found")
		return store.Stack{}, false
	}
	st, err := s.Store.GetStack(r.Context(), id)
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
	// Compose files and .env often hold passwords, like container env values.
	canView := roleRank[userFrom(r).Role] >= roleRank["admin"]
	if !canView {
		st.Compose, st.Env = "", ""
	}
	writeJSON(w, http.StatusOK, map[string]any{"stack": st, "summary": sum, "filesVisible": canView})
}

type stackInput struct {
	HostID    string    `json:"hostId"`
	Name      string    `json:"name"`
	Compose   string    `json:"compose"`
	Env       string    `json:"env"`
	UpdatedAt time.Time `json:"updatedAt"` // edits: the version the editor loaded
	Deploy    bool      `json:"deploy"`    // deploy right after saving
	Adopt     bool      `json:"adopt"`     // take over a compose project already running on the host
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
	return nil
}

func (s *Server) createStack(w http.ResponseWriter, r *http.Request) {
	var in stackInput
	if !readJSONLimit(w, r, &in, 1<<20) {
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
	u := userFrom(r)
	st, err := s.Store.CreateStack(r.Context(), store.Stack{HostID: h.ID, Name: in.Name, Compose: in.Compose, Env: in.Env, UpdatedBy: u.Username})
	if errors.Is(err, store.ErrConflict) {
		jsonErr(w, http.StatusConflict, "this host already has a stack called "+in.Name)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	details := map[string]string{"host": h.Name}
	if in.Adopt {
		details["adopted"] = "yes"
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
	if !readJSONLimit(w, r, &in, 1<<20) {
		return
	}
	in.Name = st.Name // names are fixed: renaming would orphan the running project
	if err := in.validate(); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	u := userFrom(r)
	st, err := s.Store.UpdateStack(r.Context(), st.ID, in.Compose, in.Env, u.Username, in.UpdatedAt)
	if errors.Is(err, store.ErrStale) {
		jsonErr(w, http.StatusConflict, "someone else saved this stack after you opened it; reload to see their changes")
		return
	}
	if err != nil {
		s.notFoundOr(w, err)
		return
	}
	s.record(r, &u, "stack.updated", s.hostName(r.Context(), st.HostID)+"/"+st.Name, nil)
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

// startStackJob sends a stack command to the host's agent and follows it in
// the background: output goes to the job, and the outcome to the audit log.
func (s *Server) startStackJob(r *http.Request, st store.Stack, action string, pull, volumes bool) (*job, error) {
	a := s.Reg.Get(st.HostID)
	if a == nil {
		return nil, registry.ErrOffline
	}
	if hello := a.Status().Hello; hello == nil || hello.Stacks == nil {
		return nil, errors.New("this host's agent is too old for stacks; update it to the latest tainerhub-agent image")
	} else if hello.Stacks.Error != "" {
		return nil, errors.New("stacks aren't set up on this host: " + hello.Stacks.Error)
	}
	u := userFrom(r)
	j, ok := s.jobs.start(&job{StackID: st.ID, Stack: st.Name, HostID: st.HostID, Host: a.Name, Action: actionLabel(action, pull), Actor: u.Username})
	if !ok {
		return nil, &busyError{jobID: j.ID, action: j.Action}
	}
	cmd := wire.StackCommand{Action: action, Name: st.Name, Pull: pull, Volumes: volumes}
	hash := ""
	if action == wire.StackDeploy {
		cmd.Compose, cmd.Env = st.Compose, st.Env
		hash = stackHash(st.Compose, st.Env)
	}
	sendCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	ch, err := a.Stack(sendCtx, cmd)
	cancel()
	if err != nil {
		j.finish(err.Error())
		return nil, err
	}
	ip := s.Auth.ClientIP(r)
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
		if final == "" {
			switch action {
			case wire.StackDeploy:
				if err := s.Store.MarkStackDeployed(ctx, st.ID, hash); err != nil {
					s.Log.Warn("mark stack deployed", "err", err)
				}
			case wire.StackRemove:
				if err := s.Store.DeleteStack(ctx, st.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
					s.Log.Warn("delete stack", "err", err)
				}
			}
			s.Reg.AddActivity(registry.Activity{Kind: "user", Action: "stack." + j.Action, HostID: st.HostID, Host: a.Name, Container: st.Name, Actor: u.Username})
		}
		detail := map[string]any{"host": a.Name}
		if pull {
			detail["pull"] = true
		}
		if volumes {
			detail["volumes"] = true
		}
		if final != "" {
			detail["error"] = final
		}
		e := store.AuditEntry{Actor: u.Username, UserID: &u.ID, Action: "stack." + j.Action, Target: a.Name + "/" + st.Name, IP: ip}
		e.Detail, _ = json.Marshal(detail)
		if err := s.Store.Audit(ctx, e); err != nil {
			s.Log.Warn("audit write failed", "err", err)
		}
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
