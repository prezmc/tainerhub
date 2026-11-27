package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/store"
	"github.com/prezmc/tainerhub/internal/wire"
)

const anonVol = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// fakeCtrEnv is fakeEnv with a standalone container "web" on host A.
type fakeCtrEnv struct {
	*fakeEnv
	exp     wire.ContainerExport
	nets    map[string][]wire.Network
	created *wire.ContainerCreate
}

func newFakeCtrEnv() *fakeCtrEnv {
	f := &fakeCtrEnv{fakeEnv: newFakeEnv()}
	for _, h := range f.hellos {
		h.ContainerMove = true
	}
	f.ctrs[hostA] = append(f.ctrs[hostA], wire.Container{ID: "web-id", Name: "web", State: "running", Status: "Up 3 days", Ports: []string{"8081->80/tcp"}})
	f.exp = wire.ContainerExport{
		ID: "web-id", Name: "web", Image: "nginx:1", Running: true,
		Spec: json.RawMessage(`{"Image":"nginx:1"}`),
		Volumes: []wire.ExportVolume{
			{Name: "web_data", Destination: "/data", Driver: "local", Size: 2 << 20},
			{Name: anonVol, Destination: "/cache", Driver: "local", Anonymous: true, Size: 1000, Labels: map[string]string{"com.docker.volume.anonymous": ""}},
		},
		Binds:    []string{"/srv/web/conf"},
		Networks: []wire.NetworkSpec{{Name: "bridge", Driver: "bridge", Builtin: true}, {Name: "web-net", Driver: "bridge"}},
		Ports:    []string{"8081/tcp"},
	}
	f.nets = map[string][]wire.Network{hostB: {{Name: "bridge", Driver: "bridge", Builtin: true}}}
	return f
}

func (f *fakeCtrEnv) operate(ctx context.Context, id string, op wire.Operation) (json.RawMessage, error) {
	who := map[string]string{hostA: "A", hostB: "B"}[id]
	switch op.Kind {
	case wire.OpContainerExport:
		if id != hostA || (op.Target != "web-id" && op.Target != "web") {
			return nil, errors.New("no such container")
		}
		return json.Marshal(f.exp)
	case wire.OpNetworksList:
		return json.Marshal(f.nets[id])
	case wire.OpContainerCreate:
		if err := wire.ValidContainerCreate(op.Create); err != nil {
			return nil, err
		}
		if err := f.call(fmt.Sprintf("%s container.create %s start=%v", who, op.Create.Name, op.Create.Start)); err != nil {
			return nil, err
		}
		f.created = op.Create
		state, status := "created", "Created"
		if op.Create.Start {
			state, status = "running", "Up 1 second"
			if !f.healthy {
				status = "Up 1 second (unhealthy)"
			}
		}
		f.mu.Lock()
		f.ctrs[id] = append(f.ctrs[id], wire.Container{ID: "new-id", Name: op.Create.Name, State: state, Status: status})
		f.mu.Unlock()
		res := wire.ContainerCreateResult{ID: "new-id"}
		for _, n := range op.Create.Networks {
			res.Networks = append(res.Networks, n.Name)
		}
		return json.Marshal(res)
	}
	return f.fakeEnv.operate(ctx, id, op)
}

func (f *fakeCtrEnv) action(_ context.Context, id, container, action string) error {
	who := map[string]string{hostA: "A", hostB: "B"}[id]
	if err := f.call(fmt.Sprintf("%s %s %s", who, action, container)); err != nil {
		return err
	}
	if action == wire.ActionRemove {
		f.mu.Lock()
		defer f.mu.Unlock()
		keep := f.ctrs[id][:0]
		for _, c := range f.ctrs[id] {
			if c.ID != container && c.Name != container {
				keep = append(keep, c)
			}
		}
		f.ctrs[id] = keep
	}
	return nil
}

type fakeCtrRec struct {
	committed bool
	failed    string
	left      *store.MoveLeftovers
	during    *store.MoveLeftovers
}

func (r *fakeCtrRec) leftovers(_ context.Context, l *store.MoveLeftovers) error {
	r.during = l
	return nil
}
func (r *fakeCtrRec) done(_ context.Context, _ int64, l *store.MoveLeftovers) error {
	r.committed, r.left = true, l
	return nil
}
func (r *fakeCtrRec) fail(_ context.Context, e string, _ int64, l *store.MoveLeftovers) error {
	r.failed, r.left = e, l
	return nil
}

