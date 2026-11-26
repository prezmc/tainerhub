// Package registry tracks connected agents on the hub: their latest
// container snapshot, and in-flight commands and log streams.
package registry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

var (
	ErrOffline     = errors.New("host is offline")
	ErrTimeout     = errors.New("agent did not respond in time")
	ErrTooManyLogs = errors.New("too many open log streams for this host; close some and retry")
	// ErrTooManyExecs and ErrTooManyStackOps also match ErrTooManyLogs
	// with errors.Is, so callers can treat every stream limit alike.
	ErrTooManyExecs    error = tooManyStreams("too many terminals open on this host; close some and retry")
	ErrTooManyStackOps error = tooManyStreams("too many stack operations running on this host; wait for one to finish and retry")
	// ErrExecOverflow ends a terminal whose output arrived faster than it
	// was read for longer than its buffer could absorb.
	ErrExecOverflow = errors.New("terminal output arrived faster than it could be shown; the session was closed")
)

type tooManyStreams string

func (e tooManyStreams) Error() string        { return string(e) }
func (e tooManyStreams) Is(target error) bool { return target == ErrTooManyLogs }

// Per-host caps on concurrent streams, to protect the agent. Each kind has
// its own, so many open log views can't block a deploy or a terminal.
const (
	MaxLogStreams   = 16
	MaxExecStreams  = 8
	MaxStackStreams = 8
)

// Stream buffer sizes, in chunks of at most 32 KiB. Log and stack output
// is dropped when its reader falls behind; a terminal missing bytes would be
// garbled, so it gets a much larger buffer and is closed if even that fills.
const (
	streamBuffer = 256
	execBuffer   = 2048
)

type streamKind uint8

const (
	streamLogs streamKind = iota
	streamExec
	streamStack
)

func (k streamKind) limit() (int, error) {
	switch k {
	case streamExec:
		return MaxExecStreams, ErrTooManyExecs
	case streamStack:
		return MaxStackStreams, ErrTooManyStackOps
	}
	return MaxLogStreams, ErrTooManyLogs
}

type Registry struct {
	mu     sync.RWMutex
	agents map[string]*Agent
	// OnEvent, if set, is called for every Docker event an agent reports.
	OnEvent func(hostID string, ev wire.DockerEvent)
	// AddAuth, if set, attaches the registry logins a command that pulls
	// images needs, just before it is sent (see api's addRegistryAuth). Set
	// it before agents connect.
	AddAuth func(ctx context.Context, a *Agent, cmd *wire.Command)

	chMu    sync.Mutex
	changed chan struct{}

	actMu    sync.Mutex
	activity []Activity // newest last, capped at MaxActivity

	watchMu  sync.Mutex
	watchers int // open fleet streams (browser tabs)

	dockerStatsMu sync.Mutex
	dockerStats   map[string]bool // host ID -> stats from Docker instead of cgroup files

	loadMu sync.Mutex
	load   map[string]*loadHistory // host ID -> recent host load; kept across reconnects

	xferMu sync.Mutex
	xfers  map[string]*Transfer // active data transfers, by ID

	checkMu sync.Mutex
	checks  map[string]*hostCheck // host ID -> image update checks; kept across reconnects
	now     func() time.Time      // tests set a fake clock; nil: time.Now
}

// SetStatsFromDocker records a host's stats source and tells its agent now
// if it's connected (or when it next connects).
func (r *Registry) SetStatsFromDocker(hostID string, on bool) {
	r.dockerStatsMu.Lock()
	if r.dockerStats == nil {
		r.dockerStats = map[string]bool{}
	}
	if on {
		r.dockerStats[hostID] = true
	} else {
		delete(r.dockerStats, hostID)
	}
	r.dockerStatsMu.Unlock()
	if a := r.Get(hostID); a != nil {
		a.tell(wire.HubMessage{StatsFromDocker: &on})
	}
}

// StatsFromDockerHosts lists the hosts told to ask Docker for stats, sorted.
func (r *Registry) StatsFromDockerHosts() []string {
	r.dockerStatsMu.Lock()
	defer r.dockerStatsMu.Unlock()
	ids := make([]string, 0, len(r.dockerStats))
	for id := range r.dockerStats {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// StatsFromDocker reports whether the host's agent was told to ask Docker for stats.
func (r *Registry) StatsFromDocker(hostID string) bool {
	r.dockerStatsMu.Lock()
	defer r.dockerStatsMu.Unlock()
	return r.dockerStats[hostID]
}

// Watch records an open fleet stream until release is called. Agents are told
// when the first one opens and the last one closes, so they only sample CPU
// and memory often while someone is looking.
func (r *Registry) Watch() (release func()) {
	r.watchMu.Lock()
	r.watchers++
	first := r.watchers == 1
	r.watchMu.Unlock()
	if first {
		r.broadcastWatching(true)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			r.watchMu.Lock()
			r.watchers--
			last := r.watchers == 0
			r.watchMu.Unlock()
			if last {
				r.broadcastWatching(false)
			}
		})
	}
}

