package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/prezmc/tainerhub/internal/registry"
	"github.com/prezmc/tainerhub/internal/store"
	"github.com/prezmc/tainerhub/internal/wire"
)

// Moving a stack to another host:
//
//  1. Check (no downtime): both hosts can copy data, the target has the
//     room, ports and names free, and pulls the stack's images now so that
//     downtime is only the copy.
//  2. Stop the stack on the source (containers kept, for a quick rollback).
//  3. Copy its volumes and stack folder through the hub.
//  4. Deploy on the target and wait until what ran before runs and is healthy.
//  5. Make the target the stack's host, then remove the containers on the
//     source. Its data stays there as a backup until an admin deletes it.
//
// Any failure from step 2 on removes what reached the target and starts the
// stack on the source again.

// moveEnv is what a move needs from the hosts. The registry implements it
// for real (regEnv); tests use a fake.
type moveEnv interface {
	host(id string) (name string, hello *wire.Hello, online bool)
	containers(hostID string) []wire.Container
	operate(ctx context.Context, hostID string, op wire.Operation) (json.RawMessage, error)
	stack(ctx context.Context, hostID string, c wire.StackCommand, out io.Writer) error
	copy(ctx context.Context, src, dst string, item wire.TransferItem, max int64, progress func(int64)) (registry.CopyResult, error)
	diskFree(hostID string) int64 // bytes free on the stacks disk; 0 if unknown
}

// moveItem is one thing a move copies.
type moveItem struct {
	Kind    string            `json:"kind"` // wire.TransferVolume or wire.TransferStackDir
	Name    string            `json:"name"`
	Size    int64             `json:"size"`              // bytes, -1 if unknown
	Partial bool              `json:"partial,omitempty"` // size is a lower bound
	labels  map[string]string // volume labels to recreate it with
}

// moveReport is the result of the checks, shown before the move starts.
type moveReport struct {
	From, To     string     `json:"-"`
	FromName     string     `json:"fromName"`
	ToName       string     `json:"toName"`
	Blockers     []string   `json:"blockers"`  // reasons the move can't start
	Warnings     []string   `json:"warnings"`  // things to know; some need confirming
	Confirm      []string   `json:"confirm"`   // paths outside the stack folder that won't move
	Copy         []moveItem `json:"copy"`      // what will be copied
	Recreated    []string   `json:"recreated"` // volumes compose recreates (data stays where it is)
	TotalBytes   int64      `json:"totalBytes"`
	SizeUnknown  bool       `json:"sizeUnknown,omitempty"`
	DiskFree     int64      `json:"diskFree,omitempty"`
	Images       []string   `json:"images"`
	wasRunning   []string   // container names running on the source
	targetVolume map[string]bool
}

