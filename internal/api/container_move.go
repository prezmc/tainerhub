package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/prezmc/tainerhub/internal/store"
	"github.com/prezmc/tainerhub/internal/wire"
)

// Moving a standalone container (one started with `docker run`, not part
// of a compose project) to another host. It follows the stack move:
//
//  1. Check (no downtime): both agents can export, create and copy; the
//     target has the name, ports and room free; pull its image there now.
//  2. Stop the container on the source (kept, for a quick rollback).
//  3. Copy its volumes through the hub, anonymous ones too: they are created
//     on the target under the same name and the new container mounts them
//     by name, so its data comes along.
//  4. Create it on the target from the source's settings (creating missing
//     bridge networks), start it if it ran, and wait until it's healthy.
//  5. Record the move, then remove the container on the source. Its volumes
//     stay there as a backup until an admin deletes them.
//
// Any failure from step 2 on removes what reached the target and starts the
// container on the source again.

// ctrMoveEnv is moveEnv plus container actions.
type ctrMoveEnv interface {
	moveEnv
	action(ctx context.Context, hostID, container, action string) error
}

// ctrMoveReport is the result of the checks, shown before the move starts.
// It has the stack report's shape so the UI can show both alike.
type ctrMoveReport struct {
	From, To    string     `json:"-"`
	FromName    string     `json:"fromName"`
	ToName      string     `json:"toName"`
	Container   string     `json:"container"`
	Running     bool       `json:"running"`
	Blockers    []string   `json:"blockers"`
	Warnings    []string   `json:"warnings"`
	Confirm     []string   `json:"confirm"` // host folders it mounts, which aren't copied
	Copy        []moveItem `json:"copy"`
	Recreated   []string   `json:"recreated"` // always empty for a container
	Networks    []string   `json:"networks"`  // networks created on the target
	TotalBytes  int64      `json:"totalBytes"`
	SizeUnknown bool       `json:"sizeUnknown,omitempty"`
	DiskFree    int64      `json:"diskFree,omitempty"`
	Images      []string   `json:"images"`

	exp        wire.ContainerExport
	createNets []wire.NetworkSpec
	shared     map[string]bool // volumes other containers on the source use too
}

// findContainer looks a container up by ID or name in a host's snapshot.
func findContainer(cs []wire.Container, ref string) (wire.Container, bool) {
	for _, c := range cs {
		if c.ID == ref || c.Name == ref {
			return c, true
		}
	}
	return wire.Container{}, false
}

// publishedPorts lists the host ports a host's containers publish, e.g. "8080/tcp".
func publishedPorts(cs []wire.Container) map[string]bool {
	taken := map[string]bool{}
	for _, c := range cs {
		for _, p := range c.Ports {
			if pub, _, ok := strings.Cut(p, "->"); ok {
				_, proto, _ := strings.Cut(p, "/")
				taken[pub+"/"+proto] = true
			}
		}
	}
	return taken
}

