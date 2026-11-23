package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/prezmc/tainerhub/internal/registry"
	"github.com/prezmc/tainerhub/internal/store"
	"github.com/prezmc/tainerhub/internal/wire"
)

// Moving a stack is for admins: it writes the compose file onto another host.

// regEnv runs a move against the real fleet.
type regEnv struct{ s *Server }

func (e regEnv) host(id string) (string, *wire.Hello, bool) {
	name := e.s.hostName(context.Background(), id)
	a := e.s.Reg.Get(id)
	if a == nil {
		return name, nil, false
	}
	h := a.Status().Hello
	return name, h, h != nil
}

func (e regEnv) containers(hostID string) []wire.Container {
	if a := e.s.Reg.Get(hostID); a != nil {
		return a.Status().Containers
	}
	return nil
}

func (e regEnv) operate(ctx context.Context, hostID string, op wire.Operation) (json.RawMessage, error) {
	a := e.s.Reg.Get(hostID)
	if a == nil {
		return nil, registry.ErrOffline
	}
	return a.Operate(ctx, op)
}

// stack runs a compose operation and copies its output to out.
func (e regEnv) stack(ctx context.Context, hostID string, c wire.StackCommand, out io.Writer) error {
	a := e.s.Reg.Get(hostID)
	if a == nil {
		return registry.ErrOffline
	}
	ch, err := a.Stack(ctx, c)
	if err != nil {
		return err
	}
	timeout := time.NewTimer(35 * time.Minute) // the agent gives up after 30
	defer timeout.Stop()
	for {
		select {
		case chunk, open := <-ch:
			if !open {
				return errors.New("lost track of the operation")
			}
			if len(chunk.Data) > 0 {
				_, _ = out.Write(chunk.Data)
			}
			if chunk.EOF {
				if chunk.Error != "" {
					return errors.New(chunk.Error)
				}
				return nil
			}
		case <-timeout.C:
			return errors.New("no result from the agent after 35 minutes")
		}
	}
}

func (e regEnv) copy(ctx context.Context, src, dst string, item wire.TransferItem, max int64, progress func(int64)) (registry.CopyResult, error) {
	return e.s.Reg.Copy(ctx, src, dst, item, max, progress)
}

func (e regEnv) diskFree(hostID string) int64 {
	if l := e.s.Reg.Load(hostID); l != nil {
		return l.DiskFree
	}
	return 0
}

// storeRecorder saves a move's progress in the database.
type storeRecorder struct {
	st    *store.Store
	move  store.StackMove
	stack store.Stack
}

func (r storeRecorder) leftovers(ctx context.Context, l *store.MoveLeftovers) error {
	return r.st.SetMoveLeftovers(ctx, r.move.ID, l)
}

func (r storeRecorder) commit(ctx context.Context, hash string, bytes int64, l *store.MoveLeftovers) error {
	return r.st.CommitMove(ctx, r.move.ID, r.stack.ID, r.move.ToHost, hash, bytes, l)
}

func (r storeRecorder) fail(ctx context.Context, errText string, bytes int64, l *store.MoveLeftovers) error {
	return r.st.FailMove(ctx, r.move.ID, errText, bytes, l)
}

// moveCheck answers GET /api/stacks/{sid}/move?host=: what moving the stack
// there would copy, and anything in the way.
func (s *Server) moveCheck(w http.ResponseWriter, r *http.Request) {
	st, ok := s.loadStack(w, r)
	if !ok {
		return
	}
	to := r.URL.Query().Get("host")
	if !validUUID(to) {
		jsonErr(w, http.StatusBadRequest, "choose a host")
		return
	}
	stacks, err := s.Store.ListStacks(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, checkMove(r.Context(), regEnv{s}, st, to, stacks))
}

type moveInput struct {
	HostID string `json:"hostId"`
	// Outside lists the folders outside the stack's folder the admin has
	// seen won't be copied; it must match what the check reports.
	Outside []string `json:"outside"`
}

