package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/prezmc/tainerhub/internal/registry"
	"github.com/prezmc/tainerhub/internal/store"
	"github.com/prezmc/tainerhub/internal/wire"
)

// Stack rollback. Right before every deploy, Pull & redeploy, automatic
// update and rollback, the hub asks the host which image each of the
// stack's services runs and saves that as a rollback point (the last
// store.RollbackKeep per stack). The agent tags those images
// tainerhub-rollback/<stack>:<service> so TainerHub's prunes leave them.
//
// "Roll back" goes back to the newest point: the agent pins each service to
// its old image with an override file and runs `up` (nothing is pulled or
// built). The stack then stays pinned, through restarts and stops, until
// the next deploy ("Return to latest"). The stack's files and its deployed
// hash don't change, so a rollback never shows as "changes not deployed".

var errNoRollbackPoint = errors.New("there is nothing to roll back to yet: TainerHub records what a stack runs right before each deploy, Pull & redeploy or automatic update")

// rollbackTrigger names what a rollback point was recorded for.
func rollbackTrigger(action string, pull bool, actor jobActor) string {
	switch {
	case action == wire.StackRollback:
		return "rollback"
	case pull && actor.Auto:
		return "auto"
	case pull:
		return "pull"
	}
	return "deploy"
}

// auditStackAction is the audit log's name for a stack job.
func auditStackAction(label string) string {
	if label == wire.StackRollback {
		return "stack.rolled_back"
	}
	return "stack." + label
}

