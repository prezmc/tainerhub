package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// A job is a long-running stack operation. It runs on the hub independently
// of the browser request that started it, so closing the tab doesn't stop a
// deploy halfway, and anyone can reattach to its output while it runs and
// for a while afterwards.
type job struct {
	ID      string    `json:"id"`
	StackID string    `json:"stackId"`
	Stack   string    `json:"stack"`
	HostID  string    `json:"hostId"`
	Host    string    `json:"host"`
	Action  string    `json:"action"`
	Actor   string    `json:"actor"`
	Started time.Time `json:"startedAt"`

	mu       sync.Mutex
	out      []byte // the most recent output, at most maxJobOutput bytes
	dropped  int    // bytes trimmed from the front of out
	done     bool
	err      string
	finished time.Time
	changed  chan struct{} // closed and replaced on every update
}

const (
	maxJobOutput = 1 << 20
	// Once output passes maxJobOutput it's cut back to jobOutputKeep, so the
	// copy happens once per 256 KiB of new output rather than on every write.
	jobOutputKeep = 768 << 10
	jobKeep       = time.Hour // finished jobs stay readable this long
)

func (j *job) append(b []byte) {
	j.mu.Lock()
	j.out = append(j.out, b...)
	if len(j.out) > maxJobOutput {
		over := len(j.out) - jobOutputKeep
		// Slide the tail to the front in place; readers keep absolute
		// offsets, so only dropped needs to move with it.
		j.out = j.out[:copy(j.out, j.out[over:])]
		j.dropped += over
	}
	j.wake()
	j.mu.Unlock()
}

func (j *job) finish(err string) {
	j.mu.Lock()
	j.done, j.err, j.finished = true, err, time.Now()
	j.wake()
	j.mu.Unlock()
}

func (j *job) wake() { close(j.changed); j.changed = make(chan struct{}) }

// read returns output after absolute offset from, the next offset, and state.
func (j *job) read(from int) (data []byte, next int, done bool, errText string, changed <-chan struct{}) {
	j.mu.Lock()
	defer j.mu.Unlock()
	start := max(from-j.dropped, 0)
	data = append([]byte(nil), j.out[min(start, len(j.out)):]...)
	return data, j.dropped + len(j.out), j.done, j.err, j.changed
}

type jobRef struct {
	ID     string `json:"id"`
	Action string `json:"action"`
}

type jobList struct {
	mu   sync.Mutex
	byID map[string]*job
}

// start registers a job unless one is already running for the same stack.
func (l *jobList) start(j *job) (*job, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byID == nil {
		l.byID = map[string]*job{}
	}
	for id, o := range l.byID {
		o.mu.Lock()
		expired := o.done && time.Since(o.finished) > jobKeep
		running := !o.done && o.StackID == j.StackID
		o.mu.Unlock()
		if expired {
			delete(l.byID, id)
		}
		if running {
			return o, false
		}
	}
	j.ID = randID()
	j.Started = time.Now()
	j.changed = make(chan struct{})
	l.byID[j.ID] = j
	return j, true
}

func (l *jobList) get(id string) *job {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.byID[id]
}

// running returns the active job for each stack ID.
func (l *jobList) running() map[string]jobRef {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]jobRef{}
	for _, j := range l.byID {
		j.mu.Lock()
		if !j.done {
			out[j.StackID] = jobRef{ID: j.ID, Action: j.Action}
		}
		j.mu.Unlock()
	}
	return out
}

// jobStream sends a job's output as Server-Sent Events: "output" events with
// a JSON string, then one "done" event with {ok, error}. It replays what has
// been written so far, so a page can reattach at any time.
func (s *Server) jobStream(w http.ResponseWriter, r *http.Request) {
	j := s.jobs.get(r.PathValue("jid"))
	if j == nil {
		jsonErr(w, http.StatusNotFound, "this operation has finished and its output has expired")
		return
	}
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	meta, _ := json.Marshal(j.meta())
	fmt.Fprintf(w, "event: job\ndata: %s\n\n", meta)

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	offset := 0
	for {
		data, next, done, errText, changed := j.read(offset)
		offset = next
		if len(data) > 0 {
			b, _ := json.Marshal(string(data))
			fmt.Fprintf(w, "event: output\ndata: %s\n\n", b)
		}
		if done {
			b, _ := json.Marshal(map[string]any{"ok": errText == "", "error": errText})
			fmt.Fprintf(w, "event: done\ndata: %s\n\n", b)
			_ = rc.Flush()
			return
		}
		if err := rc.Flush(); err != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
		case <-changed:
		}
	}
}

func (j *job) meta() map[string]any {
	return map[string]any{"id": j.ID, "stackId": j.StackID, "stack": j.Stack, "host": j.Host, "action": j.Action, "actor": j.Actor, "startedAt": j.Started}
}