// startMove answers POST /api/stacks/{sid}/move: it checks again and runs
// the move as a job.
func (s *Server) startMove(w http.ResponseWriter, r *http.Request) {
	st, ok := s.loadStack(w, r)
	if !ok {
		return
	}
	var in moveInput
	if !readJSON(w, r, &in) {
		return
	}
	if !validUUID(in.HostID) {
		jsonErr(w, http.StatusBadRequest, "choose a host")
		return
	}
	if st.DeployedHash != stackHash(st.Compose, st.Env) {
		jsonErr(w, http.StatusConflict, "this stack has saved changes that aren't deployed; deploy them first, so what moves is what runs")
		return
	}
	stacks, err := s.Store.ListStacks(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	rep := checkMove(r.Context(), regEnv{s}, st, in.HostID, stacks)
	if len(rep.Blockers) > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{"error": rep.Blockers[0], "report": rep})
		return
	}
	seen := slices.Clone(in.Outside)
	slices.Sort(seen)
	want := slices.Clone(rep.Confirm)
	slices.Sort(want)
	if !slices.Equal(seen, want) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "the folders outside the stack changed; check again", "report": rep})
		return
	}

	u := userFrom(r)
	j, started := s.jobs.start(&job{StackID: st.ID, Stack: st.Name, HostID: st.HostID, Host: rep.FromName, Action: "move", Actor: u.Username})
	if !started {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a " + j.Action + " is already running for this stack", "jobId": j.ID})
		return
	}
	mv, err := s.Store.StartMove(r.Context(), st, in.HostID, u.Username)
	if err != nil {
		j.finish(err.Error())
		if errors.Is(err, store.ErrConflict) {
			jsonErr(w, http.StatusConflict, "this stack is already being moved")
			return
		}
		s.fail(w, err)
		return
	}
	target := rep.FromName + "/" + st.Name
	s.record(r, &u, "stack.move_started", target, map[string]any{"to": rep.ToName, "bytes": rep.TotalBytes})
	ip := s.Auth.ClientIP(r)
	m := &mover{
		env: regEnv{s}, rec: storeRecorder{st: s.Store, move: mv, stack: st}, out: jobWriter{j}, st: st, rep: rep,
		health: 3 * time.Minute, stable: 15 * time.Second, poll: 2 * time.Second,
	}
	go func() {
		// The move outlives the request; it has its own limits.
		ctx := context.Background()
		start := time.Now()
		m.say("Moving %s from %s to %s.", st.Name, rep.FromName, rep.ToName)
		err := m.run(ctx)
		final, action := "", "stack.moved"
		if err != nil {
			final, action = err.Error(), "stack.move_failed"
		} else {
			m.say("Done in %s.", time.Since(start).Round(time.Second))
			s.Reg.AddActivity(registry.Activity{Kind: "user", Action: "stack.moved", HostID: in.HostID, Host: rep.ToName, Container: st.Name, Actor: u.Username})
		}
		detail := map[string]any{"from": rep.FromName, "to": rep.ToName, "bytes": m.bytes}
		if final != "" {
			detail["error"] = final
		}
		e := store.AuditEntry{Actor: u.Username, UserID: &u.ID, Action: action, Target: target, IP: ip}
		e.Detail, _ = json.Marshal(detail)
		actx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := s.Store.Audit(actx, e); err != nil {
			s.Log.Warn("audit write failed", "err", err)
		}
		cancel()
		s.HostsChanged()
		j.finish(final)
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"jobId": j.ID, "moveId": mv.ID})
}

// jobWriter sends a move's output to its job.
type jobWriter struct{ j *job }

func (w jobWriter) Write(p []byte) (int, error) {
	w.j.append(bytes.Clone(p))
	return len(p), nil
}

// removeLeftovers answers DELETE /api/moves/{mid}/leftovers: it deletes the
// data a move left on a host (the old copy, or a partial one).
func (s *Server) removeLeftovers(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("mid")
	if !validUUID(id) {
		jsonErr(w, http.StatusNotFound, "move not found")
		return
	}
	mv, err := s.Store.GetMove(r.Context(), id)
	if err != nil {
		s.notFoundOr(w, err)
		return
	}
	if mv.State == "running" {
		jsonErr(w, http.StatusConflict, "the move is still running")
		return
	}
	if mv.Leftovers.Empty() || mv.LeftoversRemovedAt != nil {
		jsonErr(w, http.StatusConflict, "nothing left to remove")
		return
	}
	left := mv.Leftovers
	// Never touch the host the stack runs on now (it may have moved back).
	if mv.StackID != nil {
		if st, err := s.Store.GetStack(r.Context(), *mv.StackID); err == nil && st.HostID == left.HostID {
			jsonErr(w, http.StatusConflict, "the stack runs on that host now, so its data there is in use")
			return
		}
	}
	// Nor a stack of the same name made there since: its volumes have the same names.
	stacks, err := s.Store.ListStacks(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, o := range stacks {
		if o.HostID == left.HostID && o.Name == mv.StackName {
			jsonErr(w, http.StatusConflict, "a stack called "+o.Name+" runs on that host now; its data there may be in use")
			return
		}
	}
	env := regEnv{s}
	hostName, _, online := env.host(left.HostID)
	if !online {
		jsonErr(w, http.StatusServiceUnavailable, hostName+" is offline")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Minute)
	defer cancel()
	var out bytes.Buffer
	var problems []string
	// Containers a partial or old copy may still have (none after a clean move).
	if err := env.stack(ctx, left.HostID, wire.StackCommand{Action: wire.StackRemove, Name: mv.StackName}, &out); err != nil {
		problems = append(problems, fmt.Sprintf("remove its containers: %v", err))
	}
	for _, v := range left.Volumes {
		if _, err := env.operate(ctx, left.HostID, wire.Operation{Kind: wire.OpVolumeRemove, Target: v}); err != nil {
			problems = append(problems, fmt.Sprintf("volume %s: %v", v, err))
		}
	}
	if left.Dir != "" {
		if _, err := env.operate(ctx, left.HostID, wire.Operation{Kind: wire.OpStackDirRemove, Target: left.Dir}); err != nil {
			problems = append(problems, fmt.Sprintf("stack folder: %v", err))
		}
	}
	u := userFrom(r)
	detail := map[string]any{"host": hostName, "volumes": left.Volumes, "dir": left.Dir != ""}
	if len(problems) > 0 {
		detail["errors"] = problems
	}
	s.record(r, &u, "stack.move_leftovers_removed", hostName+"/"+mv.StackName, detail)
	if len(problems) > 0 {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "some of it couldn't be removed: " + problems[0], "problems": problems})
		return
	}
	if err := s.Store.LeftoversRemoved(r.Context(), mv.ID); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
