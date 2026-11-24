package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/prezmc/tainerhub/internal/auth"
	"github.com/prezmc/tainerhub/internal/store"
)

// API tokens: users create them on their Account page for scripts and
// tools. Creating one needs a fresh browser sign-in (a stolen session must
// not mint lasting access); creating or revoking one is never possible with
// a token. Admins can see and revoke the tokens of users ranked below them,
// by the same rules as managing those users.

const maxTokenDays = 3650

// notViaToken answers 403 and returns false when the request was
// authenticated with an API token.
func (s *Server) notViaToken(w http.ResponseWriter, r *http.Request) bool {
	if auth.TokenFrom(r.Context()) != nil {
		jsonErr(w, http.StatusForbidden, auth.ErrTokenAuth.Error())
		return false
	}
	return true
}

type newTokenIn struct {
	Name          string `json:"name"`
	Role          string `json:"role"`
	ExpiresInDays int    `json:"expiresInDays"` // 0: never
}

// check validates a create request from a caller with role callerRole and
// returns the trimmed name.
func (in newTokenIn) check(callerRole string) (string, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" || utf8.RuneCountInString(name) > 60 {
		return "", errors.New("name is required (max 60 characters)")
	}
	if roleRank[in.Role] == 0 || roleRank[in.Role] > roleRank[callerRole] {
		return "", errors.New("pick an access level no higher than your own role")
	}
	if in.ExpiresInDays < 0 || in.ExpiresInDays > maxTokenDays {
		return "", errors.New("expiry must be 1 to 3650 days, or 0 for never")
	}
	return name, nil
}

func (s *Server) listMyTokens(w http.ResponseWriter, r *http.Request) {
	ts, err := s.Store.ListAPITokens(r.Context(), userFrom(r).ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(ts))
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	// fresh also refuses token-authenticated requests and needs a session cookie.
	if !s.fresh(w, r) {
		return
	}
	var in newTokenIn
	if !readJSON(w, r, &in) {
		return
	}
	me := userFrom(r)
	name, err := in.check(me.Role)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var expires *time.Time
	if in.ExpiresInDays > 0 {
		t := time.Now().Add(time.Duration(in.ExpiresInDays) * 24 * time.Hour)
		expires = &t
	}
	token, prefix, hash := auth.NewAPIToken()
	t, err := s.Store.CreateAPIToken(r.Context(), me.ID, name, prefix, in.Role, hash, expires)
	if err != nil {
		s.fail(w, err)
		return
	}
	expiry := "never"
	if in.ExpiresInDays > 0 {
		expiry = strconv.Itoa(in.ExpiresInDays) + " days"
	}
	s.record(r, &me, "token.created", name, map[string]string{"role": in.Role, "expires": expiry})
	// The token itself is returned this once; only its hash is kept.
	writeJSON(w, http.StatusCreated, struct {
		store.APIToken
		Token string `json:"token"`
	}{t, token})
}

func (s *Server) revokeMyToken(w http.ResponseWriter, r *http.Request) {
	if !s.notViaToken(w, r) {
		return
	}
	id := r.PathValue("tid")
	if !validUUID(id) {
		jsonErr(w, http.StatusNotFound, "not found")
		return
	}
	me := userFrom(r)
	name, err := s.Store.DeleteAPIToken(r.Context(), id, me.ID)
	if err != nil {
		s.notFoundOr(w, err)
		return
	}
	s.record(r, &me, "token.revoked", name, nil)
	w.WriteHeader(http.StatusNoContent)
}

// tokenVisible reports whether me may see and revoke a token owned by a
// user with ownerRole: their own, or anyone ranked below them.
func tokenVisible(me store.User, ownerID, ownerRole string) bool {
	return ownerID == me.ID || roleRank[ownerRole] < roleRank[me.Role]
}

func (s *Server) listAllTokens(w http.ResponseWriter, r *http.Request) {
	ts, err := s.Store.ListAllAPITokens(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	me := userFrom(r)
	out := []store.APIToken{}
	for _, t := range ts {
		if tokenVisible(me, t.UserID, t.UserRole) {
			out = append(out, t)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) revokeAnyToken(w http.ResponseWriter, r *http.Request) {
	if !s.notViaToken(w, r) {
		return
	}
	id := r.PathValue("tid")
	if !validUUID(id) {
		jsonErr(w, http.StatusNotFound, "not found")
		return
	}
	t, err := s.Store.GetAPIToken(r.Context(), id)
	if err != nil {
		s.notFoundOr(w, err)
		return
	}
	me := userFrom(r)
	if !tokenVisible(me, t.UserID, t.UserRole) {
		// Same answer as a missing token: don't confirm it exists.
		jsonErr(w, http.StatusNotFound, "not found")
		return
	}
	if err := s.Store.DeleteAPITokenOf(r.Context(), id, t.UserRole); err != nil {
		s.userChangeFailed(w, err)
		return
	}
	s.record(r, &me, "token.revoked", t.Name, map[string]string{"user": t.Username})
	w.WriteHeader(http.StatusNoContent)
}