// checkContainerMove inspects both hosts and lists what blocks moving
// container ref from host from to host to, what it would copy, and what
// to be aware of.
func checkContainerMove(ctx context.Context, env ctrMoveEnv, from, ref, to string) ctrMoveReport {
	rep := ctrMoveReport{From: from, To: to, Container: ref, Blockers: []string{}, Warnings: []string{}, Confirm: []string{},
		Copy: []moveItem{}, Recreated: []string{}, Networks: []string{}, Images: []string{}, shared: map[string]bool{}}
	block := func(f string, a ...any) { rep.Blockers = append(rep.Blockers, fmt.Sprintf(f, a...)) }
	warn := func(f string, a ...any) { rep.Warnings = append(rep.Warnings, fmt.Sprintf(f, a...)) }

	fromName, fromHello, fromOn := env.host(from)
	toName, toHello, toOn := env.host(to)
	rep.FromName, rep.ToName = fromName, toName
	if to == from {
		block("The container is already on %s.", fromName)
		return rep
	}
	if !fromOn || !toOn {
		if !fromOn {
			block("%s is offline.", fromName)
		}
		if !toOn {
			block("%s is offline.", toName)
		}
		return rep
	}
	for _, h := range []struct {
		name  string
		hello *wire.Hello
	}{{fromName, fromHello}, {toName, toHello}} {
		if !h.hello.Transfer || !h.hello.ContainerMove {
			block("The agent on %s is too old to move containers. Update it to the latest tainerhub-agent image.", h.name)
		}
	}
	if len(rep.Blockers) > 0 {
		return rep
	}

	c, ok := findContainer(env.containers(from), ref)
	if !ok {
		block("There's no container %s on %s.", ref, fromName)
		return rep
	}
	rep.Container = c.Name
	if c.Project != "" {
		block("%s belongs to the compose project %s. Move it with its stack instead (Stacks → %s → Move to another host).", c.Name, c.Project, c.Project)
		return rep
	}
	if ownContainer(c, "") {
		block("TainerHub's own containers can't be moved.")
		return rep
	}

	raw, err := env.operate(ctx, from, wire.Operation{Kind: wire.OpContainerExport, Target: c.ID})
	if err != nil {
		block("Couldn't read the container's settings on %s: %v", fromName, err)
		return rep
	}
	var exp wire.ContainerExport
	if err := json.Unmarshal(raw, &exp); err != nil {
		block("Couldn't read the container's settings on %s: unreadable answer", fromName)
		return rep
	}
	rep.exp = exp
	rep.Running = exp.Running
	if exp.Project != "" { // the snapshot may predate a relabel; trust the inspection
		block("%s belongs to the compose project %s. Move it with its stack instead.", c.Name, exp.Project)
	}
	rep.Blockers = append(rep.Blockers, exp.Problems...)
	if exp.Image != "" {
		rep.Images = []string{exp.Image}
	}
	if fromHello.Arch != "" && toHello.Arch != "" && fromHello.Arch != toHello.Arch {
		warn("%s is %s and %s is %s: the image must support both. TainerHub pulls it on %s before stopping anything, so a missing one stops the move early.", fromName, fromHello.Arch, toName, toHello.Arch, toName)
	}
	if !strings.Contains(exp.Image, "@") {
		warn("%s pulls %s now. If that tag points to a newer version than %s runs, the moved container runs the newer one.", toName, exp.Image, fromName)
	}

	// The name and ports on the target.
	toCtrs := env.containers(to)
	if _, taken := findContainer(toCtrs, c.Name); taken {
		block("%s already has a container called %s.", toName, c.Name)
	}
	taken := publishedPorts(toCtrs)
	for _, p := range exp.Ports {
		if taken[p] {
			block("Port %s is already in use on %s.", p, toName)
		}
	}

	// Networks.
	if exp.NetworkMode == "host" {
		warn("It uses the host's network, so it gets %s's addresses and ports.", toName)
	}
	targetNets := map[string]wire.Network{}
	if raw, err := env.operate(ctx, to, wire.Operation{Kind: wire.OpNetworksList}); err == nil {
		var nets []wire.Network
		if json.Unmarshal(raw, &nets) == nil {
			for _, n := range nets {
				targetNets[n.Name] = n
			}
		}
	} else {
		block("Couldn't list the networks on %s: %v", toName, err)
	}
	for _, n := range exp.Networks {
		if n.Builtin {
			continue
		}
		if t, ok := targetNets[n.Name]; ok {
			if t.Driver != n.Driver {
				warn("Network %s exists on %s but uses the %s driver (here %s). The container joins %s's network.", n.Name, toName, t.Driver, n.Driver, toName)
			}
			continue
		}
		if n.Driver != "bridge" {
			block("Network %s uses the %s driver and doesn't exist on %s. Create it there first.", n.Name, n.Driver, toName)
			continue
		}
		rep.createNets = append(rep.createNets, n)
		rep.Networks = append(rep.Networks, n.Name)
	}

	// Volumes.
	targetVols := map[string]bool{}
	if raw, err := env.operate(ctx, to, wire.Operation{Kind: wire.OpVolumesList}); err == nil {
		var vols []wire.Volume
		if json.Unmarshal(raw, &vols) == nil {
			for _, v := range vols {
				targetVols[v.Name] = true
			}
		}
	} else {
		block("Couldn't list the volumes on %s: %v", toName, err)
	}
	for _, v := range exp.Volumes {
		label := "Volume " + v.Name
		if v.Anonymous {
			label = "Its unnamed volume at " + v.Destination
		}
		if v.Options || (v.Driver != "" && v.Driver != "local") {
			if targetVols[v.Name] {
				warn("%s keeps its data elsewhere (%s driver or options): it isn't copied, and %s's own %s is used.", label, v.Driver, toName, v.Name)
			} else {
				block("%s keeps its data elsewhere (NFS, CIFS or another driver) and %s has no volume %s. Create it there first.", label, toName, v.Name)
			}
			continue
		}
		if targetVols[v.Name] {
			block("%s already has a volume called %s. Remove it first (it may be left from an earlier move).", toName, v.Name)
		}
		if len(v.UsedBy) > 0 {
			rep.shared[v.Name] = true
			warn("Volume %s is also used by %s on %s. They keep using %s's copy; the moved container gets its own.", v.Name, strings.Join(v.UsedBy, ", "), fromName, fromName)
		}
		rep.Copy = append(rep.Copy, moveItem{Kind: wire.TransferVolume, Name: v.Name, Size: v.Size, labels: v.Labels})
	}
	rep.Confirm = append(rep.Confirm, exp.Binds...)
	if len(rep.Confirm) > 0 {
		warn("It uses folders of the host. They aren't copied: %s needs its own copy at the same paths.", toName)
	}
	if exp.Devices {
		warn("It uses devices of the host (a USB stick or GPU, say). %s needs the same devices.", toName)
	}
	if exp.Privileged {
		warn("It runs privileged, with full access to %s.", toName)
	}

	// Room on the target.
	for _, it := range rep.Copy {
		if it.Size < 0 || it.Partial {
			rep.SizeUnknown = true
		}
		if it.Size > 0 {
			rep.TotalBytes += it.Size
		}
	}
	rep.DiskFree = env.diskFree(to)
	if rep.DiskFree > 0 && rep.TotalBytes > 0 && rep.TotalBytes+rep.TotalBytes/10 > rep.DiskFree {
		block("%s doesn't have the room: the data is %s and it has %s free.", toName, humanBytes(rep.TotalBytes), humanBytes(rep.DiskFree))
	}
	return rep
}

