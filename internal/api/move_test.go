package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/registry"
	"github.com/prezmc/tainerhub/internal/store"
	"github.com/prezmc/tainerhub/internal/wire"
)

const (
	hostA = "aaaaaaaa-0000-0000-0000-000000000001"
	hostB = "bbbbbbbb-0000-0000-0000-000000000002"
)

// fakeEnv is two hosts that do what a move asks, recording each call.
type fakeEnv struct {
	mu      sync.Mutex
	hellos  map[string]*wire.Hello
	ctrs    map[string][]wire.Container
	inspect map[string]wire.StackInspect // by host
	volumes map[string][]wire.Volume     // by host
	disk    map[string]int64
	calls   []string
	failOn  map[string]error // call -> error
	healthy bool             // the target's containers come up healthy after deploy
}

func newFakeEnv() *fakeEnv {
	ok := func(arch string) *wire.Hello {
		return &wire.Hello{Transfer: true, Arch: arch, Stacks: &wire.StackSupport{Dir: "/opt/tainerhub/stacks"}}
	}
	return &fakeEnv{
		hellos: map[string]*wire.Hello{hostA: ok("amd64"), hostB: ok("amd64")},
		ctrs: map[string][]wire.Container{
			hostA: {
				{Name: "app-web-1", Project: "app", State: "running", Status: "Up 2 days (healthy)", Ports: []string{"8080->80/tcp"}},
				{Name: "app-db-1", Project: "app", State: "running", Status: "Up 2 days"},
			},
			hostB: {{Name: "other", State: "running", Ports: []string{"9000->9000/tcp"}}},
		},
		inspect: map[string]wire.StackInspect{
			hostA: {
				Deployed: true, Dir: "/opt/tainerhub/stacks/app", DirFiles: 1, DirSize: 1000,
				Services: []wire.StackService{{Name: "web", Image: "nginx:1"}, {Name: "db", Image: "postgres:17"}},
				Volumes: []wire.StackVolume{
					{Key: "db", Name: "app_db", Exists: true, Size: 5 << 20, Labels: map[string]string{"com.docker.compose.project": "app"}},
					{Key: "nfs", Name: "app_nfs", Options: true, Exists: true, Size: 0},
					{Key: "unused", Name: "app_unused", Exists: false, Size: -1},
				},
				Binds: []wire.StackBind{{Service: "web", Source: "/opt/tainerhub/stacks/app/html", Target: "/usr/share/nginx/html", InStackDir: true}},
				Ports: []string{"8080/tcp"},
			},
			hostB: {Dir: "/opt/tainerhub/stacks/app"},
		},
		volumes: map[string][]wire.Volume{hostB: {{Name: "unrelated"}}},
		disk:    map[string]int64{hostB: 100 << 30},
		failOn:  map[string]error{},
		healthy: true,
	}
}

func (f *fakeEnv) call(c string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	return f.failOn[c]
}

func (f *fakeEnv) host(id string) (string, *wire.Hello, bool) {
	name := map[string]string{hostA: "alpha", hostB: "beta"}[id]
	h := f.hellos[id]
	return name, h, h != nil
}

func (f *fakeEnv) containers(id string) []wire.Container {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ctrs[id]
}

func (f *fakeEnv) operate(_ context.Context, id string, op wire.Operation) (json.RawMessage, error) {
	switch op.Kind {
	case wire.OpStackInspect:
		return json.Marshal(f.inspect[id])
	case wire.OpVolumesList:
		return json.Marshal(f.volumes[id])
	}
	who := map[string]string{hostA: "A", hostB: "B"}[id]
	arg := op.Target + op.Arg
	if err := f.call(fmt.Sprintf("%s %s %s", who, op.Kind, arg)); err != nil {
		return nil, err
	}
	return json.RawMessage(`{}`), nil
}

func (f *fakeEnv) stack(_ context.Context, id string, c wire.StackCommand, out io.Writer) error {
	who := map[string]string{hostA: "A", hostB: "B"}[id]
	fmt.Fprintf(out, "compose %s\n", c.Action)
	if err := f.call(fmt.Sprintf("%s stack %s", who, c.Action)); err != nil {
		return err
	}
	if id == hostB && c.Action == wire.StackDeploy {
		status := "Up 1 second"
		if !f.healthy {
			status = "Up 1 second (unhealthy)"
		}
		f.mu.Lock()
		f.ctrs[hostB] = append(f.ctrs[hostB],
			wire.Container{Name: "app-web-1", Project: "app", State: "running", Status: status},
			wire.Container{Name: "app-db-1", Project: "app", State: "running", Status: "Up 1 second"})
		f.mu.Unlock()
	}
	return nil
}

func (f *fakeEnv) copy(_ context.Context, src, dst string, item wire.TransferItem, max int64, progress func(int64)) (registry.CopyResult, error) {
	if err := f.call("copy " + item.Kind + " " + item.Name); err != nil {
		return registry.CopyResult{}, err
	}
	progress(1000)
	return registry.CopyResult{Bytes: 1000, Files: 3}, nil
}

func (f *fakeEnv) diskFree(id string) int64 { return f.disk[id] }