func (r *Registry) watching() bool {
	r.watchMu.Lock()
	defer r.watchMu.Unlock()
	return r.watchers > 0
}

func (r *Registry) broadcastWatching(on bool) {
	for _, a := range r.All() {
		a.tellWatching(on)
	}
}

// tellWatching queues the watching state without blocking: a full queue
// means the agent is busy, and the next change or reconnect corrects it.
func (a *Agent) tellWatching(on bool) { a.tell(wire.HubMessage{Watching: &on}) }

func (a *Agent) tell(m wire.HubMessage) {
	select {
	case a.out <- m:
	default:
	}
}

// MaxActivity is how many recent fleet events the hub keeps in memory for
// the live activity feed (the audit log in Postgres is the durable record).
const MaxActivity = 200

// Activity is one entry in the live feed.
type Activity struct {
	At        time.Time `json:"at"`
	Kind      string    `json:"kind"` // "container", "host" or "user"
	Action    string    `json:"action"`
	HostID    string    `json:"hostId,omitempty"`
	Host      string    `json:"host,omitempty"`
	Container string    `json:"container,omitempty"`
	Actor     string    `json:"actor,omitempty"`
}

// AddActivity records an entry and wakes Changed() subscribers.
func (r *Registry) AddActivity(a Activity) {
	if a.At.IsZero() {
		a.At = time.Now()
	}
	r.actMu.Lock()
	r.activity = append(r.activity, a)
	if len(r.activity) > MaxActivity {
		r.activity = append([]Activity(nil), r.activity[len(r.activity)-MaxActivity:]...)
	}
	r.actMu.Unlock()
	r.notify()
}

// Activity returns up to n recent entries, newest first.
func (r *Registry) Activity(n int) []Activity {
	r.actMu.Lock()
	defer r.actMu.Unlock()
	out := make([]Activity, 0, n)
	for i := len(r.activity) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, r.activity[i])
	}
	return out
}

// feedActions are the Docker events worth showing in the activity feed.
var feedActions = map[string]bool{
	"start": true, "die": true, "stop": true, "restart": true, "kill": true, "oom": true,
	"pause": true, "unpause": true, "destroy": true,
	"health_status: unhealthy": true, "health_status: healthy": true,
}

func New() *Registry { return &Registry{agents: map[string]*Agent{}, changed: make(chan struct{})} }

// Changed returns a channel that is closed the next time any host connects,
// disconnects, or reports a different container list. Callers re-subscribe
// after each wake-up.
func (r *Registry) Changed() <-chan struct{} {
	r.chMu.Lock()
	defer r.chMu.Unlock()
	return r.changed
}

func (r *Registry) notify() {
	r.chMu.Lock()
	close(r.changed)
	r.changed = make(chan struct{})
	r.chMu.Unlock()
}

type Agent struct {
	HostID      string
	Name        string
	RemoteIP    string // address the agent connected from (its server's IP, seen from the hub)
	ConnectedAt time.Time

	out    chan wire.HubMessage
	done   chan struct{}
	cancel context.CancelFunc
	notify func()
	reg    *Registry

	mu         sync.Mutex
	lastSeen   time.Time
	hello      *wire.Hello
	containers []wire.Container
	snapshotAt time.Time
	snapSum    [32]byte // hash of the last snapshot's container list, if known
	pending    map[string]chan wire.CommandResult
	logs       map[string]chan wire.LogChunk // every open stream, by command ID
	kinds      map[string]streamKind         // what each stream in logs is
	open       [3]int                        // open streams per kind
}

// dropStream forgets stream id; callers hold a.mu.
func (a *Agent) dropStream(id string) {
	if _, ok := a.logs[id]; !ok {
		return
	}
	delete(a.logs, id)
	a.open[a.kinds[id]]--
	delete(a.kinds, id)
}

// Attach registers a newly connected agent, replacing (and disconnecting)
// any previous connection for the same host. The returned agent's Out()
// channel must be drained by the connection's writer until Done() closes.
func (r *Registry) Attach(hostID, name string, cancel context.CancelFunc) *Agent {
	return r.AttachFrom(hostID, name, "", cancel)
}