func runCtrMove(t *testing.T, env *fakeCtrEnv) (*fakeCtrRec, string, error) {
	t.Helper()
	rep := checkContainerMove(context.Background(), env, hostA, "web", hostB)
	if len(rep.Blockers) > 0 {
		t.Fatalf("blocked: %v", rep.Blockers)
	}
	rec := &fakeCtrRec{}
	var out strings.Builder
	m := &ctrMover{env: env, rec: rec, out: &out, rep: rep, health: 200 * time.Millisecond, stable: 0, poll: 10 * time.Millisecond}
	err := m.run(context.Background())
	return rec, out.String(), err
}

func TestCheckContainerMove(t *testing.T) {
	env := newFakeCtrEnv()
	rep := checkContainerMove(context.Background(), env, hostA, "web-id", hostB)
	if len(rep.Blockers) != 0 {
		t.Fatalf("blockers: %v", rep.Blockers)
	}
	if rep.Container != "web" || !rep.Running {
		t.Errorf("container %q running %v", rep.Container, rep.Running)
	}
	if len(rep.Copy) != 2 || rep.Copy[0].Name != "web_data" || rep.Copy[1].Name != anonVol {
		t.Errorf("copy: %+v", rep.Copy)
	}
	if _, ok := rep.Copy[1].labels["com.docker.volume.anonymous"]; !ok {
		t.Error("anonymous volume's labels not carried")
	}
	if strings.Join(rep.Networks, ",") != "web-net" || len(rep.createNets) != 1 {
		t.Errorf("networks to create: %v", rep.Networks)
	}
	if strings.Join(rep.Confirm, ",") != "/srv/web/conf" {
		t.Errorf("confirm: %v", rep.Confirm)
	}
	if strings.Join(rep.Images, ",") != "nginx:1" || rep.TotalBytes != 2<<20+1000 {
		t.Errorf("images %v total %d", rep.Images, rep.TotalBytes)
	}
	// An existing network of the same name is used, not created.
	env.nets[hostB] = append(env.nets[hostB], wire.Network{Name: "web-net", Driver: "bridge"})
	rep = checkContainerMove(context.Background(), env, hostA, "web", hostB)
	if len(rep.Networks) != 0 || len(rep.Blockers) != 0 {
		t.Errorf("networks %v blockers %v", rep.Networks, rep.Blockers)
	}
}

func TestCheckContainerMoveBlockers(t *testing.T) {
	cases := []struct {
		name  string
		ref   string
		to    string
		setup func(*fakeCtrEnv)
		want  string
	}{
		{"compose", "app-web-1", hostB, func(f *fakeCtrEnv) {}, "Move it with its stack"},
		{"missing", "nope", hostB, func(f *fakeCtrEnv) {}, "no container nope"},
		{"same host", "web", hostA, func(f *fakeCtrEnv) {}, "already on alpha"},
		{"offline", "web", hostB, func(f *fakeCtrEnv) { f.hellos[hostB] = nil }, "beta is offline"},
		{"old agent", "web", hostB, func(f *fakeCtrEnv) { f.hellos[hostB].ContainerMove = false }, "too old to move containers"},
		{"own", "tainerhub-agent", hostB, func(f *fakeCtrEnv) {
			f.ctrs[hostA] = append(f.ctrs[hostA], wire.Container{ID: "x", Name: "tainerhub-agent"})
		}, "own containers"},
		{"name taken", "web", hostB, func(f *fakeCtrEnv) {
			f.ctrs[hostB] = append(f.ctrs[hostB], wire.Container{Name: "web"})
		}, "already has a container called web"},
		{"port taken", "web", hostB, func(f *fakeCtrEnv) { f.ctrs[hostB][0].Ports = []string{"8081->80/tcp"} }, "Port 8081/tcp"},
		{"volume exists", "web", hostB, func(f *fakeCtrEnv) { f.volumes[hostB] = []wire.Volume{{Name: "web_data"}} }, "already has a volume called web_data"},
		{"overlay network", "web", hostB, func(f *fakeCtrEnv) { f.exp.Networks[1].Driver = "overlay" }, "Network web-net uses the overlay driver"},
		{"nfs volume", "web", hostB, func(f *fakeCtrEnv) { f.exp.Volumes[0].Options = true }, "keeps its data elsewhere"},
		{"export problem", "web", hostB, func(f *fakeCtrEnv) {
			f.exp.Problems = []string{"It mounts the volumes of db (--volumes-from)."}
		}, "--volumes-from"},
		{"exported project", "web", hostB, func(f *fakeCtrEnv) { f.exp.Project = "x" }, "compose project x"},
		{"no room", "web", hostB, func(f *fakeCtrEnv) { f.disk[hostB] = 1 << 20 }, "doesn't have the room"},
		{"network used by others", "web", hostB, func(f *fakeCtrEnv) {
			f.ctrs[hostA] = append(f.ctrs[hostA], wire.Container{ID: "y", Name: "torrent", NetworkOf: "web"})
		}, "torrent uses this container's network"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := newFakeCtrEnv()
			c.setup(env)
			rep := checkContainerMove(context.Background(), env, hostA, c.ref, c.to)
			if !strings.Contains(strings.Join(rep.Blockers, " | "), c.want) {
				t.Errorf("blockers %q, want one with %q", rep.Blockers, c.want)
			}
		})
	}
	// An NFS volume the target already has is used there, not copied.
	env := newFakeCtrEnv()
	env.exp.Volumes[0].Options = true
	env.volumes[hostB] = []wire.Volume{{Name: "web_data"}}
	rep := checkContainerMove(context.Background(), env, hostA, "web", hostB)
	if len(rep.Blockers) != 0 || len(rep.Copy) != 1 {
		t.Errorf("blockers %v copy %+v", rep.Blockers, rep.Copy)
	}
}

