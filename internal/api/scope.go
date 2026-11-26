package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/prezmc/tainerhub/internal/store"
)

// Host-scoped access. An admin can limit a user (or an invite) to some
// hosts (store.User.Scoped). That user's role then applies on those hosts
// only, and every other host is hidden: lists leave it out and requests
// naming it get 404, as if it didn't exist.
//
// Every route says which hosts it concerns (its scope), and need() enforces
// that in one place, so no handler can forget. TestRouteScopes checks the
// table: a route that takes a host or stack ID in its path must be scopeHost
// or scopeStack (or scopeHub), and anything else must be listed there.
//
// Hub-wide pages (users, settings, registries, everyone's API tokens, HTTPS,
// adding and removing hosts, the automatic update window) belong to admins
// with every host: a scoped admin gets 403. A scoped admin is a host admin:
// terminals, compose files, volumes and Docker upgrades on their hosts only.
// On the hub's own server they act as an operator at most, because root on
// that machine (a compose file can mount /) is root on the hub, and admins
// see container settings such as the hub's database password.

type scope int

const (
	scopeUnset    scope = iota
	scopeAny            // not about any host: the caller's own account, Docker Hub lookups, their own terminals
	scopeHub            // hub-wide: users limited to some hosts get 403
	scopeHost           // the {id} path value is a host: other hosts get 404
	scopeStack          // the {sid} path value is a stack: stacks on other hosts get 404
	scopeFiltered       // the handler filters lists by host, or checks the hosts named in the query or body
)

func (sc scope) String() string {
	return [...]string{"unset", "any", "hub", "host", "stack", "filtered"}[sc]
}

// A route is one entry of the API table (see routes).
type route struct {
	pattern string
	role    string
	scope   scope
	stream  bool // long-lived response: re-checks the session (see streams.go)
	fn      http.HandlerFunc
}

// access is what need() worked out about a request: the route's role, and
// the hosts the request concerns, which the audit log records. Handlers that
// check hosts named in the body add theirs through allowHosts.
type access struct {
	role  string
	hosts []string
}

type accessKey struct{}

func accessFrom(r *http.Request) *access {
	if a, ok := r.Context().Value(accessKey{}).(*access); ok {
		return a
	}
	return &access{}
}

func (a *access) add(id string) {
	if !validUUID(id) { // the audit column is uuid[]
		return
	}
	for _, h := range a.hosts {
		if h == id {
			return
		}
	}
	a.hosts = append(a.hosts, id)
}

// errHubOnly is the 403 for hub-wide routes.
const errHubOnly = "only an admin with access to every host can do that"

// authorize applies the route's scope after the role check. It answers the
// request itself and returns false when the caller may not go on; on true,
// r carries the (possibly reduced, see hostRole) user.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, sc scope) (*http.Request, bool) {
	u := userFrom(r)
	switch sc {
	case scopeUnset:
		// A route without a scope is a programming error (TestRouteScopes
		// catches it); refuse rather than guess.
		jsonErr(w, http.StatusForbidden, "this route has no access rule")
		return r, false
	case scopeHub:
		if u.Scoped {
			jsonErr(w, http.StatusForbidden, errHubOnly)
			return r, false
		}
	case scopeHost:
		if !s.allowHosts(w, r, r.PathValue("id")) {
			return r, false
		}
	case scopeStack:
		id := r.PathValue("sid")
		if !validUUID(id) {
			jsonErr(w, http.StatusNotFound, "stack not found")
			return r, false
		}
		st, err := s.aus().GetStack(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) || (err == nil && !u.CanSeeHost(st.HostID)) {
			jsonErr(w, http.StatusNotFound, "stack not found")
			return r, false
		}
		if err != nil {
			s.fail(w, err)
			return r, false
		}
		if !s.allowHosts(w, r, st.HostID) {
			return r, false
		}
	}
	// On the hub's own server a scoped admin counts as an operator, so the
	// handler's own role checks (revealing env values, compose files) see
	// the lower role too.
	if role := s.hostRole(u, accessFrom(r).hosts); role != u.Role {
		if roleRank[role] < roleRank[accessFrom(r).role] {
			jsonErr(w, http.StatusForbidden, "admins limited to some hosts can't do admin tasks on the hub's own server")
			return r, false
		}
		u.Role = role
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, u))
	}
	return r, true
}

// hostRole is u's role for a request about hosts: a scoped admin is an
// operator on the hub's own server (see the comment at the top).
func (s *Server) hostRole(u store.User, hosts []string) string {
	if !u.Scoped || roleRank[u.Role] < roleRank["admin"] || len(hosts) == 0 {
		return u.Role
	}
	local := s.localHostID()
	for _, h := range hosts {
		if h != "" && h == local {
			return "operator"
		}
	}
	return u.Role
}

// allowHosts checks the caller may reach every host in ids, answering 404
// ("host not found") if not, and records them as hosts the request concerns.
// Handlers call it for hosts named in the query or body (a move's target, a
// new stack's host); need() calls it for the path. For an admin-only route a
// scoped admin is refused on the hub's own server (see hostRole).
func (s *Server) allowHosts(w http.ResponseWriter, r *http.Request, ids ...string) bool {
	u := userFrom(r)
	acc := accessFrom(r)
	for _, id := range ids {
		if !u.CanSeeHost(id) {
			jsonErr(w, http.StatusNotFound, "host not found")
			return false
		}
		acc.add(id)
	}
	if roleRank[s.hostRole(u, ids)] < roleRank[acc.role] {
		jsonErr(w, http.StatusForbidden, "admins limited to some hosts can't do admin tasks on the hub's own server")
		return false
	}
	return true
}

// visible keeps the items on hosts u may see.
func visible[T any](u store.User, items []T, host func(T) string) []T {
	if !u.Scoped {
		return items
	}
	out := make([]T, 0, len(items))
	for _, it := range items {
		if u.CanSeeHost(host(it)) {
			out = append(out, it)
		}
	}
	return out
}

// sameScope reports whether a stream opened as me may go on for now (the
// same user, re-read): it ends once any host it could see is out of reach.
func sameScope(me, now store.User) bool {
	if !now.Scoped {
		return true
	}
	if !me.Scoped {
		return false
	}
	for _, h := range me.Hosts {
		if !now.CanSeeHost(h) {
			return false
		}
	}
	return true
}
