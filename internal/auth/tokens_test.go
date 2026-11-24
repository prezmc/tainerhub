package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/store"
)

type tokenRow struct {
	t store.APIToken
	u store.User
}

func (f *fakeStore) APITokenUser(ctx context.Context, tokenHash []byte) (store.APIToken, store.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.tokens[string(tokenHash)]
	if !ok {
		return store.APIToken{}, store.User{}, store.ErrNotFound
	}
	return row.t, row.u, nil
}

func (f *fakeStore) TouchAPIToken(ctx context.Context, id, ip string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touches++
	return nil
}

// addToken stores a new token with role for a user with userRole.
func (f *fakeStore) addToken(role, userRole string, expires *time.Time) string {
	tok, prefix, hash := NewAPIToken()
	if f.tokens == nil {
		f.tokens = map[string]tokenRow{}
	}
	f.tokens[string(hash)] = tokenRow{
		t: store.APIToken{ID: "t-" + prefix, Name: "backup", Prefix: prefix, Role: role, ExpiresAt: expires},
		u: store.User{ID: "u1", Username: "alex", Role: userRole},
	}
	return tok
}

func bearerRequest(tok string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/hosts", nil)
	r.RemoteAddr = "192.0.2.20:1234"
	r.Header.Set("Authorization", "Bearer "+tok)
	return r
}

func TestNewAPIToken(t *testing.T) {
	tok, prefix, hash := NewAPIToken()
	if !strings.HasPrefix(tok, "thp_") || len(tok) != 4+43 {
		t.Fatalf("token %q: want thp_ + 43 base64url characters", tok)
	}
	if prefix != tok[4:12] {
		t.Fatalf("prefix %q is not the 8 characters after thp_", prefix)
	}
	if string(hash) != string(Hash(tok)) {
		t.Fatal("hash must be SHA-256 of the whole token")
	}
}

func TestTokenAuth(t *testing.T) {
	f := &fakeStore{}
	s := testService(f, 10)
	tok := f.addToken("operator", "admin", nil)
	u, info, err := s.Authenticate(bearerRequest(tok))
	if err != nil {
		t.Fatal(err)
	}
	if u.Username != "alex" || info == nil || info.Name != "backup" || info.ID == "" {
		t.Fatalf("got user %+v token %+v", u, info)
	}
	if u.Role != "operator" {
		t.Fatalf("role %s: a token must not act above its own role", u.Role)
	}
	if f.touches != 1 {
		t.Fatalf("touches = %d, want 1 (never used before)", f.touches)
	}
	// Lower-case scheme works too.
	r := bearerRequest(tok)
	r.Header.Set("Authorization", "bearer "+tok)
	if _, _, err := s.Authenticate(r); err != nil {
		t.Fatalf("lower-case bearer: %v", err)
	}
}

// A token acts with the lower of its role and its user's current role, so
// demoting a user demotes their tokens.
func TestTokenEffectiveRole(t *testing.T) {
	for _, c := range []struct{ tokRole, userRole, want string }{
		{"admin", "admin", "admin"},
		{"viewer", "owner", "viewer"},
		{"admin", "viewer", "viewer"}, // user demoted after creating the token
		{"owner", "operator", "operator"},
	} {
		f := &fakeStore{}
		s := testService(f, 10)
		u, _, err := s.Authenticate(bearerRequest(f.addToken(c.tokRole, c.userRole, nil)))
		if err != nil || u.Role != c.want {
			t.Errorf("token %s, user %s: got %s (%v), want %s", c.tokRole, c.userRole, u.Role, err, c.want)
		}
	}
}

func TestTokenRejected(t *testing.T) {
	past := time.Now().Add(-time.Second)
	f := &fakeStore{}
	expired := f.addToken("viewer", "viewer", &past)
	revoked := f.addToken("viewer", "viewer", nil)
	for h, row := range f.tokens {
		if row.t.ExpiresAt == nil {
			delete(f.tokens, h) // revoked (or its user deleted: the row cascades away)
		}
	}
	unknown, _, _ := NewAPIToken()
	for name, tok := range map[string]string{
		"expired": expired, "revoked": revoked, "unknown": unknown,
		"no prefix": "abc", "empty": "", "too long": "thp_" + strings.Repeat("a", 200),
	} {
		s := testService(f, 10)
		if _, _, err := s.Authenticate(bearerRequest(tok)); !errors.Is(err, ErrBadToken) {
			t.Errorf("%s token: err %v, want ErrBadToken", name, err)
		}
	}
}

