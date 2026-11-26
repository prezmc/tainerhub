package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prezmc/tainerhub/internal/registry"
	"github.com/prezmc/tainerhub/internal/wire"
)

// Terminals: an admin opens one with POST, then reads its output over
// Server-Sent Events and sends keystrokes and size changes with POSTs. Only
// admins can, because a shell in a container sees everything it holds,
// including the environment values that are admin-only elsewhere.

type execSession struct {
	id      string
	userID  string
	agent   *registry.Agent
	remote  string // the agent's session (command) ID
	ch      <-chan wire.LogChunk
	stop    func()
	created time.Time

	mu       sync.Mutex
	attached bool
}

type execList struct {
	mu sync.Mutex
	m  map[string]*execSession
}

func (l *execList) add(e *execSession) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[string]*execSession{}
	}
	l.m[e.id] = e
}

func (l *execList) take(id string) {
	l.mu.Lock()
	e := l.m[id]
	delete(l.m, id)
	l.mu.Unlock()
	if e != nil {
		e.stop()
	}
}

// get returns the session if it belongs to this user.
func (l *execList) get(id, userID string) *execSession {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.m[id]
	if e == nil || e.userID != userID {
		return nil
	}
	return e
}

// shellCmd turns the UI's choice into a command line.
func shellCmd(shell string) []string {
	switch s := strings.TrimSpace(shell); s {
	case "", "auto":
		return nil // agent default: bash if present, else sh
	case "bash", "sh", "ash", "zsh":
		return []string{s}
	default:
		return strings.Fields(s)
	}
}

func (s *Server) execOpen(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Shell      string `json:"shell"`
		Cols, Rows int
	}
	if !readJSON(w, r, &in) {
		return
	}
	a := s.Reg.Get(r.PathValue("id"))
	if a == nil {
		jsonErr(w, http.StatusServiceUnavailable, "host is offline")
		return
	}
	start := wire.ExecStart{ContainerID: r.PathValue("cid"), Cmd: shellCmd(in.Shell), Cols: in.Cols, Rows: in.Rows}
	if err := wire.ValidExecStart(start); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	remote, ch, stop, err := a.Exec(r.Context(), start)
	if errors.Is(err, registry.ErrTooManyLogs) {
		jsonErr(w, http.StatusTooManyRequests, "too many terminals and log streams are open on this host; close some and retry")
		return
	}
	if err != nil {
		jsonErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	u := userFrom(r)
	e := &execSession{id: randID(), userID: u.ID, agent: a, remote: remote, ch: ch, stop: stop, created: time.Now()}
	s.execs.add(e)
	name := start.ContainerID
	for _, c := range a.Status().Containers {
		if c.ID == start.ContainerID {
			name = c.Name
		}
	}
	cmd := "auto (bash or sh)"
	if len(start.Cmd) > 0 {
		cmd = strings.Join(start.Cmd, " ")
	}
	s.record(r, &u, "container.exec", a.Name+"/"+name, map[string]string{"host": a.Name, "container": name, "command": cmd})
	// Nobody attached within 30 s (tab closed mid-open): end it.
	time.AfterFunc(30*time.Second, func() {
		e.mu.Lock()
		attached := e.attached
		e.mu.Unlock()
		if !attached {
			s.execs.take(e.id)
		}
	})
	writeJSON(w, http.StatusCreated, map[string]string{"session": e.id})
}

// execStream sends terminal output as "out" events (base64, since terminal
// bytes aren't always valid UTF-8) and one "end" event. Closing the stream
// closes the terminal.
func (s *Server) execStream(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	e := s.execs.get(r.PathValue("eid"), u.ID)
	if e == nil {
		jsonErr(w, http.StatusNotFound, "terminal not found or already closed")
		return
	}
	e.mu.Lock()
	if e.attached {
		e.mu.Unlock()
		jsonErr(w, http.StatusConflict, "this terminal is already open in another tab")
		return
	}
	e.attached = true
	e.mu.Unlock()
	defer s.execs.take(e.id)

	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "event: ready\ndata: {}\n\n")
	_ = rc.Flush()

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
		case c, ok := <-e.ch:
			if !ok {
				fmt.Fprint(w, "event: end\ndata: {}\n\n")
				_ = rc.Flush()
				return
			}
			if len(c.Data) > 0 {
				fmt.Fprintf(w, "event: out\ndata: %s\n\n", base64.StdEncoding.EncodeToString(c.Data))
			}
			if c.EOF {
				b, _ := json.Marshal(map[string]string{"error": c.Error})
				fmt.Fprintf(w, "event: end\ndata: %s\n\n", b)
				_ = rc.Flush()
				return
			}
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}

func (s *Server) execInput(w http.ResponseWriter, r *http.Request) {
	e := s.execs.get(r.PathValue("eid"), userFrom(r).ID)
	if e == nil {
		jsonErr(w, http.StatusNotFound, "terminal not found or already closed")
		return
	}
	var in struct {
		Data string `json:"data"` // base64
	}
	if !readJSON(w, r, &in) {
		return
	}
	b, err := base64.StdEncoding.DecodeString(in.Data)
	if err != nil || len(b) > 32<<10 {
		jsonErr(w, http.StatusBadRequest, "invalid input")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := e.agent.ExecInput(ctx, e.remote, b); err != nil {
		jsonErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) execResize(w http.ResponseWriter, r *http.Request) {
	e := s.execs.get(r.PathValue("eid"), userFrom(r).ID)
	if e == nil {
		jsonErr(w, http.StatusNotFound, "terminal not found or already closed")
		return
	}
	var in struct{ Cols, Rows int }
	if !readJSON(w, r, &in) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := e.agent.ExecResize(ctx, e.remote, in.Cols, in.Rows); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) execClose(w http.ResponseWriter, r *http.Request) {
	if e := s.execs.get(r.PathValue("eid"), userFrom(r).ID); e != nil {
		s.execs.take(e.id)
	}
	w.WriteHeader(http.StatusNoContent)
}