// checkMove inspects both hosts and lists what blocks the move, what it
// would copy, and what to be aware of.
func checkMove(ctx context.Context, env moveEnv, st store.Stack, to string, stacks []store.Stack) moveReport {
	rep := moveReport{From: st.HostID, To: to, Blockers: []string{}, Warnings: []string{}, Confirm: []string{}, Copy: []moveItem{}, Recreated: []string{}, Images: []string{}}
	block := func(f string, a ...any) { rep.Blockers = append(rep.Blockers, fmt.Sprintf(f, a...)) }
	warn := func(f string, a ...any) { rep.Warnings = append(rep.Warnings, fmt.Sprintf(f, a...)) }

	fromName, fromHello, fromOn := env.host(st.HostID)
	toName, toHello, toOn := env.host(to)
	rep.FromName, rep.ToName = fromName, toName
	if to == st.HostID {
		block("The stack is already on %s.", fromName)
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
		if !h.hello.Transfer {
			block("The agent on %s is too old to copy data. Update it to the latest tainerhub-agent image.", h.name)
		}
		if h.hello.Stacks == nil || h.hello.Stacks.Error != "" {
			block("Stacks aren't set up on %s.", h.name)
		}
	}
	if len(rep.Blockers) > 0 {
		return rep
	}
	if fromHello.Arch != "" && toHello.Arch != "" && fromHello.Arch != toHello.Arch {
		warn("%s is %s and %s is %s: every image must support both. TainerHub pulls them on %s before stopping anything, so a missing one stops the move early.", fromName, fromHello.Arch, toName, toHello.Arch, toName)
	}

	// Names: the target can't already have this stack.
	for _, o := range stacks {
		if o.HostID == to && o.Name == st.Name {
			block("%s already has a stack called %s.", toName, st.Name)
		}
	}
	for _, c := range env.containers(to) {
		if c.Project == st.Name {
			block("A compose project called %s is already running on %s.", st.Name, toName)
			break
		}
	}

	src, err := inspectOn(ctx, env, st.HostID, st.Name)
	if err != nil {
		block("Couldn't inspect the stack on %s: %v", fromName, err)
		return rep
	}
	if !src.Deployed {
		block("The stack isn't deployed on %s. Deploy it first.", fromName)
		return rep
	}
	dst, err := inspectOn(ctx, env, to, st.Name)
	if err != nil {
		block("Couldn't check %s: %v", toName, err)
		return rep
	}
	if dst.Deployed || dst.DirFiles > 0 {
		block("%s already has a folder for %s with files in it (%s). Remove it first.", toName, st.Name, dst.Dir)
	}

	// Services.
	images := map[string]bool{}
	for _, svc := range src.Services {
		if svc.Build {
			block("Service %s builds its image, which TainerHub stacks don't support.", svc.Name)
		}
		if svc.Image != "" {
			images[svc.Image] = true
		}
		if svc.HostNetwork {
			warn("Service %s uses the host's network, so it gets %s's addresses and ports.", svc.Name, toName)
		}
		if svc.Devices {
			warn("Service %s uses devices of the host (a USB stick or GPU, say). %s needs the same devices.", svc.Name, toName)
		}
	}
	for img := range images {
		rep.Images = append(rep.Images, img)
	}
	sort.Strings(rep.Images)

	// Ports.
	taken := map[string]bool{}
	for _, c := range env.containers(to) {
		for _, p := range c.Ports {
			if pub, _, ok := strings.Cut(p, "->"); ok {
				_, proto, _ := strings.Cut(p, "/")
				taken[pub+"/"+proto] = true
			}
		}
	}
	for _, p := range src.Ports {
		if taken[p] {
			block("Port %s is already in use on %s.", p, toName)
		}
	}

	// Volumes on the target.
	rep.targetVolume = map[string]bool{}
	if raw, err := env.operate(ctx, to, wire.Operation{Kind: wire.OpVolumesList}); err == nil {
		var vols []wire.Volume
		if json.Unmarshal(raw, &vols) == nil {
			for _, v := range vols {
				rep.targetVolume[v.Name] = true
			}
		}
	} else {
		block("Couldn't list the volumes on %s: %v", toName, err)
	}

	// What to copy.
	for _, v := range src.Volumes {
		switch {
		case v.External:
			if rep.targetVolume[v.Name] {
				warn("Volume %s is external: it isn't copied, and %s's own %s is used.", v.Name, toName, v.Name)
			} else {
				block("Volume %s is external (made outside the stack) and %s has no volume by that name. Create it there first.", v.Name, toName)
			}
		case v.Options || (v.Driver != "" && v.Driver != "local"):
			rep.Recreated = append(rep.Recreated, v.Name)
		case !v.Exists:
			// Declared but never created: nothing to copy.
		default:
			if rep.targetVolume[v.Name] {
				block("%s already has a volume called %s. Remove it first (it may be left from an earlier move).", toName, v.Name)
			}
			rep.Copy = append(rep.Copy, moveItem{Kind: wire.TransferVolume, Name: v.Name, Size: v.Size, labels: v.Labels})
		}
	}
	if src.DirFiles > 0 {
		rep.Copy = append(rep.Copy, moveItem{Kind: wire.TransferStackDir, Name: st.Name, Size: src.DirSize, Partial: src.DirSizePartial})
	}
	seen := map[string]bool{}
	for _, b := range src.Binds {
		if !b.InStackDir && !seen[b.Source] {
			seen[b.Source] = true
			rep.Confirm = append(rep.Confirm, b.Source)
		}
	}
	if len(rep.Confirm) > 0 {
		warn("Some services use folders outside the stack's folder. They aren't copied: %s needs its own copy at the same paths.", toName)
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

	for _, c := range env.containers(st.HostID) {
		if c.Project == st.Name && c.State == "running" {
			rep.wasRunning = append(rep.wasRunning, c.Name)
		}
	}
	sort.Strings(rep.wasRunning)
	return rep
}

func inspectOn(ctx context.Context, env moveEnv, hostID, name string) (wire.StackInspect, error) {
	raw, err := env.operate(ctx, hostID, wire.Operation{Kind: wire.OpStackInspect, Target: name})
	if err != nil {
		return wire.StackInspect{}, err
	}
	var out wire.StackInspect
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("unreadable answer: %w", err)
	}
	return out, nil
}