type fakeRec struct {
	committed bool
	failed    string
	left      *store.MoveLeftovers
	during    *store.MoveLeftovers
}

func (r *fakeRec) leftovers(_ context.Context, l *store.MoveLeftovers) error {
	r.during = l
	return nil
}
func (r *fakeRec) commit(_ context.Context, _ string, _ int64, l *store.MoveLeftovers) error {
	r.committed, r.left = true, l
	return nil
}
func (r *fakeRec) fail(_ context.Context, e string, _ int64, l *store.MoveLeftovers) error {
	r.failed, r.left = e, l
	return nil
}

var appStack = store.Stack{ID: "s1", HostID: hostA, Name: "app", Compose: "services: {}", Env: ""}

func runMove(t *testing.T, env *fakeEnv) (*fakeRec, string, error) {
	t.Helper()
	rep := checkMove(context.Background(), env, appStack, hostB, nil)
	if len(rep.Blockers) > 0 {
		t.Fatalf("blocked: %v", rep.Blockers)
	}
	rec := &fakeRec{}
	var out strings.Builder
	m := &mover{env: env, rec: rec, out: &out, st: appStack, rep: rep, health: 200 * time.Millisecond, stable: 0, poll: 10 * time.Millisecond}
	err := m.run(context.Background())
	return rec, out.String(), err
}

func TestCheckMove(t *testing.T) {
	env := newFakeEnv()
	rep := checkMove(context.Background(), env, appStack, hostB, nil)
	if len(rep.Blockers) != 0 {
		t.Fatalf("blockers: %v", rep.Blockers)
	}
	if len(rep.Copy) != 2 || rep.Copy[0].Name != "app_db" || rep.Copy[1].Kind != wire.TransferStackDir {
		t.Errorf("copy: %+v", rep.Copy)
	}
	if rep.Copy[0].labels["com.docker.compose.project"] != "app" {
		t.Error("volume labels not carried")
	}
	if strings.Join(rep.Recreated, ",") != "app_nfs" {
		t.Errorf("recreated: %v", rep.Recreated)
	}
	if strings.Join(rep.Images, ",") != "nginx:1,postgres:17" {
		t.Errorf("images: %v", rep.Images)
	}
	if rep.TotalBytes != 5<<20+1000 {
		t.Errorf("total %d", rep.TotalBytes)
	}
	if strings.Join(rep.wasRunning, ",") != "app-db-1,app-web-1" {
		t.Errorf("was running: %v", rep.wasRunning)
	}
}

func TestCheckMoveBlockers(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*fakeEnv)
		want  string
	}{
		{"port taken", func(f *fakeEnv) { f.ctrs[hostB][0].Ports = []string{"8080->80/tcp"} }, "Port 8080/tcp"},
		{"volume exists", func(f *fakeEnv) { f.volumes[hostB] = []wire.Volume{{Name: "app_db"}} }, "already has a volume called app_db"},
		{"folder not empty", func(f *fakeEnv) { f.inspect[hostB] = wire.StackInspect{Dir: "/x", DirFiles: 2} }, "folder for app"},
		{"project running", func(f *fakeEnv) {
			f.ctrs[hostB] = append(f.ctrs[hostB], wire.Container{Name: "app-x-1", Project: "app"})
		}, "already running on beta"},
		{"offline", func(f *fakeEnv) { f.hellos[hostB] = nil }, "beta is offline"},
		{"old agent", func(f *fakeEnv) { f.hellos[hostB].Transfer = false }, "too old"},
		{"no room", func(f *fakeEnv) { f.disk[hostB] = 1 << 20 }, "doesn't have the room"},
		{"not deployed", func(f *fakeEnv) { i := f.inspect[hostA]; i.Deployed = false; f.inspect[hostA] = i }, "isn't deployed"},
		{"build", func(f *fakeEnv) {
			i := f.inspect[hostA]
			i.Services = append(i.Services, wire.StackService{Name: "x", Build: true})
			f.inspect[hostA] = i
		}, "builds its image"},
		{"external missing", func(f *fakeEnv) {
			i := f.inspect[hostA]
			i.Volumes = append(i.Volumes, wire.StackVolume{Name: "shared", External: true, Exists: true})
			f.inspect[hostA] = i
		}, "Volume shared is external"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := newFakeEnv()
			c.setup(env)
			rep := checkMove(context.Background(), env, appStack, hostB, nil)
			if !strings.Contains(strings.Join(rep.Blockers, " | "), c.want) {
				t.Errorf("blockers %q, want one with %q", rep.Blockers, c.want)
			}
		})
	}
	// A stack of the same name already managed on the target.
	rep := checkMove(context.Background(), newFakeEnv(), appStack, hostB, []store.Stack{{HostID: hostB, Name: "app"}})
	if len(rep.Blockers) == 0 {
		t.Error("same-name stack on the target not blocked")
	}
	// Folders outside the stack folder need confirming but don't block.
	env := newFakeEnv()
	i := env.inspect[hostA]
	i.Binds = append(i.Binds, wire.StackBind{Service: "web", Source: "/srv/media", Target: "/media"})
	env.inspect[hostA] = i
	rep = checkMove(context.Background(), env, appStack, hostB, nil)
	if len(rep.Blockers) != 0 || strings.Join(rep.Confirm, ",") != "/srv/media" {
		t.Errorf("blockers %v, confirm %v", rep.Blockers, rep.Confirm)
	}
}

