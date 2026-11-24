package api

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/prezmc/tainerhub/internal/auth"
	"github.com/prezmc/tainerhub/internal/store"
)

// Users: admins invite people with a role through a one-time link, change
// roles and remove users. Nobody can change or remove the owner, their own
// account, or anyone of equal or higher rank; nobody can hand out a role
// above their own.

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
		out = append(out, view{User: u, HasPassword: u.PasswordHash != "", Manageable: u.ID != me.ID && roleRank[u.Role] < roleRank[me.Role]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out, "invites": nonNil(invites)})
}

func (s *Server) createInvite(w http.ResponseWriter, r *http.Request) {
	// An invite is lasting access: a stolen session must not mint them.
	if !s.fresh(w, r) {
		return
	}
	var in struct{ Role, Note string }
	if !readJSON(w, r, &in) {
		return
	}
	me := userFrom(r)
	if !assignable[in.Role] || roleRank[in.Role] > roleRank[me.Role] {
		jsonErr(w, http.StatusBadRequest, "pick a role: viewer, operator or admin (no higher than your own)")
		return
	}
	in.Note = truncate(strings.TrimSpace(in.Note), 100)
	token, hash := auth.NewInviteToken()
	inv, err := s.Store.CreateInvite(r.Context(), hash, in.Role, in.Note, me.ID, time.Now().Add(inviteTTL))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.record(r, &me, "user.invited", in.Note, map[string]string{"role": in.Role})
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
	if roleRank[t.Role] >= roleRank[me.Role] {
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
	if err := s.Store.SetUserRole(r.Context(), t.ID, t.Role, in.Role, invitesRevokedFor(in.Role)); err != nil {
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
	if err := s.Store.DeleteUser(r.Context(), t.ID, t.Role); err != nil {
		s.userChangeFailed(w, err)
		return
	}
	me := userFrom(r)
	s.record(r, &me, "user.removed", t.Username, map[string]string{"role": t.Role})
	w.WriteHeader(http.StatusNoContent)
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
	s.record(r, &u, "user.joined", u.Username, map[string]string{"role": u.Role})
	writeJSON(w, http.StatusOK, map[string]any{"user": u, "recoveryCodes": codes})
}
