package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/prezmc/tainerhub/internal/registry"
	"github.com/prezmc/tainerhub/internal/store"
	"github.com/prezmc/tainerhub/internal/wire"
)

// Moving a container is for admins, like moving a stack: it recreates the
// container, with all its settings, on another host.

func (e regEnv) action(ctx context.Context, hostID, container, action string) error {
	a := e.s.Reg.Get(hostID)
	if a == nil {
		return registry.ErrOffline
	}
	return a.Do(ctx, wire.ContainerAction{ContainerID: container, Action: action})
}

// ctrStoreRecorder saves a container move's progress in the database.
type ctrStoreRecorder struct {
	st   *store.Store
	move store.StackMove
}

func (r ctrStoreRecorder) leftovers(ctx context.Context, l *store.MoveLeftovers) error {
	return r.st.SetMoveLeftovers(ctx, r.move.ID, l)
}

func (r ctrStoreRecorder) done(ctx context.Context, bytes int64, l *store.MoveLeftovers) error {
	return r.st.CommitContainerMove(ctx, r.move.ID, bytes, l)
}

func (r ctrStoreRecorder) fail(ctx context.Context, errText string, bytes int64, l *store.MoveLeftovers) error {
	return r.st.FailMove(ctx, r.move.ID, errText, bytes, l)
}

// containerRef reads and checks the host and container of a container URL.
func containerRef(w http.ResponseWriter, r *http.Request) (hostID, cid string, ok bool) {
	hostID, cid = r.PathValue("id"), r.PathValue("cid")
	if !validUUID(hostID) || !wire.ValidContainerRef(cid) {
		jsonErr(w, http.StatusNotFound, "container not found")
		return "", "", false
	}
	return hostID, cid, true
}

// containerMoveCheck answers GET /api/hosts/{id}/containers/{cid}/move?host=.
func (s *Server) containerMoveCheck(w http.ResponseWriter, r *http.Request) {
	from, cid, ok := containerRef(w, r)
	if !ok {
		return
	}
	to := r.URL.Query().Get("host")
	if !validUUID(to) {
		jsonErr(w, http.StatusBadRequest, "choose a host")
		return
	}
	if !s.allowHosts(w, r, to) { // a move needs both hosts
		return
	}
	writeJSON(w, http.StatusOK, checkContainerMove(r.Context(), regEnv{s}, from, cid, to))
}