// AttachFrom is Attach, recording the address the agent connected from.
func (r *Registry) AttachFrom(hostID, name, remoteIP string, cancel context.CancelFunc) *Agent {
	a := &Agent{
		HostID:      hostID,
		Name:        name,
		RemoteIP:    remoteIP,
		ConnectedAt: time.Now(),
		lastSeen:    time.Now(),
		out:         make(chan wire.HubMessage, 64),
		done:        make(chan struct{}),
		cancel:      cancel,
		notify:      r.notify,
		reg:         r,
		pending:     map[string]chan wire.CommandResult{},
		logs:        map[string]chan wire.LogChunk{},
		kinds:       map[string]streamKind{},
	}
	r.mu.Lock()
	old := r.agents[hostID]
	r.agents[hostID] = a
	r.mu.Unlock()
	a.tellWatching(r.watching())
	fromDocker := r.StatsFromDocker(hostID)
	a.tell(wire.HubMessage{StatsFromDocker: &fromDocker})
	if old != nil {
		old.cancel()
	} else {
		r.AddActivity(Activity{Kind: "host", Action: "connected", HostID: hostID, Host: name})
	}
	r.notify()
	return a
}

// Detach removes the agent if it is still the current connection for its host.
func (r *Registry) Detach(a *Agent) {
	r.mu.Lock()
	removed := r.agents[a.HostID] == a
	if removed {
		delete(r.agents, a.HostID)
	}
	r.mu.Unlock()
	a.close()
	if removed {
		r.AddActivity(Activity{Kind: "host", Action: "disconnected", HostID: a.HostID, Host: a.Name})
	}
}

// Disconnect forcibly drops a host's connection (used on revocation,
// re-enrollment and certificate renewal), and ends any data transfer the
// host takes part in: those run over their own requests, which may be
// using the same superseded certificate.
func (r *Registry) Disconnect(hostID string) {
	r.mu.RLock()
	a := r.agents[hostID]
	r.mu.RUnlock()
	if a != nil {
		a.cancel()
	}
	r.abortTransfers(hostID, ErrOffline)
}

func (r *Registry) Get(hostID string) *Agent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.agents[hostID]
}

func (r *Registry) All() []*Agent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Agent, 0, len(r.agents))
	for _, a := range r.agents {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ---- agent side of the hub ----

func (a *Agent) Out() <-chan wire.HubMessage { return a.out }
func (a *Agent) Done() <-chan struct{}       { return a.done }

func (a *Agent) close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	select {
	case <-a.done:
		return
	default:
	}
	close(a.done)
	for id, ch := range a.pending {
		ch <- wire.CommandResult{CommandID: id, Error: ErrOffline.Error()}
		delete(a.pending, id)
	}
	for id, ch := range a.logs {
		deliverFinal(ch, wire.LogChunk{CommandID: id, EOF: true, Error: ErrOffline.Error()})
		close(ch)
		a.dropStream(id)
	}
}

// Handle processes one message received from the agent.
func (a *Agent) Handle(m wire.AgentMessage, onEvent func(string, wire.DockerEvent)) {
	changed := false
	a.mu.Lock()
	a.lastSeen = time.Now()
	switch {
	case m.Hello != nil:
		a.hello = m.Hello
		changed = true
	case m.Snapshot != nil:
		changed = !reflect.DeepEqual(a.containers, m.Snapshot.Containers) || m.Snapshot.Host != nil
		a.containers = m.Snapshot.Containers
		a.snapshotAt = time.Unix(m.Snapshot.At, 0)
		a.snapSum = [32]byte{}
	case m.Result != nil:
		if ch, ok := a.pending[m.Result.CommandID]; ok {
			ch <- *m.Result
			delete(a.pending, m.Result.CommandID)
		}
	case m.Log != nil:
		if ch, ok := a.logs[m.Log.CommandID]; ok {
			if m.Log.EOF {
				// The final chunk carries the outcome, so it must arrive.
				deliverFinal(ch, *m.Log)
				close(ch)
				a.dropStream(m.Log.CommandID)
			} else {
				select {
				case ch <- *m.Log:
				default:
					// Slow consumer: never stall the whole agent stream.
					// Logs just lose some lines; a terminal is closed
					// instead, since silently missing bytes would garble it.
					if a.kinds[m.Log.CommandID] == streamExec {
						id := m.Log.CommandID
						deliverFinal(ch, wire.LogChunk{CommandID: id, EOF: true, Error: ErrExecOverflow.Error()})
						close(ch)
						a.dropStream(id)
						a.tell(wire.HubMessage{Command: &wire.Command{ID: newID(), Cancel: id}})
					}
				}
			}
		}
	}
	a.mu.Unlock()
	if m.Snapshot != nil && a.reg != nil {
		a.reg.recordLoad(a.HostID, m.Snapshot.Host, time.Now())
	}
	if changed && a.notify != nil {
		a.notify()
	}
	if m.Event != nil {
		if a.reg != nil && feedActions[m.Event.Action] {
			a.reg.AddActivity(Activity{At: time.Unix(m.Event.Time, 0), Kind: "container", Action: m.Event.Action, HostID: a.HostID, Host: a.Name, Container: m.Event.Name})
		}
		if onEvent != nil {
			onEvent(a.HostID, *m.Event)
		}
	}
}

