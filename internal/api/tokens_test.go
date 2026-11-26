package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prezmc/tainerhub/internal/auth"
	"github.com/prezmc/tainerhub/internal/store"
)

// The CSRF header and Origin checks are skipped only for bearer requests,
// which are never authenticated by cookies.
func TestCSRFSkippedOnlyForBearer(t *testing.T) {
	s := &Server{Auth: &auth.Service{}}
	reached := false
	h := s.secure(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	for name, c := range map[string]struct {
		set  func(r *http.Request)
		pass bool
	}{
		"cookie, no header":    {func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "th_session", Value: "x"}) }, false},
		"cookie, cross-origin": {func(r *http.Request) { r.Header.Set("X-TainerHub", "1"); r.Header.Set("Origin", "http://evil.example") }, false},
		"bearer, no header":    {func(r *http.Request) { r.Header.Set("Authorization", "Bearer thp_x") }, true},
		"bearer, cross-origin": {func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer thp_x")
			r.Header.Set("Origin", "http://evil.example")
		}, true},
		"basic auth, no header": {func(r *http.Request) { r.Header.Set("Authorization", "Basic eDp5") }, false},
		"cookie, with header":   {func(r *http.Request) { r.Header.Set("X-TainerHub", "1") }, true},
	} {
		reached = false
		r := httptest.NewRequest(http.MethodPost, "http://192.168.1.10:8585/api/hosts/x/containers/y/restart", nil)
		c.set(r)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if reached != c.pass {
			t.Errorf("%s: reached handler = %v (status %d), want %v", name, reached, w.Code, c.pass)
		}
	}
}

// need() answers 401 for bad tokens, 429 when rate limited, and marks the
// request context with the token it was authenticated by.
func TestNeedWithToken(t *testing.T) {
	for name, c := range map[string]struct {
		err  error
		want int
	}{
		"bad token":    {auth.ErrBadToken, http.StatusUnauthorized},
		"rate limited": {auth.ErrRateLimited, http.StatusTooManyRequests},
		"no session":   {auth.ErrUnauthorized, http.StatusUnauthorized},
	} {
		s := &Server{authn: func(*http.Request) (store.User, *auth.TokenInfo, error) { return store.User{}, nil, c.err }}
		w := httptest.NewRecorder()
		s.need("viewer", scopeAny, func(http.ResponseWriter, *http.Request) { t.Fatal("handler ran") }).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
		if w.Code != c.want || !strings.Contains(w.Body.String(), `"error"`) {
			t.Errorf("%s: %d %s, want %d with a JSON error", name, w.Code, w.Body.String(), c.want)
		}
	}

	s := &Server{authn: func(*http.Request) (store.User, *auth.TokenInfo, error) {
		return store.User{ID: "u1", Username: "alex", Role: "viewer"}, &auth.TokenInfo{ID: "t1", Name: "backup"}, nil
	}}
	var got *auth.TokenInfo
	ok := s.need("viewer", scopeAny, func(w http.ResponseWriter, r *http.Request) { got = auth.TokenFrom(r.Context()) })
	ok.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if got == nil || got.Name != "backup" {
		t.Fatalf("token in context = %+v", got)
	}
	// The effective role (already lowered by auth) still goes through the role check.
	w := httptest.NewRecorder()
	s.need("operator", scopeAny, func(http.ResponseWriter, *http.Request) { t.Fatal("viewer token ran an operator handler") }).ServeHTTP(w, httptest.NewRequest("POST", "/", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", w.Code)
	}
}

func withUser(r *http.Request, u store.User, tok *auth.TokenInfo) *http.Request {
	ctx := context.WithValue(r.Context(), ctxKey{}, u)
	if tok != nil {
		ctx = auth.WithToken(ctx, tok)
	}
	return r.WithContext(ctx)
}

// Anything that mints lasting access refuses token-authenticated requests
// with a clear 403, even when the session check would pass. The Server has
// no Store, so reaching the database would panic.
func TestTokenCantMintAccess(t *testing.T) {
	s := &Server{freshCheck: func(*http.Request) error { return nil }}
	owner := store.User{ID: "u1", Role: "owner", Username: "ann"}
	for name, h := range map[string]http.HandlerFunc{
		"createInvite":   s.createInvite,
		"setRole":        s.setRole,
		"deleteUser":     s.deleteUser,
		"httpsEnable":    s.httpsEnable,
		"httpsDisable":   s.httpsDisable,
		"createHost":     s.createHost,
		"newHostToken":   s.newHostToken,
		"addPasskey":     s.addPasskeyBegin,
		"deletePasskey":  s.deletePasskey,
		"regenRecovery":  s.regenRecovery,
		"changePassword": s.changePassword,
		"signOutOthers":  s.signOutOthers,
		"createToken":    s.createToken,
		"revokeMyToken":  s.revokeMyToken,
		"revokeAnyToken": s.revokeAnyToken,
	} {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"role":"viewer","name":"x","url":"https://hub.example"}`))
		r.SetPathValue("id", "6f1c2f2e-6a8e-4a43-9a65-1d0f6ad7b0a1")
		r.SetPathValue("uid", "6f1c2f2e-6a8e-4a43-9a65-1d0f6ad7b0a2")
		r.SetPathValue("tid", "6f1c2f2e-6a8e-4a43-9a65-1d0f6ad7b0a3")
		r = withUser(r, owner, &auth.TokenInfo{ID: "t1", Name: "ci"})
		w := httptest.NewRecorder()
		h(w, r)
		var body struct {
			Error  string
			Reauth bool
		}
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if w.Code != http.StatusForbidden || body.Reauth || !strings.Contains(body.Error, "API token") {
			t.Errorf("%s: status %d body %s, want 403 naming the API token", name, w.Code, w.Body.String())
		}
	}
}

// Creating a token needs a fresh sign-in (from a session cookie).
func TestCreateTokenNeedsFreshSignIn(t *testing.T) {
	s := &Server{freshCheck: func(*http.Request) error { return auth.ErrReauth }}
	r := withUser(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"ci","role":"viewer"}`)), store.User{ID: "u1", Role: "admin"}, nil)
	w := httptest.NewRecorder()
	s.createToken(w, r)
	var body struct{ Reauth bool }
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != http.StatusForbidden || !body.Reauth {
		t.Fatalf("status %d body %s, want 403 with reauth", w.Code, w.Body.String())
	}
	// No session cookie at all (e.g. a bearer request): not signed in.
	s.freshCheck = func(*http.Request) error { return auth.ErrUnauthorized }
	w = httptest.NewRecorder()
	s.createToken(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", w.Code)
	}
}