// recordRollbackPoint saves what the stack's services run now, writing a
// line about it to the job. It never stops the job: a deploy still runs
// when the point can't be recorded (an older agent, say). A rollback keeps
// the current images itself, after pinning the old ones, so it doesn't ask
// for the tags here (they would move off the images it goes back to).
func (s *Server) recordRollbackPoint(a *registry.Agent, st store.Stack, trigger string, actor jobActor, j *job) *store.RollbackPoint {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	op := wire.Operation{Kind: wire.OpStackImages, Target: st.Name}
	if trigger != "rollback" {
		op.Arg = wire.StackImagesProtect
	}
	raw, err := a.Operate(ctx, op)
	var res wire.StackImages
	if err == nil {
		err = json.Unmarshal(raw, &res)
	}
	if err != nil {
		j.append([]byte("Couldn't record a rollback point: " + err.Error() + "\n"))
		return nil
	}
	if len(res.Services) == 0 {
		j.append([]byte("Nothing is running yet, so there is no rollback point to record.\n"))
		return nil
	}
	p := store.RollbackPoint{StackID: st.ID, Trigger: trigger, CreatedBy: actor.Name, ComposeHash: st.DeployedHash, Services: map[string]store.RollbackService{}}
	for svc, im := range res.Services {
		if !wire.ValidServiceName(svc) || !wire.ValidImageID(im.ImageID) {
			continue
		}
		p.Services[svc] = store.RollbackService{Ref: im.Ref, ImageID: im.ImageID, Digest: im.Digest, Created: im.Created}
	}
	saved, err := s.aus().AddRollbackPoint(ctx, p)
	if err != nil {
		s.Log.Warn("save rollback point", "stack", st.Name, "err", err)
		j.append([]byte("Couldn't save the rollback point: " + err.Error() + "\n"))
		return nil
	}
	j.append([]byte(fmt.Sprintf("Recorded a rollback point: %d %s and the images they run now.\n", len(p.Services), plural(len(p.Services), "service", "services"))))
	return &saved
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// rollbackServices is what a rollback command sends for a point.
func rollbackServices(p store.RollbackPoint) []wire.RollbackService {
	out := make([]wire.RollbackService, 0, len(p.Services))
	for svc, im := range p.Services {
		rs := wire.RollbackService{Service: svc, ImageID: im.ImageID}
		if wire.ValidImageRef(im.Ref) {
			rs.Ref = im.Ref
		}
		out = append(out, rs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}

// pointImages is service -> image for the audit log.
func pointImages(p store.RollbackPoint) map[string]string {
	out := map[string]string{}
	for svc, im := range p.Services {
		out[svc] = imageLabel(im.Ref, im.ImageID)
	}
	return out
}

func imageLabel(ref, id string) string {
	short := id
	if len(short) > 19 {
		short = short[:19] // sha256: + 12
	}
	if ref == "" {
		return short
	}
	return ref + " (" + short + ")"
}

// rollbackRow is one service in the rollback preview.
type rollbackRow struct {
	Service  string                 `json:"service"`
	Current  *store.RollbackService `json:"current"`  // nil: not running now
	Previous store.RollbackService  `json:"previous"` // what a rollback goes back to
	Same     bool                   `json:"same"`     // already runs that image
}

// getRollback answers GET /api/stacks/{sid}/rollback: what a rollback
// would change, service by service.
func (s *Server) getRollback(w http.ResponseWriter, r *http.Request) {
	st, ok := s.loadStack(w, r)
	if !ok {
		return
	}
	p, err := s.aus().LatestRollbackPoint(r.Context(), st.ID)
	if errors.Is(err, store.ErrNotFound) {
		jsonErr(w, http.StatusNotFound, errNoRollbackPoint.Error())
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	cur, curErr := s.currentImages(r.Context(), st)
	rows := []rollbackRow{}
	for svc, prev := range p.Services {
		row := rollbackRow{Service: svc, Previous: prev}
		if c, ok := cur[svc]; ok {
			c := c
			row.Current = &c
			row.Same = c.ImageID != "" && c.ImageID == prev.ImageID
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Service < rows[j].Service })
	res := map[string]any{
		"point":        map[string]any{"id": p.ID, "createdAt": p.CreatedAt, "trigger": p.Trigger, "createdBy": p.CreatedBy},
		"services":     rows,
		"rolledBackTo": st.RolledBackTo,
		// The rollback changes images only: a compose file deployed since
		// the point was recorded stays as it is.
		"composeChanged": p.ComposeHash != "" && p.ComposeHash != st.DeployedHash,
	}
	if curErr != nil {
		res["currentError"] = curErr.Error()
	}
	writeJSON(w, http.StatusOK, res)
}

// currentImages is what the stack's services run now: asked from the host,
// or from its last container list when the agent can't say (image IDs are
// then unknown).
func (s *Server) currentImages(ctx context.Context, st store.Stack) (map[string]store.RollbackService, error) {
	a := s.Reg.Get(st.HostID)
	if a == nil {
		return nil, registry.ErrOffline
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out := map[string]store.RollbackService{}
	raw, err := a.Operate(ctx, wire.Operation{Kind: wire.OpStackImages, Target: st.Name})
	var res wire.StackImages
	if err == nil {
		err = json.Unmarshal(raw, &res)
	}
	if err == nil {
		for svc, im := range res.Services {
			out[svc] = store.RollbackService{Ref: im.Ref, ImageID: im.ImageID, Digest: im.Digest, Created: im.Created}
		}
		return out, nil
	}
	for _, c := range a.Status().Containers {
		if svc := c.Labels["com.docker.compose.service"]; c.Project == st.Name && svc != "" {
			out[svc] = store.RollbackService{Ref: c.Image}
		}
	}
	return out, err
}

// rollbackStack answers POST /api/stacks/{sid}/rollback: go back to the
// newest rollback point. Operators may, like Pull & redeploy.
func (s *Server) rollbackStack(w http.ResponseWriter, r *http.Request) {
	st, ok := s.loadStack(w, r)
	if !ok {
		return
	}
	p, err := s.aus().LatestRollbackPoint(r.Context(), st.ID)
	if errors.Is(err, store.ErrNotFound) {
		jsonErr(w, http.StatusNotFound, errNoRollbackPoint.Error())
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	if s.Reg.Get(st.HostID) == nil {
		jsonErr(w, http.StatusServiceUnavailable, "host is offline")
		return
	}
	j, err := s.launchStackJob(st, wire.StackRollback, stackJobOpts{rollback: &p}, s.requestActor(r))
	var busy *busyError
	switch {
	case errors.As(err, &busy):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "jobId": busy.jobID})
	case errors.Is(err, registry.ErrOffline):
		jsonErr(w, http.StatusServiceUnavailable, "host is offline")
	case errors.Is(err, errNoRollbackPoint):
		jsonErr(w, http.StatusNotFound, err.Error())
	case err != nil:
		jsonErr(w, http.StatusBadGateway, err.Error())
	default:
		writeJSON(w, http.StatusAccepted, map[string]string{"jobId": j.ID})
	}
}