func TestContainerMoveSucceeds(t *testing.T) {
	env := newFakeCtrEnv()
	env.exp.Volumes[0].UsedBy = []string{"backup"} // shared: stays on the source, not a leftover
	rec, out, err := runCtrMove(t, env)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := []string{
		"B image.pull nginx:1",
		"A stop web-id",
		"copy volume web_data", "copy volume " + anonVol,
		"B container.create web start=true",
		"A remove web-id",
	}
	if strings.Join(env.calls, "|") != strings.Join(want, "|") {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(env.calls, "\n"), strings.Join(want, "\n"))
	}
	if len(env.created.Networks) != 1 || env.created.Networks[0].Name != "web-net" || string(env.created.Spec) != `{"Image":"nginx:1"}` {
		t.Errorf("created %+v", env.created)
	}
	if !rec.committed || rec.failed != "" {
		t.Fatalf("done %v failed %q", rec.committed, rec.failed)
	}
	if rec.left == nil || rec.left.HostID != hostA || strings.Join(rec.left.Volumes, ",") != anonVol || rec.left.Container != "" {
		t.Errorf("leftovers on the source: %+v", rec.left)
	}
	if rec.during == nil || rec.during.HostID != hostB || len(rec.during.Volumes) != 2 {
		t.Errorf("leftovers during the copy: %+v", rec.during)
	}
	if !strings.Contains(out, "web now runs on beta") {
		t.Errorf("output:\n%s", out)
	}
}

func TestContainerMoveStopped(t *testing.T) {
	env := newFakeCtrEnv()
	env.exp.Running = false
	env.exp.Volumes = nil
	env.failOn["A remove web-id"] = errors.New("busy")
	rec, out, err := runCtrMove(t, env)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := "B image.pull nginx:1|B container.create web start=false|A remove web-id"
	if strings.Join(env.calls, "|") != want {
		t.Errorf("calls %s\nwant %s", strings.Join(env.calls, "|"), want)
	}
	// The old container couldn't be removed: it's left to delete.
	if !rec.committed || rec.left == nil || rec.left.Container != "web" {
		t.Errorf("rec %+v left %+v", rec, rec.left)
	}
}

func TestContainerMoveFailsBeforeStopping(t *testing.T) {
	env := newFakeCtrEnv()
	env.failOn["B image.pull nginx:1"] = errors.New("no matching manifest for linux/arm64")
	rec, out, err := runCtrMove(t, env)
	if err == nil || !strings.Contains(err.Error(), "no matching manifest") {
		t.Fatalf("err = %v", err)
	}
	if len(env.calls) != 1 || rec.failed == "" || rec.committed || !strings.Contains(out, "still runs on alpha") {
		t.Errorf("calls %v rec %+v\n%s", env.calls, rec, out)
	}
}