// moveRecorder saves a move's progress; the store implements it for real.
type moveRecorder interface {
	leftovers(ctx context.Context, left *store.MoveLeftovers) error
	commit(ctx context.Context, hash string, bytes int64, left *store.MoveLeftovers) error
	fail(ctx context.Context, errText string, bytes int64, left *store.MoveLeftovers) error
}

// mover runs one move, writing what it does to out.
type mover struct {
	env    moveEnv
	rec    moveRecorder
	out    io.Writer
	st     store.Stack
	rep    moveReport
	health time.Duration // how long to wait for the target to be healthy
	stable time.Duration // how long it must stay healthy
	poll   time.Duration

	bytes  int64
	copied []moveItem // reached the target so far
}

func (m *mover) say(f string, a ...any) { fmt.Fprintf(m.out, f+"\n", a...) }

// run carries out the move. The returned error says why it didn't happen.
func (m *mover) run(ctx context.Context) (err error) {
	from, to := m.rep.From, m.rep.To
	fromName, toName := m.rep.FromName, m.rep.ToName

	// 1. Images on the target, while the stack still runs.
	for _, img := range m.rep.Images {
		m.say("Pulling %s on %s…", img, toName)
		if _, err := m.env.operate(ctx, to, wire.Operation{Kind: wire.OpImagePull, Arg: img}); err != nil {
			return m.failEarly(ctx, fmt.Errorf("pull %s on %s: %w", img, toName, err))
		}
	}

	// From here on the stack is down until it runs again somewhere.
	m.say("Stopping %s on %s…", m.st.Name, fromName)
	if err := m.env.stack(ctx, from, wire.StackCommand{Action: wire.StackStop, Name: m.st.Name}, m.out); err != nil {
		// compose may have stopped some containers: start them again.
		return m.rollback(ctx, fmt.Errorf("stop on %s: %w", fromName, err))
	}

	// 2. Copy.
	_ = m.rec.leftovers(ctx, m.targetLeftovers(m.rep.Copy)) // if the hub stops now, say where to look
	for _, it := range m.rep.Copy {
		label := "volume " + it.Name
		if it.Kind == wire.TransferStackDir {
			label = "the stack folder"
		}
		m.say("Copying %s (%s)…", label, sizeText(it))
		var last time.Time
		res, err := m.env.copy(ctx, from, to, wire.TransferItem{Kind: it.Kind, Name: it.Name, Labels: it.labels}, copyLimit(it), func(n int64) {
			if time.Since(last) >= 5*time.Second {
				last = time.Now()
				m.say("  %s copied", humanBytes(n))
			}
		})
		if err != nil {
			m.copied = append(m.copied, it) // the receiver may have left part of it
			return m.rollback(ctx, fmt.Errorf("copy %s: %w", label, err))
		}
		m.copied = append(m.copied, it)
		m.bytes += res.Bytes
		m.say("  done: %s, %d files", humanBytes(res.Bytes), res.Files)
	}

	// 3. Deploy on the target and wait for it.
	m.say("Deploying on %s…", toName)
	if err := m.env.stack(ctx, to, wire.StackCommand{Action: wire.StackDeploy, Name: m.st.Name, Compose: m.st.Compose, Env: m.st.Env}, m.out); err != nil {
		return m.rollback(ctx, fmt.Errorf("deploy on %s: %w", toName, err))
	}
	if err := m.waitHealthy(ctx); err != nil {
		return m.rollback(ctx, err)
	}

	// 4. The target is now the stack's host.
	left := &store.MoveLeftovers{HostID: from}
	for _, it := range m.copied {
		if it.Kind == wire.TransferVolume {
			left.Volumes = append(left.Volumes, it.Name)
		} else {
			left.Dir = it.Name
		}
	}
	if err := m.rec.commit(ctx, stackHash(m.st.Compose, m.st.Env), m.bytes, left); err != nil {
		return m.rollback(ctx, fmt.Errorf("save the move: %w", err))
	}
	m.say("%s now runs on %s.", m.st.Name, toName)
	m.say("Removing the old containers on %s (its data stays there as a backup)…", fromName)
	if err := m.env.stack(ctx, from, wire.StackCommand{Action: wire.StackRemove, Name: m.st.Name}, m.out); err != nil {
		m.say("Couldn't remove them: %v. Remove them on %s by hand; the stack runs on %s either way.", err, fromName, toName)
	}
	return nil
}

// failEarly records a move that stopped before anything changed.
func (m *mover) failEarly(ctx context.Context, cause error) error {
	m.say("Nothing was changed: the stack still runs on %s.", m.rep.FromName)
	m.record(ctx, cause, nil)
	return cause
}