// ctrMoveRecorder saves a container move's progress; the store implements it.
type ctrMoveRecorder interface {
	leftovers(ctx context.Context, left *store.MoveLeftovers) error
	done(ctx context.Context, bytes int64, left *store.MoveLeftovers) error
	fail(ctx context.Context, errText string, bytes int64, left *store.MoveLeftovers) error
}

// ctrMover runs one container move, writing what it does to out.
type ctrMover struct {
	env    ctrMoveEnv
	rec    ctrMoveRecorder
	out    io.Writer
	rep    ctrMoveReport
	health time.Duration
	stable time.Duration
	poll   time.Duration

	bytes   int64
	copied  []moveItem
	created *wire.ContainerCreateResult // what the target made
}

func (m *ctrMover) say(f string, a ...any) { fmt.Fprintf(m.out, f+"\n", a...) }

func (m *ctrMover) run(ctx context.Context) error {
	r := m.rep
	name := r.Container

	// 1. The image on the target, while the container still runs.
	for _, img := range r.Images {
		m.say("Pulling %s on %s…", img, r.ToName)
		if _, err := m.env.operate(ctx, r.To, wire.Operation{Kind: wire.OpImagePull, Arg: img}); err != nil {
			m.say("Nothing was changed: %s still runs on %s.", name, r.FromName)
			err = fmt.Errorf("pull %s on %s: %w", img, r.ToName, err)
			m.record(ctx, err, nil)
			return err
		}
	}

	// From here on the container is down until it runs again somewhere.
	if r.Running {
		m.say("Stopping %s on %s…", name, r.FromName)
		if err := m.env.action(ctx, r.From, r.exp.ID, wire.ActionStop); err != nil {
			return m.rollback(ctx, fmt.Errorf("stop on %s: %w", r.FromName, err))
		}
	}

	// 2. Copy.
	_ = m.rec.leftovers(ctx, m.volumesOn(r.To, r.Copy))
	for _, it := range r.Copy {
		label := "volume " + it.Name
		m.say("Copying %s (%s)…", label, sizeText(it))
		var last time.Time
		res, err := m.env.copy(ctx, r.From, r.To, wire.TransferItem{Kind: it.Kind, Name: it.Name, Labels: it.labels}, copyLimit(it), func(n int64) {
			if time.Since(last) >= 5*time.Second {
				last = time.Now()
				m.say("  %s copied", humanBytes(n))
			}
		})
		m.copied = append(m.copied, it) // even on failure: the receiver may have left part of it
		if err != nil {
			return m.rollback(ctx, fmt.Errorf("copy %s: %w", label, err))
		}
		m.bytes += res.Bytes
		m.say("  done: %s, %d files", humanBytes(res.Bytes), res.Files)
	}

	// 3. Create on the target and wait for it.
	if len(r.Networks) > 0 {
		m.say("Creating network %s on %s…", strings.Join(r.Networks, ", "), r.ToName)
	}
	m.say("Creating %s on %s…", name, r.ToName)
	raw, err := m.env.operate(ctx, r.To, wire.Operation{Kind: wire.OpContainerCreate, Create: &wire.ContainerCreate{
		Name: name, Spec: r.exp.Spec, Networks: r.createNets, Start: r.Running,
	}})
	if err != nil {
		return m.rollback(ctx, fmt.Errorf("create on %s: %w", r.ToName, err))
	}
	var created wire.ContainerCreateResult
	_ = json.Unmarshal(raw, &created)
	m.created = &created
	if r.Running {
		if err := m.waitHealthy(ctx); err != nil {
			return m.rollback(ctx, err)
		}
	}

	// 4. Done: the old copy's data stays on the source.
	left := &store.MoveLeftovers{HostID: r.From}
	for _, it := range m.copied {
		if r.shared[it.Name] {
			continue // other containers there still use it
		}
		left.Volumes = append(left.Volumes, it.Name)
	}
	m.say("Removing the old container on %s (its volumes stay there as a backup)…", r.FromName)
	if err := m.env.action(ctx, r.From, r.exp.ID, wire.ActionRemove); err != nil {
		m.say("Couldn't remove it: %v. Delete the old copy here, or remove it on %s by hand.", err, r.FromName)
		left.Container = name
	}
	if left.Empty() {
		left = nil
	}
	if err := m.rec.done(ctx, m.bytes, left); err != nil {
		m.say("(Couldn't save the outcome: %v)", err)
	}
	m.say("%s now runs on %s.", name, r.ToName)
	return nil
}