func TestContainerMoveRollsBack(t *testing.T) {
	t.Run("copy fails", func(t *testing.T) {
		env := newFakeCtrEnv()
		env.failOn["copy volume "+anonVol] = errors.New("the transfer stalled")
		rec, _, err := runCtrMove(t, env)
		if err == nil || rec.committed {
			t.Fatalf("err %v done %v", err, rec.committed)
		}
		after := strings.Join(env.calls[2:], "|") // after the pull and stop
		want := "copy volume web_data|copy volume " + anonVol + "|B volume.remove web_data|B volume.remove " + anonVol + "|A start web-id"
		if after != want {
			t.Errorf("calls %s\nwant %s", after, want)
		}
		if rec.left != nil {
			t.Errorf("leftovers after a clean rollback: %+v", rec.left)
		}
	})
	t.Run("unhealthy", func(t *testing.T) {
		env := newFakeCtrEnv()
		env.healthy = false
		rec, _, err := runCtrMove(t, env)
		if err == nil || !strings.Contains(err.Error(), "web is unhealthy") {
			t.Fatalf("err = %v", err)
		}
		after := strings.Join(env.calls[5:], "|") // after creating it
		want := "B remove web|B volume.remove web_data|B volume.remove " + anonVol + "|B network.remove web-net|A start web-id"
		if after != want || rec.committed {
			t.Errorf("calls %s\nwant %s", after, want)
		}
	})
	t.Run("create fails", func(t *testing.T) {
		env := newFakeCtrEnv()
		env.failOn["B container.create web start=true"] = errors.New("invalid mount config")
		_, _, err := runCtrMove(t, env)
		if err == nil || !strings.Contains(err.Error(), "invalid mount config") {
			t.Fatalf("err = %v", err)
		}
		for _, c := range env.calls {
			if c == "B remove web" || strings.HasPrefix(c, "B network.remove") {
				t.Errorf("%q: the agent already undid a failed create", c)
			}
		}
		if env.calls[len(env.calls)-1] != "A start web-id" {
			t.Errorf("last call %s", env.calls[len(env.calls)-1])
		}
	})
	t.Run("cleanup fails", func(t *testing.T) {
		env := newFakeCtrEnv()
		env.healthy = false
		env.failOn["B volume.remove web_data"] = errors.New("volume is in use")
		env.failOn["B remove web"] = errors.New("no response")
		rec, out, _ := runCtrMove(t, env)
		if rec.left == nil || rec.left.HostID != hostB || rec.left.Container != "web" || strings.Join(rec.left.Volumes, ",") != "web_data" {
			t.Errorf("leftovers %+v", rec.left)
		}
		if !strings.Contains(out, "Couldn't remove the copy of volume web_data") {
			t.Errorf("output:\n%s", out)
		}
	})
	t.Run("receiver already cleaned up", func(t *testing.T) {
		env := newFakeCtrEnv()
		env.failOn["copy volume web_data"] = errors.New("stalled")
		env.failOn["B volume.remove web_data"] = errors.New("docker: get web_data: no such volume")
		rec, out, _ := runCtrMove(t, env)
		if rec.left != nil || strings.Contains(out, "Couldn't remove") {
			t.Errorf("leftovers %+v\n%s", rec.left, out)
		}
	})
	t.Run("restart fails", func(t *testing.T) {
		env := newFakeCtrEnv()
		env.failOn["copy volume web_data"] = errors.New("boom")
		env.failOn["A start web-id"] = errors.New("port already allocated")
		_, out, err := runCtrMove(t, env)
		if err == nil || !strings.Contains(err.Error(), "starting it again on alpha failed") || !strings.Contains(out, "COULDN'T START IT AGAIN") {
			t.Errorf("err %v\n%s", err, out)
		}
	})
	t.Run("stopped container stays stopped", func(t *testing.T) {
		env := newFakeCtrEnv()
		env.exp.Running = false
		env.failOn["copy volume web_data"] = errors.New("boom")
		_, out, err := runCtrMove(t, env)
		if err == nil || strings.Contains(strings.Join(env.calls, "|"), "A start") || !strings.Contains(out, "(stopped)") {
			t.Errorf("err %v calls %v\n%s", err, env.calls, out)
		}
	})
}

func TestRankHostsForContainers(t *testing.T) {
	hosts := []hostInfo{
		{ID: "a", Name: "a", Online: true, Hello: &wire.Hello{}},
		{ID: "b", Name: "b", Online: true, Hello: &wire.Hello{Stacks: &wire.StackSupport{Error: "no compose"}}},
	}
	for _, p := range rankHostsFor(hosts, footprint{}, "", false) {
		if !p.Eligible {
			t.Errorf("%s not eligible for a container: %s", p.HostName, p.Reason)
		}
	}
	for _, p := range rankHosts(hosts, footprint{}, "") {
		if p.Eligible {
			t.Errorf("%s eligible for a stack without stacks set up", p.HostName)
		}
	}
}