// HandleSnapshot stores a container snapshot whose list hashes to sum (a
// hash of its raw JSON). Comparing hashes tells whether the list changed far
// more cheaply than comparing up to 8 MiB of decoded containers field by
// field. A zero sum falls back to that comparison.
func (a *Agent) HandleSnapshot(s *wire.Snapshot, sum [32]byte) {
	a.mu.Lock()
	a.lastSeen = time.Now()
	var changed bool
	if sum != ([32]byte{}) && a.snapSum != ([32]byte{}) {
		changed = sum != a.snapSum
	} else {
		changed = !reflect.DeepEqual(a.containers, s.Containers)
	}
	a.containers = s.Containers
	a.snapshotAt = time.Unix(s.At, 0)
	a.snapSum = sum
	a.mu.Unlock()
	if a.reg != nil {
		a.reg.recordLoad(a.HostID, s.Host, time.Now())
	}
	if changed && a.notify != nil {
		a.notify()
	}
}

type Status struct {
	Hello      *wire.Hello
	Containers []wire.Container
	SnapshotAt time.Time
	LastSeen   time.Time
}

func (a *Agent) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	cs := make([]wire.Container, len(a.containers))
	copy(cs, a.containers)
	return Status{Hello: a.hello, Containers: cs, SnapshotAt: a.snapshotAt, LastSeen: a.lastSeen}
}

