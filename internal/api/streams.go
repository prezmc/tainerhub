package api

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/prezmc/tainerhub/internal/store"
)

// Long-lived streams (container logs, terminals, job output) are authorized
// once when they open. Without a re-check they would keep running after the
// user is removed, demoted or signed out elsewhere, so every such stream
// re-validates its session on a timer, like /api/events does.

// streamRecheck is how often an open stream re-validates its session.
var streamRecheck = time.Minute

// stream is need() for long-lived responses: the handler's request context
// is cancelled as soon as the session stops passing the role check, which
// ends the stream (and, for logs and terminals, the agent-side session).
func (s *Server) stream(role string, sc scope, fn http.HandlerFunc) http.Handler {
	return s.need(role, sc, s.rechecked(role, fn))
}

// rechecked runs fn with a context that is cancelled once the caller's
// session no longer holds role (see stillAllowed).
func (s *Server) rechecked(role string, fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me := userFrom(r)
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		every := streamRecheck
		go func() {
			t := time.NewTicker(every)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if !s.stillAllowed(r, me, role) {
						cancel()
						return
					}
				}
			}
		}()
		fn(w, r.WithContext(ctx))
	}
}

// stillAllowed re-reads the session behind r: it must still be valid, belong
// to the same user, and that user must not have been demoted (a promotion is
// fine) nor fallen below role, nor lost any host they could reach.
func (s *Server) stillAllowed(r *http.Request, me store.User, role string) bool {
	whoami := s.whoami
	if whoami == nil {
		whoami = s.Auth.UserFromRequest
	}
	u, err := whoami(r)
	if err != nil || u.ID != me.ID {
		return false
	}
	return roleRank[u.Role] >= roleRank[me.Role] && roleRank[u.Role] >= roleRank[role] && sameScope(me, u)
}

// Per-user caps on open log streams. The registry caps streams per host for
// everyone together; without a per-user limit, one viewer opening many tabs
// could take every slot on a host and lock others (and terminals) out.
const (
	maxLogStreamsPerHost = 4
	maxLogStreamsPerUser = 8
)

type logStreamCounts struct {
	mu     sync.Mutex
	byUser map[string]int
	byHost map[[2]string]int // {userID, hostID}
}

// acquire takes a slot for userID on hostID, reporting false if either cap
// is reached. Each successful acquire must be paired with release.
func (c *logStreamCounts) acquire(userID, hostID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byUser == nil {
		c.byUser, c.byHost = map[string]int{}, map[[2]string]int{}
	}
	k := [2]string{userID, hostID}
	if c.byUser[userID] >= maxLogStreamsPerUser || c.byHost[k] >= maxLogStreamsPerHost {
		return false
	}
	c.byUser[userID]++
	c.byHost[k]++
	return true
}

func (c *logStreamCounts) release(userID, hostID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := [2]string{userID, hostID}
	if c.byHost[k]--; c.byHost[k] <= 0 {
		delete(c.byHost, k)
	}
	if c.byUser[userID]--; c.byUser[userID] <= 0 {
		delete(c.byUser, userID)
	}
}