// rollback undoes a move that got past stopping the source: removes what
// reached the target and starts the stack on the source again.
func (m *mover) rollback(ctx context.Context, cause error) error {
	// Undo even if the caller's context ended (say the request that started
	// the move is gone): leaving the stack down is worse.
	ctx = context.WithoutCancel(ctx)
	m.say("")
	m.say("The move failed: %v", cause)
	m.say("Undoing it…")
	to, toName := m.rep.To, m.rep.ToName
	var left store.MoveLeftovers
	left.HostID = to
	if err := m.env.stack(ctx, to, wire.StackCommand{Action: wire.StackRemove, Name: m.st.Name}, m.out); err != nil {
		m.say("Couldn't remove the stack on %s: %v", toName, err)
	}
	for _, it := range m.copied {
		var err error
		if it.Kind == wire.TransferVolume {
			_, err = m.env.operate(ctx, to, wire.Operation{Kind: wire.OpVolumeRemove, Target: it.Name})
			if err != nil {
				left.Volumes = append(left.Volumes, it.Name)
			}
		} else {
			_, err = m.env.operate(ctx, to, wire.Operation{Kind: wire.OpStackDirRemove, Target: it.Name})
			if err != nil {
				left.Dir = it.Name
			}
		}
		if err != nil {
			m.say("Couldn't remove the copy of %s on %s: %v", it.Name, toName, err)
		}
	}
	m.say("Starting %s on %s again…", m.st.Name, m.rep.FromName)
	if err := m.env.stack(ctx, m.rep.From, wire.StackCommand{Action: wire.StackStart, Name: m.st.Name}, m.out); err != nil {
		m.say("COULDN'T START IT AGAIN: %v. Start it from its page.", err)
		cause = fmt.Errorf("%w; then starting it again on %s failed: %v", cause, m.rep.FromName, err)
	} else {
		m.say("It runs on %s again.", m.rep.FromName)
	}
	var l *store.MoveLeftovers
	if !left.Empty() {
		l = &left
	}
	m.record(ctx, cause, l)
	return cause
}

func (m *mover) record(ctx context.Context, cause error, left *store.MoveLeftovers) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := m.rec.fail(ctx, cause.Error(), m.bytes, left); err != nil {
		m.say("(Couldn't save the outcome: %v)", err)
	}
}

func (m *mover) targetLeftovers(items []moveItem) *store.MoveLeftovers {
	l := &store.MoveLeftovers{HostID: m.rep.To}
	for _, it := range items {
		if it.Kind == wire.TransferVolume {
			l.Volumes = append(l.Volumes, it.Name)
		} else {
			l.Dir = it.Name
		}
	}
	return l
}

// waitHealthy waits until every container that ran on the source runs on
// the target, none is unhealthy, restarting or still starting its health
// check, and that holds for m.stable.
func (m *mover) waitHealthy(ctx context.Context) error {
	m.say("Waiting for it to be healthy on %s…", m.rep.ToName)
	deadline := time.Now().Add(m.health)
	var okSince time.Time
	var why string
	for {
		why = m.notReady()
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

// notReady says what isn't ready yet on the target, or "".
func (m *mover) notReady() string {
	byName := map[string]wire.Container{}
	for _, c := range m.env.containers(m.rep.To) {
		if c.Project == m.st.Name {
			byName[c.Name] = c
		}
	}
	want := m.rep.wasRunning
	if len(want) == 0 { // nothing ran before: any container is enough
		if len(byName) == 0 {
			return "no containers yet"
		}
		return ""
	}
	for _, n := range want {
		c, ok := byName[n]
		switch {
		case !ok:
			return n + " doesn't exist"
		case c.State != "running":
			return n + " is " + c.State
		case strings.Contains(c.Status, "(unhealthy)"):
			return n + " is unhealthy"
		case strings.Contains(c.Status, "(health: starting)"):
			return n + " is still starting"
		}
	}
	return ""
}

// copyLimit is the most a copy may send: a margin over the measured size,
// since the measurement and the archive format differ a little.
func copyLimit(it moveItem) int64 {
	if it.Size < 0 || it.Partial {
		return wire.MaxTransferBytes
	}
	return min(it.Size+it.Size/4+64<<20, wire.MaxTransferBytes)
}

func sizeText(it moveItem) string {
	switch {
	case it.Size < 0:
		return "size unknown"
	case it.Partial:
		return "at least " + humanBytes(it.Size)
	}
	return humanBytes(it.Size)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
