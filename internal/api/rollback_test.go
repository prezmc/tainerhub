package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/store"
	"github.com/prezmc/tainerhub/internal/wire"
)

// rollbackRig is a host running stack "app" (one service, web) that is
// opted in to automatic updates and has a newer image.
func rollbackRig(t *testing.T) (*autoRig, *fakeHost, store.Stack) {
	t.Helper()
	x := newAutoRig(t)
	stack := deployedStack("app", "services:\n  web:\n    image: app:1\n")
	stack.ID = stackX
	x.db.stacks[stack.ID] = stack
	h := x.host(autoHostA, "alpha", wire.Container{ID: "c1", Name: "app-web-1", Image: "app:1", Project: "app", State: "running", Status: "Up 2 days"})
	h.updates = map[string]bool{"app:1": true}
	_ = x.db.EnableStackAutoUpdate(context.Background(), stack.ID, "ann")
	return x, h, stack
}

func (x *autoRig) auditActions() []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	var out []string
	for _, e := range x.audit {
		out = append(out, e.Action)
	}
	return out
}

func has(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestAutoRollbackWhenHealthFails(t *testing.T) {
	x, h, stack := rollbackRig(t)
	h.newState, h.newStatus = "running", "Up 1 minute (unhealthy)"
	run := x.runNow()
	o := outcome(t, run, "app")
	if o.Result != autoRolledBack || o.RollbackJobID == "" || !strings.Contains(o.Reason, "app-web-1 is unhealthy") || !strings.Contains(o.Reason, "previous images were put back") {
		t.Fatalf("outcome: %+v", o)
	}
	// The rollback went back to what ran before the update.
	if len(h.rollbacks) != 1 {
		t.Fatalf("%d rollbacks, want 1", len(h.rollbacks))
	}
	rb := h.rollbacks[0]
	if len(rb.Rollback) != 1 || rb.Rollback[0].Service != "web" || rb.Rollback[0].ImageID != fakeImageID("c1") || rb.Rollback[0].Ref != "app:1" {
		t.Fatalf("rollback command: %+v", rb.Rollback)
	}
	// The update's point was recorded (and protected) first; the rollback
	// recorded the failed images as the next point without moving tags.
	if got := strings.Join(h.ops, " | "); !strings.Contains(got, "images app protect | stack app") || !strings.Contains(got, "images app  | rollback app") {
		t.Fatalf("ops: %s", got)
	}
	pts := x.db.points[stack.ID]
	if len(pts) != 2 || pts[0].Trigger != "auto" || pts[1].Trigger != "rollback" || pts[1].Services["web"].ImageID != fakeImageID("new-app-web-1") {
		t.Fatalf("points: %+v", pts)
	}
	if j := x.s.jobs.get(o.RollbackJobID); j == nil || j.Action != wire.StackRollback || j.Actor != autoActorName {
		t.Fatalf("rollback job: %+v", j)
	}
	acts := x.auditActions()
	if !has(acts, "update.auto_rolled_back") || !has(acts, "stack.rolled_back") || has(acts, "update.auto_failed") {
		t.Fatalf("audit: %v", acts)
	}
	if got := x.db.target("app"); got.LastResult != autoRolledBack {
		t.Fatalf("target row: %+v", got)
	}
	// The stack is pinned to the rollback, its files still count as deployed.
	if st := x.db.stacks[stack.ID]; st.RolledBackTo == nil || !st.RolledBackTo.Equal(pts[0].CreatedAt) || st.DeployedHash != stackHash(st) {
		t.Fatalf("stack after rollback: %+v", st)
	}
	// ... and the next window leaves it alone.
	if o := outcome(t, x.runNow(), "app"); o.Result != autoSkipped || !strings.Contains(o.Reason, "Return to latest") {
		t.Fatalf("next run: %+v", o)
	}
}

func TestAutoRollbackWhenDeployFails(t *testing.T) {
	x, h, _ := rollbackRig(t)
	h.failWith = "pull access denied"
	o := outcome(t, x.runNow(), "app")
	if o.Result != autoRolledBack || !strings.Contains(o.Reason, "pull access denied") || len(h.rollbacks) != 1 {
		t.Fatalf("outcome: %+v (%d rollbacks)", o, len(h.rollbacks))
	}
}

func TestAutoRollbackThatFails(t *testing.T) {
	x, h, stack := rollbackRig(t)
	h.newState, h.newStatus = "exited", "Exited (1) 1 second ago"
	h.failRollback = "the previous image is gone from this host for web"
	o := outcome(t, x.runNow(), "app")
	if o.Result != autoFailed || !strings.Contains(o.Reason, "app-web-1 is exited; rollback failed: the previous image is gone") {
		t.Fatalf("outcome: %+v", o)
	}
	if acts := x.auditActions(); !has(acts, "update.auto_failed") || has(acts, "update.auto_rolled_back") {
		t.Fatalf("audit: %v", acts)
	}
	// A rollback that failed partway may still have pinned the stack.
	if x.db.stacks[stack.ID].RolledBackTo == nil {
		t.Fatal("a failed rollback that changed things should leave the stack marked rolled back")
	}
	// One that failed before changing anything doesn't.
	x2, h2, stack2 := rollbackRig(t)
	h2.newState, h2.newStatus = "exited", "Exited (1) 1 second ago"
	h2.failRollback = "the previous image is gone from this host for web (app:1): it was removed outside TainerHub; " + wire.RollbackUnchanged
	x2.runNow()
	if x2.db.stacks[stack2.ID].RolledBackTo != nil {
		t.Fatal("a rollback that changed nothing marked the stack rolled back")
	}
}

func TestAutoRollbackTurnedOff(t *testing.T) {
	x, h, stack := rollbackRig(t)
	_, _ = x.db.SetStackAutoRollback(context.Background(), stack.ID, false)
	h.newState, h.newStatus = "exited", "Exited (1) 1 second ago"
	o := outcome(t, x.runNow(), "app")
	if o.Result != autoFailed || len(h.rollbacks) != 0 || strings.Contains(o.Reason, "rollback") {
		t.Fatalf("outcome: %+v (%d rollbacks)", o, len(h.rollbacks))
	}
}

func TestContainerRecreateRollbackOutcome(t *testing.T) {
	x := newAutoRig(t)
	h := x.host(autoHostA, "alpha", wire.Container{ID: "c1", Name: "web", Image: "nginx:1", State: "running"})
	h.updates = map[string]bool{"nginx:1": true}
	h.failWith = "start the new container: port is taken (the old container was put back)"
	x.optIn(autoHostA, "web")
	if o := outcome(t, x.runNow(), "web"); o.Result != autoRolledBack || !strings.Contains(o.Reason, "put back") {
		t.Fatalf("outcome: %+v", o)
	}
	if !has(x.auditActions(), "update.auto_rolled_back") {
		t.Fatalf("audit: %v", x.auditActions())
	}
}

func TestAutoRollbackSettingDefaultsOn(t *testing.T) {
	x, _, stack := rollbackRig(t)
	if !x.db.target("app").AutoRollback {
		t.Fatal("automatic rollback should be on by default")
	}
	w := x.do("PUT", "/api/stacks/"+stack.ID+"/auto-update", `{"enabled":true,"autoRollback":false}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"autoRollback":false`) {
		t.Fatalf("turn off: %d %s", w.Code, w.Body)
	}
	if x.db.target("app").AutoRollback {
		t.Fatal("still on")
	}
	if w := x.do("PUT", "/api/stacks/"+stack.ID+"/auto-update", `{"enabled":true}`); !strings.Contains(w.Body.String(), `"autoRollback":false`) {
		t.Fatalf("enabling again without the field should keep the choice: %s", w.Body)
	}
}

// waitJob waits for a stack job started through the API.
func waitJob(t *testing.T, x *autoRig, body []byte) *job {
	t.Helper()
	var res struct{ JobID string }
	_ = json.Unmarshal(body, &res)
	j := x.s.jobs.get(res.JobID)
	if j == nil {
		t.Fatalf("no job in %s", body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if errText, err := j.wait(ctx); err != nil || errText != "" {
		t.Fatalf("job: %v %q", err, errText)
	}
	return j
}

func TestRollbackAPI(t *testing.T) {
	x, h, stack := rollbackRig(t)
	path := "/api/stacks/" + stack.ID + "/rollback"
	// Nothing recorded yet.
	if w := x.do("GET", path, ""); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "nothing to roll back to") {
		t.Fatalf("preview without a point: %d %s", w.Code, w.Body)
	}
	if w := x.do("POST", path, ""); w.Code != http.StatusNotFound {
		t.Fatalf("rollback without a point: %d", w.Code)
	}

	// Pull & redeploy records a point first, and its job says so.
	x.user.Role = "operator"
	w := x.do("POST", "/api/stacks/"+stack.ID+"/pull", "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("pull: %d %s", w.Code, w.Body)
	}
	j := waitJob(t, x, w.Body.Bytes())
	if out, _, _, _, _ := j.read(0); !strings.Contains(string(out), "Recorded a rollback point: 1 service") {
		t.Fatalf("job output: %q", out)
	}
	if pts := x.db.points[stack.ID]; len(pts) != 1 || pts[0].Trigger != "pull" || pts[0].CreatedBy != "ann" || pts[0].ComposeHash != stack.DeployedHash {
		t.Fatalf("points: %+v", pts)
	}

	// Viewers see the preview: web, from the new image back to the old one.
	x.user.Role = "viewer"
	w = x.do("GET", path, "")
	var prev struct {
		Point struct {
			Trigger string
		}
		Services []rollbackRow
	}
	if err := json.Unmarshal(w.Body.Bytes(), &prev); err != nil || w.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", w.Code, w.Body)
	}
	if len(prev.Services) != 1 || prev.Point.Trigger != "pull" {
		t.Fatalf("preview: %s", w.Body)
	}
	row := prev.Services[0]
	if row.Service != "web" || row.Previous.ImageID != fakeImageID("c1") || row.Current == nil || row.Current.ImageID != fakeImageID("new-app-web-1") || row.Same {
		t.Fatalf("row: %+v", row)
	}
	// ... but can't roll back.
	if w := x.do("POST", path, ""); w.Code != http.StatusForbidden {
		t.Fatalf("viewer rollback: %d", w.Code)
	}

	x.user.Role = "operator"
	x.db.deployed = map[string]string{}
	w = x.do("POST", path, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("rollback: %d %s", w.Code, w.Body)
	}
	waitJob(t, x, w.Body.Bytes())
	if len(h.rollbacks) != 1 || h.rollbacks[0].Rollback[0].ImageID != fakeImageID("c1") {
		t.Fatalf("rollback command: %+v", h.rollbacks)
	}
	x.mu.Lock()
	var entry store.AuditEntry
	for _, e := range x.audit {
		if e.Action == "stack.rolled_back" {
			entry = e
		}
	}
	x.mu.Unlock()
	if entry.Actor != "ann" || entry.Target != "alpha/app" || !strings.Contains(string(entry.Detail), "app:1") {
		t.Fatalf("audit entry: %+v %s", entry, entry.Detail)
	}
	st := x.db.stacks[stack.ID]
	if st.RolledBackTo == nil {
		t.Fatal("not marked rolled back")
	}
	// A rollback is not a deploy: the stack's files and hash are untouched,
	// so it doesn't show "changes not deployed".
	if _, marked := x.db.deployed[stack.ID]; marked || st.DeployedHash != stackHash(st) {
		t.Fatal("the rollback changed the deployed hash")
	}

	// Return to latest is an ordinary deploy, and clears the pin.
	w = x.do("POST", "/api/stacks/"+stack.ID+"/pull", "")
	waitJob(t, x, w.Body.Bytes())
	if x.db.stacks[stack.ID].RolledBackTo != nil {
		t.Fatal("deploy didn't clear the rollback")
	}
	if n := len(x.db.points[stack.ID]); n != 3 {
		t.Fatalf("%d points, want 3 (pull, rollback, pull)", n)
	}
}

func TestRollbackPointsKeepFive(t *testing.T) {
	x, _, stack := rollbackRig(t)
	x.user.Role = "operator"
	for i := 0; i < 7; i++ {
		w := x.do("POST", "/api/stacks/"+stack.ID+"/pull", "")
		waitJob(t, x, w.Body.Bytes())
	}
	if n := len(x.db.points[stack.ID]); n != store.RollbackKeep {
		t.Fatalf("%d points kept, want %d", n, store.RollbackKeep)
	}
}

func TestNoRollbackPointWhenNothingRuns(t *testing.T) {
	x := newAutoRig(t)
	stack := deployedStack("app", "services:\n  web:\n    image: app:1\n")
	stack.ID = stackX
	x.db.stacks[stack.ID] = stack
	x.host(autoHostA, "alpha")
	w := x.do("POST", "/api/stacks/"+stack.ID+"/pull", "")
	j := waitJob(t, x, w.Body.Bytes())
	if out, _, _, _, _ := j.read(0); !strings.Contains(string(out), "no rollback point") {
		t.Fatalf("job output: %q", out)
	}
	if len(x.db.points[stack.ID]) != 0 {
		t.Fatal("recorded a point for a stack that wasn't running")
	}
}

func TestRollbackTrigger(t *testing.T) {
	for _, tc := range []struct {
		action string
		pull   bool
		auto   bool
		want   string
	}{
		{wire.StackDeploy, false, false, "deploy"},
		{wire.StackDeploy, true, false, "pull"},
		{wire.StackDeploy, true, true, "auto"},
		{wire.StackRollback, false, true, "rollback"},
	} {
		if got := rollbackTrigger(tc.action, tc.pull, jobActor{Auto: tc.auto}); got != tc.want {
			t.Errorf("%+v: %s", tc, got)
		}
	}
	if auditStackAction("rollback") != "stack.rolled_back" || auditStackAction("pull") != "stack.pull" {
		t.Error("audit names")
	}
}

func TestForcedImageRemoveIsForAdmins(t *testing.T) {
	x := newAutoRig(t)
	h := x.host(autoHostA, "alpha")
	id := strings.Repeat("ab", 32)
	x.user.Role = "operator"
	if w := x.do("DELETE", "/api/hosts/"+autoHostA+"/images/"+id+"?force=1", ""); w.Code != http.StatusForbidden {
		t.Fatalf("operator force: %d %s", w.Code, w.Body)
	}
	x.user.Role = "admin"
	if w := x.do("DELETE", "/api/hosts/"+autoHostA+"/images/"+id+"?force=1", ""); w.Code >= 300 {
		t.Fatalf("admin force: %d %s", w.Code, w.Body)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if strings.Join(h.ops, ",") != "op "+wire.OpImageRemove+" "+wire.ImageRemoveForce {
		t.Fatalf("ops: %v", h.ops)
	}
}

func TestRollbackNeedsANewEnoughAgent(t *testing.T) {
	x, h, stack := rollbackRig(t)
	x.user.Role = "operator"
	w := x.do("POST", "/api/stacks/"+stack.ID+"/pull", "")
	waitJob(t, x, w.Body.Bytes())
	h.a.Handle(wire.AgentMessage{Hello: &wire.Hello{OwnProject: "tainerhub", Stacks: &wire.StackSupport{Dir: "/stacks"}}}, nil)
	if w := x.do("POST", "/api/stacks/"+stack.ID+"/rollback", ""); w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "can't roll back") {
		t.Fatalf("old agent: %d %s", w.Code, w.Body)
	}
	if x.db.stacks[stack.ID].RolledBackTo != nil || len(h.rollbacks) != 0 {
		t.Fatal("an agent that can't roll back was sent a rollback")
	}
}
