package api

import (
	"errors"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/prezmc/tainerhub/internal/auth"
	"github.com/prezmc/tainerhub/internal/store"
)

// Users: admins invite people with a role through a one-time link, change
// roles and remove users. Nobody can change or remove the owner, their own
// account, or anyone of equal or higher rank; nobody can hand out a role
// above their own. An admin can also limit a user or invite to some hosts
// (see scope.go); an admin with every host outranks one limited to some.

const inviteTTL = 24 * time.Hour

var assignable = map[string]bool{"admin": true, "operator": true, "viewer": true}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.Store.ListUsers(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	invites, err := s.Store.PendingInvites(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	me := userFrom(r)
	type view struct {
		store.User
		HasPassword bool `json:"hasPassword"`
		Manageable  bool `json:"manageable"` // this admin may change or remove them
	}
	out := []view{}
	for _, u := range users {
		out = append(out, view{User: u, HasPassword: u.PasswordHash != "", Manageable: u.ID != me.ID && outranks(me, u)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out, "invites": nonNil(invites)})
}

func (s *Server) createInvite(w http.ResponseWriter, r *http.Request) {
	// An invite is lasting access: a stolen session must not mint them.
	if !s.fresh(w, r) {
		return
	}
	var in struct {
		Role, Note string
		Hosts      *[]string // nil: every host
	}
	if !readJSON(w, r, &in) {
		return
	}
	me := userFrom(r)
	if !assignable[in.Role] || roleRank[in.Role] > roleRank[me.Role] {
		jsonErr(w, http.StatusBadRequest, "pick a role: viewer, operator or admin (no higher than your own)")
		return
	}
	in.Note = truncate(strings.TrimSpace(in.Note), 100)
	detail := map[string]any{"role": in.Role}
	var hosts []string
	if in.Hosts != nil {
		ids, names, ok := s.checkHostScope(w, r, in.Role, *in.Hosts)
		if !ok {
			return
		}
		hosts, detail["hosts"] = ids, names
	}
	token, hash := auth.NewInviteToken()
	inv, err := s.Store.CreateInvite(r.Context(), hash, in.Role, in.Note, me.ID, time.Now().Add(inviteTTL), in.Hosts != nil, hosts)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.record(r, &me, "user.invited", in.Note, detail)
	writeJSON(w, http.StatusCreated, map[string]any{"invite": inv, "path": "/invite?token=" + token})
}

func (s *Server) deleteInvite(w http.ResponseWriter, r *http.Request) {
	if !validUUID(r.PathValue("iid")) {
		jsonErr(w, http.StatusNotFound, "not found")
		return
	}
	if err := s.Store.DeleteInvite(r.Context(), r.PathValue("iid")); err != nil {
		s.notFoundOr(w, err)
		return
	}
	me := userFrom(r)
	s.record(r, &me, "user.invite_revoked", r.PathValue("iid"), nil)
	w.WriteHeader(http.StatusNoContent)
}

// manageTarget loads the user in the path and checks the caller may change them.
func (s *Server) manageTarget(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	id := r.PathValue("uid")
	if !validUUID(id) {
		jsonErr(w, http.StatusNotFound, "user not found")
		return store.User{}, false
	}
	t, err := s.Store.UserByID(r.Context(), id)
	if err != nil {
		s.notFoundOr(w, err)
		return store.User{}, false
	}
	me := userFrom(r)
	if t.ID == me.ID {
		jsonErr(w, http.StatusForbidden, "you can't change or remove your own account here")
		return store.User{}, false
	}
	if !outranks(me, t) {
		jsonErr(w, http.StatusForbidden, "you can only manage users with a lower role than yours")
		return store.User{}, false
	}
	return t, true
}

func (s *Server) setRole(w http.ResponseWriter, r *http.Request) {
	if !s.fresh(w, r) {
		return
	}
	t, ok := s.manageTarget(w, r)
	if !ok {
		return
	}
	var in struct{ Role string }
	if !readJSON(w, r, &in) {
		return
	}
	me := userFrom(r)
	if !assignable[in.Role] || roleRank[in.Role] > roleRank[me.Role] {
		jsonErr(w, http.StatusBadRequest, "pick a role: viewer, operator or admin (no higher than your own)")
		return
	}
	if t.Scoped && roleRank[in.Role] >= roleRank["admin"] && slices.Contains(t.Hosts, s.localHostID()) {
		jsonErr(w, http.StatusBadRequest, hubHostAdminMsg)
		return
	}
	if err := s.Store.SetUserRole(r.Context(), t.ID, t.Role, t.Scoped, in.Role, invitesRevokedFor(in.Role)); err != nil {
		s.userChangeFailed(w, err)
		return
	}
	s.record(r, &me, "user.role_changed", t.Username, map[string]string{"from": t.Role, "to": in.Role})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	if !s.fresh(w, r) {
		return
	}
	t, ok := s.manageTarget(w, r)
	if !ok {
		return
	}
	if err := s.Store.DeleteUser(r.Context(), t.ID, t.Role, t.Scoped); err != nil {
		s.userChangeFailed(w, err)
		return
	}
	me := userFrom(r)
	s.record(r, &me, "user.removed", t.Username, map[string]string{"role": t.Role})
	w.WriteHeader(http.StatusNoContent)
}

// setHosts answers PUT /api/users/{uid}/hosts: {"hosts": [ids]} limits the
// user to those hosts, {"hosts": null} gives them every host. Their sessions
// and API tokens follow at once; open streams about other hosts end within
// a minute (see stillAllowed).
func (s *Server) setHosts(w http.ResponseWriter, r *http.Request) {
	if !s.fresh(w, r) {
		return
	}
	t, ok := s.manageTarget(w, r)
	if !ok {
		return
	}
	var in struct {
		Hosts *[]string `json:"hosts"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	var ids []string
	to := any("all hosts")
	if in.Hosts != nil {
		var names []string
		if ids, names, ok = s.checkHostScope(w, r, t.Role, *in.Hosts); !ok {
			return
		}
		to = names
	}
	from := any("all hosts")
	if t.Scoped {
		from = s.hostNames(r, t.Hosts)
	}
	if err := s.Store.SetUserHosts(r.Context(), t.ID, t.Role, t.Scoped, in.Hosts != nil, ids); err != nil {
		s.userChangeFailed(w, err)
		return
	}
	me := userFrom(r)
	s.record(r, &me, "user.hosts_changed", t.Username, map[string]any{"from": from, "to": to})
	w.WriteHeader(http.StatusNoContent)
}

// outranks reports whether me may manage t: a higher role, or the same
// role with every host while t is limited to some.
func outranks(me, t store.User) bool {
	if roleRank[me.Role] != roleRank[t.Role] {
		return roleRank[me.Role] > roleRank[t.Role]
	}
	return !me.Scoped && t.Scoped
}

const hubHostAdminMsg = "an admin limited to some hosts can't include the hub's own server: root there is root on the hub. Give them every host, or make them an operator"

// checkHostScope validates the hosts an admin picked for a user or invite
// with role, answering 400 if they won't do. It returns the IDs, sorted by
// host name, and the names for the audit log.
func (s *Server) checkHostScope(w http.ResponseWriter, r *http.Request, role string, picked []string) ([]string, []string, bool) {
	hs, err := s.Store.ListHosts(r.Context())
	if err != nil {
		s.fail(w, err)
		return nil, nil, false
	}
	ids, names, msg := validateScope(role, picked, hs, s.localHostID())
	if msg != "" {
		jsonErr(w, http.StatusBadRequest, msg)
		return nil, nil, false
	}
	return ids, names, true
}

// validateScope is checkHostScope's rule: at least one host, every one
// known, no repeats, and no admin limited to hosts that include the hub's
// own server (local).
func validateScope(role string, picked []string, all []store.Host, local string) (ids, names []string, problem string) {
	if len(picked) == 0 {
		return nil, nil, "pick at least one host, or give access to all hosts"
	}
	byID := map[string]string{}
	for _, h := range all {
		byID[h.ID] = h.Name
	}
	seen := map[string]bool{}
	for _, id := range picked {
		if _, ok := byID[id]; !ok {
			return nil, nil, "one of the chosen hosts no longer exists; reload and try again"
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		if id == local && roleRank[role] >= roleRank["admin"] {
			return nil, nil, hubHostAdminMsg
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return byID[ids[i]] < byID[ids[j]] })
	for _, id := range ids {
		names = append(names, byID[id])
	}
	return ids, names, ""
}

// hostNames names hosts for the audit log; a removed host shows as its ID.
func (s *Server) hostNames(r *http.Request, ids []string) []string {
	names := make([]string, 0, len(ids))
	hs, _ := s.Store.ListHosts(r.Context())
	byID := map[string]string{}
	for _, h := range hs {
		byID[h.ID] = h.Name
	}
	for _, id := range ids {
		if n, ok := byID[id]; ok {
			names = append(names, n)
		} else {
			names = append(names, id)
		}
	}
	return names
}

// invitesRevokedFor lists the roles of pending invites a user moved to role
// loses: those ranked above the new role, or all of them once the user is no
// longer an admin (only admins may invite at all).
func invitesRevokedFor(role string) []string {
	var out []string
	for r := range assignable {
		if roleRank[r] > roleRank[role] || roleRank[role] < roleRank["admin"] {
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out
}

// userChangeFailed answers a failed role change or removal. ErrChanged means
// the user's role changed after manageTarget checked it, so that permission
// check no longer holds: refuse rather than act on stale data.
func (s *Server) userChangeFailed(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrChanged) {
		jsonErr(w, http.StatusConflict, "this user's role just changed; reload and try again")
		return
	}
	s.notFoundOr(w, err)
}

// ---- public: the invite page ----

func (s *Server) checkInvite(w http.ResponseWriter, r *http.Request) {
	var in struct{ Token string }
	if !readJSON(w, r, &in) {
		return
	}
	role, err := s.Auth.InviteRole(r.Context(), in.Token)
	if errors.Is(err, auth.ErrBadInvite) {
		jsonErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"role": role})
}

func (s *Server) acceptInvite(w http.ResponseWriter, r *http.Request) {
	var in auth.AcceptInvite
	if !readJSON(w, r, &in) {
		return
	}
	u, codes, err := s.Auth.AcceptInvite(w, r, in)
	switch {
	case errors.Is(err, auth.ErrRateLimited):
		jsonErr(w, http.StatusTooManyRequests, err.Error())
		return
	case err != nil:
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	detail := map[string]any{"role": u.Role}
	if u.Scoped {
		detail["hosts"] = s.hostNames(r, u.Hosts)
	}
	s.record(r, &u, "user.joined", u.Username, detail)
	writeJSON(w, http.StatusOK, map[string]any{"user": u, "recoveryCodes": codes})
}