func TestNewTokenValidation(t *testing.T) {
	for _, c := range []struct {
		in     newTokenIn
		caller string
		ok     bool
	}{
		{newTokenIn{Name: "backup", Role: "operator", ExpiresInDays: 90}, "admin", true},
		{newTokenIn{Name: "backup", Role: "owner"}, "owner", true},
		{newTokenIn{Name: "x", Role: "admin", ExpiresInDays: 3650}, "admin", true},
		{newTokenIn{Name: "backup", Role: "admin"}, "operator", false}, // above own role
		{newTokenIn{Name: "backup", Role: "root"}, "owner", false},
		{newTokenIn{Name: "  ", Role: "viewer"}, "viewer", false},
		{newTokenIn{Name: strings.Repeat("n", 61), Role: "viewer"}, "viewer", false},
		{newTokenIn{Name: "ci", Role: "viewer", ExpiresInDays: 3651}, "viewer", false},
		{newTokenIn{Name: "ci", Role: "viewer", ExpiresInDays: -1}, "viewer", false},
	} {
		if _, err := c.in.check(c.caller); (err == nil) != c.ok {
			t.Errorf("%+v by %s: err %v, want ok=%v", c.in, c.caller, err, c.ok)
		}
	}
}

// Audit entries made with a token keep the username as actor and name the
// token in the detail.
func TestAuditIncludesTokenName(t *testing.T) {
	u := store.User{ID: "u1", Username: "alex"}
	r := withUser(httptest.NewRequest("POST", "/", nil), u, &auth.TokenInfo{ID: "t1", Name: "backup"})
	e := auditEntry(r, &u, "container.restart", "web", map[string]string{"host": "nas"})
	var d map[string]string
	if err := json.Unmarshal(e.Detail, &d); err != nil {
		t.Fatal(err)
	}
	if e.Actor != "alex" || d["token"] != "backup" || d["host"] != "nas" {
		t.Fatalf("actor %q detail %s", e.Actor, e.Detail)
	}
	e = auditEntry(r, &u, "stack.deploy", "web", nil)
	if string(e.Detail) != `{"token":"backup"}` {
		t.Fatalf("detail %s", e.Detail)
	}
	// Browser sessions are unchanged.
	r = withUser(httptest.NewRequest("POST", "/", nil), u, nil)
	if e := auditEntry(r, &u, "stack.deploy", "web", nil); e.Detail != nil {
		t.Fatalf("detail %s, want none", e.Detail)
	}
}

// Admins see and revoke tokens by the user-management rank rules.
func TestTokenVisible(t *testing.T) {
	admin := store.User{ID: "a", Role: "admin"}
	owner := store.User{ID: "o", Role: "owner"}
	for _, c := range []struct {
		me             store.User
		ownerID, oRole string
		want           bool
	}{
		{admin, "a", "admin", true}, // own
		{admin, "x", "operator", true},
		{admin, "x", "admin", false}, // peer
		{admin, "o", "owner", false},
		{owner, "x", "admin", true},
		{owner, "o", "owner", true},
	} {
		if got := tokenVisible(c.me, c.ownerID, c.oRole); got != c.want {
			t.Errorf("%s sees %s's (%s) token: %v, want %v", c.me.Role, c.ownerID, c.oRole, got, c.want)
		}
	}
}