// startContainerMove answers POST /api/hosts/{id}/containers/{cid}/move:
// it checks again and runs the move as a job.
func (s *Server) startContainerMove(w http.ResponseWriter, r *http.Request) {
	from, cid, ok := containerRef(w, r)
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
	if !s.allowHosts(w, r, in.HostID) { // a move needs both hosts
		return
	}
	rep := checkContainerMove(r.Context(), regEnv{s}, from, cid, in.HostID)
	if len(rep.Blockers) > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{"error": rep.Blockers[0], "report": rep})
		return
	}
	seen := slices.Clone(in.Outside)
	slices.Sort(seen)
	want := slices.Clone(rep.Confirm)
	slices.Sort(want)
	if !slices.Equal(seen, want) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "the host folders it uses changed; check again", "report": rep})
		return
	}

	u := userFrom(r)
	j, started := s.jobs.start(&job{StackID: "container:" + from + "/" + rep.Container, Stack: rep.Container, HostID: from, Host: rep.FromName, Action: "move", Actor: u.Username})
	if !started {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a " + j.Action + " is already running for this container", "jobId": j.ID})
		return
	}
	mv, err := s.Store.StartContainerMove(r.Context(), rep.Container, from, in.HostID, u.Username)
	if err != nil {
		j.finish(err.Error())
		if errors.Is(err, store.ErrConflict) {
			jsonErr(w, http.StatusConflict, "this container is already being moved")
			return
		}
		s.fail(w, err)
		return
	}
	target := rep.FromName + "/" + rep.Container
	s.record(r, &u, "container.move_started", target, map[string]any{"to": rep.ToName, "bytes": rep.TotalBytes, "networks": rep.Networks})
	ip := s.Auth.ClientIP(r)
	m := &ctrMover{
		env: regEnv{s}, rec: ctrStoreRecorder{st: s.Store, move: mv}, out: jobWriter{j}, rep: rep,
		health: 3 * time.Minute, stable: 15 * time.Second, poll: 2 * time.Second,
	}
	go func() {
		ctx := context.Background() // the move outlives the request
		start := time.Now()
		m.say("Moving %s from %s to %s.", rep.Container, rep.FromName, rep.ToName)
		err := m.run(ctx)
		final, action := "", "container.moved"
		if err != nil {
			final, action = err.Error(), "container.move_failed"
		} else {
			m.say("Done in %s.", time.Since(start).Round(time.Second))
			s.Reg.AddActivity(registry.Activity{Kind: "user", Action: "container.moved", HostID: in.HostID, Host: rep.ToName, Container: rep.Container, Actor: u.Username})
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

// containerMoves answers GET /api/container-moves: recent container moves,
// so the UI can offer to delete what they left behind.
func (s *Server) containerMoves(w http.ResponseWriter, r *http.Request) {
	ms, err := s.aus().ContainerMoves(r.Context(), 100)
	if err != nil {
		s.fail(w, err)
		return
	}
	u := userFrom(r)
	writeJSON(w, http.StatusOK, visible(u, nonNil(ms), func(m store.StackMove) string {
		if !moveVisible(u, m) {
			return ""
		}
		return m.FromHost
	}))
}

// moveVisible: a move is shown only to those who can see both its hosts.
func moveVisible(u store.User, m store.StackMove) bool {
	return u.CanSeeHost(m.FromHost) && u.CanSeeHost(m.ToHost)
}

// removeContainerLeftovers deletes what a container move left on a host:
// the old copy's volumes after a move, or a partial copy after a failed one.
func (s *Server) removeContainerLeftovers(w http.ResponseWriter, r *http.Request, mv store.StackMove) {
	left := mv.Leftovers
	env := regEnv{s}
	hostName, _, online := env.host(left.HostID)
	if !online {
		jsonErr(w, http.StatusServiceUnavailable, hostName+" is offline")
		return
	}
	ctrs := env.containers(left.HostID)
	if c, ok := findContainer(ctrs, mv.StackName); ok {
		if left.Container == "" {
			// It was moved back, or another container took the name: its data is in use.
			jsonErr(w, http.StatusConflict, "a container called "+mv.StackName+" is on that host now; its data there may be in use")
			return
		}
		if c.State == "running" || c.State == "restarting" {
			jsonErr(w, http.StatusConflict, mv.StackName+" is running on that host; stop it first if it really is the old copy")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	var problems []string
	if left.Container != "" {
		if c, ok := findContainer(ctrs, left.Container); ok {
			if err := env.action(ctx, left.HostID, c.ID, wire.ActionRemove); err != nil {
				problems = append(problems, fmt.Sprintf("container %s: %v", left.Container, err))
			}
		}
	}
	for _, v := range left.Volumes {
		_, err := env.operate(ctx, left.HostID, wire.Operation{Kind: wire.OpVolumeRemove, Target: v})
		if err != nil && strings.Contains(strings.ToLower(err.Error()), "no such volume") {
			err = nil // already gone
		}
		if err != nil {
			problems = append(problems, fmt.Sprintf("volume %s: %v", v, err))
		}
	}
	u := userFrom(r)
	detail := map[string]any{"host": hostName, "volumes": left.Volumes, "container": left.Container}
	if len(problems) > 0 {
		detail["errors"] = problems
	}
	s.record(r, &u, "container.move_leftovers_removed", hostName+"/"+mv.StackName, detail)
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