func TestMoveSucceeds(t *testing.T) {
	env := newFakeEnv()
	rec, out, err := runMove(t, env)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := []string{
		"B image.pull nginx:1", "B image.pull postgres:17",
		"A stack stop",
		"copy volume app_db", "copy stackdir app",
		"B stack deploy",
		"A stack remove",
	}
	if strings.Join(env.calls, "|") != strings.Join(want, "|") {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(env.calls, "\n"), strings.Join(want, "\n"))
	}
	if !rec.committed || rec.failed != "" {
		t.Fatalf("committed %v, failed %q", rec.committed, rec.failed)
	}
	if rec.left.HostID != hostA || strings.Join(rec.left.Volumes, ",") != "app_db" || rec.left.Dir != "app" {
		t.Errorf("leftovers on the source: %+v", rec.left)
	}
	if rec.during == nil || rec.during.HostID != hostB {
		t.Errorf("leftovers during the copy should point at the target: %+v", rec.during)
	}
}

func TestMoveFailsBeforeStopping(t *testing.T) {
	env := newFakeEnv()
	env.failOn["B image.pull postgres:17"] = errors.New("no matching manifest for linux/arm64")
	rec, out, err := runMove(t, env)
	if err == nil || !strings.Contains(err.Error(), "no matching manifest") {
		t.Fatalf("err = %v", err)
	}
	for _, c := range env.calls {
		if strings.Contains(c, "stack") || strings.HasPrefix(c, "copy") {
			t.Errorf("%q ran after a failed check", c)
		}
	}
	if rec.failed == "" || rec.committed || !strings.Contains(out, "still runs on alpha") {
		t.Errorf("rec %+v\n%s", rec, out)
	}
}

func TestMoveRollsBack(t *testing.T) {
	t.Run("copy fails", func(t *testing.T) {
		env := newFakeEnv()
		env.failOn["copy stackdir app"] = errors.New("the transfer stalled")
		rec, _, err := runMove(t, env)
		if err == nil || rec.committed {
			t.Fatalf("err %v committed %v", err, rec.committed)
		}
		after := strings.Join(env.calls[3:], "|") // after pulls and stop
		want := "copy volume app_db|copy stackdir app|B stack remove|B volume.remove app_db|B stack.remove_dir app|A stack start"
		if after != want {
			t.Errorf("calls %s\nwant %s", after, want)
		}
		if rec.left != nil {
			t.Errorf("leftovers after a clean rollback: %+v", rec.left)
		}
	})
	t.Run("unhealthy", func(t *testing.T) {
		env := newFakeEnv()
		env.healthy = false
		rec, _, err := runMove(t, env)
		if err == nil || !strings.Contains(err.Error(), "app-web-1 is unhealthy") {
			t.Fatalf("err = %v", err)
		}
		if rec.committed || env.calls[len(env.calls)-1] != "A stack start" {
			t.Errorf("committed %v, last call %s", rec.committed, env.calls[len(env.calls)-1])
		}
	})
	t.Run("cleanup fails", func(t *testing.T) {
		env := newFakeEnv()
		env.healthy = false
		env.failOn["B stack.remove_dir app"] = errors.New("permission denied")
		rec, out, _ := runMove(t, env)
		if rec.left == nil || rec.left.HostID != hostB || rec.left.Dir != "app" || len(rec.left.Volumes) != 0 {
			t.Errorf("leftovers %+v", rec.left)
		}
		if !strings.Contains(out, "Couldn't remove the copy of app") {
			t.Errorf("output:\n%s", out)
		}
	})
	t.Run("restart fails", func(t *testing.T) {
		env := newFakeEnv()
		env.failOn["copy volume app_db"] = errors.New("boom")
		env.failOn["A stack start"] = errors.New("no such image")
		_, out, err := runMove(t, env)
		if err == nil || !strings.Contains(err.Error(), "starting it again on alpha failed") || !strings.Contains(out, "COULDN'T START IT AGAIN") {
			t.Errorf("err %v\n%s", err, out)
		}
	})
}

func TestCopyLimitAndSizes(t *testing.T) {
	if copyLimit(moveItem{Size: -1}) != wire.MaxTransferBytes || copyLimit(moveItem{Size: 10, Partial: true}) != wire.MaxTransferBytes {
		t.Error("unknown size should allow the maximum")
	}
	if got := copyLimit(moveItem{Size: 1 << 30}); got != 1<<30+1<<28+64<<20 {
		t.Errorf("limit %d", got)
	}
	if humanBytes(1536) != "1.5 KB" || humanBytes(5<<30) != "5.0 GB" || humanBytes(12) != "12 B" {
		t.Errorf("%s %s %s", humanBytes(1536), humanBytes(5<<30), humanBytes(12))
	}
}