// rollback undoes a move that got past stopping the source: removes what
// reached the target and starts the container on the source again.
func (m *ctrMover) rollback(ctx context.Context, cause error) error {
	ctx = context.WithoutCancel(ctx) // leaving it down is worse than finishing late
	r := m.rep
	m.say("")
	m.say("The move failed: %v", cause)
	m.say("Undoing it…")
	left := store.MoveLeftovers{HostID: r.To}
	_, onTarget := findContainer(m.env.containers(r.To), r.Container)
	if m.created != nil || onTarget {
		if err := m.env.action(ctx, r.To, r.Container, wire.ActionRemove); err != nil {
			m.say("Couldn't remove the container on %s: %v", r.ToName, err)
			left.Container = r.Container
		}
	}
	for _, it := range m.copied {
		_, err := m.env.operate(ctx, r.To, wire.Operation{Kind: wire.OpVolumeRemove, Target: it.Name})
		if err != nil && strings.Contains(strings.ToLower(err.Error()), "no such volume") {
			err = nil // the receiver already removed its partial copy
		}
		if err != nil {
			m.say("Couldn't remove the copy of volume %s on %s: %v", it.Name, r.ToName, err)
			left.Volumes = append(left.Volumes, it.Name)
		}
	}
	if m.created != nil {
		for _, n := range m.created.Networks {
			if _, err := m.env.operate(ctx, r.To, wire.Operation{Kind: wire.OpNetworkRemove, Target: n}); err != nil {
				m.say("Couldn't remove network %s on %s: %v", n, r.ToName, err)
			}
		}
	}
	if r.Running {
		m.say("Starting %s on %s again…", r.Container, r.FromName)
		if err := m.env.action(ctx, r.From, r.exp.ID, wire.ActionStart); err != nil {
			m.say("COULDN'T START IT AGAIN: %v. Start it from the Containers page.", err)
			cause = fmt.Errorf("%w; then starting it again on %s failed: %v", cause, r.FromName, err)
		} else {
			m.say("It runs on %s again.", r.FromName)
		}
	} else {
		m.say("It's back as it was on %s (stopped).", r.FromName)
	}
	var l *store.MoveLeftovers
	if !left.Empty() {
		l = &left
	}
	m.record(ctx, cause, l)
	return cause
}

func (m *ctrMover) record(ctx context.Context, cause error, left *store.MoveLeftovers) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := m.rec.fail(ctx, cause.Error(), m.bytes, left); err != nil {
		m.say("(Couldn't save the outcome: %v)", err)
	}
}

func (m *ctrMover) volumesOn(host string, items []moveItem) *store.MoveLeftovers {
	l := &store.MoveLeftovers{HostID: host}
	for _, it := range items {
		l.Volumes = append(l.Volumes, it.Name)
	}
	return l
}

// waitHealthy waits until the container runs on the target, isn't
// unhealthy, restarting or still starting its health check, and that holds
// for m.stable.
func (m *ctrMover) waitHealthy(ctx context.Context) error {
	m.say("Waiting for it to be healthy on %s…", m.rep.ToName)
	deadline := time.Now().Add(m.health)
	var okSince time.Time
	for {
		why := m.notReady()
		if why == "" {
			if okSince.IsZero() {
				okSince = time.Now()
			}
			if time.Since(okSince) >= m.stable {
				m.say("Healthy.")
				return nil
			}
		} else {
			okSince = time.Time{}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not healthy on %s after %s: %s", m.rep.ToName, m.health.Round(time.Second), why)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(m.poll):
		}
	}
}

func (m *ctrMover) notReady() string {
	n := m.rep.Container
	c, ok := findContainer(m.env.containers(m.rep.To), n)
	switch {
	case !ok:
		return n + " doesn't exist yet"
	case c.State != "running":
		return n + " is " + c.State
	case strings.Contains(c.Status, "(unhealthy)"):
		return n + " is unhealthy"
	case strings.Contains(c.Status, "(health: starting)"):
		return n + " is still starting"
	}
	return ""
}
