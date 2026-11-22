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
)

// MaxLogStreams caps concurrent log streams per host to protect the agent.
const MaxLogStreams = 16

type Registry struct {
	mu     sync.RWMutex
	agents map[string]*Agent
	// OnEvent, if set, is called for every Docker event an agent reports.
	OnEvent func(hostID string, ev wire.DockerEvent)

	chMu    sync.Mutex
	changed chan struct{}

	actMu    sync.Mutex
	activity []Activity // newest last, capped at MaxActivity
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
	pending    map[string]chan wire.CommandResult
	logs       map[string]chan wire.LogChunk
}

// Attach registers a newly connected agent, replacing (and disconnecting)
// any previous connection for the same host. The returned agent's Out()
// channel must be drained by the connection's writer until Done() closes.
func (r *Registry) Attach(hostID, name string, cancel context.CancelFunc) *Agent {
	a := &Agent{
		HostID:      hostID,
		Name:        name,
		ConnectedAt: time.Now(),
		lastSeen:    time.Now(),
		out:         make(chan wire.HubMessage, 64),
		done:        make(chan struct{}),
		cancel:      cancel,
		notify:      r.notify,
		reg:         r,
		pending:     map[string]chan wire.CommandResult{},
		logs:        map[string]chan wire.LogChunk{},
	}
	r.mu.Lock()
	old := r.agents[hostID]
	r.agents[hostID] = a
	r.mu.Unlock()
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

// Disconnect forcibly drops a host's connection (used on revocation).
func (r *Registry) Disconnect(hostID string) {
	r.mu.RLock()
	a := r.agents[hostID]
	r.mu.RUnlock()
	if a != nil {
		a.cancel()
	}
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
		select {
		case ch <- wire.LogChunk{CommandID: id, EOF: true, Error: ErrOffline.Error()}:
		default:
		}
		close(ch)
		delete(a.logs, id)
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
		changed = !reflect.DeepEqual(a.containers, m.Snapshot.Containers)
		a.containers = m.Snapshot.Containers
		a.snapshotAt = time.Unix(m.Snapshot.At, 0)
	case m.Result != nil:
		if ch, ok := a.pending[m.Result.CommandID]; ok {
			ch <- *m.Result
			delete(a.pending, m.Result.CommandID)
		}
	case m.Log != nil:
		if ch, ok := a.logs[m.Log.CommandID]; ok {
			select {
			case ch <- *m.Log:
			default: // slow browser: drop rather than stall the whole stream
			}
			if m.Log.EOF {
				close(ch)
				delete(a.logs, m.Log.CommandID)
			}
		}
	}
	a.mu.Unlock()
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
func (a *Agent) Operate(ctx context.Context, op wire.Operation) (json.RawMessage, error) {
	if err := wire.ValidOperation(op); err != nil {
		return nil, err
	}
	timeout := 70 * time.Second
	if op.Kind == wire.OpImagePull {
		timeout = 16 * time.Minute
	}
	res, err := a.call(ctx, wire.Command{Op: &op}, timeout)
	return res.Data, err
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
	id := newID()
	ch := make(chan wire.LogChunk, 256)
	a.mu.Lock()
	select {
	case <-a.done:
		a.mu.Unlock()
		return nil, nil, ErrOffline
	default:
	}
	if len(a.logs) >= MaxLogStreams {
		a.mu.Unlock()
		return nil, nil, ErrTooManyLogs
	}
	a.logs[id] = ch
	a.mu.Unlock()
	if err := a.send(ctx, wire.HubMessage{Command: &wire.Command{ID: id, Logs: &req}}); err != nil {
		a.mu.Lock()
		delete(a.logs, id)
		a.mu.Unlock()
		return nil, nil, err
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			a.mu.Lock()
			_, running := a.logs[id]
			delete(a.logs, id)
			a.mu.Unlock()
			if running {
				sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = a.send(sctx, wire.HubMessage{Command: &wire.Command{ID: newID(), Cancel: id}})
			}
		})
	}
	return ch, stop, nil
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