func (a *Agent) send(ctx context.Context, m wire.HubMessage) error {
	select {
	case a.out <- m:
		return nil
	case <-a.done:
		return ErrOffline
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Do sends a container action and waits for its result.
func (a *Agent) Do(ctx context.Context, act wire.ContainerAction) error {
	_, err := a.call(ctx, wire.Command{Action: &act}, 60*time.Second)
	return err
}

// Operate runs a resource operation on the agent and returns its JSON result.
func (a *Agent) Operate(ctx context.Context, op wire.Operation) (_ json.RawMessage, err error) {
	if err := wire.ValidOperation(op); err != nil {
		return nil, err
	}
	timeout := 70 * time.Second
	switch op.Kind {
	case wire.OpImagePull, wire.OpImagesPrune, wire.OpContainerRecreate, wire.OpImagesCheckUpdates, wire.OpDockerPlan, wire.OpDockerUpgrade, wire.OpHostOSStatus, wire.OpHostOSUpgrade, wire.OpStackDirRemove:
		timeout = 16 * time.Minute // the agent gives up after 15
	}
	cmd := wire.Command{Op: &op}
	a.addAuth(ctx, &cmd)
	if op.Kind == wire.OpImagesCheckUpdates && a.reg != nil {
		// Recorded for the scheduler (no overlapping checks, next one a
		// full period later) and for "checked X ago" in the UI.
		a.reg.beginImageCheck(a.HostID)
		defer func() { a.reg.endImageCheck(a.HostID, err == nil) }()
	}
	res, err := a.call(ctx, cmd, timeout)
	return res.Data, err
}

// addAuth lets the registry's AddAuth hook attach registry logins.
func (a *Agent) addAuth(ctx context.Context, cmd *wire.Command) {
	if a.reg != nil && a.reg.AddAuth != nil {
		a.reg.AddAuth(ctx, a, cmd)
	}
}

func (a *Agent) call(ctx context.Context, cmd wire.Command, timeout time.Duration) (wire.CommandResult, error) {
	id := newID()
	cmd.ID = id
	ch := make(chan wire.CommandResult, 1)
	a.mu.Lock()
	select {
	case <-a.done:
		a.mu.Unlock()
		return wire.CommandResult{}, ErrOffline
	default:
	}
	a.pending[id] = ch
	a.mu.Unlock()

	if err := a.send(ctx, wire.HubMessage{Command: &cmd}); err != nil {
		a.forget(id)
		return wire.CommandResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case res := <-ch:
		if !res.OK {
			if res.Error == "" {
				res.Error = "operation failed"
			}
			return res, errors.New(res.Error)
		}
		return res, nil
	case <-ctx.Done():
		a.forget(id)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return wire.CommandResult{}, ErrTimeout
		}
		return wire.CommandResult{}, ctx.Err()
	}
}

func (a *Agent) forget(id string) {
	a.mu.Lock()
	delete(a.pending, id)
	a.mu.Unlock()
}

// Logs starts a log stream. The returned channel closes at EOF. Call stop
// when the consumer goes away; it tells the agent to stop following.
func (a *Agent) Logs(ctx context.Context, req wire.LogsRequest) (<-chan wire.LogChunk, func(), error) {
	_, ch, stop, err := a.stream(ctx, wire.Command{Logs: &req}, streamLogs)
	return ch, stop, err
}

// Exec opens a terminal in a container. Output arrives on the channel (the
// final chunk, EOF, carries any error); send keystrokes with ExecInput and
// size changes with ExecResize using the returned session ID; stop closes it.
func (a *Agent) Exec(ctx context.Context, e wire.ExecStart) (string, <-chan wire.LogChunk, func(), error) {
	if err := wire.ValidExecStart(e); err != nil {
		return "", nil, nil, err
	}
	return a.stream(ctx, wire.Command{Exec: &e}, streamExec)
}

func (a *Agent) ExecInput(ctx context.Context, session string, data []byte) error {
	return a.send(ctx, wire.HubMessage{Command: &wire.Command{ID: newID(), ExecInput: &wire.ExecInput{Session: session, Data: data}}})
}

func (a *Agent) ExecResize(ctx context.Context, session string, cols, rows int) error {
	r := wire.ExecResize{Session: session, Cols: cols, Rows: rows}
	if !wire.ValidExecResize(r) {
		return errors.New("invalid terminal size")
	}
	return a.send(ctx, wire.HubMessage{Command: &wire.Command{ID: newID(), ExecResize: &r}})
}

// Stack starts a compose operation on the host. Its output arrives on the
// returned channel, which closes after a final chunk with EOF set; that
// chunk's Error is the outcome ("" for success, ErrOffline if the host
// disconnected). The hub never cancels it: stopping a half-finished deploy
// would leave the stack in a worse state than letting it finish.
func (a *Agent) Stack(ctx context.Context, c wire.StackCommand) (<-chan wire.LogChunk, error) {
	if err := wire.ValidStackCommand(c); err != nil {
		return nil, err
	}
	cmd := wire.Command{Stack: &c}
	a.addAuth(ctx, &cmd)
	_, ch, _, err := a.stream(ctx, cmd, streamStack)
	return ch, err
}

// deliverFinal sends the last chunk of a stream even when the buffer is full,
// by dropping the oldest buffered chunk. Callers hold a.mu, and only holders
// of a.mu send on these channels, so after one receive there is room.
func deliverFinal(ch chan wire.LogChunk, c wire.LogChunk) {
	select {
	case ch <- c:
		return
	default:
	}
	select {
	case <-ch:
	default:
	}
	ch <- c
}

// stream sends a command whose output comes back as LogChunks.
func (a *Agent) stream(ctx context.Context, cmd wire.Command, kind streamKind) (string, <-chan wire.LogChunk, func(), error) {
	id := newID()
	cmd.ID = id
	size := streamBuffer
	if kind == streamExec {
		size = execBuffer
	}
	ch := make(chan wire.LogChunk, size)
	a.mu.Lock()
	select {
	case <-a.done:
		a.mu.Unlock()
		return "", nil, nil, ErrOffline
	default:
	}
	if max, err := kind.limit(); a.open[kind] >= max {
		a.mu.Unlock()
		return "", nil, nil, err
	}
	a.logs[id] = ch
	a.kinds[id] = kind
	a.open[kind]++
	a.mu.Unlock()
	if err := a.send(ctx, wire.HubMessage{Command: &cmd}); err != nil {
		a.mu.Lock()
		a.dropStream(id)
		a.mu.Unlock()
		return "", nil, nil, err
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			a.mu.Lock()
			_, running := a.logs[id]
			a.dropStream(id)
			a.mu.Unlock()
			if running {
				sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = a.send(sctx, wire.HubMessage{Command: &wire.Command{ID: newID(), Cancel: id}})
			}
		})
	}
	return id, ch, stop, nil
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