func TestTokenNotExpiredYet(t *testing.T) {
	future := time.Now().Add(time.Hour)
	f := &fakeStore{}
	s := testService(f, 10)
	if _, _, err := s.Authenticate(bearerRequest(f.addToken("viewer", "viewer", &future))); err != nil {
		t.Fatal(err)
	}
}

// Last-used is written at most once a minute per token.
func TestTokenTouchThrottled(t *testing.T) {
	f := &fakeStore{}
	s := testService(f, 10)
	tok := f.addToken("viewer", "viewer", nil)
	for h, row := range f.tokens {
		recent := time.Now().Add(-10 * time.Second)
		row.t.LastUsedAt = &recent
		f.tokens[h] = row
	}
	if _, _, err := s.Authenticate(bearerRequest(tok)); err != nil {
		t.Fatal(err)
	}
	if f.touches != 0 {
		t.Fatalf("touched %d times within a minute of the last use", f.touches)
	}
}

// Wrong tokens count against the sign-in limiter; once it's full even a good
// token is refused with ErrRateLimited (429). Good tokens don't use it up.
func TestTokenRateLimited(t *testing.T) {
	f := &fakeStore{}
	s := testService(f, 3)
	good := f.addToken("viewer", "viewer", nil)
	for i := 0; i < 20; i++ {
		if _, _, err := s.Authenticate(bearerRequest(good)); err != nil {
			t.Fatalf("good token call %d: %v", i, err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, _, err := s.Authenticate(bearerRequest("thp_wrong")); !errors.Is(err, ErrBadToken) {
			t.Fatalf("bad token %d: %v", i, err)
		}
	}
	if _, _, err := s.Authenticate(bearerRequest("thp_wrong")); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("after the limit: %v, want ErrRateLimited", err)
	}
	// A valid token still works from the limited address: only bad attempts
	// are limited, so nobody sharing the address can lock out real scripts.
	if _, _, err := s.Authenticate(bearerRequest(good)); err != nil {
		t.Fatalf("good token from a limited IP: %v, want it accepted", err)
	}
}

// With a bearer token present, cookies are ignored entirely: a bad token
// doesn't fall back to the session, and session-only operations refuse.
func TestBearerIgnoresCookies(t *testing.T) {
	f := &fakeStore{session: store.Session{CreatedAt: time.Now()}}
	s := testService(f, 10)
	r := bearerRequest("thp_wrong")
	r.AddCookie(&http.Cookie{Name: "th_session", Value: "tok"})
	if _, _, err := s.Authenticate(r); !errors.Is(err, ErrBadToken) {
		t.Fatalf("err %v, want ErrBadToken (no cookie fallback)", err)
	}
	if s.sessionHash(r) != nil {
		t.Fatal("sessionHash must ignore cookies on bearer requests")
	}
	if err := s.RequireFreshSignIn(r); !errors.Is(err, ErrTokenAuth) {
		t.Fatalf("RequireFreshSignIn: %v, want ErrTokenAuth", err)
	}
	if _, err := s.SignOutOtherSessions(r, store.User{ID: "u1"}); !errors.Is(err, ErrTokenAuth) {
		t.Fatalf("SignOutOtherSessions: %v, want ErrTokenAuth", err)
	}
	if err := s.ChangePassword(r, store.User{ID: "u1"}, "", "a-long-new-password"); !errors.Is(err, ErrTokenAuth) {
		t.Fatalf("ChangePassword: %v, want ErrTokenAuth", err)
	}
	if f.revokeCalls != 0 {
		t.Fatal("no session may be touched through a bearer request")
	}
}

func TestTokenContext(t *testing.T) {
	ctx := context.Background()
	if TokenFrom(ctx) != nil {
		t.Fatal("plain context reported a token")
	}
	ctx = WithToken(ctx, &TokenInfo{ID: "1", Name: "ci"})
	if got := TokenFrom(ctx); got == nil || got.Name != "ci" {
		t.Fatalf("TokenFrom = %+v", got)
	}
}

// Bad tokens have their own limit: they don't stop people signing in with a
// password from the same address.
func TestBadTokensDontBlockSignIn(t *testing.T) {
	f := &fakeStore{}
	s := testService(f, 3)
	for i := 0; i < 5; i++ {
		_, _, _ = s.Authenticate(bearerRequest("thp_wrong"))
	}
	if !s.Limiter.Allow("192.0.2.20") {
		t.Fatal("bad API tokens used up the sign-in limit")
	}
}
